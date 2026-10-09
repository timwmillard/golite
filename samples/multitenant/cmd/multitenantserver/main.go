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
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/api"
	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
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
