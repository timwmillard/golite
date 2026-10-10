package tenant

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/timwmillard/golite/server"
)

// The master has a vals table the copy mirror copies from; each tenant
// database has a copy table it copies into.
var (
	masterMigrations = fstest.MapFS{
		"0001_vals.sql": {Data: []byte(`create table vals (key text primary key, value text not null)`)},
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
		Sync: func(ctx context.Context, tx *sql.Tx, ref, key string) error {
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
	s, err := NewSync(ctx, SyncConfig{DBs: d, Master: master, Mirrors: []Mirror{copyMirror(master, pause)}})
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

func (e *syncEnv) create(t *testing.T, ref, dbID string) {
	t.Helper()
	err := e.tx(t, func(tx *sql.Tx) error { return CreateTx(t.Context(), e.river, tx, ref, dbID) })
	if err != nil {
		t.Fatalf("CreateTx(%s, %s): %v", ref, dbID, err)
	}
	if err := e.s.Settle(t.Context(), ref); err != nil {
		t.Fatalf("Settle(%s): %v", ref, err)
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

func (e *syncEnv) copied(t *testing.T, dbID, key string) (string, bool) {
	t.Helper()
	var v string
	err := e.d.Do(t.Context(), dbID, func(db *sql.DB) error {
		return db.QueryRow(`select value from copy where key = ?`, key).Scan(&v)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read copy from %s: %v", dbID, err)
	}
	return v, true
}

func dbID(t *testing.T, e *syncEnv, ref string) string {
	t.Helper()
	id, err := DBID(t.Context(), e.master, ref)
	if err != nil {
		t.Fatalf("DBID(%s): %v", ref, err)
	}
	return id
}

func TestSync_Lifecycle(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()

	// Create: registered, then Settle makes the database.
	err := e.tx(t, func(tx *sql.Tx) error { return CreateTx(ctx, e.river, tx, "1", "acme") })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.d.Acquire(ctx, "acme"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("database exists before Settle: %v", err)
	}
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatalf("Settle create: %v", err)
	}
	e.setVal(t, "k", "v1")
	if err := e.runSync(SyncArgs{Mirror: "copy", Ref: "1", Key: "k"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Rename: DBID keeps the old id until Settle moves the file.
	err = e.tx(t, func(tx *sql.Tx) error { return RenameTx(ctx, e.river, tx, "1", "acme-inc") })
	if err != nil {
		t.Fatal(err)
	}
	if got := dbID(t, e, "1"); got != "acme" {
		t.Errorf("DBID before Settle = %s, want acme", got)
	}
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatalf("Settle rename: %v", err)
	}
	if got := dbID(t, e, "1"); got != "acme-inc" {
		t.Errorf("DBID after Settle = %s, want acme-inc", got)
	}
	if v, _ := e.copied(t, "acme-inc", "k"); v != "v1" {
		t.Errorf("renamed database has copy %q, want v1", v)
	}
	if _, _, err := e.d.Acquire(ctx, "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("old database still there: %v", err)
	}
	// Syncs follow the rename.
	e.setVal(t, "k", "v2")
	if err := e.runSync(SyncArgs{Mirror: "copy", Ref: "1", Key: "k"}); err != nil {
		t.Fatalf("sync after rename: %v", err)
	}
	if v, _ := e.copied(t, "acme-inc", "k"); v != "v2" {
		t.Errorf("copy after rename = %q, want v2", v)
	}

	// Delete: gone from DBID at once, file and registry row once settled.
	err = e.tx(t, func(tx *sql.Tx) error { return DeleteTx(ctx, e.river, tx, "1") })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DBID(ctx, e.master, "1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DBID after DeleteTx = %v, want ErrNotFound", err)
	}
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatalf("Settle delete: %v", err)
	}
	if _, _, err := e.d.Acquire(ctx, "acme-inc"); !errors.Is(err, ErrNotExist) {
		t.Errorf("database still there after delete: %v", err)
	}
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Errorf("Settle of a settled delete: %v", err)
	}
	// Its id is free again.
	e.create(t, "2", "acme-inc")
}

// Database ids stay reserved while their file might exist, so a new or
// renamed tenant can't open another's database.
func TestSync_IDsReserved(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	e.create(t, "1", "acme")
	e.create(t, "2", "globex")

	createTx := func(ref, id string) error {
		return e.tx(t, func(tx *sql.Tx) error { return CreateTx(ctx, e.river, tx, ref, id) })
	}
	renameTx := func(ref, id string) error {
		return e.tx(t, func(tx *sql.Tx) error { return RenameTx(ctx, e.river, tx, ref, id) })
	}

	if err := createTx("3", "acme"); !errors.Is(err, ErrIDTaken) {
		t.Errorf("create with a live tenant's id = %v, want ErrIDTaken", err)
	}
	if err := renameTx("2", "acme"); !errors.Is(err, ErrIDTaken) {
		t.Errorf("rename to a live tenant's id = %v, want ErrIDTaken", err)
	}

	// A pending rename holds both ids.
	if err := renameTx("1", "acme-inc"); err != nil {
		t.Fatal(err)
	}
	if err := renameTx("1", "acme-co"); !errors.Is(err, ErrRenamePending) {
		t.Errorf("second rename while pending = %v, want ErrRenamePending", err)
	}
	if err := createTx("3", "acme-inc"); !errors.Is(err, ErrIDTaken) {
		t.Errorf("create with a pending rename's new id = %v, want ErrIDTaken", err)
	}
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	// Renaming a tenant to its own id is a no-op.
	if err := renameTx("1", "acme-inc"); err != nil {
		t.Errorf("rename to own id: %v", err)
	}

	// A deleted tenant holds its id until its database is gone.
	if err := e.tx(t, func(tx *sql.Tx) error { return DeleteTx(ctx, e.river, tx, "2") }); err != nil {
		t.Fatal(err)
	}
	if err := createTx("3", "globex"); !errors.Is(err, ErrIDTaken) {
		t.Errorf("create with an id still being deleted = %v, want ErrIDTaken", err)
	}
	if err := e.s.Settle(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if err := createTx("3", "globex"); err != nil {
		t.Errorf("create after the delete settled: %v", err)
	}

	if err := renameTx("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename of unknown tenant = %v, want ErrNotFound", err)
	}
	if err := e.tx(t, func(tx *sql.Tx) error { return DeleteTx(ctx, e.river, tx, "nope") }); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of unknown tenant = %v, want ErrNotFound", err)
	}
}

// If the process dies after a rename moves the file but before the
// registry records it, the settle job's retry finishes it.
func TestSync_RenameFinishesAfterCrash(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	e.create(t, "1", "acme")
	e.setVal(t, "k", "v1")
	if err := e.runSync(SyncArgs{Mirror: "copy", Ref: "1", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := e.tx(t, func(tx *sql.Tx) error { return RenameTx(ctx, e.river, tx, "1", "acme-inc") }); err != nil {
		t.Fatal(err)
	}

	e.d.closeIdle(time.Now())
	from, _ := e.d.Path("acme")
	to, _ := e.d.Path("acme-inc")
	if err := os.Rename(from, to); err != nil { // the move before the "crash"
		t.Fatal(err)
	}
	if got := dbID(t, e, "1"); got != "acme" {
		t.Fatalf("registry already moved: %s", got)
	}

	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatalf("Settle after crash: %v", err)
	}
	if got := dbID(t, e, "1"); got != "acme-inc" {
		t.Errorf("DBID = %s, want acme-inc", got)
	}
	if v, _ := e.copied(t, "acme-inc", "k"); v != "v1" {
		t.Errorf("copy = %q after recovery, want v1", v)
	}
}

// Deleting a tenant whose rename crashed half done removes the file under
// either id.
func TestSync_DeleteAfterCrashedRename(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	e.create(t, "1", "acme")
	if err := e.tx(t, func(tx *sql.Tx) error { return RenameTx(ctx, e.river, tx, "1", "acme-inc") }); err != nil {
		t.Fatal(err)
	}
	e.d.closeIdle(time.Now())
	from, _ := e.d.Path("acme")
	to, _ := e.d.Path("acme-inc")
	os.Rename(from, to)

	if err := e.tx(t, func(tx *sql.Tx) error { return DeleteTx(ctx, e.river, tx, "1") }); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{from, to} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left after delete", filepath.Base(p))
		}
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
	e.create(t, "1", "acme")
	args := SyncArgs{Mirror: "copy", Ref: "1", Key: "k"}

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

func TestSync_States(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	var cancel *river.JobCancelError
	var snooze *river.JobSnoozeError

	if err := e.runSync(SyncArgs{Mirror: "copy", Ref: "nope"}); !errors.As(err, &cancel) {
		t.Errorf("sync of unknown tenant = %v, want JobCancel", err)
	}
	if err := e.runSync(SyncArgs{Mirror: "nope", Ref: "1"}); !errors.As(err, &cancel) {
		t.Errorf("sync with unknown mirror = %v, want JobCancel", err)
	}

	// Registered but not yet created: wait for the settle job.
	if err := e.tx(t, func(tx *sql.Tx) error { return CreateTx(ctx, e.river, tx, "1", "acme") }); err != nil {
		t.Fatal(err)
	}
	if err := e.runSync(SyncArgs{Mirror: "copy", Ref: "1"}); !errors.As(err, &snooze) {
		t.Errorf("sync before created = %v, want JobSnooze", err)
	}
	if _, _, err := e.d.Acquire(ctx, "acme"); !errors.Is(err, ErrNotExist) {
		t.Errorf("sync created the database: %v", err)
	}

	// Deleted: the sync gives up, leaving the database to the settle job.
	if err := e.s.Settle(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := e.tx(t, func(tx *sql.Tx) error { return DeleteTx(ctx, e.river, tx, "1") }); err != nil {
		t.Fatal(err)
	}
	if err := e.runSync(SyncArgs{Mirror: "copy", Ref: "1"}); !errors.As(err, &cancel) {
		t.Errorf("sync of deleted tenant = %v, want JobCancel", err)
	}
}

func TestSync_MigrateAll(t *testing.T) {
	e := newSyncEnv(t, nil)
	ctx := t.Context()
	e.create(t, "1", "a")
	e.create(t, "2", "b")
	// Registered but not created, and deleted: both skipped.
	if err := e.tx(t, func(tx *sql.Tx) error { return CreateTx(ctx, e.river, tx, "3", "c") }); err != nil {
		t.Fatal(err)
	}
	if err := e.tx(t, func(tx *sql.Tx) error { return DeleteTx(ctx, e.river, tx, "2") }); err != nil {
		t.Fatal(err)
	}
	e.d.closeIdle(time.Now())

	syncMigrations["0002_more.sql"] = &fstest.MapFile{Data: []byte(`create table more (id integer primary key)`)}
	t.Cleanup(func() { delete(syncMigrations, "0002_more.sql") })
	if err := e.s.MigrateAll(ctx); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	err := e.d.Do(ctx, "a", func(db *sql.DB) error { _, err := db.Exec(`insert into more default values`); return err })
	if err != nil {
		t.Errorf("a not migrated: %v", err)
	}
	if _, _, err := e.d.Acquire(ctx, "c"); !errors.Is(err, ErrNotExist) {
		t.Errorf("MigrateAll created c: %v", err)
	}
}

func TestNewSync_Validates(t *testing.T) {
	e := newSyncEnv(t, nil)
	noop := func(context.Context, *sql.Tx, string, string) error { return nil }

	bad := []SyncConfig{
		{Master: e.master},
		{DBs: e.d},
		{DBs: e.d, Master: e.master, Mirrors: []Mirror{{Name: "", Sync: noop}}},
		{DBs: e.d, Master: e.master, Mirrors: []Mirror{{Name: "a"}}},
		{DBs: e.d, Master: e.master, Mirrors: []Mirror{{Name: "a", Sync: noop}, {Name: "a", Sync: noop}}},
	}
	for i, cfg := range bad {
		if _, err := NewSync(t.Context(), cfg); err == nil {
			t.Errorf("config %d: NewSync succeeded, want error", i)
		}
	}
}

// Requests during a rename get a 503, and one that resolved the old id just
// before the rename finished is resolved again.
func TestMiddleware_Rename(t *testing.T) {
	d := newDBs(t, Config{Name: "club"})
	if err := d.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		current = "old"
		stale   bool // resolve the old id once more, as if just before the rename
	)
	resolve := func(*http.Request) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if stale {
			stale = false
			return "old", nil
		}
		return current, nil
	}
	var served string
	h := d.Middleware(resolve)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = ID(r.Context())
	}))
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w
	}

	// Mid-rename: 503.
	_, release := acquire(t, d, "old")
	renamed := make(chan error)
	go func() {
		renamed <- d.Rename(t.Context(), "old", "new", func(context.Context) error {
			mu.Lock()
			current = "new"
			mu.Unlock()
			return nil
		})
	}()
	waitFor(t, func() bool { return get().Code == http.StatusServiceUnavailable })
	if w := get(); w.Header().Get("Retry-After") != "1" {
		t.Errorf("503 without Retry-After: %v", w.Header())
	}
	release()
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}

	// Resolved before the rename, opened after: resolved again.
	mu.Lock()
	stale = true
	mu.Unlock()
	if w := get(); w.Code != http.StatusOK || served != "new" {
		t.Errorf("stale request: status %d, served %q; want 200 from new", w.Code, served)
	}
}
