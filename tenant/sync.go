package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/riverqueue/river"
)

// Mirror copies one kind of master database record into tenant databases.
type Mirror struct {
	// Name identifies the mirror in jobs, e.g. "tenant" or "bank_account".
	Name string

	// Sync makes the tenant's copy of the record named key match the
	// master database: upsert it if it's there, delete it if not. tx is a
	// transaction on the tenant's database, committed if Sync returns nil.
	// Read the master record inside Sync: syncs of the same tenant run one
	// at a time, so reading here means the last to run writes the latest.
	Sync func(ctx context.Context, tx *sql.Tx, tenantID, key string) error
}

// SyncConfig configures a Sync.
type SyncConfig struct {
	DBs *DBs

	// Exists reports whether the tenant is still in the master database.
	// Syncs for a tenant that isn't remove its database instead.
	Exists func(ctx context.Context, tenantID string) (bool, error)

	Mirrors []Mirror
}

// Sync keeps tenant databases in step with the master database, using
// River jobs on the master database as an outbox. Change a record in a
// master transaction and, in the same transaction, insert a job with
// InsertSyncTx (or InsertDeleteTx for a deleted tenant): the job exists if
// and only if the change committed, and River retries it until the tenant
// database matches. A crash between the master commit and the tenant write
// can't lose the write.
//
// A sync holds the tenant's database while it reads the master database, so
// never hold a master transaction while using a tenant's database: with one
// connection to each, the two could deadlock.
type Sync struct {
	dbs     *DBs
	exists  func(context.Context, string) (bool, error)
	mirrors map[string]Mirror
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

// AddWorkers registers the workers for SyncArgs and DeleteArgs.
func (s *Sync) AddWorkers(workers *river.Workers) {
	river.AddWorker(workers, &syncWorker{s: s})
	river.AddWorker(workers, &deleteWorker{dbs: s.dbs})
}

// SyncArgs is the job InsertSyncTx inserts.
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

// DeleteArgs is the job InsertDeleteTx inserts.
type DeleteArgs struct {
	Tenant string `json:"tenant"`
}

func (DeleteArgs) Kind() string { return "tenant_delete" }

// InsertSyncTx inserts, in master transaction tx, a job that syncs record
// key of mirror into tenantID's database, creating the database if needed.
// Insert one whenever the record is created, changed or deleted.
func InsertSyncTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, mirror, tenantID, key string) error {
	_, err := client.InsertTx(ctx, tx, SyncArgs{Mirror: mirror, Tenant: tenantID, Key: key}, nil)
	return err
}

// InsertDeleteTx inserts, in master transaction tx, a job that deletes
// tenantID's database once requests in flight on it finish. Insert it in
// the transaction that removes the tenant from the master database.
func InsertDeleteTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, tenantID string) error {
	_, err := client.InsertTx(ctx, tx, DeleteArgs{Tenant: tenantID}, nil)
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

	if err := w.s.dbs.Create(ctx, args.Tenant); err != nil {
		return err
	}

	// The tenant transaction holds its database's only connection, so
	// syncs of one tenant run one at a time, each reading the master
	// database after the previous one committed. It also means a Delete of
	// the tenant waits for this, or ran first and Exists sees it gone.
	var gone bool
	err := w.s.dbs.Do(ctx, args.Tenant, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		exists, err := w.s.exists(ctx, args.Tenant)
		if err != nil {
			return fmt.Errorf("check tenant exists: %w", err)
		}
		if !exists {
			gone = true
			return nil
		}

		if err := m.Sync(ctx, tx, args.Tenant, args.Key); err != nil {
			return fmt.Errorf("sync %s %q: %w", args.Mirror, args.Key, err)
		}
		return tx.Commit()
	})
	if err != nil {
		// Including ErrNotExist, if the tenant was deleted between Create
		// and Do: the retry finds it gone.
		return err
	}

	if gone {
		// Deleted before (or while) we ran, and Create above may have
		// recreated its database: remove it again.
		if err := w.s.dbs.Delete(ctx, args.Tenant); err != nil {
			return err
		}
		return river.JobCancel(fmt.Errorf("tenant %s no longer exists", args.Tenant))
	}
	return nil
}

type deleteWorker struct {
	river.WorkerDefaults[DeleteArgs]
	dbs *DBs
}

// Work waits for requests in flight on the tenant to finish (they resolved
// it before it left the master database), then removes its database. If
// they take longer than the job's timeout, River retries.
func (w *deleteWorker) Work(ctx context.Context, job *river.Job[DeleteArgs]) error {
	return w.dbs.Delete(ctx, job.Args.Tenant)
}
