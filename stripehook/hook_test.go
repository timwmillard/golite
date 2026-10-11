package stripehook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/riverqueue/river"

	"github.com/timwmillard/golite/server"
)

const secret = "whsec_test"

type env struct {
	db    *sql.DB
	hook  *Hook
	river *river.Client[*sql.Tx]
	srv   *httptest.Server
}

// newEnv makes a database with the schema plus a log table for handlers to
// write to, and serves hook, after register adds its handlers.
func newEnv(t *testing.T, register func(*Hook)) *env {
	t.Helper()
	ctx := t.Context()
	migrations := fstest.MapFS{
		"0001_stripe.sql": {Data: []byte(Schema)},
		"0002_log.sql":    {Data: []byte(`create table log (event_id text not null, note text not null)`)},
	}
	db, err := server.OpenDB(ctx, filepath.Join(t.TempDir(), "app.db"), migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hook := New(Config{DB: db, Secret: secret, Logger: logger})
	if register != nil {
		register(hook)
	}
	workers := river.NewWorkers()
	hook.AddWorkers(workers)
	// server.New migrates River's tables and makes the client; it's only
	// used to insert, never started.
	s, err := server.New(ctx, server.Config{DB: db, Workers: workers, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hook.Handler(s.River))
	t.Cleanup(srv.Close)
	return &env{db: db, hook: hook, river: s.River, srv: srv}
}

func event(id, typ string, created int64, object string) []byte {
	return fmt.Appendf(nil, `{"id":%q,"object":"event","type":%q,"account":"acct_conn","livemode":false,"api_version":"2099-01-01","created":%d,"data":{"object":%s}}`,
		id, typ, created, object)
}

func (e *env) post(t *testing.T, payload []byte, sig string) int {
	t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Stripe-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) deliver(t *testing.T, payload []byte) {
	t.Helper()
	if code := e.post(t, payload, Sign(payload, secret, time.Now())); code != http.StatusOK {
		t.Fatalf("delivery: status %d, want 200", code)
	}
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) jobs(t *testing.T) int {
	return e.count(t, `select count(*) from river_job where kind = 'stripehook_event'`)
}

func logNote(note string) HandlerFunc {
	return func(ctx context.Context, tx *sql.Tx, ev Event) error {
		_, err := tx.ExecContext(ctx, `insert into log (event_id, note) values (?, ?)`, ev.ID, note)
		return err
	}
}

func TestHandler_RecordsEventAndObject(t *testing.T) {
	e := newEnv(t, func(h *Hook) { h.OnEvent("account.updated", logNote("x")) })
	e.deliver(t, event("evt_1", "account.updated", 100, `{"id":"acct_1","object":"account","charges_enabled":true}`))

	var typ, account, payload string
	var processed sql.NullInt64
	err := e.db.QueryRow(`select type, account, payload, processed_at from stripe_event where id = 'evt_1'`).Scan(&typ, &account, &payload, &processed)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "account.updated" || account != "acct_conn" || !strings.Contains(payload, `"charges_enabled":true`) || processed.Valid {
		t.Errorf("stripe_event = %q %q %q processed=%v", typ, account, payload, processed)
	}

	var otype, data, eventID string
	err = e.db.QueryRow(`select type, data, event_id from stripe_object where id = 'acct_1'`).Scan(&otype, &data, &eventID)
	if err != nil {
		t.Fatal(err)
	}
	if otype != "account" || data != `{"id":"acct_1","object":"account","charges_enabled":true}` || eventID != "evt_1" {
		t.Errorf("stripe_object = %q %q %q", otype, data, eventID)
	}

	if n := e.jobs(t); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

func TestHandler_DuplicateDeliveryIsNoop(t *testing.T) {
	e := newEnv(t, func(h *Hook) { h.OnEvent("account.updated", logNote("x")) })
	payload := event("evt_1", "account.updated", 100, `{"id":"acct_1","object":"account"}`)
	e.deliver(t, payload)
	e.deliver(t, payload)

	if n := e.count(t, `select count(*) from stripe_event`); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
	if n := e.jobs(t); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

func TestHandler_NoHandlerNoJob(t *testing.T) {
	e := newEnv(t, func(h *Hook) { h.OnEvent("account.updated", logNote("x")) })
	e.deliver(t, event("evt_1", "customer.created", 100, `{"id":"cus_1","object":"customer"}`))

	if n := e.count(t, `select count(*) from stripe_event`); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
	if n := e.count(t, `select count(*) from stripe_object where id = 'cus_1'`); n != 1 {
		t.Errorf("objects = %d, want 1", n)
	}
	if n := e.jobs(t); n != 0 {
		t.Errorf("jobs = %d, want 0", n)
	}
}

func TestHandler_OlderEventDoesntOverwriteObject(t *testing.T) {
	e := newEnv(t, nil)
	e.deliver(t, event("evt_new", "account.updated", 200, `{"id":"acct_1","object":"account","v":"new"}`))
	e.deliver(t, event("evt_old", "account.updated", 100, `{"id":"acct_1","object":"account","v":"old"}`))

	var eventID string
	if err := e.db.QueryRow(`select event_id from stripe_object where id = 'acct_1'`).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if eventID != "evt_new" {
		t.Errorf("object from %s, want evt_new", eventID)
	}

	e.deliver(t, event("evt_newer", "account.updated", 300, `{"id":"acct_1","object":"account","v":"newer"}`))
	if err := e.db.QueryRow(`select event_id from stripe_object where id = 'acct_1'`).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if eventID != "evt_newer" {
		t.Errorf("object from %s, want evt_newer", eventID)
	}
}

func TestHandler_Rejects(t *testing.T) {
	e := newEnv(t, nil)
	payload := event("evt_1", "account.updated", 100, `{"id":"acct_1","object":"account"}`)

	tests := []struct {
		name    string
		payload []byte
		sig     string
		want    int
	}{
		{"no signature", payload, "", http.StatusBadRequest},
		{"wrong secret", payload, Sign(payload, "whsec_other", time.Now()), http.StatusBadRequest},
		{"too old", payload, Sign(payload, secret, time.Now().Add(-time.Hour)), http.StatusBadRequest},
		{"not an event", []byte(`{"hello":1}`), Sign([]byte(`{"hello":1}`), secret, time.Now()), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := e.post(t, tt.payload, tt.sig); code != tt.want {
				t.Errorf("status %d, want %d", code, tt.want)
			}
		})
	}
	if n := e.count(t, `select count(*) from stripe_event`); n != 0 {
		t.Errorf("events = %d, want 0", n)
	}

	e.hook.SetSecret("")
	if code := e.post(t, payload, Sign(payload, secret, time.Now())); code != http.StatusServiceUnavailable {
		t.Errorf("no secret: status %d, want 503", code)
	}
}

func TestProcess(t *testing.T) {
	type account struct {
		ID             string `json:"id"`
		ChargesEnabled bool   `json:"charges_enabled"`
	}
	fail := false
	e := newEnv(t, func(h *Hook) {
		h.OnEvent("account.updated", logNote("first"))
		On(h, "account.updated", func(ctx context.Context, tx *sql.Tx, ev Event, acct *account) error {
			if fail {
				return errors.New("boom")
			}
			_, err := tx.ExecContext(ctx, `insert into log (event_id, note) values (?, ?)`, ev.ID, fmt.Sprintf("%s %v", acct.ID, acct.ChargesEnabled))
			return err
		})
	})
	e.deliver(t, event("evt_1", "account.updated", 100, `{"id":"acct_1","object":"account","charges_enabled":true}`))
	ctx := t.Context()

	// A failing handler rolls back the others' writes and leaves the event
	// unprocessed.
	fail = true
	if err := e.hook.Process(ctx, "evt_1"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Process = %v, want boom", err)
	}
	if n := e.count(t, `select count(*) from log`); n != 0 {
		t.Errorf("log rows after failure = %d, want 0", n)
	}
	if n := e.count(t, `select count(*) from stripe_event where processed_at is not null`); n != 0 {
		t.Errorf("processed after failure")
	}

	fail = false
	if err := e.hook.Process(ctx, "evt_1"); err != nil {
		t.Fatal(err)
	}
	rows, err := e.db.Query(`select note from log order by rowid`)
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if got := strings.Join(notes, "|"); got != "first|acct_1 true" {
		t.Errorf("log = %q, want %q", got, "first|acct_1 true")
	}

	// Processed events aren't processed again.
	if err := e.hook.Process(ctx, "evt_1"); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `select count(*) from log`); n != 2 {
		t.Errorf("log rows after reprocess = %d, want 2", n)
	}

	if err := e.hook.Process(ctx, "evt_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Process(missing) = %v, want ErrNotFound", err)
	}
}

func TestOn_BadObjectCancels(t *testing.T) {
	e := newEnv(t, func(h *Hook) {
		On(h, "account.updated", func(context.Context, *sql.Tx, Event, struct{ ID int }) error { return nil })
	})
	e.deliver(t, event("evt_1", "account.updated", 100, `{"id":"acct_1","object":"account"}`))

	var cancel *river.JobCancelError
	if err := e.hook.Process(t.Context(), "evt_1"); !errors.As(err, &cancel) {
		t.Errorf("Process = %v, want JobCancel", err)
	}
}

func TestHandler_NoClientWithHandlers(t *testing.T) {
	e := newEnv(t, func(h *Hook) { h.OnEvent("account.updated", logNote("x")) })
	srv := httptest.NewServer(e.hook.Handler(nil))
	defer srv.Close()
	e.srv = srv

	payload := event("evt_1", "account.updated", 100, `{"id":"acct_1","object":"account"}`)
	if code := e.post(t, payload, Sign(payload, secret, time.Now())); code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", code)
	}
	if n := e.count(t, `select count(*) from stripe_event`); n != 0 {
		t.Errorf("events = %d, want 0 (rolled back)", n)
	}
}
