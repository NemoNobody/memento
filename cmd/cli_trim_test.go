package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordedCall is one request the fake OpenSearch saw.
type recordedCall struct {
	method string
	path   string
	body   string
}

// cliTrimBuild is the module-root build directory for the CLI subprocess
// tests: the binary is built ONCE per test run (not per subtest — ten `go
// build` invocations are ten times the CI cost of the whole point), into a
// pid-scoped temp dir removed by TestMain via cleanupCLITrimBuild.
var (
	cliTrimBuildOnce sync.Once
	cliTrimBuildDir  string
	cliTrimBuildErr  error
)

func cleanupCLITrimBuild() {
	if cliTrimBuildDir != "" {
		_ = os.RemoveAll(cliTrimBuildDir)
	}
}

func cliTrimBinary(t *testing.T) string {
	t.Helper()
	cliTrimBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "memento-cli-trim-")
		if err != nil {
			cliTrimBuildErr = err
			return
		}
		cliTrimBuildDir = dir
		exe := filepath.Join(dir, "memento-under-test")
		build := exec.Command("go", "build", "-o", exe, ".")
		build.Dir = projectRoot(t)
		if out, err := build.CombinedOutput(); err != nil {
			cliTrimBuildErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if cliTrimBuildErr != nil {
		t.Fatalf("build memento for CLI test: %v", cliTrimBuildErr)
	}
	return filepath.Join(cliTrimBuildDir, "memento-under-test")
}

// trimFacts is the source served for document GETs when found=true: a minimal
// but complete fact, keyed exactly like the padded test key's trimmed form.
const trimFacts = `{"key":"k","value":"v1","category":"config","project":"global","status":"verified"}`

// startTrimFake runs a stand-in OpenSearch that records every request and
// answers the few shapes the CLI edges exercise: index HEAD, document GET
// (found or missing), search (empty page), and index/update writes.
// The recording carries a mutex: the handler runs on the server's goroutine,
// and the reads after the subprocess exits must not depend on OS-level
// ordering accidents.
func startTrimFake(t *testing.T, found bool) (*httptest.Server, func() []recordedCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedCall
	snapshot := func() []recordedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedCall(nil), calls...)
	}

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		_ = r.Body.Close()
		mu.Lock()
		calls = append(calls, recordedCall{method: r.Method, path: r.URL.Path, body: body.String()})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/page":
			// A routable page for the CLI fetch edge (the same fake plays the
			// OpenSearch store and the target host: nothing leaves the box).
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><title>Doc Page</title></head>` +
				`<body><h1>Heading</h1><p>Some documentation text for the trim test.</p></body></html>`))
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_doc/"):
			if found {
				_, _ = w.Write([]byte(`{"_index":"memento","_id":"k","_version":1,"found":true,"_source":` + trimFacts + `}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"document_missing_exception","reason":"Document with id k not found.","index":"memento"},"status":404}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_search"):
			_, _ = w.Write([]byte(`{"took":1,"timed_out":false,"hits":{"total":{"value":0},"max_score":null,"hits":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"_index":"memento","_id":"k","_version":1,"result":"ok","_shards":{"total":1,"successful":1,"failed":0}}`))
		}
	}))
	t.Cleanup(fake.Close)
	return fake, snapshot
}

// runCLISubprocess executes the built binary as the real CLI (the test binary
// is not the CLI: the mcp_test TestMain only re-execs as a server on demand),
// with a pinned environment so no local config, keyring, or user password can
// leak into the child.
func runCLISubprocess(t *testing.T, fakeURL string, args ...string) (string, int) {
	t.Helper()
	return runCLISubprocessEnv(t, fakeURL, nil, args...)
}

func runCLISubprocessEnv(t *testing.T, fakeURL string, extraEnv []string, args ...string) (string, int) {
	t.Helper()

	// The CLI edges talk to a store; the binary is built once per run (see
	// cliTrimBinary), so the whole table costs a single `go build`.
	exe := cliTrimBinary(t)

	dir := t.TempDir()
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	// t.TempDir HOME: no user ~/.memento.conf; MEMENTO_CONF="" blocks the
	// cwd config; the env endpoint/password pin the fake exactly.
	env := childEnv(append([]string{
		"HOME=" + dir,
		"MEMENTO_CONF=",
		"MEMENTO_ALLOW_PLAINTEXT=1",
		"OPENSEARCH_URL=" + fakeURL,
		"OPENSEARCH_USER=testuser",
		"OPENSEARCH_PASSWORD=testpw",
	}, extraEnv...))
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), code
}

// projectRoot locates the module root from the test's working directory
// (go test runs in the package dir; the module root is the cmd/ parent).
func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

// TestCLITrimParity pins the round-4 trim contract end to end: every CLI edge
// that takes a key or a filter argument must send and echo the TRIMMED form,
// exactly like the MCP edges. Until now the trims were structural only — this
// test is what stops a future refactor from dropping one.
//
// The fake records the actual store requests; a lost TrimSpace anywhere on
// these paths would show up as a padded key/filter on the wire or in the
// command's own echo.
func TestCLITrimParity(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     []string
		prefix  string // prepended to args after the fake URL is known (fetch targets the fake host)
		found   bool
		wantOut []string // substrings that must appear in the CLI output
		wantReq []string // substrings that must appear in some request path/body
		deny    []string // substrings that must appear nowhere (paths and bodies)
	}{
		{
			name:    "get",
			args:    []string{"get", " k "},
			found:   true,
			wantOut: []string{`key: "k"`},
			wantReq: []string{"/memento/_doc/k"},
			deny:    []string{"%20k", "k%20", `" k "`},
		},
		{
			name:    "add",
			args:    []string{"add", " k ", "v1", "config"},
			found:   false,
			wantOut: []string{`key="k"`},
			wantReq: []string{"_doc/k"},
			deny:    []string{"%20k", "k%20", `" k "`},
		},
		{
			name:    "obsolete",
			args:    []string{"obsolete", " k ", "subject gone"},
			found:   true,
			wantReq: []string{"_update/k"},
			deny:    []string{"%20k", "k%20", `" k "`},
		},
		{
			name:    "prune",
			args:    []string{"prune", " k "},
			found:   true,
			wantOut: []string{`key "k"`},
			wantReq: []string{"_doc/k"},
			deny:    []string{"%20k", "k%20", `" k "`},
		},
		{
			name:    "journal",
			args:    []string{"journal", " k "},
			found:   true,
			wantOut: []string{`journal for "k"`},
			wantReq: []string{`"fact_key":"k"`},
			deny:    []string{`" k "`, "journal for \" k\""},
		},
		{
			name:    "list-project",
			args:    []string{"list", " proj "},
			found:   true,
			wantReq: []string{`"project":"proj"`},
			deny:    []string{`"project":" proj "`},
		},
		{
			name:    "search-project",
			args:    []string{"search", "q", "--project", " proj "},
			found:   true,
			wantReq: []string{`"project":"proj"`},
			deny:    []string{`"project":" proj "`},
		},
		{
			name:    "keys-match",
			args:    []string{"keys", "--match", " k "},
			found:   true,
			wantReq: []string{`"key":"*k*"`},
			deny:    []string{`"key":"* k *"`, `"project":" proj "`},
		},
		{
			name:    "docs-project",
			args:    []string{"docs", "q", "--project", " proj "},
			found:   true,
			wantReq: []string{`"project":"proj"`},
			deny:    []string{`"project":" proj "`},
		},
		{
			// fetch stores the tag in the doc; the MCP fetchDoc edge trims it,
			// and the docs term filter is exact on spaces — a padded CLI tag
			// would make the MCP search_docs edge miss the CLI's own doc.
			// FetchAllowPrivate lets the fetch target the local fake host.
			name:    "fetch-project",
			prefix:  "/page",
			args:    []string{"--project", " proj "},
			env:     []string{"MEMENTO_FETCH_ALLOW_PRIVATE=1"},
			found:   true,
			wantReq: []string{`"project":"proj"`},
			deny:    []string{`"project":" proj "`},
		},
		{
			// Whitespace-only --match/--project mean "no filter given"
			// exactly like the MCP keys edge: the prefix still defines the
			// query, the padded filters must vanish (not reach the store as
			// padded terms or empty wildcards).
			name:    "keys-padded-empty",
			args:    []string{"keys", "k ", "--match", "  ", "--project", "  "},
			found:   true,
			wantOut: []string{"no matching keys"},
			wantReq: []string{`"key":"k"`},
			deny:    []string{`"key":"**"`, `"project":"  "`, `"key":"k "`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, calls := startTrimFake(t, tc.found)
			args := tc.args
			if tc.prefix != "" {
				args = append([]string{"fetch", fake.URL + tc.prefix}, args...)
			}
			out, code := runCLISubprocessEnv(t, fake.URL, tc.env, args...)
			if code != 0 {
				t.Fatalf("%v exited %d, want 0; output: %s", args, code, out)
			}

			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("CLI output missing %q; got: %s", want, out)
				}
			}

			// The echo/quotes above prove the trimmed form reached the user;
			// the deny set proves the padded form never reached the store.
			for _, c := range calls() {
				line := c.method + " " + c.path + " " + c.body
				for _, bad := range tc.deny {
					if strings.Contains(line, bad) {
						t.Errorf("request %q contains padded form %q", line, bad)
					}
				}
			}
			for _, want := range tc.wantReq {
				found := false
				for _, c := range calls() {
					if strings.Contains(c.path, want) || strings.Contains(c.body, want) {
						found = true
						break
					}
				}
				if !found {
					var seen []string
					for _, c := range calls() {
						seen = append(seen, c.method+" "+c.path)
					}
					t.Errorf("no request contains %q; requests were: %v", want, seen)
				}
			}
		})
	}
}

// TestCLIStatusAndLimitShapeBeforeStore pins the "shape before store contact"
// contract on the CLI search/keys edges: an invalid --status or a negative
// --limit is answered as a caller error with zero store requests (the MCP
// edges validate the same way; a negative limit would otherwise read as
// "no cap" and widen the page).
func TestCLIStatusAndLimitShapeBeforeStore(t *testing.T) {
	fake, calls := startTrimFake(t, true)
	cases := [][]string{
		{"search", "q", "--status", "bogus"},
		{"search", "q", "--limit", "-1"},
		{"keys", "k", "--status", "bogus"},
		{"list", "p", "--prefix", "a*b"},
		{"doc", ""},
		{"docs", "  "},
	}
	for _, args := range cases {
		out, code := runCLISubprocess(t, fake.URL, args...)
		if code == 0 {
			t.Errorf("%v exited 0, want a shape refusal; output: %s", args, out)
		}
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("the refused queries reached the store: %v", got)
	}
}

// TestCLIKeysPaddedPrefixGuard pins the shape-error half of the keys edge:
// a prefix that normalizes to "" (" " or "*") with no usable --match is a
// caller error answered before any store contact (MCP returns the same
// contract instead of a store-wrapped "do not retry" advisory).
func TestCLIKeysPaddedPrefixGuard(t *testing.T) {
	fake, calls := startTrimFake(t, true)
	for _, args := range [][]string{
		{"keys", " "},
		{"keys", "*"},
		{"keys", "  ", "--match", " "},
	} {
		out, code := runCLISubprocess(t, fake.URL, args...)
		if code == 0 {
			t.Errorf("%v exited 0, want a shape refusal; output: %s", args, out)
		}
		if !strings.Contains(out, "provide a key prefix") {
			t.Errorf("%v output = %q, want the empty-query refusal", args, out)
		}
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("the refused queries reached the store: %v", got)
	}
}

// TestCLITrimTabsNewline pins the same contract for invisible whitespace: a
// key padded with tabs and newlines must arrive at the store as the trimmed
// document id, never with the padding URL- or byte-encoded into the path.
func TestCLITrimTabsNewline(t *testing.T) {
	fake, calls := startTrimFake(t, true)
	out, code := runCLISubprocess(t, fake.URL, "get", "\t k\n")
	if code != 0 {
		t.Fatalf("get with tab/newline padding exited %d: %s", code, out)
	}

	var sawGet bool
	for _, c := range calls() {
		if c.method == http.MethodGet && strings.HasPrefix(c.path, "/memento/_doc/") {
			sawGet = true
			if got := strings.TrimPrefix(c.path, "/memento/_doc/"); got != "k" {
				t.Errorf("document id path = %q, want %q", got, "k")
			}
		}
	}
	if !sawGet {
		t.Fatalf("no document GET recorded; output: %s", out)
	}
}
