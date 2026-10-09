// Command todoserver serves the todo API: an OpenAPI spec (api/spec.yaml,
// served by oapi-codegen's strict server) over sqlc queries on SQLite.
//
//	go generate ./...   # after editing api/spec.yaml or queries/
//	go run ./cmd/todoserver
//	curl -X POST localhost:7880/v1/tasks -d '{"title":"Buy milk"}'
//	curl localhost:7880/v1/tasks
//	curl -X PATCH localhost:7880/v1/tasks/1 -d '{"done":true}'
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/timwmillard/golite/server"

	"github.com/timwmillard/golite/samples/todo/api"
	"github.com/timwmillard/golite/samples/todo/migrations"
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

	db, err := server.OpenDB(ctx, filepath.Join(*dataDir, "todo.db"), migrations.FS)
	if err != nil {
		return err
	}
	defer db.Close()
	cfg.DB = db

	srv, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}
	api.Register(srv.Mux, db)

	return srv.Run(ctx) // blocks until SIGINT/SIGTERM
}
