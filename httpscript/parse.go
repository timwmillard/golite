package httpscript

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var methods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "OPTIONS": true, "CONNECT": true, "TRACE": true,
}

// entry is one request of a file and what its response must satisfy.
type entry struct {
	method, url string
	headers     []field
	query, form []field
	body        string
	resp        *response // nil when the entry has no HTTP line
}

type field struct{ key, value string }

type response struct {
	line     int
	status   int // 0 matches any
	headers  []lineField
	captures []capture
	asserts  []assertion
}

type lineField struct {
	line int
	field
}

type capture struct {
	line int
	name string
	q    query
}

type assertion struct {
	line  int
	src   string
	q     query
	not   bool
	pred  string
	value any // string, json.Number, bool, nil or *regexp.Regexp
}

type query struct {
	kind    string // status, body, header, cookie, jsonpath, regex
	arg     string
	path    *jsonPath
	re      *regexp.Regexp
	filters []string
}

// rawEntry is an entry's lines, from its request line, before variables
// are expanded.
type rawEntry struct {
	line  int // of the request line, 1-based
	lines []string
}

// splitEntries splits a file at its request lines.
func splitEntries(src string) []rawEntry {
	var entries []rawEntry
	for i, l := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if isRequestLine(l) {
			entries = append(entries, rawEntry{line: i + 1})
		}
		if n := len(entries); n > 0 {
			entries[n-1].lines = append(entries[n-1].lines, l)
		}
	}
	return entries
}

func isRequestLine(l string) bool {
	method, url, ok := strings.Cut(l, " ")
	return ok && methods[method] && strings.TrimSpace(url) != "" && !strings.ContainsAny(strings.TrimSpace(url), " \t")
}

func isComment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }

// parseEntry parses an entry whose variables have been expanded. first is
// the line number of lines[0].
func parseEntry(lines []string, first int) (*entry, error) {
	method, url, _ := strings.Cut(lines[0], " ")
	e := &entry{method: method, url: strings.TrimSpace(url)}

	const (
		head = iota
		body
		resp
	)
	phase, section := head, ""
	var bodyLines []string
	for i := 1; i < len(lines); i++ {
		l, n := lines[i], first+i
		errf := func(format string, args ...any) error {
			return fmt.Errorf("%d: %s", n, fmt.Sprintf(format, args...))
		}
		trimmed := strings.TrimSpace(l)

		if phase != resp && strings.HasPrefix(l, "HTTP ") {
			r, err := parseStatus(trimmed, n)
			if err != nil {
				return nil, err
			}
			e.resp, phase, section = r, resp, ""
			continue
		}

		switch phase {
		case head:
			switch {
			case trimmed == "":
				phase = body
			case isComment(l):
			case trimmed == "[Query]" || trimmed == "[Form]":
				section = trimmed
			case strings.HasPrefix(trimmed, "["):
				return nil, errf("unknown request section %s", trimmed)
			default:
				f, err := parseField(trimmed)
				if err != nil {
					return nil, errf("%v", err)
				}
				switch section {
				case "":
					e.headers = append(e.headers, f)
				case "[Query]":
					e.query = append(e.query, f)
				case "[Form]":
					e.form = append(e.form, f)
				}
			}

		case body:
			bodyLines = append(bodyLines, l)

		case resp:
			switch {
			case trimmed == "" || isComment(l):
			case trimmed == "[Captures]" || trimmed == "[Asserts]":
				section = trimmed
			case strings.HasPrefix(trimmed, "["):
				return nil, errf("unknown response section %s", trimmed)
			case section == "":
				f, err := parseField(trimmed)
				if err != nil {
					return nil, errf("%v", err)
				}
				e.resp.headers = append(e.resp.headers, lineField{n, f})
			case section == "[Captures]":
				c, err := parseCapture(trimmed)
				if err != nil {
					return nil, errf("%v", err)
				}
				c.line = n
				e.resp.captures = append(e.resp.captures, c)
			case section == "[Asserts]":
				a, err := parseAssertion(trimmed)
				if err != nil {
					return nil, errf("%v", err)
				}
				a.line = n
				e.resp.asserts = append(e.resp.asserts, a)
			}
		}
	}

	for len(bodyLines) > 0 && strings.TrimSpace(bodyLines[len(bodyLines)-1]) == "" {
		bodyLines = bodyLines[:len(bodyLines)-1]
	}
	e.body = strings.Join(bodyLines, "\n")
	if e.body != "" && len(e.form) > 0 {
		return nil, fmt.Errorf("%d: request has both a [Form] and a body", first)
	}
	return e, nil
}

func parseStatus(l string, n int) (*response, error) {
	s := strings.TrimSpace(strings.TrimPrefix(l, "HTTP "))
	r := &response{line: n}
	if s == "*" {
		return r, nil
	}
	code, err := strconv.Atoi(s)
	if err != nil || code < 100 || code > 999 {
		return nil, fmt.Errorf("%d: bad status %q", n, s)
	}
	r.status = code
	return r, nil
}

func parseField(l string) (field, error) {
	k, v, ok := strings.Cut(l, ":")
	k = strings.TrimSpace(k)
	if !ok || k == "" || strings.ContainsAny(k, " \t") {
		return field{}, fmt.Errorf("want \"name: value\", got %q", l)
	}
	return field{k, strings.TrimSpace(v)}, nil
}

func parseCapture(l string) (capture, error) {
	name, rest, ok := strings.Cut(l, ":")
	name = strings.TrimSpace(name)
	if !ok || !varName.MatchString(name) {
		return capture{}, fmt.Errorf("want \"name: query\", got %q", l)
	}
	q, rest, err := parseQuery(rest)
	if err != nil {
		return capture{}, err
	}
	if rest != "" {
		return capture{}, fmt.Errorf("unexpected %q after query", rest)
	}
	return capture{name: name, q: q}, nil
}

var predicates = map[string]bool{
	"==": true, "!=": true, ">": true, ">=": true, "<": true, "<=": true,
	"contains": true, "startsWith": true, "endsWith": true, "matches": true,
	"includes": true, "exists": true, "isEmpty": true,
}

func parseAssertion(l string) (assertion, error) {
	a := assertion{src: l}
	q, rest, err := parseQuery(l)
	if err != nil {
		return a, err
	}
	a.q = q
	word, rest := nextWord(rest)
	if word == "not" {
		a.not = true
		word, rest = nextWord(rest)
	}
	if !predicates[word] {
		return a, fmt.Errorf("want a predicate, got %q", word)
	}
	a.pred = word
	if word == "exists" || word == "isEmpty" {
		if rest != "" {
			return a, fmt.Errorf("unexpected %q after %s", rest, word)
		}
		return a, nil
	}
	if rest == "" {
		return a, fmt.Errorf("%s needs a value", word)
	}
	if a.value, err = parseValue(rest); err != nil {
		return a, err
	}
	if s, ok := a.value.(string); ok && word == "matches" {
		if a.value, err = regexp.Compile(s); err != nil {
			return a, err
		}
	}
	return a, nil
}

// parseQuery parses a query and its filters off the front of s.
func parseQuery(s string) (query, string, error) {
	var q query
	q.kind, s = nextWord(s)
	switch q.kind {
	case "status", "body":
	case "header", "cookie", "jsonpath", "regex":
		var err error
		if q.kind == "regex" && strings.HasPrefix(s, "/") {
			var re string
			if re, s, err = regexLiteral(s); err == nil {
				q.re, err = regexp.Compile(re)
			}
		} else if q.arg, s, err = quoted(s); err == nil {
			switch q.kind {
			case "jsonpath":
				q.path, err = parseJSONPath(q.arg)
			case "regex":
				q.re, err = regexp.Compile(q.arg)
			}
		}
		if err != nil {
			return q, "", fmt.Errorf("%s: %v", q.kind, err)
		}
	case "":
		return q, "", errors.New("missing query")
	default:
		return q, "", fmt.Errorf("unknown query %q", q.kind)
	}
	for {
		w, rest := nextWord(s)
		if w != "count" {
			return q, s, nil
		}
		q.filters, s = append(q.filters, w), rest
	}
}

var number = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][+-]?\d+)?$`)

func parseValue(s string) (any, error) {
	switch {
	case strings.HasPrefix(s, `"`):
		v, rest, err := quoted(s)
		if err == nil && rest != "" {
			err = fmt.Errorf("unexpected %q after value", rest)
		}
		return v, err
	case strings.HasPrefix(s, "/"):
		re, rest, err := regexLiteral(s)
		if err == nil && rest != "" {
			err = fmt.Errorf("unexpected %q after value", rest)
		}
		if err != nil {
			return nil, err
		}
		return regexp.Compile(re)
	case s == "true", s == "false":
		return s == "true", nil
	case s == "null":
		return nil, nil
	case number.MatchString(s):
		return json.Number(s), nil
	}
	return nil, fmt.Errorf("bad value %q (strings need quotes)", s)
}

// nextWord returns the first space-separated word of s and the rest.
func nextWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	w, rest, _ := strings.Cut(s, " ")
	return w, strings.TrimSpace(rest)
}

// quoted unquotes the Go string literal at the front of s.
func quoted(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	lit, err := strconv.QuotedPrefix(s)
	if err != nil || !strings.HasPrefix(lit, `"`) {
		return "", "", fmt.Errorf("want a \"quoted string\", got %q", s)
	}
	v, _ := strconv.Unquote(lit)
	return v, strings.TrimSpace(s[len(lit):]), nil
}

// regexLiteral reads the /regex/ at the front of s, where \/ is a slash.
func regexLiteral(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '/':
			b.WriteByte('/')
			i++
		case s[i] == '/':
			return b.String(), strings.TrimSpace(s[i+1:]), nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", fmt.Errorf("unterminated regex %s", s)
}
