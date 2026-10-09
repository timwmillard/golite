# golite

Utility packages for my default Go + SQLite setup.

| Package | |
|---|---|
| `migrate` | Forward-only `NNNN_name.sql` migrations from an `fs.FS`, plus optional `pragmas.sql` |
| `server` | HTTP server with graceful shutdown, config from `.env` / env vars / flags, `GET /health`, optional River job queue and RiverUI, `OpenDB` to open and migrate the SQLite database, `RequestError` / `ResponseError` JSON error handlers for oapi-codegen's strict server, and `WriteError` for the same `{"error": ...}` body elsewhere |
| `tenant` | One SQLite database per tenant for apps with thousands of them: opened and migrated on use, closed least-recently-used past a cap or when idle (never while in use), plus middleware that puts the request's tenant database in its context |
| `conv` | Converts sqlc values for the API: int64 ↔ string IDs, `sql.Null*` ↔ pointers, 0/1 ↔ bool, unix seconds ↔ `time.Time` |
| `colorlog` | Colored `slog.Handler`, plus `cmd/colorlog` to pretty-print JSON logs |

```go
cfg := server.Config{Port: 7880, RiverUI: true}
dataDir := flag.String("data", "data", "data directory") // also settable as $DATA
if err := cfg.Parse(); err != nil { // defaults < .env < env vars < flags
	log.Fatal(err)
}

// migrations.FS is an embed.FS; pragmas live in migrations/pragmas.sql
db, err := server.OpenDB(ctx, filepath.Join(*dataDir, "app.db"), migrations.FS)

workers := river.NewWorkers()
river.AddWorker(workers, &worker.Hello{})
cfg.DB, cfg.Workers = db, workers

srv, err := server.New(ctx, cfg)
api.Register(srv.Mux, db) // the app's oapi-codegen package; see samples/todo
err = srv.Run(ctx) // blocks until SIGINT/SIGTERM
```

Runnable samples:

- [`samples/server`](samples/server): River worker, RiverUI and hand-written JSON handlers.
- [`samples/todo`](samples/todo): OpenAPI spec (oapi-codegen strict server) plus sqlc queries. It's a separate module so the codegen tools stay out of golite's `go.mod`.
- [`samples/multitenant`](samples/multitenant): the todo API made multi-tenant with `tenant`: a master database lists the tenants and runs River, and each tenant's tasks live in its own database. River jobs inserted in the same transaction as each master change carry it to the tenant's database. Also a separate module.
