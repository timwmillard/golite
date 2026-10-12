package stripehook

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/timwmillard/golite/server"
)

// tables returns the CREATE statements of db's stripe_ tables, except
// stripe_migrations.
func tables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`select sql from sqlite_master
		where name like 'stripe\_%' escape '\' and name != ? order by name`, MigrationsTable)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func openDB(t *testing.T, migrations fstest.MapFS) *sql.DB {
	t.Helper()
	db, err := server.OpenDB(t.Context(), filepath.Join(t.TempDir(), "app.db"), migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrate(t *testing.T) {
	ctx := t.Context()
	// The app's own 0001 doesn't stop stripehook's 0001 running, or the
	// reverse.
	db := openDB(t, fstest.MapFS{"0001_app.sql": {Data: []byte(`create table app (id integer primary key)`)}})

	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	got := tables(t, db)
	if len(got) != 2 {
		t.Fatalf("stripe tables = %d, want 2 (stripe_event, stripe_object)", len(got))
	}

	var appVersions, stripeVersions int
	if err := db.QueryRow(`select count(*) from schema_migrations`).Scan(&appVersions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`select count(*) from ` + MigrationsTable).Scan(&stripeVersions); err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(Migrations, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if appVersions != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", appVersions)
	}
	if stripeVersions != len(files) {
		t.Errorf("%s rows = %d, want %d", MigrationsTable, stripeVersions, len(files))
	}
}

// Schema, what sqlc sees, makes the same tables as Migrate.
func TestSchemaMatchesMigrate(t *testing.T) {
	migrated := openDB(t, nil)
	if err := Migrate(t.Context(), migrated); err != nil {
		t.Fatal(err)
	}
	fromSchema := openDB(t, nil)
	if _, err := fromSchema.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	if got, want := tables(t, fromSchema), tables(t, migrated); !slices.Equal(got, want) {
		t.Errorf("Schema makes\n%v\nMigrate makes\n%v", got, want)
	}
}
