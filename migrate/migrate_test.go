package migrate

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	_ "github.com/mattn/go-sqlite3"
)

// testMigrations stands in for a project's embedded migrations directory.
var testMigrations = fstest.MapFS{
	"pragmas.sql": {Data: []byte(`
		pragma foreign_keys = on;
		pragma journal_mode = wal;
		pragma busy_timeout = 5000;
	`)},
	"0001_init.sql":  {Data: []byte(`create table farm (id integer primary key, name text not null);`)},
	"0002_field.sql": {Data: []byte(`create table field (id integer primary key, farm_id integer not null references farm (id));`)},
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	migs, err := readMigrations(testMigrations)
	if err != nil {
		t.Fatalf("readMigrations: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, "select count(*) from schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != len(migs) {
		t.Errorf("schema_migrations has %d rows, want %d (one per embedded migration file)", count, len(migs))
	}
}

func TestApply_OnlyRunsPendingMigrations(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Seed schema_migrations as if only version 1 had ever been applied,
	// without actually running its SQL — this simulates "existing
	// database, new migration shipped" without needing a second real
	// migration file to seed against.
	if _, err := db.ExecContext(ctx, `
		create table if not exists schema_migrations (
			version    integer primary key,
			applied_at integer not null
		)
	`); err != nil {
		t.Fatalf("seed schema_migrations table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `insert into schema_migrations (version, applied_at) values (1, 0)`); err != nil {
		t.Fatalf("seed schema_migrations row: %v", err)
	}

	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// version 1 was pre-seeded, so its SQL (which creates the `farm`
	// table) never ran via Apply — querying it should fail.
	if _, err := db.QueryContext(ctx, "select count(*) from farm"); err == nil {
		t.Errorf("expected version 1's SQL not to have been re-run, but the farm table exists")
	}

	migs, err := readMigrations(testMigrations)
	if err != nil {
		t.Fatalf("readMigrations: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, "select count(*) from schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != len(migs) {
		t.Errorf("schema_migrations has %d rows, want %d", count, len(migs))
	}
}

func TestApply_PragmasFileNotTrackedAsMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var mode string
	if err := db.QueryRowContext(ctx, "pragma journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "memory" {
		// :memory: databases can't use WAL, but pragmas.sql's other
		// settings (foreign_keys, busy_timeout) still applied without
		// error — this just confirms Apply didn't choke trying to treat
		// pragmas.sql as a numbered migration file.
		t.Logf("journal_mode = %q (expected memory-only override for :memory: db)", mode)
	}

	var fk int
	if err := db.QueryRowContext(ctx, "pragma foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (from pragmas.sql)", fk)
	}
}

func TestApply_Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
}

func TestApply_SetsJournalModeWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", dir+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var mode string
	if err := db.QueryRowContext(ctx, "pragma journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestPragmas(t *testing.T) {
	got, err := Pragmas(fstest.MapFS{"pragmas.sql": {Data: []byte(`pragma foreign_keys = on;`)}})
	if err != nil || got != `pragma foreign_keys = on;` {
		t.Errorf("Pragmas = %q, %v; want the file's contents", got, err)
	}

	got, err = Pragmas(fstest.MapFS{"0001_init.sql": {Data: []byte(`select 1`)}})
	if err != nil || got != "" {
		t.Errorf("Pragmas without pragmas.sql = %q, %v; want \"\", nil", got, err)
	}
}

// A library's migrations, tracked in their own table, run alongside the
// app's even though both start at 0001.
func TestApplyTable_IndependentOfApply(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	lib := fstest.MapFS{"0001_lib.sql": {Data: []byte(`create table lib (id integer primary key);`)}}
	if err := Apply(ctx, db, testMigrations); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := ApplyTable(ctx, db, lib, "lib_migrations"); err != nil {
		t.Fatalf("ApplyTable: %v", err)
	}
	if err := ApplyTable(ctx, db, lib, "lib_migrations"); err != nil {
		t.Fatalf("second ApplyTable: %v", err)
	}

	for table, want := range map[string]int{"schema_migrations": 2, "lib_migrations": 1, "lib": 0} {
		var n int
		if err := db.QueryRowContext(ctx, "select count(*) from "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}

	if err := ApplyTable(ctx, db, lib, "bad name; drop table lib"); err == nil {
		t.Error("ApplyTable with an invalid table name succeeded")
	}
}
