package main

import (
	"log/slog"
	"testing"
	"time"
)

func TestParseJSONRecord(t *testing.T) {
	r, ok := parseRecord(`{"time":"2024-01-02T15:04:05.123Z","level":"WARN","msg":"hello world","status":503}`)
	if !ok {
		t.Fatal("parseRecord returned false")
	}

	if r.Time.Format(time.RFC3339Nano) != "2024-01-02T15:04:05.123Z" {
		t.Fatalf("time = %s", r.Time.Format(time.RFC3339Nano))
	}
	if r.Level != slog.LevelWarn {
		t.Fatalf("level = %s, want WARN", r.Level)
	}
	if r.Message != "hello world" {
		t.Fatalf("message = %q, want hello world", r.Message)
	}

	assertAttr(t, r, "status", float64(503))
}

func TestParseTextRecord(t *testing.T) {
	r, ok := parseRecord(`time=2024-01-02T15:04:05.123Z level=ERROR msg="hello world" method=GET path=/v1/users`)
	if !ok {
		t.Fatal("parseRecord returned false")
	}

	if r.Time.Format(time.RFC3339Nano) != "2024-01-02T15:04:05.123Z" {
		t.Fatalf("time = %s", r.Time.Format(time.RFC3339Nano))
	}
	if r.Level != slog.LevelError {
		t.Fatalf("level = %s, want ERROR", r.Level)
	}
	if r.Message != "hello world" {
		t.Fatalf("message = %q, want hello world", r.Message)
	}

	assertAttr(t, r, "method", "GET")
	assertAttr(t, r, "path", "/v1/users")
}

func TestParseTextRecordWithEscapedQuote(t *testing.T) {
	r, ok := parseRecord(`time=2024-01-02T15:04:05.123Z level=INFO msg="hello \"quoted\" world"`)
	if !ok {
		t.Fatal("parseRecord returned false")
	}
	if r.Message != `hello "quoted" world` {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestParseRecordRejectsPlainLine(t *testing.T) {
	if _, ok := parseRecord("ordinary line without key values"); ok {
		t.Fatal("parseRecord returned true")
	}
}

func assertAttr(t *testing.T, r slog.Record, key string, want any) {
	t.Helper()

	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != key {
			return true
		}
		found = true
		if got := a.Value.Any(); got != want {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
		return false
	})
	if !found {
		t.Fatalf("missing attr %s", key)
	}
}
