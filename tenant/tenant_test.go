package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var testMigrations = fstest.MapFS{
	"0001_init.sql": {Data: []byte(`create table things (id integer primary key)`)},
}

func newDBs(t *testing.T, cfg Config) *DBs {
	t.Helper()
	cfg.Dir = t.TempDir()
	if cfg.Migrations == nil {
		cfg.Migrations = testMigrations
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = -1 // tests drive closeIdle themselves
	}
	d := New(cfg)
	t.Cleanup(func() { d.Close() })
	return d
}

func acquire(t *testing.T, d *DBs, id string) (*sql.DB, func()) {
	t.Helper()
	if err := d.Create(t.Context(), id); err != nil {
		t.Fatalf("Create(%q): %v", id, err)
	}
	db, release, err := d.Acquire(t.Context(), id)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", id, err)
	}
	return db, release
}

func isClosed(db *sql.DB) bool {
	return db.Ping() != nil
}

func TestAcquire_OpensOnceAndMigrates(t *testing.T) {
	d := newDBs(t, Config{})
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	d.closeIdle(time.Now()) // so the Acquires below race to open it

	var wg sync.WaitGroup
	got := make([]*sql.DB, 10)
	for i := range got {
		wg.Go(func() {
			db, release, err := d.Acquire(t.Context(), "acme")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer release()
			got[i] = db
		})
	}
	wg.Wait()

	for i := range got {
		if got[i] != got[0] {
			t.Fatalf("Acquire returned different DBs for the same tenant")
		}
	}

	err := d.Do(t.Context(), "acme", func(db *sql.DB) error {
		_, err := db.Exec(`insert into things (id) values (1)`)
		return err
	})
	if err != nil {
		t.Fatalf("insert into migrated table: %v", err)
	}
}

func TestAcquire_SeparateDatabases(t *testing.T) {
	d := newDBs(t, Config{})

	a, ra := acquire(t, d, "a")
	defer ra()
	b, rb := acquire(t, d, "b")
	defer rb()
	if _, err := a.Exec(`insert into things (id) values (1)`); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := b.QueryRow(`select count(*) from things`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tenant b sees %d rows written to tenant a", n)
	}
}

func TestAcquire_InvalidID(t *testing.T) {
	d := newDBs(t, Config{})
	for _, id := range []string{"", "../escape", "a/b", "a.b"} {
		if _, _, err := d.Acquire(t.Context(), id); err == nil {
			t.Errorf("Acquire(%q) succeeded, want error", id)
		}
	}
}

func TestAcquire_RetriesAfterError(t *testing.T) {
	fsys := fstest.MapFS{"0001_bad.sql": {Data: []byte(`not sql`)}}
	d := newDBs(t, Config{Migrations: fsys})

	if err := d.Create(t.Context(), "acme"); err == nil {
		t.Fatal("Create with a bad migration succeeded")
	}
	if s := d.Stats(); s.Open != 0 {
		t.Errorf("failed open still counted: %+v", s)
	}
	fsys["0001_bad.sql"] = &fstest.MapFile{Data: []byte(`create table ok (id integer)`)}
	_, release := acquire(t, d, "acme")
	release()
}

func TestAcquire_NotExist(t *testing.T) {
	d := newDBs(t, Config{})

	if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Acquire before Create = %v, want ErrNotExist", err)
	}
	path, _ := d.Path("acme")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("Acquire created the database file")
	}

	_, release := acquire(t, d, "acme")
	release()
	if err := d.Delete(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Acquire after Delete = %v, want ErrNotExist", err)
	}
	if err := d.Do(t.Context(), "acme", func(*sql.DB) error { return nil }); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Do after Delete = %v, want ErrNotExist", err)
	}
}

func TestCreate_Idempotent(t *testing.T) {
	d := newDBs(t, Config{})

	db, release := acquire(t, d, "acme")
	defer release()
	if _, err := db.Exec(`insert into things (id) values (1)`); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	d.closeIdle(time.Now().Add(time.Hour)) // no-op: acme is in use
	release()
	d.closeIdle(time.Now().Add(time.Hour))
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatalf("Create of closed tenant: %v", err)
	}

	var n int
	d.Do(t.Context(), "acme", func(db *sql.DB) error { return db.QueryRow(`select count(*) from things`).Scan(&n) })
	if n != 1 {
		t.Errorf("%d rows after re-Create, want 1", n)
	}
}

func TestAcquire_EvictsLeastRecentlyUsed(t *testing.T) {
	d := newDBs(t, Config{MaxOpen: 2})

	a, release := acquire(t, d, "a")
	release()
	b, release := acquire(t, d, "b")
	release()
	_, release = acquire(t, d, "a") // a is now more recently used than b
	release()

	_, release = acquire(t, d, "c")
	release()

	if isClosed(a) {
		t.Error("a was closed; want the least recently used, b")
	}
	if !isClosed(b) {
		t.Error("b, the least recently used, is still open")
	}
	if s := d.Stats(); s.Open != 2 || s.InUse != 0 {
		t.Errorf("Stats = %+v, want 2 open, 0 in use", s)
	}
}

func TestAcquire_NeverClosesInUse(t *testing.T) {
	d := newDBs(t, Config{MaxOpen: 1})

	a, releaseA := acquire(t, d, "a")
	b, releaseB := acquire(t, d, "b") // over the cap, but a is in use

	if isClosed(a) || isClosed(b) {
		t.Fatal("closed a database in use")
	}
	if s := d.Stats(); s.Open != 2 || s.InUse != 2 {
		t.Errorf("Stats = %+v, want 2 open, 2 in use", s)
	}

	releaseA()
	releaseA() // a second call is a no-op
	if !isClosed(a) {
		t.Error("a still open after release with 2 open and MaxOpen 1")
	}
	if isClosed(b) {
		t.Error("b closed while in use")
	}
	releaseB()
	if s := d.Stats(); s.Open != 1 || s.InUse != 0 {
		t.Errorf("Stats = %+v, want 1 open, 0 in use", s)
	}
}

func TestCloseIdle(t *testing.T) {
	d := newDBs(t, Config{})

	old, release := acquire(t, d, "old")
	release()
	cutoff := time.Now()
	recent, release := acquire(t, d, "recent")
	release()
	busy, releaseBusy := acquire(t, d, "busy")
	defer releaseBusy()

	d.closeIdle(cutoff)

	if !isClosed(old) {
		t.Error("idle database not closed")
	}
	if isClosed(recent) || isClosed(busy) {
		t.Error("closed a recently used or busy database")
	}

	// Reopening an idle-closed tenant gets its data back.
	if _, err := old.Exec(`select 1`); err == nil {
		t.Fatal("closed DB still usable")
	}
	reopened, release := acquire(t, d, "old")
	defer release()
	if isClosed(reopened) {
		t.Error("reopened DB is closed")
	}
}

func TestIdleSweeper(t *testing.T) {
	d := New(Config{Dir: t.TempDir(), IdleTimeout: 10 * time.Millisecond})
	defer d.Close()

	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	db, release, err := d.Acquire(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	release()

	// The sweeper ticks every second at most.
	deadline := time.Now().Add(3 * time.Second)
	for !isClosed(db) {
		if time.Now().After(deadline) {
			t.Fatal("idle database not closed by the sweeper")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDelete(t *testing.T) {
	d := newDBs(t, Config{})

	db, release := acquire(t, d, "acme")
	release()
	if err := d.Delete(t.Context(), "acme"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	path, _ := d.Path("acme")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Delete", path+suffix)
		}
	}
	if !isClosed(db) {
		t.Error("deleted tenant's DB still open")
	}
	if s := d.Stats(); s.Open != 0 {
		t.Errorf("Stats = %+v after Delete, want 0 open", s)
	}

	// Deleting a tenant that isn't open isn't an error.
	if err := d.Delete(t.Context(), "never-opened"); err != nil {
		t.Errorf("Delete of unopened tenant: %v", err)
	}
}

func TestDelete_WaitsForUsers(t *testing.T) {
	d := newDBs(t, Config{})

	db, release := acquire(t, d, "acme")

	deleted := make(chan error)
	go func() { deleted <- d.Delete(t.Context(), "acme") }()

	// Until released, Delete waits and new Acquires are refused.
	waitFor(t, func() bool {
		_, r, err := d.Acquire(t.Context(), "acme")
		if err == nil {
			r() // Delete hasn't started yet
		}
		return errors.Is(err, ErrDeleting)
	})
	if _, err := db.Exec(`insert into things (id) values (1)`); err != nil {
		t.Fatalf("DB closed while still acquired: %v", err)
	}

	release()
	if err := <-deleted; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !isClosed(db) {
		t.Error("DB still open after Delete")
	}
}

func TestDelete_GivesUpWithContext(t *testing.T) {
	d := newDBs(t, Config{})

	db, release := acquire(t, d, "acme")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := d.Delete(ctx, "acme"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delete of in-use tenant = %v, want DeadlineExceeded", err)
	}

	// The tenant is usable again, and goes back to idle on release.
	_, release2 := acquire(t, d, "acme")
	release2()
	release()
	if isClosed(db) {
		t.Error("DB closed after a cancelled Delete")
	}
	if s := d.Stats(); s.Open != 1 || s.InUse != 0 {
		t.Errorf("Stats = %+v, want 1 open, 0 in use", s)
	}
}

func TestClose(t *testing.T) {
	d := New(Config{Dir: t.TempDir()}) // with the sweeper running
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	db, release, err := d.Acquire(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	release()

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !isClosed(db) {
		t.Error("DB open after Close")
	}
	if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrClosed) {
		t.Errorf("Acquire after Close = %v, want ErrClosed", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestConcurrent churns many tenants through a small cap; run with -race.
func TestConcurrent(t *testing.T) {
	d := newDBs(t, Config{MaxOpen: 4})
	for id := range 12 {
		if err := d.Create(t.Context(), fmt.Sprint(id)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 50 {
				id := fmt.Sprint((w + i) % 12)
				err := d.Do(t.Context(), id, func(db *sql.DB) error {
					_, err := db.Exec(`insert into things default values`)
					return err
				})
				if err != nil {
					t.Errorf("Do(%s): %v", id, err)
					return
				}
				if i%17 == 0 {
					d.closeIdle(time.Now())
				}
			}
		})
	}
	wg.Wait()

	if s := d.Stats(); s.Open > 4 || s.InUse != 0 {
		t.Errorf("Stats = %+v after all released, want <= 4 open, 0 in use", s)
	}
	var total int
	for id := range 12 {
		d.Do(t.Context(), fmt.Sprint(id), func(db *sql.DB) error {
			var n int
			err := db.QueryRow(`select count(*) from things`).Scan(&n)
			total += n
			return err
		})
	}
	if total != 8*50 {
		t.Errorf("%d rows across tenants, want %d", total, 8*50)
	}
}

func TestMiddleware(t *testing.T) {
	d := newDBs(t, Config{})
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}

	resolve := func(r *http.Request) (string, error) {
		switch id := r.PathValue("tenant"); id {
		case "":
			return "", nil
		case "missing":
			return "", ErrNotFound
		default:
			return id, nil
		}
	}

	var gotID string
	var inUse int
	mux := http.NewServeMux()
	h := d.Middleware(resolve)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = ID(r.Context())
		inUse = d.Stats().InUse
		if gotID != "" {
			if err := DB(r.Context()).PingContext(r.Context()); err != nil {
				t.Errorf("tenant DB: %v", err)
			}
		}
	}))
	mux.Handle("/t/{tenant}", h)
	mux.Handle("/master", h)

	tests := []struct {
		path   string
		status int
		id     string
		inUse  int
	}{
		{"/t/acme", http.StatusOK, "acme", 1},
		{"/t/missing", http.StatusNotFound, "", 0},
		{"/t/nodb", http.StatusNotFound, "", 0}, // resolves, but never created
		{"/master", http.StatusOK, "", 0},
	}
	for _, tt := range tests {
		gotID, inUse = "", 0
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", tt.path, nil))
		if w.Code != tt.status {
			t.Errorf("%s: status %d, want %d", tt.path, w.Code, tt.status)
		}
		if gotID != tt.id {
			t.Errorf("%s: tenant ID %q, want %q", tt.path, gotID, tt.id)
		}
		if inUse != tt.inUse {
			t.Errorf("%s: %d in use during request, want %d", tt.path, inUse, tt.inUse)
		}
	}
	if s := d.Stats(); s.InUse != 0 {
		t.Errorf("%d still in use after requests", s.InUse)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
