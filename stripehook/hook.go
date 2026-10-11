// Package stripehook receives Stripe webhooks into SQLite. Each delivery's
// signature is checked, the event is recorded in stripe_event (once, however
// often Stripe redelivers it), and the object it carries is kept in
// stripe_object, so the app has the latest copy of each Stripe object
// without asking Stripe. What to do about an event is up to the app: it
// registers handlers by event type, and they run in a River job inserted in
// the same transaction as the event, so Stripe is only told the delivery
// arrived once the work is sure to happen, and River retries it until it
// succeeds.
//
//	hook := stripehook.New(stripehook.Config{DB: db, Secret: os.Getenv("STRIPE_WEBHOOK_SECRET")})
//	stripehook.On(hook, "account.updated", func(ctx context.Context, tx *sql.Tx, ev stripehook.Event, acct *stripe.Account) error {
//		_, err := tx.ExecContext(ctx, `update firm set charges_enabled = ? where stripe_account_id = ?`, acct.ChargesEnabled, acct.ID)
//		return err
//	})
//	hook.AddWorkers(workers) // before server.New
//	srv, err := server.New(ctx, cfg)
//	srv.Mux.Handle("POST /webhooks/stripe", hook.Handler(srv.River))
//
// The package doesn't import stripe-go, so it never ties the app to a
// stripe-go version: On decodes each object into whatever type the app
// gives it, usually its own stripe-go's. Apps that don't need Go can
// instead put SQLite triggers on stripe_object or stripe_event, which fire
// in the delivery's transaction.
//
// Events of a type with no handler are recorded, but no job is inserted.
// Only snapshot events are supported, not Stripe's v2 thin events.
package stripehook

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/riverqueue/river"
)

// Schema creates the stripe_event and stripe_object tables. Copy it into
// one of the app's migrations rather than executing it, so the app's
// migrations (and sqlc) know the tables.
//
//go:embed schema.sql
var Schema string

// MaxBodyBytes caps how much of a delivery is read.
const MaxBodyBytes = 1 << 20

// ErrNotFound is returned by Process for an event that isn't in
// stripe_event.
var ErrNotFound = errors.New("stripehook: event not found")

// Event is a Stripe event as delivered. Raw is the whole payload, e.g. to
// decode into stripe-go's stripe.Event.
type Event struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Account    string    `json:"account,omitempty"`
	Livemode   bool      `json:"livemode"`
	APIVersion string    `json:"api_version,omitempty"`
	Created    int64     `json:"created"`
	Data       EventData `json:"data"`

	Raw json.RawMessage `json:"-"`
}

// EventData is the event's data: the object it's about, and for *.updated
// events the previous values of the fields that changed.
type EventData struct {
	Object             json.RawMessage `json:"object"`
	PreviousAttributes json.RawMessage `json:"previous_attributes,omitempty"`
}

// HandlerFunc handles an event. tx is a transaction on Config.DB, committed
// with the event marked processed if every handler for its type returns
// nil. Use only tx for the database: with server.OpenDB's one connection,
// a query on the *sql.DB waits forever for the connection tx holds.
//
// A handler may run more than once for an event (a retry after another
// handler failed, or after a write elsewhere that tx doesn't cover), so make
// writes outside tx idempotent. Return river.JobCancel to give up on an
// event, or river.JobSnooze to try it later.
type HandlerFunc func(ctx context.Context, tx *sql.Tx, ev Event) error

// Config configures a Hook.
type Config struct {
	// DB holds stripe_event and stripe_object, and River's tables.
	DB *sql.DB

	// Secret is the endpoint's signing secret (whsec_...). If it's empty,
	// deliveries are refused until SetSecret sets one.
	Secret string

	// Tolerance is how old a delivery's signature may be. Defaults to
	// DefaultTolerance.
	Tolerance time.Duration

	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Hook receives Stripe webhooks. Register handlers with On and OnEvent
// before it receives any.
type Hook struct {
	db        *sql.DB
	log       *slog.Logger
	tolerance time.Duration
	secret    atomic.Pointer[string]
	handlers  map[string][]HandlerFunc
}

// New returns a Hook. Register its worker with AddWorkers, and serve
// Handler at the endpoint registered with Stripe.
func New(cfg Config) *Hook {
	h := &Hook{
		db:        cfg.DB,
		log:       cfg.Logger,
		tolerance: cfg.Tolerance,
		handlers:  map[string][]HandlerFunc{},
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.tolerance == 0 {
		h.tolerance = DefaultTolerance
	}
	h.secret.Store(&cfg.Secret)
	return h
}

// SetSecret changes the signing secret on a running Hook. Listen uses it
// to pass on the secret the Stripe CLI makes up each time it starts.
func (h *Hook) SetSecret(secret string) {
	h.secret.Store(&secret)
	h.log.Info("stripehook: signing secret updated")
}

// OnEvent adds a handler for events of eventType, e.g. "account.updated".
// An event's handlers run in the order they were added, in one transaction.
func (h *Hook) OnEvent(eventType string, fn HandlerFunc) {
	h.handlers[eventType] = append(h.handlers[eventType], fn)
}

// On adds a handler for events of eventType that's given the event's object
// decoded into a T, usually a stripe-go type such as *stripe.Account. An
// object that won't decode cancels the job, as retrying can't help.
func On[T any](h *Hook, eventType string, fn func(ctx context.Context, tx *sql.Tx, ev Event, obj T) error) {
	h.OnEvent(eventType, func(ctx context.Context, tx *sql.Tx, ev Event) error {
		var obj T
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return river.JobCancel(fmt.Errorf("decode %s object: %w", ev.Type, err))
		}
		return fn(ctx, tx, ev, obj)
	})
}

// Handler returns the webhook endpoint. client inserts the jobs that run
// the handlers; it may be nil only if there are none.
//
// It responds 200 once the event is recorded (or was already), 400 to a
// delivery that isn't signed with the secret, 503 while there's no secret,
// and 500 if recording fails, so Stripe redelivers.
func (h *Hook) Handler(client *river.Client[*sql.Tx]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := *h.secret.Load()
		if secret == "" {
			h.log.Error("stripehook: delivery refused, no signing secret")
			http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			http.Error(w, "can't read body", http.StatusBadRequest)
			return
		}
		if err := Verify(body, r.Header.Get("Stripe-Signature"), secret, h.tolerance); err != nil {
			h.log.Warn("stripehook: bad signature", "err", err)
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}

		ev, err := parseEvent(body)
		if err != nil {
			h.log.Warn("stripehook: bad event", "err", err)
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}

		if err := h.record(r.Context(), client, ev); err != nil {
			h.log.Error("stripehook: record event", "event", ev.ID, "type", ev.Type, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func parseEvent(payload []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return Event{}, err
	}
	if ev.ID == "" || ev.Type == "" {
		return Event{}, errors.New("no id or type")
	}
	ev.Raw = payload
	return ev, nil
}

// record saves ev and, if it has handlers, inserts the job to run them, in
// one transaction. A redelivery of a recorded event changes nothing.
func (h *Hook) record(ctx context.Context, client *river.Client[*sql.Tx], ev Event) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `insert into stripe_event
		(id, type, account, livemode, api_version, created, received_at, payload)
		values (?, ?, ?, ?, ?, ?, ?, ?)
		on conflict (id) do nothing`,
		ev.ID, ev.Type, nullString(ev.Account), ev.Livemode, nullString(ev.APIVersion), ev.Created, now, string(ev.Raw))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		h.log.Info("stripehook: duplicate delivery", "event", ev.ID, "type", ev.Type)
		return nil
	}

	if err := upsertObject(ctx, tx, ev, now); err != nil {
		return err
	}

	if len(h.handlers[ev.Type]) > 0 {
		if client == nil {
			return errors.New("stripehook: event has handlers but Handler has no River client")
		}
		if _, err := client.InsertTx(ctx, tx, EventArgs{EventID: ev.ID}, nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// upsertObject saves ev's object in stripe_object, unless an event created
// later has already saved a newer copy: Stripe doesn't deliver events in
// order.
func upsertObject(ctx context.Context, tx *sql.Tx, ev Event, now int64) error {
	var obj struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if len(ev.Data.Object) == 0 || json.Unmarshal(ev.Data.Object, &obj) != nil || obj.ID == "" || obj.Object == "" {
		return nil // nothing identifiable to keep
	}
	_, err := tx.ExecContext(ctx, `insert into stripe_object
		(id, type, account, data, event_id, event_created, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)
		on conflict (id) do update set
			type = excluded.type,
			account = excluded.account,
			data = excluded.data,
			event_id = excluded.event_id,
			event_created = excluded.event_created,
			updated_at = excluded.updated_at
		where excluded.event_created >= stripe_object.event_created`,
		obj.ID, obj.Object, nullString(ev.Account), string(ev.Data.Object), ev.ID, ev.Created, now)
	return err
}

// EventArgs is the job Handler inserts to run an event's handlers.
type EventArgs struct {
	EventID string `json:"event_id"`
}

func (EventArgs) Kind() string { return "stripehook_event" }

// AddWorkers registers the worker for EventArgs.
func (h *Hook) AddWorkers(workers *river.Workers) {
	river.AddWorker(workers, &eventWorker{h: h})
}

type eventWorker struct {
	river.WorkerDefaults[EventArgs]
	h *Hook
}

func (w *eventWorker) Work(ctx context.Context, job *river.Job[EventArgs]) error {
	err := w.h.Process(ctx, job.Args.EventID)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	}
	return err
}

// Process runs the handlers for recorded event id now, as its job does,
// and marks it processed. An event already processed is left alone. Use it
// where there's no River worker, such as a test or a script replaying
// events after adding a handler (events with no handler when they arrived
// have no job, and stay unprocessed).
func (h *Hook) Process(ctx context.Context, id string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var payload string
	var processed sql.NullInt64
	err = tx.QueryRowContext(ctx, `select payload, processed_at from stripe_event where id = ?`, id).Scan(&payload, &processed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if processed.Valid {
		return nil
	}

	ev, err := parseEvent([]byte(payload))
	if err != nil {
		return river.JobCancel(fmt.Errorf("stripehook: event %s: %w", id, err))
	}
	for _, fn := range h.handlers[ev.Type] {
		if err := fn(ctx, tx, ev); err != nil {
			return fmt.Errorf("stripehook: %s %s: %w", ev.Type, ev.ID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `update stripe_event set processed_at = ? where id = ?`, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
