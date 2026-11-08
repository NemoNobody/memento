package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/keyring"
	"github.com/NemoNobody/memento/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverEnvVar makes the test binary act as the MCP server child process.
const serverEnvVar = "MEMENTO_TEST_MCP_SERVER"

// TestMain makes the cmd suite hermetic against the machine's keyring:
// after a real `memento creds set` the dev entry (127.0.0.1:9200) exists in
// the user's wallet, and a Go test binary asking for it triggers a desktop
// access prompt that hangs the suite. The dispatcher default is a fake; the
// creds tests install their own fakes via fakeKeyringCmd. In-process code
// paths must never call the real keyring provider.
func TestMain(m *testing.M) {
	if os.Getenv(serverEnvVar) == "1" {
		// Child process: serve MCP over stdio until the client disconnects.
		if err := runMCPServer(""); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	get, set, del := keyring.Get, keyring.Set, keyring.Delete
	keyring.Get = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	keyring.Set = func(string, string, string) error { return nil }
	keyring.Delete = func(string, string) error { return keyring.ErrNotFound }
	code := m.Run()
	keyring.Get, keyring.Set, keyring.Delete = get, set, del
	os.Exit(code)
}

// connectTestServer spawns this test binary as an MCP server and connects to it
// over stdio, exercising the real handshake. extraEnv entries override the
// inherited environment.
func connectTestServer(t *testing.T, extraEnv ...string) *mcp.ClientSession {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// The child is a separate process: the parent's dispatcher fake does not
	// reach it. Pin env so the child resolves the password from env (never
	// the keyring) and fails fast on store access (port 9 = discard).
	cmd := exec.Command(exe)
	cmd.Env = append(childEnv(append([]string{
		"OPENSEARCH_PASSWORD=testpw",
		"OPENSEARCH_URL=https://127.0.0.1:9",
	}, extraEnv...)), serverEnvVar+"=1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "memento-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to MCP server: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// childEnv returns the parent environment with the overridden variables
// removed, so the last value always wins in the child.
func childEnv(overrides []string) []string {
	names := map[string]bool{}
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 {
			names[kv[:i]] = true
		}
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && names[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, overrides...)
}

var wantTools = []string{
	"memento_status", "memento_search", "memento_get",
	"memento_add", "memento_list", "memento_keys", "memento_prune", "memento_obsolete",
	"memento_journal", "memento_history_info",
	"memento_fetch_doc", "memento_get_doc", "memento_search_docs",
}

func TestMCPServerExposesAllTools(t *testing.T) {
	session := connectTestServer(t)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := map[string]string{}
	for _, tool := range tools.Tools {
		got[tool.Name] = tool.Description
	}
	for _, name := range wantTools {
		desc, ok := got[name]
		if !ok {
			t.Errorf("tool %q not advertised", name)
			continue
		}
		if desc == "" {
			t.Errorf("tool %q has an empty description", name)
		}
	}
	if len(got) != len(wantTools) {
		t.Errorf("got %d tools, want %d: %v", len(got), len(wantTools), got)
	}
}

// TestMCPServerStatusNeverFails checks the status contract:
// memento_status must report availability instead of failing, so an agent can
// explain the situation to the user even when OpenSearch is missing.
func TestMCPServerStatusNeverFails(t *testing.T) {
	session := connectTestServer(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "memento_status"})
	if err != nil {
		t.Fatalf("CallTool(memento_status): %v", err)
	}
	if res.IsError {
		t.Fatalf("memento_status reported a tool error: %+v", res.Content)
	}

	// The structured result must carry the ok flag and a URL.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		OK   bool   `json:"ok"`
		URL  string `json:"url"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", raw, err)
	}
	if out.URL == "" {
		t.Errorf("url is empty in status output: %s", raw)
	}
	if !out.OK && out.Hint == "" {
		t.Errorf("ok=false but no hint given: %s", raw)
	}
}

// TestMCPServerStoreToolsFailWhenUnreachable is the counterpart of the status
// contract: when the store is unreachable, the store tools must fail loudly.
// An agent that receives an empty list instead of an error would conclude
// "there is no such fact" — which is exactly the wrong lesson.
func TestMCPServerStoreToolsFailWhenUnreachable(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"memento_search", map[string]any{"query": "anything"}},
		{"memento_get", map[string]any{"key": "any:key"}},
		{"memento_list", map[string]any{"project": "any"}},
		{"memento_prune", map[string]any{"key": "any:key", "confirm": true}},
		{"memento_fetch_doc", map[string]any{"url": "https://example.com/x"}},
		{"memento_get_doc", map[string]any{"hash": "abc123"}},
		{"memento_search_docs", map[string]any{"query": "anything"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool(%s): %v", tc.name, err)
			}
			if !res.IsError {
				t.Errorf("%s reported success with an unreachable store; "+
					"an empty result would be mistaken for a real answer", tc.name)
			}
		})
	}
}

// TestMCPServerDocToolsValidateOffline pins the offline validation contract:
// a bad input is the caller's problem, not an availability problem. The store
// is unreachable on purpose: the tools must reject the input BEFORE touching
// it, so the agent gets an actionable message ("the host is not allowed"),
// not a store timeout.
func TestMCPServerDocToolsValidateOffline(t *testing.T) {
	session := connectTestServer(t,
		"OPENSEARCH_URL=https://127.0.0.1:9",
		"MEMENTO_FETCH_HOSTS=allowed.example.com",
	)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"memento_fetch_doc", map[string]any{"url": ""}, "empty url"},
		{"memento_fetch_doc", map[string]any{"url": "ftp://allowed.example.com/x"}, "scheme"},
		{"memento_fetch_doc", map[string]any{"url": "https://denied.example.com/x"}, "fetch_allow_hosts"},
		{"memento_search_docs", map[string]any{"query": ""}, "query"},
		{"memento_get_doc", map[string]any{"hash": ""}, "hash"},
	} {
		t.Run(tc.name+" "+tc.want, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool(%s): %v", tc.name, err)
			}
			if !res.IsError {
				t.Fatalf("%s accepted invalid input: %+v", tc.name, tc.args)
			}
			text := toolText(res)
			if !strings.Contains(text, tc.want) {
				t.Errorf("error must mention %q, got: %s", tc.want, text)
			}
			if strings.Contains(text, "127.0.0.1:9") {
				t.Errorf("validation ran after the store was reached: %s", text)
			}
		})
	}
}

// TestMCPServerAddValidatesShapeOffline pins that memento_add rejects a blank
// key, a blank value, and an unknown category before touching the store: an
// empty key produces a cryptic store error, and an unknown category never
// ages (silent rot). The store is a discard port: any text mentioning it
// means validation ran too late.
func TestMCPServerAddValidatesShapeOffline(t *testing.T) {
	session := connectTestServer(t, "OPENSEARCH_URL=https://127.0.0.1:9")
	ctx := context.Background()

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"key": "", "value": "v", "category": "config"}, "key is required"},
		{map[string]any{"key": "  ", "value": "v", "category": "config"}, "key is required"},
		{map[string]any{"key": "k", "value": "", "category": "config"}, "value is required"},
		{map[string]any{"key": "k", "value": "  ", "category": "config"}, "value is required"},
		{map[string]any{"key": "k", "value": "v", "category": "secrets"}, "category"},
		{map[string]any{"key": "k", "value": "v", "category": ""}, "category"},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "memento_add", Arguments: tc.args})
		if err != nil {
			t.Fatalf("CallTool(memento_add): %v", err)
		}
		if !res.IsError {
			t.Fatalf("memento_add accepted invalid input: %+v", tc.args)
		}
		text := toolText(res)
		if !strings.Contains(text, tc.want) {
			t.Errorf("error must mention %q, got: %s", tc.want, text)
		}
		if strings.Contains(text, "127.0.0.1:9") {
			t.Errorf("validation ran after the store was reached: %s", text)
		}
	}
}

// TestMCPServerObsoleteFailsOnBrokenConfig pins that a config load failure
// cannot silently disable the secret guard: the guard policy lives in the
// config, so an unloadable policy must fail the write loudly — never proceed
// with an unscanned reason into the plaintext store.
func TestMCPServerObsoleteFailsOnBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(conf, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := connectTestServer(t, "MEMENTO_CONF="+conf)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "memento_obsolete",
		Arguments: map[string]any{"key": "k", "reason": "the file was deleted"},
	})
	if err != nil {
		t.Fatalf("CallTool(memento_obsolete): %v", err)
	}
	if !res.IsError {
		t.Fatal("obsolete wrote with an unloadable guard policy")
	}
	text := toolText(res)
	if !strings.Contains(text, "memento_obsolete") {
		t.Errorf("error must name the tool: %s", text)
	}
	if strings.Contains(text, "127.0.0.1:9") {
		t.Errorf("the write reached store contact despite the broken config: %s", text)
	}
}

// TestMCPGetDocMissingIsFoundFalse pins the tool's contract: a document that
// does not exist is an empty result (found:false), not a store-failure report.
// An agent that is taught "the store is down" by a missing doc starts
// reporting phantom outages.
func TestMCPGetDocMissingIsFoundFalse(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK) // the facts index "exists"
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"type":"document_missing_exception","reason":"Document with id abcd not found.","index":"memento-docs"},"status":404}`)
	}))
	t.Cleanup(fake.Close)

	session := connectTestServer(t, "OPENSEARCH_URL="+fake.URL)
	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "memento_get_doc",
		Arguments: map[string]any{"hash": "abcd1234abcd1234"},
	})
	if err != nil {
		t.Fatalf("CallTool(memento_get_doc): %v", err)
	}
	if res.IsError {
		t.Fatalf("missing doc must be found:false, got tool error: %s", toolText(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"found":false`) {
		t.Fatalf("want found:false, got: %s", raw)
	}
}

func TestResolveStatus(t *testing.T) {
	cases := map[string]string{
		"":         "verified", // default: only trusted facts
		"any":      "",         // explicit: no filter
		"obsolete": "obsolete",
		"VERIFIED": "verified", // stored statuses are lowercase: canonicalize
		"Obsolete": "obsolete",
		"Any":      "",
		"  ":       "verified", // whitespace-only means "no filter given"
	}
	for in, want := range cases {
		got, err := resolveStatus(in)
		if err != nil {
			t.Errorf("resolveStatus(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("resolveStatus(%q) = %q, want %q", in, got, want)
		}
	}
	// An unknown spelling must be a loud error, not a silent zero-match filter.
	if _, err := resolveStatus("bogus"); err == nil {
		t.Error("resolveStatus(bogus): want error, got nil")
	}
}

// toolText flattens the text content of a tool result for assertions.
func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestMCPServerAddSecretGuard pins the write-time guard rail end to end. The
// store is unreachable on purpose: a rejected write must be rejected by the
// guard before any store contact, and the guard's message must carry masked
// previews only (the whole point is not to echo the secret into the agent's
// context).
func TestMCPServerAddSecretGuard(t *testing.T) {
	const sentinel = "AKIAIOSFODNN7EXAMPLE" // the AWS documentation example key
	storeDown := "OPENSEARCH_URL=https://127.0.0.1:9"
	secretArgs := map[string]any{
		"key":      "guard:test",
		"value":    "the admin key is " + sentinel,
		"category": "config",
	}

	t.Run("reject blocks before the store", func(t *testing.T) {
		session := connectTestServer(t, "MEMENTO_SECRET_GUARD=reject", storeDown)
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "memento_add", Arguments: secretArgs})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("reject mode stored credential-like text")
		}
		text := toolText(res)
		if !strings.Contains(text, "secret-guard") {
			t.Errorf("want a secret-guard error, got: %s", text)
		}
		if strings.Contains(text, sentinel) {
			t.Errorf("guard message echoes the secret: %s", text)
		}
	})

	t.Run("confirm does not bypass reject", func(t *testing.T) {
		session := connectTestServer(t, "MEMENTO_SECRET_GUARD=reject", storeDown)
		args := map[string]any{"confirm_secret": true}
		for k, v := range secretArgs {
			args[k] = v
		}
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "memento_add", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError || !strings.Contains(toolText(res), "secret-guard") {
			t.Errorf("reject must ignore confirm_secret; got: %s", toolText(res))
		}
	})

	t.Run("ask needs the confirmation flag", func(t *testing.T) {
		session := connectTestServer(t, "MEMENTO_SECRET_GUARD=ask", storeDown)
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "memento_add", Arguments: secretArgs})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("ask mode stored credential-like text without confirmation")
		}
		text := toolText(res)
		if !strings.Contains(text, "confirm_secret") {
			t.Errorf("ask message must name the confirmation parameter: %s", text)
		}
		if strings.Contains(text, sentinel) {
			t.Errorf("guard message echoes the secret: %s", text)
		}
	})

	t.Run("ask with confirmation reaches the store", func(t *testing.T) {
		session := connectTestServer(t, "MEMENTO_SECRET_GUARD=ask", storeDown)
		args := map[string]any{"confirm_secret": true}
		for k, v := range secretArgs {
			args[k] = v
		}
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "memento_add", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatal("the store is unreachable; a success means the test wired the env wrong")
		}
		text := toolText(res)
		if strings.Contains(text, "secret-guard") {
			t.Errorf("confirmed ask must pass the guard; got: %s", text)
		}
		if !strings.Contains(text, "memento_add failed") {
			t.Errorf("want the store error after the guard passed; got: %s", text)
		}
	})
}

// TestNewFactViewCarriesTimeAndHistory pins the MCP contract: the view exposes
// the observation time and the staleness signal; history ships only when
// include_history is set (default off: state keys get corrected daily and every
// past observation bloats the agent's context). A zero timestamp (facts created
// before observed_at existed) must map to "".
func TestNewFactViewCarriesTimeAndHistory(t *testing.T) {
	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := model.Fact{
		Key: "k", Value: "v", Category: "config", Project: "p",
		Status:     "obsolete",
		Source:     "experiment",
		Notes:      "note",
		ObservedAt: observed,
		History: []model.HistoryEntry{
			{Value: "old", ObservedAt: observed.Add(-time.Hour), Source: "tool:read_file"},
			{Value: "older", Source: "doc:README"}, // zero time → ""
		},
	}

	v := newFactView(f, true)
	if v.ObservedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("ObservedAt = %q, want RFC3339", v.ObservedAt)
	}
	if v.Status != "obsolete" || v.Notes != "note" {
		t.Errorf("status/notes not carried: %+v", v)
	}
	if len(v.History) != 2 {
		t.Fatalf("history len = %d, want 2", len(v.History))
	}
	if v.History[0].Value != "old" || v.History[0].ObservedAt != "2026-01-02T02:04:05Z" ||
		v.History[0].Source != "tool:read_file" {
		t.Errorf("history[0] = %+v", v.History[0])
	}
	if v.History[1].ObservedAt != "" {
		t.Errorf("zero observed_at must map to empty string, got %q", v.History[1].ObservedAt)
	}

	off := newFactView(f, false)
	if len(off.History) != 0 {
		t.Errorf("history must be stripped by default, got %v", off.History)
	}
	if off.Value != "v" {
		t.Errorf("stripping history must not strip the current value, got %q", off.Value)
	}

	// A config observation from 2026-01-02 is far past the 14-day TTL by any
	// plausible test date, so the staleness signal must be on.
	if v.AgeDays <= model.TTLDays("config") {
		t.Errorf("AgeDays = %d, want past the config TTL", v.AgeDays)
	}
	if !v.Stale {
		t.Error("Stale = false for a long-past-TTL fact; the agent would trust it blindly")
	}

	// Fresh facts must not raise the signal.
	now := time.Now().UTC()
	fresh := model.Fact{Key: "k", Value: "v", Category: "location", ObservedAt: now}
	fv := newFactView(fresh, false)
	if fv.AgeDays != 0 || fv.Stale {
		t.Errorf("fresh fact: age_days = %d, stale = %v, want 0/false", fv.AgeDays, fv.Stale)
	}

	// Pre-observed_at facts: age is unknown (-1) and TTL'd categories are stale.
	empty := newFactView(model.Fact{Key: "k", Value: "v"}, false)
	if empty.ObservedAt != "" {
		t.Errorf("zero ObservedAt must map to empty string, got %q", empty.ObservedAt)
	}
	if len(empty.History) != 0 {
		t.Errorf("empty history must stay empty, got %v", empty.History)
	}
	if empty.AgeDays != -1 {
		t.Errorf("zero observed_at: age_days = %d, want -1", empty.AgeDays)
	}
	if empty.Stale {
		t.Error("category with no TTL must not be stale when observed_at is unknown")
	}
	legacyConfig := newFactView(model.Fact{Key: "k", Value: "v", Category: "config"}, false)
	if !legacyConfig.Stale {
		t.Error("unknown observation time must be stale for TTL'd categories")
	}
}

// TestMCPKeysEmptyQuery pins the memento_keys guard: an empty key query is a
// context flood, rejected before the store is touched. The store is
// unreachable on purpose: the rejection must not depend on it.
func TestMCPKeysEmptyQuery(t *testing.T) {
	session := connectTestServer(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "memento_keys",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "prefix") {
		t.Errorf("empty keys query must be rejected with a prefix hint; got: %s", toolText(res))
	}
}
