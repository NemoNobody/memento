package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverEnvVar makes the test binary act as the MCP server child process.
const serverEnvVar = "MEMENTO_TEST_MCP_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(serverEnvVar) == "1" {
		// Child process: serve MCP over stdio until the client disconnects.
		if err := runMCPServer(""); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
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
	cmd := exec.Command(exe)
	cmd.Env = append(childEnv(extraEnv), serverEnvVar+"=1")

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
	"memento_add", "memento_list", "memento_prune", "memento_obsolete",
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

func TestResolveStatus(t *testing.T) {
	cases := map[string]string{
		"":         "verified", // default: only trusted facts
		"any":      "",         // explicit: no filter
		"obsolete": "obsolete",
	}
	for in, want := range cases {
		if got := resolveStatus(in); got != want {
			t.Errorf("resolveStatus(%q) = %q, want %q", in, got, want)
		}
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
