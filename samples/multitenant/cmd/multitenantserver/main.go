// Command multitenantserver serves the todo API for many companies, each
// with its own database: a tenant, in golite's terms. data/master.db lists
// the companies and holds the River job queue; each company's tasks live in
// its own data/companies/company_<id>.db. Company databases open when first
// used and close when idle or when too many are open, so there can be far
// more companies than open files. tenant.Sync's River jobs carry changes
// from master to the company databases (see package mirror).
//
//	go generate ./...   # after editing api/spec.yaml or the queries
//	go run ./cmd/multitenantserver
//	curl -X POST localhost:7880/v1/companies -d '{"slug":"acme","name":"Acme Corp"}'
//	curl -X POST localhost:7880/v1/companies/acme/tasks -d '{"title":"Buy milk"}'
//	curl localhost:7880/v1/companies/acme/tasks
//	curl -X PATCH localhost:7880/v1/companies/acme -d '{"name":"Acme Inc"}'
//	curl -X DELETE localhost:7880/v1/companies/acme
//	open localhost:7880/riverui/
//
// Each company's database is migrated when it's opened. On startup the
// server also migrates every company in the background, so ones not in use
// are brought up to date and a failing migration is logged soon after a
// deploy. To do that up front instead, then exit:
//
//	go run ./cmd/multitenantserver -migrate-companies
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/conv"
	"github.com/timwmillard/golite/server"
	"github.com/timwmillard/golite/tenant"

	"github.com/timwmillard/golite/samples/multitenant/api"
	companymigrations "github.com/timwmillard/golite/samples/multitenant/companydb/migrations"
	mastermigrations "github.com/timwmillard/golite/samples/multitenant/master/migrations"
	mastermodel "github.com/timwmillard/golite/samples/multitenant/master/model"
	"github.com/timwmillard/golite/samples/multitenant/mirror"
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
	maxOpen := flag.Int("max-open-companies", 256, "company databases to keep open")
	idleTimeout := flag.Duration("company-idle-timeout", 10*time.Minute, "close a company database after this long unused")
	migrateCompanies := flag.Bool("migrate-companies", false, "migrate every company's database, then exit")
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
		Name:        "company",
		Dir:         filepath.Join(*dataDir, "companies"),
		Migrations:  companymigrations.FS,
		MaxOpen:     *maxOpen,
		IdleTimeout: *idleTimeout,
	})
	defer dbs.Close()

	if *migrateCompanies {
		return migrateAll(ctx, master, dbs)
	}

	companySync, err := tenant.NewSync(tenant.SyncConfig{
		DBs:     dbs,
		Exists:  mirror.Exists(master),
		Mirrors: mirror.All(master),
	})
	if err != nil {
		return err
	}
	workers := river.NewWorkers()
	companySync.AddWorkers(workers)
	cfg.DB, cfg.Workers = master, workers // River runs on master; GET /health pings it

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}
	api.Register(srv.Mux, master, dbs, srv.River)

	// Stopped when Run returns, and waited for before dbs.Close.
	bgCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Go(func() {
		err := migrateAll(bgCtx, master, dbs)
		if errors.Is(err, context.Canceled) {
			slog.Info("Company migration stopped by shutdown")
		} else if err != nil {
			slog.Error("Company migration failed", "err", err)
		}
	})

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}

// migrateAll applies pending migrations to every company in master.
func migrateAll(ctx context.Context, master *sql.DB, dbs *tenant.DBs) error {
	ids, err := mastermodel.New(master).ListCompanyIDs(ctx)
	if err != nil {
		return fmt.Errorf("list companies: %w", err)
	}

	start := time.Now()
	slog.Info("Migrating companies", "count", len(ids))
	if err := dbs.MigrateAll(ctx, conv.FormatIDs(ids)); err != nil {
		return err
	}
	slog.Info("Migrated companies", "count", len(ids), "took", time.Since(start).Round(time.Millisecond))
	return nil
}
