package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestError(t *testing.T) {
	rec := httptest.NewRecorder()
	RequestError(rec, httptest.NewRequest(http.MethodPost, "/", nil), errors.New("can't decode JSON body"))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"can't decode JSON body"}` {
		t.Errorf("body = %s", body)
	}
}

func TestResponseError(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	ResponseError(rec, httptest.NewRequest(http.MethodGet, "/v1/farms", nil), errors.New("no such table: farms"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"internal error"}` {
		t.Errorf("body = %s", body)
	}
	for _, want := range []string{`err="no such table: farms"`, "method=GET", "path=/v1/farms"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %s: %s", want, logs.String())
		}
	}
}
