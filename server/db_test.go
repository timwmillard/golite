package server

import (
	"database/sql/driver"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestOpenDB_CreatesDirAndMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "app.db")
	fsys := fstest.MapFS{
		"0001_init.sql": {Data: []byte(`create table things (id integer primary key)`)},
	}

	db, err := OpenDB(t.Context(), path, fsys)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(t.Context(), `insert into things (id) values (1)`); err != nil {
		t.Fatalf("insert into migrated table: %v", err)
	}
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}
}

func TestOpenDB_NilMigrations(t *testing.T) {
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	db.Close()
}

func TestOpenDB_BadMigration(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_bad.sql": {Data: []byte(`not sql`)},
	}
	if _, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "app.db"), fsys); err == nil {
		t.Fatal("OpenDB: want error for bad migration")
	}
}

func TestOpenDB_PragmasOnEveryConnection(t *testing.T) {
	fsys := fstest.MapFS{
		"pragmas.sql":   {Data: []byte(`pragma foreign_keys = on; pragma busy_timeout = 4321;`)},
		"0001_init.sql": {Data: []byte(`create table things (id integer primary key)`)},
	}
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "app.db"), fsys)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	check := func(when string) {
		t.Helper()
		var fk, timeout int
		if err := db.QueryRowContext(t.Context(), `pragma foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(t.Context(), `pragma busy_timeout`).Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || timeout != 4321 {
			t.Errorf("%s: foreign_keys = %d, busy_timeout = %d; want 1, 4321", when, fk, timeout)
		}
	}
	check("first connection")

	// Returning ErrBadConn makes database/sql discard the connection, so the
	// next query opens a new one.
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn.Raw(func(any) error { return driver.ErrBadConn })
	conn.Close()
	if got := db.Stats().OpenConnections; got != 0 {
		t.Fatalf("OpenConnections = %d after discarding, want 0", got)
	}
	check("replacement connection")
}

func TestOpenDB_BadPragmas(t *testing.T) {
	fsys := fstest.MapFS{"pragmas.sql": {Data: []byte(`not sql`)}}
	if _, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "app.db"), fsys); err == nil {
		t.Fatal("OpenDB: want error for bad pragmas.sql")
	}
}
