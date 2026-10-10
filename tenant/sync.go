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
// database and, in the same transaction, call CreateTx, RenameTx, DeleteTx
// or SyncTx. Each records what should happen and inserts a job to do it,
// so the job exists if and only if the change committed, and River retries
// it until the tenant databases match. A crash between the master commit
// and the tenant write can't lose the write.
//
// Sync keeps a registry of tenant databases in the master database, table
// tenant_db, keyed by the app's stable reference for each tenant (ref, such
// as a numeric id) and recording its database's id (a DBs id, such as a
// slug) and whether it exists yet. Database ids are reserved until their
// file is gone, so a new or renamed tenant can never open another tenant's
// database, even one still being deleted.
//
// A sync holds the tenant's database while it reads the master database, so
// never hold a master transaction while using a tenant's database: with one
// connection to each, the two could deadlock.
type Sync struct {
	dbs     *DBs
	master  *sql.DB
	mirrors map[string]Mirror
}

// Mirror copies one kind of master database record into tenant databases.
type Mirror struct {
	// Name identifies the mirror in jobs, e.g. "company" or "member".
	Name string

	// Sync makes the tenant's copy of the record named key match the
	// master database: upsert it if it's there, delete it if not. ref is
	// the tenant's reference, as given to SyncTx. tx is a transaction on
	// the tenant's database, committed if Sync returns nil. Read the master
	// record inside Sync: syncs of the same tenant run one at a time, so
	// reading here means the last to run writes the latest.
	Sync func(ctx context.Context, tx *sql.Tx, ref, key string) error
}

// SyncConfig configures a Sync.
type SyncConfig struct {
	DBs *DBs

	// Master is the master database, holding the registry and River's
	// jobs.
	Master *sql.DB

	Mirrors []Mirror
}

// ErrIDTaken is returned by CreateTx and RenameTx when the database id is in
// use by another tenant, or by one being deleted or renamed away from it.
var ErrIDTaken = errors.New("tenant: database id is taken")

// ErrRenamePending is returned by RenameTx while the tenant's last rename
// hasn't finished.
var ErrRenamePending = errors.New("tenant: a rename is already pending")

const registrySchema = `
create table if not exists tenant_db (
    ref        text    primary key,
    db_id      text    not null unique,
    next_db_id text    unique,           -- set while a rename is pending
    created    integer not null default 0,
    deleted    integer not null default 0 -- kept until its database is gone
)`

// NewSync returns a Sync for the given mirrors, creating its registry table
// in the master database if needed. Register its workers with AddWorkers.
func NewSync(ctx context.Context, cfg SyncConfig) (*Sync, error) {
	if cfg.DBs == nil || cfg.Master == nil {
		return nil, errors.New("tenant: SyncConfig needs DBs and Master")
	}
	s := &Sync{dbs: cfg.DBs, master: cfg.Master, mirrors: map[string]Mirror{}}
	for _, m := range cfg.Mirrors {
		if m.Name == "" || m.Sync == nil {
			return nil, errors.New("tenant: Mirror needs Name and Sync")
		}
		if _, dup := s.mirrors[m.Name]; dup {
			return nil, fmt.Errorf("tenant: duplicate mirror %q", m.Name)
		}
		s.mirrors[m.Name] = m
	}
	if _, err := cfg.Master.ExecContext(ctx, registrySchema); err != nil {
		return nil, fmt.Errorf("create tenant_db table: %w", err)
	}
	return s, nil
}

// AddWorkers registers the workers for SettleArgs and SyncArgs.
func (s *Sync) AddWorkers(workers *river.Workers) {
	river.AddWorker(workers, &settleWorker{s: s})
	river.AddWorker(workers, &syncWorker{s: s})
}

// SettleArgs is the job CreateTx, RenameTx and DeleteTx insert: it brings
// the tenant's database in line with the registry (see Sync.Settle).
type SettleArgs struct {
	Ref string `json:"ref"`
}

func (SettleArgs) Kind() string { return "tenant_settle" }

// SyncArgs is the job SyncTx inserts.
//
// It isn't a unique job: River can't dedupe without counting running jobs,
// and an edit made while a sync is running would then be dropped. Each sync
// reads the latest master record, so extras are cheap and harmless.
type SyncArgs struct {
	Mirror string `json:"mirror"`
	Ref    string `json:"ref"`
	Key    string `json:"key,omitempty"`
}

func (SyncArgs) Kind() string { return "tenant_sync" }

// Querier is a *sql.DB or *sql.Tx on the master database.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CreateTx registers a new tenant ref whose database is to be dbID, in
// master transaction tx, and inserts a job that creates the database. Call
// Settle after committing to create it straight away. It fails with
// ErrIDTaken if dbID is in use.
func CreateTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, ref, dbID string) error {
	if !validID.MatchString(dbID) {
		return fmt.Errorf("tenant: invalid id %q", dbID)
	}
	if err := checkFree(ctx, tx, dbID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into tenant_db (ref, db_id) values (?, ?)`, ref, dbID); err != nil {
		return fmt.Errorf("register tenant %s: %w", ref, err)
	}
	return insertSettle(ctx, client, tx, ref)
}

// RenameTx records, in master transaction tx, that tenant ref's database is
// to become dbID, and inserts a job that renames it. Until the rename is
// done DBID keeps returning the old id; call Settle after committing to do
// it straight away. It fails with ErrIDTaken if dbID is in use,
// ErrRenamePending if an earlier rename isn't done, and ErrNotFound if
// there's no such tenant.
func RenameTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, ref, dbID string) error {
	if !validID.MatchString(dbID) {
		return fmt.Errorf("tenant: invalid id %q", dbID)
	}
	r, err := getRow(ctx, tx, ref)
	if err != nil {
		return err
	}
	if r.deleted {
		return ErrNotFound
	}
	if r.next != "" {
		return ErrRenamePending
	}
	if r.dbID == dbID {
		return nil
	}
	if err := checkFree(ctx, tx, dbID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update tenant_db set next_db_id = ? where ref = ?`, dbID, ref); err != nil {
		return fmt.Errorf("rename tenant %s: %w", ref, err)
	}
	return insertSettle(ctx, client, tx, ref)
}

// DeleteTx marks tenant ref deleted, in master transaction tx, and inserts
// a job that deletes its database once requests in flight on it finish.
// DBID fails with ErrNotFound for it from then on, but its database id
// stays reserved until the database is gone.
func DeleteTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, ref string) error {
	res, err := tx.ExecContext(ctx, `update tenant_db set deleted = 1 where ref = ?`, ref)
	if err != nil {
		return fmt.Errorf("delete tenant %s: %w", ref, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return insertSettle(ctx, client, tx, ref)
}

// SyncTx inserts, in master transaction tx, a job that syncs record key of
// mirror into tenant ref's database. Insert one whenever the record is
// created, changed or deleted.
func SyncTx(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, mirror, ref, key string) error {
	_, err := client.InsertTx(ctx, tx, SyncArgs{Mirror: mirror, Ref: ref, Key: key}, nil)
	return err
}

func insertSettle(ctx context.Context, client *river.Client[*sql.Tx], tx *sql.Tx, ref string) error {
	_, err := client.InsertTx(ctx, tx, SettleArgs{Ref: ref}, nil)
	return err
}

// DBID returns tenant ref's database id, for DBs, or ErrNotFound if there's
// no such tenant or it's been deleted. q is the master database or a
// transaction on it.
func DBID(ctx context.Context, q Querier, ref string) (string, error) {
	r, err := getRow(ctx, q, ref)
	if err != nil {
		return "", err
	}
	if r.deleted {
		return "", ErrNotFound
	}
	return r.dbID, nil
}

// MigrateAll applies pending migrations to every tenant database the
// registry has created (see DBs.MigrateAll).
func (s *Sync) MigrateAll(ctx context.Context) error {
	rows, err := s.master.QueryContext(ctx, `select db_id from tenant_db where created = 1 and deleted = 0 order by db_id`)
	if err != nil {
		return fmt.Errorf("list tenant databases: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return s.dbs.MigrateAll(ctx, ids)
}

// Settle brings tenant ref's database in line with the registry: creates
// it, renames it, or deletes it. It's what the tenant_settle job does;
// handlers call it after committing CreateTx or RenameTx so the change
// takes effect straight away, leaving the job to retry if it fails. It
// waits, until ctx is done, for requests in flight on the database before
// renaming or deleting it.
func (s *Sync) Settle(ctx context.Context, ref string) error {
	r, err := getRow(ctx, s.master, ref)
	if errors.Is(err, ErrNotFound) {
		return nil // deleted and settled
	}
	if err != nil {
		return err
	}

	if r.deleted {
		if err := s.dbs.Delete(ctx, r.dbID); err != nil {
			return err
		}
		if r.next != "" {
			// In case a rename moved the file and then the process died.
			if err := s.dbs.Delete(ctx, r.next); err != nil {
				return err
			}
		}
		_, err := s.master.ExecContext(ctx, `delete from tenant_db where ref = ? and deleted = 1`, ref)
		return err
	}

	if !r.created {
		// Nothing can have moved it: a rename waits until it's created.
		if err := s.dbs.Create(ctx, r.dbID); err != nil {
			return err
		}
		if _, err := s.master.ExecContext(ctx, `update tenant_db set created = 1 where ref = ? and db_id = ?`, ref, r.dbID); err != nil {
			return err
		}
	}

	if r.next != "" {
		return s.dbs.Rename(ctx, r.dbID, r.next, func(ctx context.Context) error {
			res, err := s.master.ExecContext(ctx, `
				update tenant_db set db_id = next_db_id, next_db_id = null
				where ref = ? and db_id = ? and next_db_id = ? and deleted = 0`,
				ref, r.dbID, r.next)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errChanged
			}
			return nil
		})
	}
	return nil
}

// errChanged means the registry row changed under a settle or sync, which
// retries.
var errChanged = errors.New("tenant: registry changed; retrying")

type row struct {
	dbID    string
	next    string
	created bool
	deleted bool
}

func getRow(ctx context.Context, q Querier, ref string) (row, error) {
	var (
		r    row
		next sql.NullString
	)
	err := q.QueryRowContext(ctx, `select db_id, next_db_id, created, deleted from tenant_db where ref = ?`, ref).
		Scan(&r.dbID, &next, &r.created, &r.deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, ErrNotFound
	}
	if err != nil {
		return row{}, fmt.Errorf("read tenant_db %s: %w", ref, err)
	}
	r.next = next.String
	return r, nil
}

// checkFree fails with ErrIDTaken if any tenant, including ones being
// deleted or renamed, holds dbID.
func checkFree(ctx context.Context, q Querier, dbID string) error {
	var n int
	err := q.QueryRowContext(ctx, `select count(*) from tenant_db where db_id = ?1 or next_db_id = ?1`, dbID).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrIDTaken
	}
	return nil
}

// busy reports whether err is a passing state worth snoozing for rather
// than counting as a failed attempt.
func busy(err error) bool {
	return errors.Is(err, ErrRenaming) || errors.Is(err, ErrDeleting) || errors.Is(err, errChanged)
}

type settleWorker struct {
	river.WorkerDefaults[SettleArgs]
	s *Sync
}

func (w *settleWorker) Work(ctx context.Context, job *river.Job[SettleArgs]) error {
	err := w.s.Settle(ctx, job.Args.Ref)
	if busy(err) {
		return river.JobSnooze(time.Second)
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

	r, err := getRow(ctx, w.s.master, args.Ref)
	if errors.Is(err, ErrNotFound) || err == nil && r.deleted {
		return river.JobCancel(fmt.Errorf("%s %s no longer exists", w.s.dbs.Name(), args.Ref))
	}
	if err != nil {
		return err
	}
	if !r.created {
		return river.JobSnooze(time.Second) // its settle job creates it
	}

	// The tenant transaction holds its database's only connection, so
	// syncs of one tenant run one at a time, each reading the master
	// database after the previous one committed. Rename and Delete wait
	// for it too, so the registry can't change under it unnoticed.
	var gone bool
	err = w.s.dbs.Do(ctx, r.dbID, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		cur, err := getRow(ctx, w.s.master, args.Ref)
		if errors.Is(err, ErrNotFound) || err == nil && cur.deleted {
			gone = true
			return nil
		}
		if err != nil {
			return err
		}
		if cur.dbID != r.dbID {
			return errChanged // renamed since we looked
		}

		if err := m.Sync(ctx, tx, args.Ref, args.Key); err != nil {
			return fmt.Errorf("sync %s %q: %w", args.Mirror, args.Key, err)
		}
		return tx.Commit()
	})
	switch {
	case gone:
		return river.JobCancel(fmt.Errorf("%s %s no longer exists", w.s.dbs.Name(), args.Ref))
	case busy(err):
		return river.JobSnooze(time.Second)
	}
	// Including ErrNotExist, if it was renamed or deleted between reading
	// the registry and opening it: the retry looks again.
	return err
}
