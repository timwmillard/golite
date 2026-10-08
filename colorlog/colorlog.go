// Package colorlog provides a color handler for slog.
package colorlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// 24-bit true color codes matching humanlog's dark theme.
const (
	reset    = "\033[0m"
	clrTime  = "\033[38;2;158;158;158m" // #9e9e9e
	clrKey   = "\033[38;2;72;223;97m"   // #48df61
	clrVal   = "\033[38;2;140;136;124m" // #8c887c
	clrDebug = "\033[38;2;211;54;130m"  // #d33682
	clrInfo  = "\033[38;2;42;161;152m"  // #2aa198
	clrWarn  = "\033[38;2;255;136;0m"   // #ff8800
	clrError = "\033[38;2;255;106;106m" // #ff6a6a
	clrMsg   = "\033[38;2;255;255;255m" // #ffffff
)

// Handler is a slog.Handler that outputs humanlog-style colored text.
type Handler struct {
	w     io.Writer
	opts  slog.HandlerOptions
	mu    *sync.Mutex
	attrs []slog.Attr
	group string
}

// NewHandler returns a new ColorHandler that writes to w.
func NewHandler(w io.Writer, opts *slog.HandlerOptions) *Handler {
	h := &Handler{
		w:  w,
		mu: &sync.Mutex{},
	}
	if opts != nil {
		h.opts = *opts
	}
	return h
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return level >= minLevel
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)

	// Timestamp
	buf = append(buf, clrTime...)
	buf = append(buf, r.Time.Format("Jan 02 15:04:05.000")...)
	buf = append(buf, reset...)
	buf = append(buf, ' ')

	// Level tag with color
	var levelColor, levelStr string
	switch {
	case r.Level >= slog.LevelError:
		levelColor = clrError
		levelStr = "ERRO"
	case r.Level >= slog.LevelWarn:
		levelColor = clrWarn
		levelStr = "WARN"
	case r.Level >= slog.LevelInfo:
		levelColor = clrInfo
		levelStr = "INFO"
	default:
		levelColor = clrDebug
		levelStr = "DEBU"
	}
	buf = append(buf, '|')
	buf = append(buf, levelColor...)
	buf = append(buf, levelStr...)
	buf = append(buf, reset...)
	buf = append(buf, '|')
	buf = append(buf, ' ')

	// Message
	buf = append(buf, clrMsg...)
	buf = append(buf, r.Message...)
	buf = append(buf, reset...)

	// Handler-level attrs (from WithAttrs)
	for _, a := range h.attrs {
		buf = h.appendAttr(buf, a)
	}

	// Record attrs
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, a)
		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *Handler) appendAttr(buf []byte, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}

	if a.Value.Kind() == slog.KindGroup {
		prefix := a.Key
		if h.group != "" {
			prefix = h.group + "." + prefix
		}
		for _, ga := range a.Value.Group() {
			buf = append(buf, ' ')
			buf = append(buf, clrKey...)
			if prefix != "" {
				buf = append(buf, prefix...)
				buf = append(buf, '.')
			}
			buf = append(buf, ga.Key...)
			buf = append(buf, reset...)
			buf = append(buf, '=')
			buf = append(buf, clrVal...)
			buf = append(buf, fmt.Sprintf("%v", ga.Value.Any())...)
			buf = append(buf, reset...)
		}
		return buf
	}

	buf = append(buf, ' ')
	buf = append(buf, clrKey...)
	if h.group != "" {
		buf = append(buf, h.group...)
		buf = append(buf, '.')
	}
	buf = append(buf, a.Key...)
	buf = append(buf, reset...)
	buf = append(buf, '=')
	buf = append(buf, clrVal...)
	buf = append(buf, fmt.Sprintf("%v", a.Value.Any())...)
	buf = append(buf, reset...)
	return buf
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &Handler{
		w:     h.w,
		opts:  h.opts,
		mu:    h.mu,
		attrs: newAttrs,
		group: h.group,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	newGroup := name
	if h.group != "" {
		newGroup = h.group + "." + name
	}
	return &Handler{
		w:     h.w,
		opts:  h.opts,
		mu:    h.mu,
		attrs: h.attrs,
		group: newGroup,
	}
}
