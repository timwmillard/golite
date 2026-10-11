// Command frontendserver serves the frontend sample: server-rendered templ
// pages that use htmx to add, toggle and delete tasks, styled with
// Tailwind, over sqlc queries on SQLite.
//
//	go generate ./...   # after editing queries/, app/*.templ or Tailwind classes
//	go run ./cmd/frontendserver
//	open http://localhost:7880/
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/timwmillard/golite/server"

	"github.com/timwmillard/golite/samples/frontend/app"
	"github.com/timwmillard/golite/samples/frontend/migrations"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg := server.Config{Port: 7880}
	dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
	if err := cfg.Parse(); err != nil {
		return err
	}

	db, err := server.OpenDB(ctx, filepath.Join(*dataDir, "frontend.db"), migrations.FS)
	if err != nil {
		return err
	}
	defer db.Close()
	cfg.DB = db

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}
	app.Register(srv.Mux, db)

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}
