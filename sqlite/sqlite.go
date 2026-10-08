// Package sqlite opens a SQLite database with the default settings for a
// single-process Go server: WAL, foreign keys, a busy timeout, immediate
// write transactions, and a single pooled connection.
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

// DSNParams are appended to the file path when opening. They're applied by
// the driver on every new connection, so unlike a pragmas.sql they survive
// the pool recycling a connection.
const DSNParams = "_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate"

// Open creates path's parent directory if needed, opens the database, and
// applies migrations from fsys (see package migrate). fsys may be nil to
// skip migrations.
//
// The pool is limited to one connection: SQLite only allows one writer at a
// time (River's SQLite driver relies on this too), and a single connection
// avoids SQLITE_BUSY errors under concurrent access.
func Open(ctx context.Context, path string, fsys fs.FS) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite3", "file:"+path+"?"+DSNParams)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if fsys != nil {
		if err := migrate.Apply(ctx, db, fsys); err != nil {
			db.Close()
			return nil, err
		}
	}

	return db, nil
}
