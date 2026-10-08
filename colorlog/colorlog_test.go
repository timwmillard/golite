package colorlog

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stripANSI removes ANSI escape sequences from s so we can assert on plain text.
var ansiRE = regexp.MustCompile(`\033\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

func fixedRecord(level slog.Level, msg string) slog.Record {
	t := time.Date(2024, 1, 2, 15, 4, 5, 123000000, time.UTC)
	return slog.NewRecord(t, level, msg, 0)
}

func handle(h *Handler, r slog.Record) string {
	var buf bytes.Buffer
	hh := &Handler{w: &buf, opts: h.opts, mu: h.mu, attrs: h.attrs, group: h.group}
	_ = hh.Handle(context.Background(), r)
	return stripANSI(buf.String())
}

func TestOutputFormat(t *testing.T) {
	h := NewHandler(nil, nil)
	got := handle(h, fixedRecord(slog.LevelInfo, "hello world"))
	want := "Jan 02 15:04:05.123 |INFO| hello world\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLevelStrings(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "|DEBU|"},
		{slog.LevelInfo, "|INFO|"},
		{slog.LevelWarn, "|WARN|"},
		{slog.LevelError, "|ERRO|"},
	}
	h := NewHandler(nil, nil)
	for _, tt := range tests {
		got := handle(h, fixedRecord(tt.level, "msg"))
		if !strings.Contains(got, tt.want) {
			t.Errorf("level %v: got %q, want it to contain %q", tt.level, got, tt.want)
		}
	}
}

func TestAttributes(t *testing.T) {
	h := NewHandler(nil, nil)
	r := fixedRecord(slog.LevelInfo, "request")
	r.AddAttrs(slog.String("method", "GET"), slog.Int("status", 200))
	got := handle(h, r)
	if !strings.Contains(got, "method=GET") {
		t.Errorf("got %q: missing method=GET", got)
	}
	if !strings.Contains(got, "status=200") {
		t.Errorf("got %q: missing status=200", got)
	}
}

func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(&buf, nil)
	h2 := h.WithAttrs([]slog.Attr{slog.String("app", "myservice")})

	var buf2 bytes.Buffer
	hh := h2.(*Handler)
	hh2 := &Handler{w: &buf2, opts: hh.opts, mu: hh.mu, attrs: hh.attrs, group: hh.group}
	_ = hh2.Handle(context.Background(), fixedRecord(slog.LevelInfo, "started"))
	got := stripANSI(buf2.String())

	if !strings.Contains(got, "app=myservice") {
		t.Errorf("got %q: missing persistent attr app=myservice", got)
	}
}

func TestWithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(&buf, nil)
	h2 := h.WithGroup("db")

	r := fixedRecord(slog.LevelInfo, "query")
	r.AddAttrs(slog.String("table", "users"))

	var buf2 bytes.Buffer
	hh := h2.(*Handler)
	hh2 := &Handler{w: &buf2, opts: hh.opts, mu: hh.mu, attrs: hh.attrs, group: hh.group}
	_ = hh2.Handle(context.Background(), r)
	got := stripANSI(buf2.String())

	if !strings.Contains(got, "db.table=users") {
		t.Errorf("got %q: missing db.table=users", got)
	}
}

func TestNestedGroup(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(&buf, nil)
	h2 := h.WithGroup("outer").(*Handler).WithGroup("inner")

	hh := h2.(*Handler)
	if hh.group != "outer.inner" {
		t.Errorf("group = %q, want outer.inner", hh.group)
	}
}

func TestGroupAttr(t *testing.T) {
	h := NewHandler(nil, nil)
	r := fixedRecord(slog.LevelInfo, "msg")
	r.AddAttrs(slog.Group("req", slog.String("method", "POST"), slog.Int("status", 201)))
	got := handle(h, r)
	if !strings.Contains(got, "req.method=POST") {
		t.Errorf("got %q: missing req.method=POST", got)
	}
	if !strings.Contains(got, "req.status=201") {
		t.Errorf("got %q: missing req.status=201", got)
	}
}

func TestEnabled(t *testing.T) {
	// Default min level is Info
	h := NewHandler(nil, nil)
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Debug to be disabled by default")
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("expected Info to be enabled by default")
	}

	// Explicit Debug level
	hd := NewHandler(nil, &slog.HandlerOptions{Level: slog.LevelDebug})
	if !hd.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Debug to be enabled when level=Debug")
	}

	// Explicit Error level
	he := NewHandler(nil, &slog.HandlerOptions{Level: slog.LevelError})
	if he.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("expected Warn to be disabled when level=Error")
	}
}

func TestZeroAttrSkipped(t *testing.T) {
	h := NewHandler(nil, nil)
	r := fixedRecord(slog.LevelInfo, "msg")
	r.AddAttrs(slog.Attr{}) // zero attr should be ignored
	got := handle(h, r)
	// Output should be just the plain line with no extra key=value
	want := "Jan 02 15:04:05.123 |INFO| msg\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestANSICodesPresent(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(&buf, nil)
	_ = h.Handle(context.Background(), fixedRecord(slog.LevelInfo, "msg"))
	raw := buf.String()
	if !strings.Contains(raw, "\033[") {
		t.Error("expected ANSI escape codes in output, got none")
	}
}

func TestOutputEndsWithNewline(t *testing.T) {
	h := NewHandler(nil, nil)
	got := handle(h, fixedRecord(slog.LevelInfo, "msg"))
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output does not end with newline: %q", got)
	}
}
