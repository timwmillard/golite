package httpscript

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestScripts(t *testing.T) {
	Run(t, "testdata", testHandler())
}

// testHandler is a small API for the scripts in testdata.
func testHandler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ User, Pass string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Pass != "secret" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad login"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s-" + in.User, Path: "/"})
		writeJSON(w, http.StatusOK, map[string]string{"token": "t-" + in.User})
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t-alice" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no token"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    json.Number("12345678901234567"),
			"name":  "alice",
			"admin": true,
			"roles": []string{"admin", "dev"},
			"teams": []map[string]any{{"name": "core", "size": 3}, {"name": "web", "size": 5}},
			"bio":   nil,
		})
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.Header().Set("X-Host", r.Host)
		writeJSON(w, http.StatusOK, map[string]any{
			"method":       r.Method,
			"query":        r.URL.Query(),
			"content_type": r.Header.Get("Content-Type"),
			"body":         string(body),
		})
	})
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("session")
		if err != nil {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "session "+c.Value+"\n")
	})
	mux.HandleFunc("GET /old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	return mux
}

func TestJSONPath(t *testing.T) {
	var doc any
	d := json.NewDecoder(strings.NewReader(`{"a":{"b":[1,2,{"c":"x"}]},"k y":true,"m":{"z":1,"y":2}}`))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path string
		want string // JSON, or "-" for no value
	}{
		{"$", `{"a":{"b":[1,2,{"c":"x"}]},"k y":true,"m":{"y":2,"z":1}}`},
		{"$.a.b[0]", `1`},
		{"$.a.b[-1].c", `"x"`},
		{"$['k y']", `true`},
		{`$["a"]["b"][1]`, `2`},
		{"$.a.b[3]", "-"},
		{"$.nope", "-"},
		{"$.a.b[*]", `[1,2,{"c":"x"}]`},
		{"$.m.*", `[2,1]`}, // by key
		{"$.a.b[*].c", `["x"]`},
		{"$.nope[*]", `[]`},
	}
	for _, tt := range tests {
		p, err := parseJSONPath(tt.path)
		if err != nil {
			t.Errorf("parseJSONPath(%q): %v", tt.path, err)
			continue
		}
		got := "-"
		if v, ok := p.eval(doc); ok {
			b, _ := json.Marshal(v)
			got = string(b)
		}
		if got != tt.want {
			t.Errorf("%s = %s, want %s", tt.path, got, tt.want)
		}
	}

	for _, bad := range []string{"a.b", "$..a", "$.a[?(@.x)]", "$.", "$['a"} {
		if _, err := parseJSONPath(bad); err == nil {
			t.Errorf("parseJSONPath(%q) succeeded, want error", bad)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ src, want string }{
		{"GET /x\nnot a header", `2: want "name: value"`},
		{"GET /x\n[Cookies]\n", "2: unknown request section [Cookies]"},
		{"GET /x\nHTTP 2xx", `2: bad status "2xx"`},
		{"GET /x\nHTTP 200\n[Asserts]\nstatus = 200", `4: want a predicate, got "="`},
		{"GET /x\nHTTP 200\n[Asserts]\nbody == hello", `4: bad value "hello"`},
		{"GET /x\nHTTP 200\n[Asserts]\nheader Location exists", `4: header: want a "quoted string"`},
		{"GET /x\nHTTP 200\n[Asserts]\nduration < 100", `4: unknown query "duration"`},
		{"GET /x\nHTTP 200\n[Asserts]\nbody matches /a(/", "4: error parsing regexp"},
		{"GET /x\nHTTP 200\n[Captures]\nid jsonpath \"$.id\"", `4: want "name: query"`},
		{"GET /x\nHTTP 200\n[Asserts]\nstatus exists 1", `4: unexpected "1" after exists`},
		{"POST /x\n[Form]\na: 1\n\nbody", "1: request has both a [Form] and a body"},
	}
	for _, tt := range tests {
		entries := splitEntries(tt.src)
		if len(entries) != 1 {
			t.Fatalf("%q: %d entries", tt.src, len(entries))
		}
		_, err := parseEntry(entries[0].lines, entries[0].line)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error %v, want %q", tt.src, err, tt.want)
		}
	}
}

func TestParseBody(t *testing.T) {
	src := `# leading comment
POST /a
Content-Type: text/plain
[Query]
q: 1

line one

# kept: comments are body text here
line three


HTTP 201
Location: /a/1
[Captures]
id: header "Location"
[Asserts]
body contains "ok"

GET /b
`
	entries := splitEntries(src)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	e, err := parseEntry(entries[0].lines, entries[0].line)
	if err != nil {
		t.Fatal(err)
	}
	if want := "line one\n\n# kept: comments are body text here\nline three"; e.body != want {
		t.Errorf("body = %q, want %q", e.body, want)
	}
	if len(e.headers) != 1 || len(e.query) != 1 || e.resp == nil || e.resp.status != 201 ||
		len(e.resp.headers) != 1 || len(e.resp.captures) != 1 || len(e.resp.asserts) != 1 {
		t.Errorf("parsed %+v, resp %+v", e, e.resp)
	}
	if e.resp.asserts[0].line != 18 {
		t.Errorf("assert line = %d, want 18", e.resp.asserts[0].line)
	}
	e, err = parseEntry(entries[1].lines, entries[1].line)
	if err != nil || e.method != "GET" || e.url != "/b" || e.body != "" || e.resp != nil {
		t.Errorf("second entry = %+v, %v", e, err)
	}
}
