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
}

// NewSync returns a Sync for the given mirrors. Register its workers with
// AddWorkers.
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
	return s, nil
}

// AddWorkers registers the workers for CreateArgs and SyncArgs.
func (s *Sync) AddWorkers(workers *river.Workers) {
	river.AddWorker(workers, &createWorker{s: s})
	river.AddWorker(workers, &syncWorker{s: s})
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
func CreateTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("tenant: invalid id %q", id)
	}
	_, err := client.InsertTx(ctx, tx, CreateArgs{Tenant: id}, nil)
	return err
}

// SyncTx inserts, in master transaction tx, a job that syncs record key of
// mirror into tenant id's database. Insert one whenever the record is
// created, changed or deleted.
func SyncTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, mirror, id, key string) error {
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
		return err
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
	var cancel *river.JobCancelError
	if errors.As(err, &cancel) {
		if derr := w.s.dbs.Delete(ctx, id, nil); derr != nil {
			return derr
		}
	}
	return err
}

type syncWorker struct {
	river.WorkerDefaults[SyncArgs]
	s *Sync
}

func (w *syncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	args := job.Args
	m, ok := w.s.mirrors[args.Mirror]
	if !ok {
		return river.JobCancel(fmt.Errorf("unknown mirror %q", args.Mirror))
	}

	// The tenant transaction holds its database's only connection, so
	// syncs of one tenant run one at a time, each reading the master
	// database after the previous one committed. Delete waits for it too,
	// so a tenant can't be deleted between checking it exists and
	// writing.
	err := w.s.dbs.Do(ctx, args.Tenant, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if err := w.s.checkExists(ctx, args.Tenant); err != nil {
			return err
		}
		if err := m.Sync(ctx, tx, args.Tenant, args.Key); err != nil {
			return fmt.Errorf("sync %s %q: %w", args.Mirror, args.Key, err)
		}
		return tx.Commit()
	})
	switch {
	case errors.Is(err, ErrDeleting):
		return river.JobSnooze(time.Second) // then it's gone, or the delete failed
	case errors.Is(err, ErrNotExist):
		// Deleted, or not created yet: its tenant_create job will, and
		// the retry finds it.
		if xerr := w.s.checkExists(ctx, args.Tenant); xerr != nil {
			return xerr
		}
	}
	return err
}

// checkExists returns a JobCancel error if tenant id isn't in the master
// database.
func (s *Sync) checkExists(ctx context.Context, id string) error {
	exists, err := s.exists(ctx, id)
	if err != nil {
		return fmt.Errorf("check %s exists: %w", s.dbs.Name(), err)
	}
	if !exists {
		return river.JobCancel(fmt.Errorf("%s %s no longer exists", s.dbs.Name(), id))
	}
	return nil
}
