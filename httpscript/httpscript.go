// Package httpscript adds an http command to testscript
// (github.com/rogpeppe/go-internal/testscript) that runs hurl-like request
// files with Go's net/http, so an API's tests are plain-text scripts run by
// go test, against an http.Handler, with no curl or hurl binary.
//
//	func TestAPI(t *testing.T) {
//		httpscript.Run(t, "testdata", app.Handler())
//	}
//
// Each testdata/*.txtar script runs the files it holds with http, and can
// use the other testscript commands too:
//
//	http login.http
//	stdout '"name": "alice"'   # stdout holds the last response's body
//
//	-- login.http --
//	POST /login
//	Content-Type: application/json
//
//	{
//	  "user": "alice",
//	  "pass": "secret"
//	}
//
//	HTTP 200
//	[Captures]
//	token: jsonpath "$.token"
//
//	GET /me
//	Authorization: Bearer {{token}}
//
//	HTTP 200
//	Content-Type: application/json
//	[Asserts]
//	jsonpath "$.name" == "alice"
//	jsonpath "$.roles" count == 2
//
// A file holds one or more entries, each a request line (METHOD URL)
// followed by:
//
//   - Headers, one "Name: value" per line.
//   - Optional [Query] and [Form] sections of "key: value" lines, after the
//     headers. [Form] sends an application/x-www-form-urlencoded body.
//   - A blank line, then the body, as written, up to the response line or
//     the next request line (trailing blank lines are dropped). A body that
//     is a JSON object or array is sent as application/json unless the
//     request sets its own Content-Type. A body line can't itself look like
//     a request line or start with "HTTP ".
//   - Optionally the expected response: "HTTP 200" (or "HTTP *" for any
//     status), then "Name: value" lines the response's headers must match,
//     then [Captures] and [Asserts] sections.
//
// Relative URLs are resolved against {{base}}, which Run sets to the test
// server's URL. Lines starting with # outside a body are comments.
//
// {{name}} is replaced, anywhere in an entry, by a variable captured by an
// earlier entry or script, or else by the environment variable of that name
// (set with testscript's env command, for one). Captures are set as
// environment variables, so the rest of the script sees them as $name.
//
// A capture is "name: query [filter]" and an assert is
// "query [filter] [not] predicate [value]", where:
//
//   - query is status, body, header "Name", cookie "name", jsonpath "$.a[0].b"
//     or regex "pattern" (its first group if it has one, else the match).
//     JSONPath supports .name, ['name'], [n] (negative from the end), and *
//     or [*], which make the result a list.
//   - filter is count, the length of a list, object or string.
//   - predicate is ==, !=, >, >=, <, <=, contains, startsWith, endsWith,
//     matches, includes (a list holds the value), exists or isEmpty.
//   - value is a "quoted string" (Go escapes), a number, true, false, null,
//     or for matches a /regex/.
//
// Like hurl, redirects aren't followed, and cookies persist across requests
// in a script.
//
// "! http file" expects an assert to fail; parse and network errors fail
// the script either way.
package httpscript

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// Run starts h on a test server and runs the scripts in dir against it.
func Run(t *testing.T, dir string, h http.Handler) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	testscript.Run(t, testscript.Params{
		Dir:   dir,
		Setup: Setup(srv.URL),
		Cmds:  Cmds,
	})
}

// Cmds holds the http command, for testscript.Params.Cmds. Scripts using
// it need Setup.
var Cmds = map[string]func(ts *testscript.TestScript, neg bool, args []string){
	"http": cmdHTTP,
}

// Setup returns a testscript.Params.Setup that gives each script its own
// cookie jar and sets {{base}} to baseURL, for Params built by hand: to run
// against a live server, or alongside other commands.
func Setup(baseURL string) func(*testscript.Env) error {
	return func(e *testscript.Env) error {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return err
		}
		e.Setenv("base", baseURL)
		e.Values[stateKey{}] = &state{
			client: &http.Client{
				Jar: jar,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
			vars: map[string]bool{"base": true},
		}
		return nil
	}
}

type stateKey struct{}

// state is a script's: its client and the variables it has captured.
type state struct {
	client *http.Client
	vars   map[string]bool
}
