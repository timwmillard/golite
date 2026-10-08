package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "test.db")

	migrations := fstest.MapFS{
		"0001_init.sql": {Data: []byte(`create table farm (id integer primary key);`)},
	}

	db, err := Open(ctx, path, migrations)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRowContext(ctx, "pragma journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}

	var fk int
	if err := db.QueryRowContext(ctx, "pragma foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	if _, err := db.ExecContext(ctx, "insert into farm (id) values (1)"); err != nil {
		t.Errorf("insert into migrated table: %v", err)
	}
}
