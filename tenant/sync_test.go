package tenant

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/timwmillard/golite/server"
)

// The master has a tenants table listing the tenants and a vals table the
// copy mirror copies from; each tenant database has a copy table it copies
// into.
var (
	masterMigrations = fstest.MapFS{
		"0001_vals.sql": {Data: []byte(`create table tenants (id text primary key);
create table vals (key text primary key, value text not null)`)},
	}
	syncMigrations = fstest.MapFS{
		"0001_copy.sql": {Data: []byte(`create table copy (key text primary key, value text not null)`)},
	}
)

type syncEnv struct {
	s      *Sync
	d      *DBs
	master *sql.DB
	river  *river.Client[*sql.Tx]
}

// copyMirror copies vals into copy. If pause is set, it's called between
// reading the master and writing the tenant.
func copyMirror(master *sql.DB, pause func()) Mirror {
	return Mirror{
		Name: "copy",
		Sync: func(ctx context.Context, tx *sql.Tx, id, key string) error {
			var v string
			err := master.QueryRowContext(ctx, `select value from vals where key = ?`, key).Scan(&v)
			if pause != nil {
				pause()
			}
			if errors.Is(err, sql.ErrNoRows) {
				_, err := tx.ExecContext(ctx, `delete from copy where key = ?`, key)
				return err
			}
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `insert into copy (key, value) values (?, ?)
				on conflict (key) do update set value = excluded.value`, key, v)
			return err
		},
	}
}

func newSyncEnv(t *testing.T, pause func()) *syncEnv {
	t.Helper()
	ctx := t.Context()
	master, err := server.OpenDB(ctx, filepath.Join(t.TempDir(), "master.db"), masterMigrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })

	d := newDBs(t, Config{Migrations: syncMigrations})
	exists := func(ctx context.Context, id string) (bool, error) {
		var ok bool
		err := master.QueryRowContext(ctx, `select exists (select 1 from tenants where id = ?)`, id).Scan(&ok)
		return ok, err
	}
	s, err := NewSync(SyncConfig{DBs: d, Exists: exists, Mirrors: []Mirror{copyMirror(master, pause)}})
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	s.AddWorkers(workers)
	// server.New migrates River's tables and makes the client; it's only
	// used to insert, never started.
	srv, err := server.New(ctx, server.Config{DB: master, Workers: workers, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &syncEnv{s: s, d: d, master: master, river: srv.River}
}

func (e *syncEnv) tx(t *testing.T, fn func(*sql.Tx) error) error {
	t.Helper()
	tx, err := e.master.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// addTenant adds tenant id to the master, with its tenant_create job, but
// doesn't create its database.
func (e *syncEnv) addTenant(t *testing.T, id string) {
	t.Helper()
	err := e.tx(t, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`insert into tenants (id) values (?)`, id); err != nil {
			return err
		}
		return CreateTx(t.Context(), e.river, tx, id)
	})
	if err != nil {
		t.Fatalf("add tenant %s: %v", id, err)
	}
}

// create adds tenant id and creates its database, as a handler would.
func (e *syncEnv) create(t *testing.T, id string) {
	t.Helper()
	e.addTenant(t, id)
	if err := e.d.Create(t.Context(), id); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
}

// remove deletes tenant id's database and, in its commit, its master row.
func (e *syncEnv) remove(t *testing.T, id string) {
	t.Helper()
	err := e.d.Delete(t.Context(), id, func(ctx context.Context) error {
		_, err := e.master.ExecContext(ctx, `delete from tenants where id = ?`, id)
		return err
	})
	if err != nil {
		t.Fatalf("Delete(%s): %v", id, err)
	}
}

func (e *syncEnv) setVal(t *testing.T, key, value string) {
	t.Helper()
	if _, err := e.master.Exec(`insert into vals (key, value) values (?, ?)
		on conflict (key) do update set value = excluded.value`, key, value); err != nil {
		t.Fatal(err)
	}
}

func (e *syncEnv) runSync(args SyncArgs) error {
	return (&syncWorker{s: e.s}).Work(context.Background(), &river.Job[SyncArgs]{JobRow: &rivertype.JobRow{}, Args: args})
}

func (e *syncEnv) runCreate(id string) error {
	return (&createWorker{s: e.s}).Work(context.Background(), &river.Job[CreateArgs]{JobRow: &rivertype.JobRow{}, Args: CreateArgs{Tenant: id}})
}

func (e *syncEnv) copied(t *testing.T, id, key string) (string, bool) {
	t.Helper()
	var v string
	err := e.d.Do(t.Context(), id, func(db *sql.DB) error {
		return db.QueryRow(`select value from copy where key = ?`, key).Scan(&v)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read copy from %s: %v", id, err)
	}
	return v, true
}

func TestSync_Lifecycle(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	var cancel *river.JobCancelError

	// Added, then the tenant_create job makes the database.
	e.addTenant(t, "acme")
	if err := e.runCreate("acme"); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := e.runCreate("acme"); err != nil {
		t.Errorf("create job run twice: %v", err)
	}

	e.setVal(t, "k", "v1")
	if err := e.runSync(SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if v, _ := e.copied(t, "acme", "k"); v != "v1" {
		t.Errorf("copy = %q, want v1", v)
	}

	// Deleted: the database goes to the trash, then the master row goes.
	e.remove(t, "acme")
	if _, _, err := e.d.Acquire(ctx, "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Acquire after delete = %v, want ErrNotExist", err)
	}
	if err := e.runSync(SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}); !errors.As(err, &cancel) {
		t.Errorf("sync of deleted tenant = %v, want JobCancel", err)
	}
	if err := e.runCreate("acme"); !errors.As(err, &cancel) {
		t.Errorf("create job of deleted tenant = %v, want JobCancel", err)
	}
	if _, _, err := e.d.Acquire(ctx, "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("a job recreated the deleted tenant's database: %v", err)
	}
}

// A new tenant given a deleted tenant's id, such as a reused slug, starts
// with an empty database.
func TestSync_ReusedIDStartsEmpty(t *testing.T) {
	e := newSyncEnv(t, nil)
	e.create(t, "acme")
	e.setVal(t, "k", "old tenant's data")
	if err := e.runSync(SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	e.remove(t, "acme")

	e.create(t, "acme")
	if v, ok := e.copied(t, "acme", "k"); ok {
		t.Errorf("new acme sees %q from the old one", v)
	}
}

func TestSync_States(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	var cancel *river.JobCancelError
	var snooze *river.JobSnoozeError

	if err := e.runSync(SyncArgs{Mirror: "copy", Tenant: "nope"}); !errors.As(err, &cancel) {
		t.Errorf("sync of unknown tenant = %v, want JobCancel", err)
	}
	if err := e.runSync(SyncArgs{Mirror: "nope", Tenant: "acme"}); !errors.As(err, &cancel) {
		t.Errorf("sync with unknown mirror = %v, want JobCancel", err)
	}

	// Added but not yet created: retry, leaving creation to the create job.
	e.addTenant(t, "acme")
	err := e.runSync(SyncArgs{Mirror: "copy", Tenant: "acme"})
	if !errors.Is(err, ErrNotExist) {
		t.Errorf("sync before created = %v, want ErrNotExist to retry", err)
	}
	if _, _, err := e.d.Acquire(ctx, "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("sync created the database: %v", err)
	}

	// Being deleted: wait for the delete to finish.
	_, release := acquire(t, e.d, "acme")
	deleted := make(chan error)
	go func() { deleted <- e.d.Delete(ctx, "acme", nil) }()
	waitFor(t, func() bool {
		err := e.runSync(SyncArgs{Mirror: "copy", Tenant: "acme"})
		return errors.As(err, &snooze)
	})
	release()
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
}

// Two syncs of the same tenant overlapping must not leave the older value:
// the second waits for the first, then reads the master afresh.
func TestSync_ConcurrentSyncsDontWriteStale(t *testing.T) {
	readV1 := make(chan struct{})
	resume := make(chan struct{})
	var first sync.Once
	e := newSyncEnv(t, func() {
		first.Do(func() {
			close(readV1)
			<-resume
		})
	})
	e.create(t, "acme")
	args := SyncArgs{Mirror: "copy", Tenant: "acme", Key: "k"}

	e.setVal(t, "k", "v1")
	errA := make(chan error)
	go func() { errA <- e.runSync(args) }()
	<-readV1 // A has read v1 and is paused before writing

	e.setVal(t, "k", "v2") // the edit that enqueues B
	errB := make(chan error)
	go func() { errB <- e.runSync(args) }()
	time.Sleep(50 * time.Millisecond) // B would have read v2 and written by now, if it could

	close(resume)
	if err := <-errA; err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("sync B: %v", err)
	}
	if v, _ := e.copied(t, "acme", "k"); v != "v2" {
		t.Errorf("copy = %q after both syncs, want the latest, v2", v)
	}
}

func TestNewSync_Validates(t *testing.T) {
	e := newSyncEnv(t, nil)
	noop := func(context.Context, *sql.Tx, string, string) error { return nil }
	exists := func(context.Context, string) (bool, error) { return true, nil }

	bad := []SyncConfig{
		{Exists: exists},
		{DBs: e.d},
		{DBs: e.d, Exists: exists, Mirrors: []Mirror{{Name: "", Sync: noop}}},
		{DBs: e.d, Exists: exists, Mirrors: []Mirror{{Name: "a"}}},
		{DBs: e.d, Exists: exists, Mirrors: []Mirror{{Name: "a", Sync: noop}, {Name: "a", Sync: noop}}},
	}
	for i, cfg := range bad {
		if _, err := NewSync(cfg); err == nil {
			t.Errorf("config %d: NewSync succeeded, want error", i)
		}
	}
}
