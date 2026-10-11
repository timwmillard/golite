// Command multitenantserver serves the todo API for many companies, each
// with its own database: a tenant, in golite's terms. data/master.db lists
// the companies and holds the River job queue; each company's tasks live in
// its own data/companies/company_<db_key>.db, where db_key is the slug the
// company was created with: the file keeps its name if the slug changes.
// Company databases open when first used and close when idle or when too
// many are open, so there can be far more companies than open files.
// tenant.Sync's River jobs carry changes from master to the company
// databases (see package mirror). A deleted company's database goes to
// data/companies/.trash, and is removed after 30 days.
//
//	go generate ./...   # after editing api/spec.yaml or the queries
//	go run ./cmd/multitenantserver
//	curl -X POST localhost:7880/v1/companies -d '{"slug":"acme","name":"Acme Corp"}'
//	curl -X POST localhost:7880/v1/companies/acme/tasks -d '{"title":"Buy milk"}'
//	curl localhost:7880/v1/companies/acme/tasks
//	curl -X PATCH localhost:7880/v1/companies/acme -d '{"name":"Acme Inc"}'
//	curl -X PUT localhost:7880/v1/companies/acme/slug -d '{"slug":"acme-inc"}'
//	curl -X DELETE localhost:7880/v1/companies/acme-inc
//	open localhost:7880/riverui/
//
// Each company's database is migrated when it's opened. On startup the
// server also migrates every company in the background, so ones not in use
// are brought up to date and a failing migration is logged soon after a
// deploy. To do that up front instead, then exit:
//
//	go run ./cmd/multitenantserver -migrate-companies
//
// To rename a company's database, say after a slug change, stop the server
// (and anything else using the database, such as Litestream), then:
//
//	go run ./cmd/multitenantserver -rename-company acme:acme-inc
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/riverqueue/river"

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
	renameCompany := flag.String("rename-company", "", "rename a company's database from one db_key to another, as `old:new`, then exit; stop the server first")
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

	q := mastermodel.New(master)
	companySync, err := tenant.NewSync(tenant.SyncConfig{
		DBs:     dbs,
		Exists:  q.DBKeyTaken,
		Mirrors: mirror.All(master),
	})
	if err != nil {
		return err
	}

	if *migrateCompanies {
		return migrateAll(ctx, dbs)
	}
	if *renameCompany != "" {
		return rename(ctx, dbs, q, *renameCompany)
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
		if err := dbs.PurgeTrash(30 * 24 * time.Hour); err != nil {
			slog.Error("Emptying company trash failed", "err", err)
		}
		err := migrateAll(bgCtx, dbs)
		if errors.Is(err, context.Canceled) {
			slog.Info("Company migration stopped by shutdown")
		} else if err != nil {
			slog.Error("Company migration failed", "err", err)
		}
	})

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}

// migrateAll applies pending migrations to every company's database.
func migrateAll(ctx context.Context, dbs *tenant.DBs) error {
	start := time.Now()
	slog.Info("Migrating companies")
	if err := dbs.MigrateAll(ctx); err != nil {
		return err
	}
	slog.Info("Migrated companies", "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// rename moves a company's database from one db_key to another and records
// the new one in master, for -rename-company old:new. Running it again
// after it failed partway finishes it.
func rename(ctx context.Context, dbs *tenant.DBs, q *mastermodel.Queries, arg string) error {
	from, to, ok := strings.Cut(arg, ":")
	if !ok {
		return fmt.Errorf("-rename-company %q: want old:new", arg)
	}
	err := dbs.Rename(ctx, from, to, func(ctx context.Context) error {
		n, err := q.RenameDBKey(ctx, mastermodel.RenameDBKeyParams{DbKey: from, NewDbKey: to})
		if err == nil && n == 0 {
			err = fmt.Errorf("no company has db_key %q", from)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("rename company database %s to %s: %w", from, to, err)
	}
	slog.Info("Renamed company database", "from", from, "to", to)
	return nil
}
