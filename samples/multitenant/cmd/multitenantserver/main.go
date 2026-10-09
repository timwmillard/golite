// Command multitenantserver serves the multi-tenant todo API. data/master.db
// lists the tenants and holds the River job queue; each tenant's tasks live
// in its own data/tenants/tenant_<id>.db. Tenant databases open when first
// used and close when idle or when too many are open, so there can be far
// more tenants than open files. River jobs (package tenantsync) carry
// changes from master to the tenant databases.
//
//	go generate ./...   # after editing api/spec.yaml or the queries
//	go run ./cmd/multitenantserver
//	curl -X POST localhost:7880/v1/tenants -d '{"slug":"acme","name":"Acme Corp"}'
//	curl -X POST localhost:7880/v1/tenants/acme/tasks -d '{"title":"Buy milk"}'
//	curl localhost:7880/v1/tenants/acme/tasks
//	curl -X PATCH localhost:7880/v1/tenants/acme -d '{"name":"Acme Inc"}'
//	curl -X DELETE localhost:7880/v1/tenants/acme
//	open localhost:7880/riverui/
//
// After a deploy that adds tenant migrations, bring every tenant's database
// up to date (otherwise each is migrated when next used):
//
//	go run ./cmd/multitenantserver -migrate-tenants
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/api"
	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	tenantmigrations "github.com/timwmillard/golite/samples/multitenant/tenantdb/migrations"
	"github.com/timwmillard/golite/samples/multitenant/tenantsync"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg := server.Config{Port: 7880, RiverUI: true}
	dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
	maxOpen := flag.Int("max-open-tenants", 256, "tenant databases to keep open")
	idleTimeout := flag.Duration("tenant-idle-timeout", 10*time.Minute, "close a tenant database after this long unused")
	migrateTenants := flag.Bool("migrate-tenants", false, "migrate every tenant's database, then exit")
	if err := cfg.Parse(); err != nil {
		return err
	}

	master, err := server.OpenDB(ctx, filepath.Join(*dataDir, "master.db"), mastermigrations.FS)
	if err != nil {
		return err
	}
	defer master.Close()

	// Deferred after master.Close so it runs first, but still after
	// srv.Run has stopped the server and River.
	dbs := tenant.New(tenant.Config{
		Dir:         filepath.Join(*dataDir, "tenants"),
		Migrations:  tenantmigrations.FS,
		MaxOpen:     *maxOpen,
		IdleTimeout: *idleTimeout,
	})
	defer dbs.Close()

	if *migrateTenants {
		return migrateAll(ctx, master, dbs)
	}

	workers := river.NewWorkers()
	tenantsync.AddWorkers(workers, master, dbs)
	cfg.DB, cfg.Workers = master, workers // River runs on master; GET /health pings it

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}
	api.Register(srv.Mux, master, dbs, srv.River)

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}

// migrateAll applies pending migrations to every tenant in master.
func migrateAll(ctx context.Context, master *sql.DB, dbs *tenant.DBs) error {
	ids, err := mastermodel.New(master).ListTenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	slog.Info("Migrating tenants", "count", len(ids))
	err = dbs.MigrateAll(ctx, conv.FormatIDs(ids))
	if err != nil {
		return err
	}
	slog.Info("Migrated tenants", "count", len(ids))
	return nil
}
