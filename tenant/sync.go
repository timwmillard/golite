package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"
)

// Sync keeps tenant databases in step with the master database, using
// River jobs on the master database as an outbox: change the master
// database and, in the same transaction, call CreateTx or SyncTx. Each
// inserts a job, so the job exists if and only if the change committed,
// and River retries it until the tenant database matches. A crash between
// the master commit and the tenant write can't lose the write.
//
// Jobs carry the tenant's id, its database's id in DBs, which never
// changes while the app is running. To remove a tenant, use DBs.Delete
// with a commit func that removes it from the master database.
//
// A sync holds the tenant's database while it reads the master database, so
// never hold a master transaction while using a tenant's database: with one
// connection to each, the two could deadlock.
type Sync struct {
	dbs     *DBs
	exists  func(context.Context, string) (bool, error)
	mirrors map[string]Mirror
}

// Mirror copies one kind of master database record into tenant databases.
type Mirror struct {
	// Name identifies the mirror in jobs, e.g. "company" or "member".
	Name string

	// Sync makes the tenant's copy of the record named key match the
	// master database: upsert it if it's there, delete it if not. id is
	// the tenant's, as given to SyncTx. tx is a transaction on the
	// tenant's database, committed if Sync returns nil. Read the master
	// record inside Sync: syncs of the same tenant run one at a time, so
	// reading here means the last to run writes the latest.
	Sync func(ctx context.Context, tx *sql.Tx, id, key string) error
}

// SyncConfig configures a Sync.
type SyncConfig struct {
	DBs *DBs

	// Exists reports whether tenant id is in the master database. Jobs for
	// a tenant that isn't are cancelled.
	Exists func(ctx context.Context, id string) (bool, error)

	Mirrors []Mirror

	// Workers, if set, gets the workers for CreateArgs and SyncArgs. Set
	// it to the registry given to server.New (so NewSync comes first). Leave
	// it nil where nothing works jobs, such as an offline script using Run.
	Workers *river.Workers
}

// NewSync returns a Sync for the given mirrors.
func NewSync(cfg SyncConfig) (*Sync, error) {
	if cfg.DBs == nil || cfg.Exists == nil {
		return nil, errors.New("tenant: SyncConfig needs DBs and Exists")
	}
	s := &Sync{dbs: cfg.DBs, exists: cfg.Exists, mirrors: map[string]Mirror{}}
	for _, m := range cfg.Mirrors {
		if m.Name == "" || m.Sync == nil {
			return nil, errors.New("tenant: Mirror needs Name and Sync")
		}
		if _, dup := s.mirrors[m.Name]; dup {
			return nil, fmt.Errorf("tenant: duplicate mirror %q", m.Name)
		}
		s.mirrors[m.Name] = m
	}
	if cfg.Workers != nil {
		river.AddWorker(cfg.Workers, &createWorker{s: s})
		river.AddWorker(cfg.Workers, &syncWorker{s: s})
	}
	return s, nil
}

// AddStubWorkers registers placeholder workers for CreateArgs and SyncArgs,
// for tests that insert those jobs (River checks a kind has a worker when
// it's inserted) but never work them. A stub worker cancels any job it's
// given. To test a mirror, use Sync.Run.
func AddStubWorkers(workers *river.Workers) {
	river.AddWorker(workers, &stubWorker[CreateArgs]{})
	river.AddWorker(workers, &stubWorker[SyncArgs]{})
}

type stubWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
}

func (*stubWorker[T]) Work(context.Context, *river.Job[T]) error {
	return river.JobCancel(errors.New("tenant: stub worker; see AddStubWorkers"))
}

// CreateArgs is the job CreateTx inserts.
type CreateArgs struct {
	Tenant string `json:"tenant"`
}

func (CreateArgs) Kind() string { return "tenant_create" }

// SyncArgs is the job SyncTx inserts.
//
// It isn't a unique job: River can't dedupe without counting running jobs,
// and an edit made while a sync is running would then be dropped. Each sync
// reads the latest master record, so extras are cheap and harmless.
type SyncArgs struct {
	Mirror string `json:"mirror"`
	Tenant string `json:"tenant"`
	Key    string `json:"key,omitempty"`
}

func (SyncArgs) Kind() string { return "tenant_sync" }

// CreateTx inserts, in master transaction tx, a job that creates tenant
// id's database. Insert it in the transaction that adds the tenant, and
// call DBs.Create after committing to create it straight away; the job
// makes sure it happens even if that fails.
func CreateTx(ctx context.Context, tx *sql.Tx, client *river.Client[*sql.Tx], id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("tenant: invalid id %q", id)
	}
	_, err := client.InsertTx(ctx, tx, CreateArgs{Tenant: id}, nil)
	return err
}

// SyncTx inserts, in master transaction tx, a job that syncs record key of
// mirror into tenant id's database. Insert one whenever the record is
// created, changed or deleted.
func SyncTx(ctx context.Context, tx *sql.Tx, client *river.Client[*sql.Tx], mirror, id, key string) error {
	_, err := client.InsertTx(ctx, tx, SyncArgs{Mirror: mirror, Tenant: id, Key: key}, nil)
	return err
}

type createWorker struct {
	river.WorkerDefaults[CreateArgs]
	s *Sync
}

func (w *createWorker) Work(ctx context.Context, job *river.Job[CreateArgs]) error {
	id := job.Args.Tenant
	if err := w.s.checkExists(ctx, id); err != nil {
		return cancelNotFound(err)
	}
	err := w.s.dbs.Create(ctx, id)
	if errors.Is(err, ErrDeleting) {
		return river.JobSnooze(time.Second)
	}
	if err != nil {
		return err
	}
	// If the tenant was deleted between the check and Create, Create made
	// an empty database for nobody: put it in the trash.
	err = w.s.checkExists(ctx, id)
	if errors.Is(err, ErrNotFound) {
		if derr := w.s.dbs.Delete(ctx, id, nil); derr != nil {
			return derr
		}
	}
	return cancelNotFound(err)
}

// cancelNotFound cancels the job if err is ErrNotFound: the tenant's gone
// from the master database, so retrying won't help.
func cancelNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	}
	return err
}

type syncWorker struct {
	river.WorkerDefaults[SyncArgs]
	s *Sync
}

func (w *syncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	args := job.Args
	if _, ok := w.s.mirrors[args.Mirror]; !ok {
		return river.JobCancel(fmt.Errorf("unknown mirror %q", args.Mirror))
	}

	err := w.s.Run(ctx, args.Mirror, args.Tenant, args.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		return river.JobCancel(err) // gone from the master database
	case errors.Is(err, ErrDeleting):
		return river.JobSnooze(time.Second) // then it's gone, or the delete failed
	}
	// Including ErrNotExist: the tenant's in master but its database isn't
	// created yet. Its tenant_create job will, and the retry finds it.
	return err
}

// Run syncs record key of mirror into tenant id's database now, as a
// tenant_sync job does, and returns once it's committed. Use it after
// committing a master change to have the tenant up to date straight away,
// e.g. right after DBs.Create for a new tenant, as Create is to CreateTx:
// still insert the SyncTx job in the transaction, so the sync happens even
// if Run fails. It's also how to run a sync where there's no River worker,
// such as an offline script or a test.
//
// It fails with an error matching ErrNotFound if the tenant isn't in the
// master database, and otherwise as Acquire does, e.g. ErrNotExist if
// the tenant's database hasn't been created yet.
//
// Like a sync job, it holds the tenant's database while reading the
// master database, so don't call it while holding a master transaction.
func (s *Sync) Run(ctx context.Context, mirror, id, key string) error {
	m, ok := s.mirrors[mirror]
	if !ok {
		return fmt.Errorf("tenant: unknown mirror %q", mirror)
	}

	// The tenant transaction holds its database's only connection, so
	// syncs of one tenant run one at a time, each reading the master
	// database after the previous one committed. Delete waits for it too,
	// so a tenant can't be deleted between checking it exists and
	// writing.
	err := s.dbs.Do(ctx, id, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if err := s.checkExists(ctx, id); err != nil {
			return err
		}
		if err := m.Sync(ctx, tx, id, key); err != nil {
			return fmt.Errorf("sync %s %q: %w", mirror, key, err)
		}
		return tx.Commit()
	})
	if errors.Is(err, ErrNotExist) {
		// Deleted, or not created yet: say which.
		if xerr := s.checkExists(ctx, id); xerr != nil {
			return xerr
		}
	}
	return err
}

// checkExists returns an error matching ErrNotFound if tenant id isn't in
// the master database.
func (s *Sync) checkExists(ctx context.Context, id string) error {
	exists, err := s.exists(ctx, id)
	if err != nil {
		return fmt.Errorf("check %s exists: %w", s.dbs.Name(), err)
	}
	if !exists {
		return fmt.Errorf("%s %s: %w", s.dbs.Name(), id, ErrNotFound)
	}
	return nil
}
