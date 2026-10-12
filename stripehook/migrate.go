package stripehook

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"strings"

	"github.com/timwmillard/golite/migrate"
)

// MigrationsTable records which of stripehook's migrations have run,
// separately from the app's schema_migrations.
const MigrationsTable = "stripe_migrations"

//go:embed migrations/*.sql
var migrationsDir embed.FS

// Migrations holds stripehook's migrations, NNNN_name.sql as for package
// migrate.
var Migrations, _ = fs.Sub(migrationsDir, "migrations")

// Migrate creates or updates the stripe_event and stripe_object tables,
// recording what it's done in MigrationsTable. Call it at startup, after
// server.OpenDB and before New's Hook receives anything, as server.New does
// for River's tables.
func Migrate(ctx context.Context, db *sql.DB) error {
	return migrate.ApplyTable(ctx, db, Migrations, MigrationsTable)
}

// Schema is every migration in order: the tables as Migrate leaves them.
// It's for sqlc, which can't read migrations from the module cache: write it
// to a file only sqlc reads, e.g. with
//
//	//go:generate go run github.com/timwmillard/golite/stripehook/cmd/stripeschema sqlc/stripehook.sql
//
// and list that file under schema in sqlc.yaml. Don't execute it: Migrate
// does that, and keeps track.
var Schema = schema()

func schema() string {
	// Glob sorts by name, which is version order for NNNN_ names.
	names, err := fs.Glob(Migrations, "*.sql")
	if err != nil {
		panic(err)
	}
	var b strings.Builder
	for _, name := range names {
		data, err := fs.ReadFile(Migrations, name)
		if err != nil {
			panic(err)
		}
		b.WriteString("-- " + name + "\n")
		b.Write(data)
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}
