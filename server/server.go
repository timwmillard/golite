// Package server runs an HTTP server with graceful shutdown and, optionally,
// a River job queue backed by the same SQLite database.
//
//	cfg := server.Config{Port: 7880, DB: db, Workers: workers, RiverUI: true}
//	if err := cfg.Parse(); err != nil { ... } // .env, env vars, then flags
//	srv, err := server.New(ctx, cfg)
//	srv.Mux.Handle("/api/", api(srv.River))
//	err = srv.Run(ctx)
package server

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riversqlite"
	"github.com/riverqueue/river/rivermigrate"
	"riverqueue.com/riverui"
)

// Config configures a Server. Only fields that are set are used; everything
// else falls back to a default.
type Config struct {
	// Addr is the listen address. Defaults to ":<Port>".
	Addr string

	// Port is used when Addr is empty. Defaults to 8080. Set by -port / PORT.
	Port int

	// LogFormat is "color", "text" or "json", and LogLevel the minimum level
	// logged. Parse builds Logger from them. Set by -log / LOG and
	// -log-level / LOG_LEVEL.
	LogFormat string
	LogLevel  slog.Level

	// Logger is used by the server and River. Defaults to slog.Default().
	Logger *slog.Logger

	// ShutdownTimeout bounds how long Run waits for in-flight requests and
	// running jobs when stopping. Defaults to 10s.
	ShutdownTimeout time.Duration

	// DB is the database River stores jobs in. Required if Workers is set.
	// When set, the server also serves GET /health, which pings it.
	DB *sql.DB

	// DisableHealth stops New from registering GET /health, so the app can
	// register its own.
	DisableHealth bool

	// Workers enables River. Leave nil for a plain HTTP server.
	Workers *river.Workers

	// Queues configures River's queues. Defaults to the default queue with
	// 5 workers.
	Queues map[string]river.QueueConfig

	// RiverUI mounts the River dashboard at /riverui/. If RiverUIUsername
	// and RiverUIPassword are both set, it's behind basic auth. Parse fills
	// the credentials from RIVERUI_USERNAME and RIVERUI_PASSWORD (env only,
	// so the password never shows up in ps).
	RiverUI         bool
	RiverUIUsername string
	RiverUIPassword string
}

// Server is an HTTP server plus an optional River client. Register routes on
// Mux between New and Run.
type Server struct {
	Mux *http.ServeMux

	// River is nil unless Config.Workers was set. It's usable for inserting
	// jobs as soon as New returns; Run starts it working them.
	River *river.Client[*sql.Tx]

	cfg     Config
	riverUI *riverui.Handler
}

// New builds a Server. If cfg.Workers is set it also applies River's schema
// migrations to cfg.DB and creates the River client.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = fmt.Sprintf(":%d", cmp.Or(cfg.Port, 8080))
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}

	s := &Server{Mux: http.NewServeMux(), cfg: cfg}

	if cfg.DB != nil && !cfg.DisableHealth {
		s.Mux.HandleFunc("GET /health", s.health)
	}

	if cfg.Workers == nil {
		if cfg.RiverUI {
			return nil, errors.New("server: RiverUI requires Workers")
		}
		return s, nil
	}
	if cfg.DB == nil {
		return nil, errors.New("server: Workers requires DB")
	}

	if err := s.setupRiver(ctx); err != nil {
		return nil, err
	}

	if cfg.RiverUI {
		if err := s.setupRiverUI(); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Server) setupRiver(ctx context.Context) error {
	driver := riversqlite.New(s.cfg.DB)

	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("run river migrations: %w", err)
	}

	queues := s.cfg.Queues
	if queues == nil {
		queues = map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 5},
		}
	}

	s.River, err = river.NewClient(driver, &river.Config{
		Queues:  queues,
		Workers: s.cfg.Workers,
		Logger:  s.cfg.Logger,
	})
	if err != nil {
		return fmt.Errorf("create river client: %w", err)
	}

	return nil
}

func (s *Server) setupRiverUI() error {
	h, err := riverui.NewHandler(&riverui.HandlerOpts{
		Endpoints: riverui.NewEndpoints(s.River, nil),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)), // riverui is noisy; discard its logs
		Prefix:    "/riverui",
	})
	if err != nil {
		return fmt.Errorf("create riverui handler: %w", err)
	}
	s.riverUI = h

	if s.cfg.RiverUIUsername != "" && s.cfg.RiverUIPassword != "" {
		s.Mux.Handle("/riverui/", BasicAuth(h, s.cfg.RiverUIUsername, s.cfg.RiverUIPassword, "RiverUI"))
	} else {
		s.cfg.Logger.Warn("RiverUI mounted without authentication")
		s.Mux.Handle("/riverui/", h)
	}

	return nil
}

// health reports whether the database is reachable: 200 {"status":"ok"} or
// 503 {"status":"unavailable"}.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status, body := http.StatusOK, `{"status":"ok"}`
	if err := s.cfg.DB.PingContext(r.Context()); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "Health check failed", "err", err)
		status, body = http.StatusServiceUnavailable, `{"status":"unavailable"}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body+"\n")
}

// Run starts River (if configured) and the HTTP server, and blocks until ctx
// is cancelled or the process gets SIGINT/SIGTERM. It then stops accepting
// requests, waits for in-flight ones, and stops River, all within
// Config.ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := s.cfg.Logger

	// River does a hard stop (cancelling running jobs) if its start context
	// is cancelled, so detach it and stop it gracefully below instead.
	bgCtx := context.WithoutCancel(ctx)

	if s.River != nil {
		if err := s.River.Start(bgCtx); err != nil {
			return fmt.Errorf("start river: %w", err)
		}
		log.Info("River started")
	}
	if s.riverUI != nil {
		if err := s.riverUI.Start(bgCtx); err != nil {
			return fmt.Errorf("start riverui: %w", err)
		}
		log.Info("RiverUI started", "path", "/riverui/")
	}

	srv := &http.Server{
		Addr:     s.cfg.Addr,
		Handler:  s.Mux,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		// Requests outlive a cancelled Run ctx so Shutdown can drain them.
		BaseContext: func(net.Listener) context.Context { return bgCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("Starting server", "addr", s.cfg.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	var runErr error
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		log.Info("Shutting down server")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shutdown server: %w", err))
	}
	if s.River != nil {
		if err := s.River.Stop(shutdownCtx); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("stop river: %w", err))
		}
	}

	if runErr == nil {
		log.Info("Server stopped")
	}
	return runErr
}
