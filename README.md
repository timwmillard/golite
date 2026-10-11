# golite

Utility packages for my default Go + SQLite setup.

| Package | |
|---|---|
| `migrate` | Forward-only `NNNN_name.sql` migrations from an `fs.FS`, plus optional `pragmas.sql` |
| `server` | HTTP server with graceful shutdown, config from `.env` / env vars / flags, `GET /health`, optional River job queue and RiverUI, `OpenDB` to open and migrate the SQLite database, `RequestError` / `ResponseError` JSON error handlers for oapi-codegen's strict server, and `WriteError` for the same `{"error": ...}` body elsewhere |
| `tenant` | One SQLite database per tenant for apps with thousands of them, as `<name>_<id>.db` in one folder (e.g. `company_acme.db`, `firm_<uuid>.db`), where the id is a unique value of the app's that never changes: opened and migrated on use, closed least-recently-used past a cap or when idle (never while in use), `MigrateAll` for deploys, middleware that puts the request's tenant database in its context, deletes that move the files to a trash folder before the tenant leaves the master database, offline renames, and `NewSync`: River jobs inserted in the same transaction as each master change to create tenant databases and copy master records into them |
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
- [`samples/multitenant`](samples/multitenant): the todo API made multi-tenant with `tenant`, where the tenants are companies: a master database lists the companies and runs River, and each company's tasks live in its own database, named after the slug it was created with (the file keeps that name if the slug changes, and can be renamed offline). `tenant.Sync` jobs carry master changes to the tenant databases; package `mirror` defines what's copied. Also a separate module.

Start a new project from a sample with `cmd/golite`:

```sh
go install github.com/timwmillard/golite/cmd/golite@latest
golite new                  # pick a sample from a menu
golite new todo myapp       # into ./myapp, module myapp
golite new -module github.com/me/myapp multitenant myapp
```
