// Package migrate applies numbered, forward-only SQL migration files to a
// SQLite database. An optional pragmas.sql in the same filesystem holds
// connection-level pragmas instead of schema — see pragmasFile.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pragmasFile, if present in fsys, holds connection-level pragmas (e.g.
// journal_mode, foreign_keys, busy_timeout) rather than schema. It's
// project-specific setup, not a migration: it's exec'd on every Apply call,
// outside any transaction (some pragmas — journal_mode=wal in particular —
// error out or silently fail to take effect inside one), and it never gets
// a row in schema_migrations. It's excluded from the NNNN_ version parsing
// in readMigrations. Optional — a caller with no special pragma needs can
// just omit the file.
const pragmasFile = "pragmas.sql"

// Apply runs every migration in fsys whose numeric prefix is greater than
// the highest version already recorded in schema_migrations, in ascending
// order, each in its own transaction.
func Apply(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if err := applyPragmas(ctx, db, fsys); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		create table if not exists schema_migrations (
			version    integer primary key,
			applied_at integer not null
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	migrations, err := readMigrations(fsys)
	if err != nil {
		return err
	}

	var current int
	row := db.QueryRowContext(ctx, `select coalesce(max(version), 0) from schema_migrations`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}

	return nil
}

// applyPragmas execs pragmasFile's contents, if present, outside of any
// transaction. It's a no-op if fsys doesn't have the file.
func applyPragmas(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	contents, err := fs.ReadFile(fsys, pragmasFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", pragmasFile, err)
	}

	if _, err := db.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("apply %s: %w", pragmasFile, err)
	}

	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func readMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migration files: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry == pragmasFile {
			continue
		}

		version, _, ok := strings.Cut(entry, "_")
		if !ok {
			return nil, fmt.Errorf("migration file %q missing NNNN_ prefix", entry)
		}
		versionNum, err := strconv.Atoi(version)
		if err != nil {
			return nil, fmt.Errorf("migration file %q has non-numeric prefix: %w", entry, err)
		}

		contents, err := fs.ReadFile(fsys, entry)
		if err != nil {
			return nil, fmt.Errorf("read migration file %q: %w", entry, err)
		}

		migrations = append(migrations, migration{version: versionNum, name: entry, sql: string(contents)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })

	return migrations, nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}

	if err := execMigration(ctx, conn, m); err != nil {
		if _, rbErr := conn.ExecContext(ctx, `rollback`); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}

	if _, err := conn.ExecContext(ctx, `commit`); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}

func execMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	if _, err := conn.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `insert into schema_migrations (version, applied_at) values (?, ?)`, m.version, time.Now().Unix()); err != nil {
		return fmt.Errorf("record applied migration: %w", err)
	}

	return nil
}
