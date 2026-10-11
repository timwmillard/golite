package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mattn/go-sqlite3"

	"github.com/timwmillard/golite/migrate"
)

// OpenDB opens the SQLite database at path, creating it and its directory if
// needed, and applies the migrations in fsys (see package migrate). fsys may
// be nil to skip migrations.
//
// fsys's pragmas.sql, if any, runs on every connection as it's opened, so
// per-connection pragmas (foreign_keys, busy_timeout) hold even if
// database/sql replaces the connection.
//
// The pool is limited to one connection: SQLite allows a single writer, and
// sharing one connection between the app and River avoids SQLITE_BUSY. So
// inside a transaction, use only the transaction: a query on the *sql.DB
// waits for the connection the transaction holds, and never gets it.
func OpenDB(ctx context.Context, path string, fsys fs.FS) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database dir: %w", err)
	}

	var pragmas string
	if fsys != nil {
		var err error
		if pragmas, err = migrate.Pragmas(fsys); err != nil {
			return nil, err
		}
	}

	db := sql.OpenDB(&connector{dsn: path, driver: &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if pragmas == "" {
				return nil
			}
			if _, err := conn.Exec(pragmas, nil); err != nil {
				return fmt.Errorf("apply pragmas.sql: %w", err)
			}
			return nil
		},
	}})
	db.SetMaxOpenConns(1)

	var err error
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

// connector opens connections with its own driver, so OpenDB can give each
// database its own ConnectHook without registering a driver name globally.
type connector struct {
	dsn    string
	driver *sqlite3.SQLiteDriver
}

func (c *connector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c *connector) Driver() driver.Driver                        { return c.driver }
