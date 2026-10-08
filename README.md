# golite

Utility packages for my default Go + SQLite setup.

| Package | |
|---|---|
| `sqlite` | `Open` a database with WAL, foreign keys, busy timeout, immediate txns, one connection, and apply migrations |
| `migrate` | Forward-only `NNNN_name.sql` migrations from an `fs.FS`, plus optional `pragmas.sql` |
| `server` | HTTP server with graceful shutdown, optional River job queue and RiverUI |
| `colorlog` | Colored `slog.Handler`, plus `cmd/colorlog` to pretty-print JSON logs |

```go
db, err := sqlite.Open(ctx, "data/app.db", migrations.FS)

workers := river.NewWorkers()
river.AddWorker(workers, &worker.Hello{})

srv, err := server.New(ctx, server.Config{Addr: ":8080", DB: db, Workers: workers, RiverUI: true})
srv.Mux.Handle("/api/", api.NewRouter(db, srv.River))
err = srv.Run(ctx) // blocks until SIGINT/SIGTERM
```
