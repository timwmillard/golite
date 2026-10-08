// Package sqlite opens a SQLite database the way the other golite packages
// expect it: one connection, schema migrated.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"

	"github.com/timwmillard/golite/migrate"
)

// Open opens the SQLite database at path, creating it and its directory if
// needed, and applies the migrations in fsys (see package migrate). fsys may
// be nil to skip migrations.
//
// The pool is limited to one connection: SQLite allows a single writer, and
// sharing one connection between the app and River avoids SQLITE_BUSY.
func Open(ctx context.Context, path string, fsys fs.FS) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database dir: %w", err)
	}

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if fsys == nil {
		err = db.PingContext(ctx)
	} else {
		err = migrate.Apply(ctx, db, fsys)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	return db, nil
}
