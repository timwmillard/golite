package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/timwmillard/golite/colorlog"
)

func main() {
	handler := colorlog.NewHandler(os.Stdout, nil)
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		r, ok := parseRecord(line)
		if !ok {
			fmt.Println(line)
			continue
		}

		_ = handler.Handle(context.Background(), r)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "error reading stdin:", err)
		os.Exit(1)
	}
}

func parseRecord(line string) (slog.Record, bool) {
	if r, ok := parseJSONRecord(line); ok {
		return r, true
	}
	return parseTextRecord(line)
}

func parseJSONRecord(line string) (slog.Record, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return slog.Record{}, false
	}

	t := time.Now()
	if ts, ok := raw["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			t = parsed
		}
		delete(raw, "time")
	}

	level := slog.LevelInfo
	if lvl, ok := raw["level"].(string); ok {
		_ = level.UnmarshalText([]byte(lvl))
		delete(raw, "level")
	}

	msg := ""
	if m, ok := raw["msg"].(string); ok {
		msg = m
		delete(raw, "msg")
	} else if m, ok := raw["message"].(string); ok {
		msg = m
		delete(raw, "message")
	}

	r := slog.NewRecord(t, level, msg, 0)
	for k, v := range raw {
		r.AddAttrs(slog.Any(k, v))
	}
	return r, true
}

func parseTextRecord(line string) (slog.Record, bool) {
	fields, ok := parseKeyValues(line)
	if !ok {
		return slog.Record{}, false
	}

	t := time.Now()
	if ts, ok := fields["time"]; ok {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			t = parsed
		}
		delete(fields, "time")
	}

	level := slog.LevelInfo
	if lvl, ok := fields["level"]; ok {
		_ = level.UnmarshalText([]byte(lvl))
		delete(fields, "level")
	}

	msg := ""
	if m, ok := fields["msg"]; ok {
		msg = m
		delete(fields, "msg")
	} else if m, ok := fields["message"]; ok {
		msg = m
		delete(fields, "message")
	}

	r := slog.NewRecord(t, level, msg, 0)
	for k, v := range fields {
		r.AddAttrs(slog.String(k, v))
	}
	return r, true
}

func parseKeyValues(line string) (map[string]string, bool) {
	fields := make(map[string]string)
	for len(line) > 0 {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			break
		}

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, false
		}
		key := line[:eq]
		if strings.ContainsAny(key, " \t") {
			return nil, false
		}
		line = line[eq+1:]

		var val string
		if strings.HasPrefix(line, `"`) {
			raw, rest, ok := quotedField(line)
			if !ok {
				return nil, false
			}
			unquoted, err := strconv.Unquote(raw)
			if err != nil {
				return nil, false
			}
			val = unquoted
			line = rest
		} else {
			end := strings.IndexAny(line, " \t")
			if end == -1 {
				val = line
				line = ""
			} else {
				val = line[:end]
				line = line[end:]
			}
		}

		fields[key] = val
	}

	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

func quotedField(s string) (raw, rest string, ok bool) {
	escaped := false
	for i := 1; i < len(s); i++ {
		switch {
		case escaped:
			escaped = false
		case s[i] == '\\':
			escaped = true
		case s[i] == '"':
			return s[:i+1], s[i+1:], true
		}
	}
	return "", "", false
}
