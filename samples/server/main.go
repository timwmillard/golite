// Command server is an example app wiring up the golite packages: config
// from .env / env vars / flags, a colored logger, SQLite with migrations, a
// River job queue with RiverUI, and a small JSON API.
//
//	go run ./samples/server -log-level debug
//	curl -X POST localhost:7880/api/greetings -d '{"name":"Tim"}'
//	curl localhost:7880/api/greetings
//	open http://localhost:7880/riverui/
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/samples/server/migrations"
	"github.com/timwmillard/golite/server"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Defaults go in the struct; .env, env vars and flags override them.
	cfg := server.Config{Port: 7880, RiverUI: true}
	dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
	if err := cfg.Parse(); err != nil {
		return err
	}
	log := cfg.Logger

	db, err := server.OpenDB(ctx, filepath.Join(*dataDir, "app.db"), migrations.FS)
	if err != nil {
		return err
	}
	defer db.Close()

	workers := river.NewWorkers()
	river.AddWorker(workers, &GreetWorker{DB: db, Logger: log})
	cfg.DB, cfg.Workers = db, workers

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}

	// GET /health is registered by server.New since cfg.DB is set.
	h := &handlers{db: db, river: srv.River}
	srv.Mux.HandleFunc("GET /api/greetings", h.listGreetings)
	srv.Mux.HandleFunc("POST /api/greetings", h.createGreeting)

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}
