# golite

Utility packages for my default Go + SQLite setup.

| Package | |
|---|---|
| `migrate` | Forward-only `NNNN_name.sql` migrations from an `fs.FS`, plus optional `pragmas.sql` |
| `server` | HTTP server with graceful shutdown, config from `.env` / env vars / flags, optional River job queue and RiverUI |
| `api` | JSON responses/errors, request decoding, string IDs, and `sql.Null*` ↔ pointer conversions |
| `colorlog` | Colored `slog.Handler`, plus `cmd/colorlog` to pretty-print JSON logs |

```go
cfg := server.Config{Port: 7880, RiverUI: true}
dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
if err := cfg.Parse(); err != nil { // defaults < .env < env vars < flags
	log.Fatal(err)
}

db, err := sql.Open("sqlite3", filepath.Join(*dataDir, "app.db"))
db.SetMaxOpenConns(1)
err = migrate.Apply(ctx, db, migrations.FS) // pragmas live in migrations/pragmas.sql

workers := river.NewWorkers()
river.AddWorker(workers, &worker.Hello{})
cfg.DB, cfg.Workers = db, workers

srv, err := server.New(ctx, cfg)
srv.Mux.Handle("/api/", api.NewRouter(db, srv.River))
err = srv.Run(ctx) // blocks until SIGINT/SIGTERM
```
