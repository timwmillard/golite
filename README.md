# golite

Utility packages for my default Go + SQLite setup.

| Package | |
|---|---|
| `sqlite` | Opens a SQLite database (creating its directory), limits it to one connection, and applies migrations |
| `migrate` | Forward-only `NNNN_name.sql` migrations from an `fs.FS`, plus optional `pragmas.sql` |
| `server` | HTTP server with graceful shutdown, config from `.env` / env vars / flags, `GET /health`, optional River job queue and RiverUI |
| `api` | JSON responses/errors (including logged 500s), request decoding, string IDs, and `sql.Null*` ↔ pointer conversions |
| `colorlog` | Colored `slog.Handler`, plus `cmd/colorlog` to pretty-print JSON logs |

```go
cfg := server.Config{Port: 7880, RiverUI: true}
dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
if err := cfg.Parse(); err != nil { // defaults < .env < env vars < flags
	log.Fatal(err)
}

// migrations.FS is an embed.FS; pragmas live in migrations/pragmas.sql
db, err := sqlite.Open(ctx, filepath.Join(*dataDir, "app.db"), migrations.FS)

workers := river.NewWorkers()
river.AddWorker(workers, &worker.Hello{})
cfg.DB, cfg.Workers = db, workers

srv, err := server.New(ctx, cfg)
srv.Mux.Handle("/api/", api.NewRouter(db, srv.River))
err = srv.Run(ctx) // blocks until SIGINT/SIGTERM
```

See [`samples/server`](samples/server) for a complete, runnable app.
