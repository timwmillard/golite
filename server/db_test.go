package server

import (
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
