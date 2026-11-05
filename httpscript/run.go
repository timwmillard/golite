package httpscript

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rogpeppe/go-internal/testscript"
)

var (
	varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	varRef  = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
)

func cmdHTTP(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: http file")
	}
	st, _ := ts.Value(stateKey{}).(*state)
	if st == nil {
		ts.Fatalf("http: script wasn't set up by httpscript.Setup")
	}
	file := args[0]

	var failures []string
	var lastBody []byte
	for _, raw := range splitEntries(ts.ReadFile(file)) {
		lines := make([]string, len(raw.lines))
		for i, l := range raw.lines {
			var err error
			if lines[i], err = expand(ts, st, l); err != nil {
				ts.Fatalf("%s:%d: %v", file, raw.line+i, err)
			}
		}
		e, err := parseEntry(lines, raw.line)
		if err != nil {
			ts.Fatalf("%s:%v", file, err)
		}

		resp, body, err := do(ts, st.client, e)
		if err != nil {
			ts.Fatalf("%s:%d: %v", file, raw.line, err)
		}
		lastBody = body
		if e.resp == nil {
			continue
		}
		for _, f := range check(ts, st, e.resp, resp, body) {
			failures = append(failures, fmt.Sprintf("%s:%s", file, f))
		}
		if len(failures) > 0 {
			break // later entries likely depend on this one
		}
	}
	ts.Stdout().Write(lastBody)

	switch {
	case len(failures) > 0 && !neg:
		ts.Fatalf("%s", strings.Join(failures, "\n"))
	case len(failures) > 0:
		ts.Logf("%s", strings.Join(failures, "\n"))
	case neg:
		ts.Fatalf("http %s: unexpected success", file)
	}
}

// expand replaces the {{name}} variables in l.
func expand(ts *testscript.TestScript, st *state, l string) (string, error) {
	var err error
	l = varRef.ReplaceAllStringFunc(l, func(ref string) string {
		name := varRef.FindStringSubmatch(ref)[1]
		v := ts.Getenv(name)
		if v == "" && !st.vars[name] && err == nil {
			err = fmt.Errorf("undefined variable %s", name)
		}
		return v
	})
	return l, err
}

func do(ts *testscript.TestScript, c *http.Client, e *entry) (*http.Response, []byte, error) {
	rawURL := e.url
	if strings.HasPrefix(rawURL, "/") {
		base := ts.Getenv("base")
		if base == "" {
			return nil, nil, fmt.Errorf("relative URL %s with no {{base}}", rawURL)
		}
		rawURL = strings.TrimSuffix(base, "/") + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if len(e.query) > 0 {
		q := u.Query()
		for _, f := range e.query {
			q.Add(f.key, f.value)
		}
		u.RawQuery = q.Encode()
	}

	body, contentType := e.body, ""
	if len(e.form) > 0 {
		form := url.Values{}
		for _, f := range e.form {
			form.Add(f.key, f.value)
		}
		body, contentType = form.Encode(), "application/x-www-form-urlencoded"
	} else if t := strings.TrimSpace(body); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		contentType = "application/json"
	}

	req, err := http.NewRequest(e.method, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if body == "" {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for _, h := range e.headers {
		if strings.EqualFold(h.key, "Host") {
			req.Host = h.value
			continue
		}
		req.Header.Add(h.key, h.value)
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	ts.Logf("%s %s: %s", e.method, u, resp.Status)
	return resp, respBody, nil
}

// check runs r's captures and asserts against resp, returning a
// "line: message" for each that fails.
func check(ts *testscript.TestScript, st *state, r *response, resp *http.Response, body []byte) []string {
	var failures []string
	fail := func(line int, format string, args ...any) {
		failures = append(failures, fmt.Sprintf("%d: %s", line, fmt.Sprintf(format, args...)))
	}

	if r.status != 0 && resp.StatusCode != r.status {
		fail(r.line, "status: got %d, want %d; body: %s", resp.StatusCode, r.status, clip(body))
	}
	for _, h := range r.headers {
		if got := resp.Header.Values(h.key); !slices.Contains(got, h.value) {
			fail(h.line, "header %s: got %q, want %q", h.key, got, h.value)
		}
	}

	ev := &evaluator{resp: resp, body: body}
	for _, c := range r.captures {
		v, found, err := ev.eval(c.q)
		switch {
		case err != nil:
			fail(c.line, "capture %s: %v", c.name, err)
		case !found:
			fail(c.line, "capture %s: no value", c.name)
		default:
			ts.Setenv(c.name, captureString(v))
			st.vars[c.name] = true
		}
	}
	for _, a := range r.asserts {
		v, found, err := ev.eval(a.q)
		if err == nil {
			err = test(a, v, found)
		}
		if err != nil {
			fail(a.line, "%s: %v", a.src, err)
		}
	}
	return failures
}

type evaluator struct {
	resp    *http.Response
	body    []byte
	json    any
	jsonErr error
	decoded bool
}

// eval returns q's value, decoded JSON values for jsonpath (numbers as
// json.Number) and strings or json.Numbers otherwise, and whether there is one.
func (ev *evaluator) eval(q query) (any, bool, error) {
	var v any
	found := true
	switch q.kind {
	case "status":
		v = json.Number(strconv.Itoa(ev.resp.StatusCode))
	case "body":
		v = string(ev.body)
	case "header":
		switch vals := ev.resp.Header.Values(q.arg); len(vals) {
		case 0:
			found = false
		case 1:
			v = vals[0]
		default:
			l := make([]any, len(vals))
			for i, s := range vals {
				l[i] = s
			}
			v = l
		}
	case "cookie":
		found = false
		for _, c := range ev.resp.Cookies() {
			if c.Name == q.arg {
				v, found = c.Value, true
			}
		}
	case "jsonpath":
		if !ev.decoded {
			d := json.NewDecoder(bytes.NewReader(ev.body))
			d.UseNumber()
			ev.jsonErr = d.Decode(&ev.json)
			ev.decoded = true
		}
		if ev.jsonErr != nil {
			return nil, false, fmt.Errorf("body isn't JSON: %v; body: %s", ev.jsonErr, clip(ev.body))
		}
		v, found = q.path.eval(ev.json)
	case "regex":
		m := q.re.FindSubmatch(ev.body)
		switch {
		case m == nil:
			found = false
		case len(m) > 1:
			v = string(m[1])
		default:
			v = string(m[0])
		}
	}

	for _, f := range q.filters {
		if !found {
			return nil, false, fmt.Errorf("%s of no value", f)
		}
		n, ok := length(v)
		if !ok {
			return nil, false, fmt.Errorf("%s of %s", f, show(v))
		}
		v = json.Number(strconv.Itoa(n))
	}
	return v, found, nil
}

func length(v any) (int, bool) {
	switch v := v.(type) {
	case []any:
		return len(v), true
	case map[string]any:
		return len(v), true
	case string:
		return utf8.RuneCountInString(v), true
	}
	return 0, false
}

// test reports why a's predicate doesn't hold for the value v.
func test(a assertion, v any, found bool) error {
	if a.pred == "exists" {
		if found == a.not {
			return fmt.Errorf("got %s", showFound(v, found))
		}
		return nil
	}
	if !found {
		return fmt.Errorf("no value")
	}

	var ok bool
	switch a.pred {
	case "isEmpty":
		n, isLen := length(v)
		if !isLen {
			return fmt.Errorf("isEmpty of %s", show(v))
		}
		ok = n == 0
	case "==":
		ok = equal(v, a.value)
	case "!=":
		ok = !equal(v, a.value)
	case ">", ">=", "<", "<=":
		c, err := compare(v, a.value)
		if err != nil {
			return err
		}
		ok = map[string]bool{">": c > 0, ">=": c >= 0, "<": c < 0, "<=": c <= 0}[a.pred]
	case "contains", "startsWith", "endsWith":
		s, isStr := v.(string)
		want, wantStr := a.value.(string)
		if !isStr || !wantStr {
			return fmt.Errorf("%s needs strings, got %s", a.pred, show(v))
		}
		ok = map[string]func(string, string) bool{
			"contains": strings.Contains, "startsWith": strings.HasPrefix, "endsWith": strings.HasSuffix,
		}[a.pred](s, want)
	case "matches":
		s, isStr := v.(string)
		if !isStr {
			return fmt.Errorf("matches needs a string, got %s", show(v))
		}
		ok = a.value.(*regexp.Regexp).MatchString(s)
	case "includes":
		l, isList := v.([]any)
		if !isList {
			return fmt.Errorf("includes needs a list, got %s", show(v))
		}
		ok = slices.ContainsFunc(l, func(x any) bool { return equal(x, a.value) })
	}
	if ok == a.not {
		return fmt.Errorf("got %s", show(v))
	}
	return nil
}

func equal(a, b any) bool {
	an, aNum := a.(json.Number)
	bn, bNum := b.(json.Number)
	if aNum || bNum {
		c, err := compareNumbers(an, bn)
		return aNum && bNum && err == nil && c == 0
	}
	switch a.(type) {
	case []any, map[string]any:
		return false
	}
	return a == b
}

func compare(a, b any) (int, error) {
	if an, ok := a.(json.Number); ok {
		if bn, ok := b.(json.Number); ok {
			return compareNumbers(an, bn)
		}
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return strings.Compare(as, bs), nil
		}
	}
	return 0, fmt.Errorf("can't compare %s with %s", show(a), show(b))
}

func compareNumbers(a, b json.Number) (int, error) {
	if ai, err := a.Int64(); err == nil {
		if bi, err := b.Int64(); err == nil {
			return cmp.Compare(ai, bi), nil
		}
	}
	af, err := a.Float64()
	if err != nil {
		return 0, err
	}
	bf, err := b.Float64()
	if err != nil {
		return 0, err
	}
	return cmp.Compare(af, bf), nil
}

// captureString is a captured value as a variable: strings and numbers as
// they are, anything else as JSON.
func captureString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func show(v any) string {
	switch v := v.(type) {
	case string:
		return strconv.Quote(clip([]byte(v)))
	case json.Number:
		return v.String()
	}
	b, _ := json.Marshal(v)
	return clip(b)
}

func showFound(v any, found bool) string {
	if !found {
		return "no value"
	}
	return show(v)
}

// clip shortens long values in failure messages.
func clip(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
