package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestName(t *testing.T) {
	dir := t.TempDir()
	d := New(Config{Dir: dir, IdleTimeout: -1})
	defer d.Close()
	if p, _ := d.Path("7"); p != filepath.Join(dir, "tenant_7.db") {
		t.Errorf("default Path = %s, want tenant_7.db", p)
	}

	club := New(Config{Name: "club", Dir: dir, IdleTimeout: -1})
	defer club.Close()
	if p, _ := club.Path("7"); p != filepath.Join(dir, "club_7.db") {
		t.Errorf("Path with Name club = %s, want club_7.db", p)
	}
	if err := os.WriteFile(filepath.Join(dir, "club_bad.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := club.MigrateAll(t.Context()); err == nil || !strings.HasPrefix(err.Error(), "club bad:") {
		t.Errorf("MigrateAll error = %v, want it to name the club", err)
	}

	defer func() {
		if recover() == nil {
			t.Error("New with an invalid Name didn't panic")
		}
	}()
	New(Config{Name: "../x", Dir: dir})
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
	if err := d.Delete(t.Context(), "acme", nil); err != nil {
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

func TestMigrateAll(t *testing.T) {
	fsys := fstest.MapFS{"0001_init.sql": testMigrations["0001_init.sql"]}
	d := newDBs(t, Config{Migrations: fsys, MaxOpen: 2})

	ids := []string{"a", "b"}
	for _, id := range ids {
		if err := d.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	d.closeIdle(time.Now())

	// A new migration ships.
	fsys["0002_more.sql"] = &fstest.MapFile{Data: []byte(`create table more (id integer primary key)`)}
	if err := d.MigrateAll(t.Context()); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	if s := d.Stats(); s.Open != 0 {
		t.Errorf("Stats = %+v after MigrateAll, want nothing left open", s)
	}
	for _, id := range ids {
		err := d.Do(t.Context(), id, func(db *sql.DB) error {
			_, err := db.Exec(`insert into more default values`)
			return err
		})
		if err != nil {
			t.Errorf("tenant %s not migrated: %v", id, err)
		}
	}
}

// A run over many tenants mustn't push the ones in use out of the open
// set, nor reorder them.
func TestMigrateAll_KeepsHotTenantsOpen(t *testing.T) {
	d := newDBs(t, Config{MaxOpen: 2})
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		if err := d.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	d.closeIdle(time.Now())

	h1, release := acquire(t, d, "hot1")
	release()
	h2, release := acquire(t, d, "hot2") // more recently used than hot1
	release()
	if err := d.MigrateAll(t.Context()); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	if isClosed(h1) || isClosed(h2) {
		t.Fatal("MigrateAll closed a tenant that was open")
	}
	if s := d.Stats(); s.Open != 2 {
		t.Errorf("Stats = %+v, want just the 2 hot tenants open", s)
	}

	// hot1 is still the least recently used, so it's the one evicted.
	_, release = acquire(t, d, "c1")
	release()
	if !isClosed(h1) || isClosed(h2) {
		t.Error("MigrateAll changed the LRU order of open tenants")
	}
}

// A database MigrateAll opened stays open if a request starts using it.
func TestMigrateAll_ColdBecomesHot(t *testing.T) {
	d := newDBs(t, Config{})
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	d.closeIdle(time.Now())

	_, releaseMigrate, err := d.acquire(t.Context(), "acme", modeMigrate)
	if err != nil {
		t.Fatal(err)
	}
	db, release := acquire(t, d, "acme")
	releaseMigrate()
	release()
	if isClosed(db) {
		t.Error("cold database closed though a request used it")
	}
	if s := d.Stats(); s.Open != 1 {
		t.Errorf("Stats = %+v, want 1 open", s)
	}
}

func TestMigrateAll_StopsWithContext(t *testing.T) {
	d := newDBs(t, Config{})
	if err := d.Create(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	d.closeIdle(time.Now())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.MigrateAll(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("MigrateAll with cancelled ctx = %v, want Canceled", err)
	}
	if s := d.Stats(); s.Open != 0 {
		t.Error("MigrateAll opened a tenant after ctx was done")
	}
}

func TestMigrateAll_ContinuesPastFailures(t *testing.T) {
	d := newDBs(t, Config{})
	if err := d.Create(t.Context(), "ok"); err != nil {
		t.Fatal(err)
	}
	d.closeIdle(time.Now())

	for _, id := range []string{"bad", "also-bad"} {
		path, _ := d.Path(id)
		if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := d.MigrateAll(t.Context())
	if err == nil {
		t.Fatal("MigrateAll with corrupt databases succeeded")
	}
	for _, want := range []string{"tenant bad:", "tenant also-bad:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't mention %s", err, want)
		}
	}
	if _, release, err := d.Acquire(t.Context(), "ok"); err != nil {
		t.Errorf("valid tenant after a failed one not migrated: %v", err)
	} else {
		release()
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
	if _, err := db.Exec(`insert into things (id) values (1)`); err != nil {
		t.Fatal(err)
	}
	release()
	committed := false
	err := d.Delete(t.Context(), "acme", func(context.Context) error {
		committed = true
		// The id is held until commit returns.
		if _, _, err := d.Acquire(t.Context(), "acme"); !errors.Is(err, ErrDeleting) {
			t.Errorf("Acquire during commit = %v, want ErrDeleting", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !committed {
		t.Error("commit not called")
	}

	path, _ := d.Path("acme")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Delete", path+suffix)
		}
	}
	// It's in the trash, intact.
	trashed, _ := filepath.Glob(filepath.Join(d.cfg.Dir, trashDir, "*_tenant_acme.db"))
	if len(trashed) != 1 {
		t.Fatalf("trash has %v, want one tenant_acme.db", trashed)
	}
	old, err := sql.Open("sqlite3", trashed[0])
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var n int
	if err := old.QueryRow(`select count(*) from things`).Scan(&n); err != nil || n != 1 {
		t.Errorf("trashed database has %d rows (%v), want 1", n, err)
	}
	if ids, _ := d.List(); len(ids) != 0 {
		t.Errorf("List after Delete = %v, want none", ids)
	}
	if !isClosed(db) {
		t.Error("deleted tenant's DB still open")
	}
	if s := d.Stats(); s.Open != 0 {
		t.Errorf("Stats = %+v after Delete, want 0 open", s)
	}

	// Deleting a tenant with no database still commits, so a failed
	// Delete can be retried.
	committed = false
	if err := d.Delete(t.Context(), "never-created", func(context.Context) error { committed = true; return nil }); err != nil {
		t.Errorf("Delete of tenant with no database: %v", err)
	}
	if !committed {
		t.Error("commit not called for a tenant with no database")
	}

	// The same id can be created afresh at once.
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	if err := d.Do(t.Context(), "acme", func(db *sql.DB) error { return db.QueryRow(`select count(*) from things`).Scan(&n) }); err != nil || n != 0 {
		t.Errorf("recreated acme has %d rows (%v), want 0", n, err)
	}
}

func TestDelete_CommitFailsMovesBack(t *testing.T) {
	d := newDBs(t, Config{})
	db, release := acquire(t, d, "acme")
	if _, err := db.Exec(`insert into things (id) values (1)`); err != nil {
		t.Fatal(err)
	}
	release()

	boom := errors.New("boom")
	if err := d.Delete(t.Context(), "acme", func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Delete = %v, want the commit error", err)
	}
	if n := count(t, d, "acme"); n != 1 {
		t.Errorf("acme has %d rows after a failed Delete, want 1", n)
	}
	if trashed, _ := filepath.Glob(filepath.Join(d.cfg.Dir, trashDir, "*")); len(trashed) != 0 {
		t.Errorf("trash has %v after a failed Delete, want nothing", trashed)
	}
}

// A -wal left without its database, as by a Delete that crashed partway,
// isn't applied to a new database of the same id.
func TestCreate_SetsAsideOrphanedWAL(t *testing.T) {
	d := newDBs(t, Config{})
	path, _ := d.Path("acme")
	if err := os.WriteFile(path+"-wal", []byte("old frames"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(t.Context(), "acme"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	trashed, _ := filepath.Glob(filepath.Join(d.cfg.Dir, trashDir, "*_tenant_acme.db-wal"))
	if len(trashed) != 1 {
		t.Errorf("trash has %v, want the orphaned -wal", trashed)
	}
}

func TestPurgeTrash(t *testing.T) {
	d := newDBs(t, Config{})
	for _, id := range []string{"a", "b"} {
		if err := d.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if err := d.Delete(t.Context(), id, nil); err != nil {
			t.Fatal(err)
		}
	}
	trash := filepath.Join(d.cfg.Dir, trashDir)
	// Backdate a's deletion by two days.
	files, _ := os.ReadDir(trash)
	for _, f := range files {
		if _, name, _ := strings.Cut(f.Name(), "_"); name == "tenant_a.db" {
			old := time.Now().Add(-48*time.Hour).UTC().Format(trashStamp) + "_" + name
			if err := os.Rename(filepath.Join(trash, f.Name()), filepath.Join(trash, old)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := d.PurgeTrash(24 * time.Hour); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}
	var left []string
	files, _ = os.ReadDir(trash)
	for _, f := range files {
		_, name, _ := strings.Cut(f.Name(), "_")
		left = append(left, name)
	}
	if len(left) != 1 || left[0] != "tenant_b.db" {
		t.Errorf("trash after PurgeTrash = %v, want only tenant_b.db", left)
	}

	if err := newDBs(t, Config{}).PurgeTrash(time.Hour); err != nil {
		t.Errorf("PurgeTrash with no trash: %v", err)
	}
}

func TestList(t *testing.T) {
	d := newDBs(t, Config{Name: "firm"})
	if ids, err := d.List(); err != nil || len(ids) != 0 {
		t.Fatalf("List of empty Dir = %v, %v", ids, err)
	}
	for _, id := range []string{"e505a392-54f0-434a-a253-df54e17f8c38", "acme"} {
		if err := d.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	// Not tenant databases: another name, sidecars, the trash.
	for _, f := range []string{"club_x.db", "firm_acme.db-wal", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(d.cfg.Dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Create(t.Context(), "gone"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(t.Context(), "gone", nil); err != nil {
		t.Fatal(err)
	}

	ids, err := d.List()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "e505a392-54f0-434a-a253-df54e17f8c38"}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("List = %v, want %v", ids, want)
	}
}

func TestDelete_WaitsForUsers(t *testing.T) {
	d := newDBs(t, Config{})

	db, release := acquire(t, d, "acme")

	deleted := make(chan error)
	go func() { deleted <- d.Delete(t.Context(), "acme", nil) }()

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
	committed := false
	if err := d.Delete(ctx, "acme", func(context.Context) error { committed = true; return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delete of in-use tenant = %v, want DeadlineExceeded", err)
	}

	// The tenant is usable again, and goes back to idle on release.
	_, release2 := acquire(t, d, "acme")
	release2()
	release()
	if isClosed(db) {
		t.Error("DB closed after a cancelled Delete")
	}
	if committed {
		t.Error("commit called by a Delete that gave up")
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
	d := newDBs(t, Config{Name: "club"})
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
		if tt.status == http.StatusNotFound && !strings.Contains(w.Body.String(), `"club not found"`) {
			t.Errorf("%s: body %s, want it to say club not found", tt.path, w.Body)
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

func TestMaintain(t *testing.T) {
	fsys := fstest.MapFS{"0001_init.sql": testMigrations["0001_init.sql"]}
	d := newDBs(t, Config{Migrations: fsys})
	for _, id := range []string{"a", "gone"} {
		if err := d.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Delete(t.Context(), "gone", nil); err != nil {
		t.Fatal(err)
	}
	d.closeIdle(time.Now())

	fsys["0002_more.sql"] = &fstest.MapFile{Data: []byte(`create table more (id integer primary key)`)}
	if err := d.Maintain(t.Context(), 0); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if files, _ := os.ReadDir(filepath.Join(d.cfg.Dir, trashDir)); len(files) != 0 {
		t.Errorf("trash after Maintain = %d files, want it emptied", len(files))
	}
	err := d.Do(t.Context(), "a", func(db *sql.DB) error {
		_, err := db.Exec(`insert into more default values`)
		return err
	})
	if err != nil {
		t.Errorf("tenant a not migrated: %v", err)
	}
}

// Close stops a running Maintain and waits for it, so an app can start it
// with go and forget it.
func TestMaintain_StoppedByClose(t *testing.T) {
	d := newDBs(t, Config{})
	for i := range 200 {
		if err := d.Create(t.Context(), fmt.Sprint("t", i)); err != nil {
			t.Fatal(err)
		}
	}
	d.closeIdle(time.Now())

	var maintErr error
	returned := make(chan struct{})
	go func() {
		maintErr = d.Maintain(t.Context(), time.Hour)
		close(returned)
	}()
	waitFor(t, func() bool { // under way, or already done
		select {
		case <-returned:
			return true
		default:
			return d.Stats().Open > 0
		}
	})
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Close returned before Maintain did")
	}
	if maintErr != nil && !errors.Is(maintErr, ErrClosed) {
		t.Errorf("Maintain stopped by Close = %v, want nil or ErrClosed", maintErr)
	}

	if err := d.Maintain(t.Context(), time.Hour); !errors.Is(err, ErrClosed) {
		t.Errorf("Maintain after Close = %v, want ErrClosed", err)
	}
}
