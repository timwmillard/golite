# httpscript

API tests as plain-text scripts, in a syntax close to [hurl](https://hurl.dev)'s, run by `go test` with Go's `net/http`: no curl, no hurl binary, nothing to install.

`httpscript` adds an `http` command to [testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript). Each script runs one or more request files and can use any other testscript command (`cmp`, `stdout`, `env`, conditions, …).

```go
func TestAPI(t *testing.T) {
	httpscript.Run(t, "testdata", app.Handler())
}
```

`Run` starts the handler on an `httptest.Server` and runs every `testdata/*.txtar` against it, each as its own parallel subtest (`go test -run TestAPI/login`).

## Example

`testdata/login.txtar`:

```
# Log in, then fetch the profile with the token.
http login.http
stdout '"name":"alice"'

-- login.http --
POST /login
Content-Type: application/json

{
  "user": "alice",
  "pass": "secret"
}

HTTP 200
[Captures]
token: jsonpath "$.token"

GET /me
Authorization: Bearer {{token}}

HTTP 200
Content-Type: application/json
[Asserts]
jsonpath "$.name" == "alice"
jsonpath "$.roles" count == 2
jsonpath "$.roles" includes "admin"
```

A failure points at the line in the request file:

```
FAIL: testdata/login.txtar:2: login.http:19: jsonpath "$.name" == "alice": got "bob"
```

## Request files

A file holds one or more entries. Each entry starts at a request line and runs until the next one.

```
METHOD URL
Header-Name: value
Another-Header: value
[Query]
key: value
[Form]
key: value

body line 1
body line 2

HTTP 200
Response-Header: value
[Captures]
name: query
[Asserts]
query predicate value
```

### Request

| Part | |
|---|---|
| `METHOD URL` | `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, `CONNECT` or `TRACE`. A URL starting with `/` is resolved against `{{base}}`, which keeps any path in the base URL. |
| Headers | One `Name: value` per line, straight after the request line. Repeat a name to send it more than once. `Host:` sets the request's host. |
| `[Query]` | `key: value` lines added to the URL's query, encoded for you. |
| `[Form]` | `key: value` lines sent as an `application/x-www-form-urlencoded` body. Can't be combined with a body. |
| Body | Everything after the first blank line, as written, up to the `HTTP` line or the next request (trailing blank lines are dropped). Blank lines and indentation inside it are kept. A JSON object or array is sent with `Content-Type: application/json` unless you set a Content-Type. |

A body line can't look like a request line (`GET /x`) or start with `HTTP `. Lines starting with `#` are comments, except inside a body.

### Response

The `HTTP` line is optional. Without it, the request is sent and nothing is checked.

| Part | |
|---|---|
| `HTTP 200` | The status the response must have. `HTTP *` accepts any status. |
| Headers | `Name: value` lines. One of the response's values for that header must equal the value. |
| `[Captures]` | `name: query [filter]` sets a variable from the response. |
| `[Asserts]` | `query [filter] [not] predicate [value]` checks the response. |

Captures run before asserts. When an entry fails, the file stops there, since later entries usually depend on it.

### Queries

| Query | Value |
|---|---|
| `status` | The status code, as a number. |
| `body` | The body, as a string. |
| `header "Name"` | The header's value, or a list if it has several. |
| `cookie "name"` | The value of a cookie the response sets. |
| `jsonpath "$.path"` | The value at that path in the JSON body. |
| `regex "pattern"` or `regex /pattern/` | Its first group in the body if it has one, else the whole match. |

JSONPath supports `$`, `.name`, `['name']`, `[n]` (negative counts from the end) and `*` / `[*]`, which make the result a list. Recursive descent (`..`) and filters (`[?(...)]`) are rejected with an error. Numbers keep their exact digits, so large IDs compare correctly.

### Filters

| Filter | |
|---|---|
| `count` | The length of a list, object or string. |

### Predicates

| Predicate | Holds when the value |
|---|---|
| `==`, `!=` | equals, or doesn't equal, the given value |
| `>`, `>=`, `<`, `<=` | compares as given (numbers, or strings) |
| `contains`, `startsWith`, `endsWith` | is a string containing, starting or ending with the given string |
| `matches` | is a string matching the `/regex/` or `"regex"` |
| `includes` | is a list with an element equal to the given value |
| `exists` | is there (a JSON `null` exists; a missing key doesn't) |
| `isEmpty` | is an empty list, object or string |

Put `not` before a predicate to negate it: `jsonpath "$.error" not exists`.

Values are `"quoted strings"` (with Go escapes such as `\n` and `\"`), numbers, `true`, `false`, `null`, or `/regex/` (use `\/` for a slash).

## Variables

`{{name}}` is replaced anywhere in an entry, including its URL, headers, body and assert values, by:

1. a variable captured earlier in the script (in this file or a previous one), or
2. the environment variable of that name, such as `base` or one set with testscript's `env name=value`.

An undefined variable fails the script.

Captures are also set as environment variables, so the rest of the script sees them as `$name`:

```
http login.http
cmpenv stdout want.txt   # want.txt can use $token
```

Strings and numbers are captured as they are. Lists and objects are captured as JSON.

## Working with testscript

- **stdout** holds the last response's body, for `stdout`, `cmp` and `cmpenv`. `cmp stdout want.json` is the way to check a whole body, and testscript's `UpdateScripts` can rewrite it for you.
- **`! http file`** expects an assert to fail. Parse errors, undefined variables and network errors fail the script either way.
- **Cookies** persist across every request in a script, and each script gets its own cookie jar.
- **Redirects** aren't followed, as in hurl. Assert on the `3xx` and its `Location` instead.
- `go test -v` logs each request and its status.

## Other servers and commands

`Run` covers the usual case. To run against a server that's already up, or to combine `http` with your own commands, build the `testscript.Params` yourself:

```go
testscript.Run(t, testscript.Params{
	Dir:   "testdata",
	Setup: httpscript.Setup("http://localhost:8080"),
	Cmds:  httpscript.Cmds, // merge in your own commands here
})
```

If you use your own `Setup` function, call the one `httpscript.Setup` returns from inside it.

## Differences from hurl

- A blank line is required before a request body, and the body is the following lines as written. Hurl's ```` ``` ```` multiline strings, `file,` bodies and JSON auto-detection without a blank line aren't supported.
- An expected body can't be written after the `HTTP` line. Use `cmp stdout file` or `body == "..."`.
- Not yet supported: `[BasicAuth]`, `[Cookies]`, `[Options]`, `[MultipartFormData]`, `xpath`, `duration`, filters other than `count`, and reports (HTML, JUnit and so on). `go test` reports results instead.
- Requests use Go's `net/http` rather than libcurl, so HTTP/3 and curl's options aren't available.
