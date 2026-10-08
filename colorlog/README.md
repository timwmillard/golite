# colorlog

A colored `slog.Handler` for Go, plus a CLI tool for pretty-printing JSON log streams. Produces output styled after [humanlog](https://github.com/humanlogio/humanlog).

## Output format

```
Jan 02 15:04:05.000 |INFO| server started addr=:8080
Jan 02 15:04:05.123 |DEBU| incoming request method=GET path=/health
Jan 02 15:04:05.124 |WARN| slow query duration=1.2s table=users
Jan 02 15:04:05.200 |ERRO| connection failed err="dial tcp: refused" retries=3
```

Timestamp and level are colored by severity. Keys are green, values are muted, messages are white.

## Library

Use `colorlog.Handler` as a drop-in `slog.Handler`:

```go
import (
    "log/slog"
    "os"

    "github.com/timwmillard/golite/colorlog"
)

func main() {
    handler := colorlog.NewHandler(os.Stderr, nil)
    slog.SetDefault(slog.New(handler))

    slog.Info("server started", "addr", ":8080")
    slog.Warn("slow query", "duration", "1.2s", "table", "users")
    slog.Error("connection failed", "err", err, "retries", 3)
}
```

### Handler options

Pass `slog.HandlerOptions` to configure the minimum log level:

```go
handler := colorlog.NewHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})
```

### WithAttrs and WithGroup

`Handler` implements the full `slog.Handler` interface, including `WithAttrs` and `WithGroup`:

```go
logger := slog.New(colorlog.NewHandler(os.Stderr, nil))

// Attach persistent fields
reqLogger := logger.With("request_id", "abc123", "user_id", 42)
reqLogger.Info("handled request", "status", 200)

// Group fields under a namespace
dbLogger := logger.WithGroup("db")
dbLogger.Info("query executed", "table", "users", "rows", 10)
// output: ... db.table=users db.rows=10
```

## CLI tool

The `colorlog` CLI reads JSON log lines from stdin and renders them with color. Use it to pretty-print the output of any program that logs structured JSON.

### Install

```sh
go install github.com/timwmillard/golite/colorlog/cmd/colorlog@latest
```

### Usage

Pipe any JSON log stream into `colorlog`:

```sh
./myapp | colorlog
```

```sh
kubectl logs my-pod | colorlog
```

```sh
tail -f /var/log/app.log | colorlog
```

Non-JSON lines are passed through unchanged, so mixed output is handled gracefully.

### Supported JSON fields

| Field | Description |
|---|---|
| `time` | RFC3339/RFC3339Nano timestamp |
| `level` | Log level (`DEBUG`, `INFO`, `WARN`, `ERROR`) |
| `msg` or `message` | Log message |
| anything else | Rendered as `key=value` attributes |

## Install

```sh
go get github.com/timwmillard/golite/colorlog
```

Requires Go 1.21 or later (uses `log/slog` from the standard library).
