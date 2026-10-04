package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpServerVersion is reported to MCP clients as the server version.
const mcpServerVersion = "0.1.0"

var mcpFlags struct {
	conf string
}

// McpCmd returns the `memento mcp` command: an MCP server speaking over stdio.
func McpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run as an MCP server over stdio (for agents: VS Code and others)",
		Long: `Run as an MCP server over stdio.

The server exposes the facts store to MCP clients (VS Code, agents) as tools.
It speaks JSON-RPC on stdin/stdout, so it is normally started by the client,
not by hand. Diagnostics go to stderr.

Config resolution: --conf flag, then MEMENTO_CONF, then the default lookup chain
(./memento.conf, next to the binary, ~/.memento.conf).`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if err := runMCPServer(mcpFlags.conf); err != nil {
				fmt.Fprintln(os.Stderr, "memento mcp:", err)
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&mcpFlags.conf, "conf", "",
		"config file to use (overrides MEMENTO_CONF and the default lookup)")
	return cmd
}

// mcpServer holds the state shared by all tool handlers.
//
// The config is loaded lazily on every call on purpose: the server must start
// even when OpenSearch is unreachable or the config file is broken. A server
// that dies on startup is invisible to the client, so it cannot even report
// the problem. Instead, every tool describes the failure in actionable terms.
type mcpServer struct {
	confPath string
}

// runMCPServer builds the server and serves it over stdio until the client
// disconnects.
func runMCPServer(confPath string) error {
	s := &mcpServer{confPath: confPath}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "memento",
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{
		Instructions: "Verified facts about the user's projects (file locations, " +
			"config and endpoint values, behavior). Consult facts before " +
			"re-verifying something the user may have already confirmed, and store " +
			"newly verified facts so they are not re-derived. Never store secrets " +
			"(passwords, tokens) — the store is plaintext. Call memento_status first " +
			"if you are unsure whether the store is available.",
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_status",
		Description: "Check whether the facts store (OpenSearch) is reachable and " +
			"report the effective configuration, plus storage state: cluster and " +
			"index health, disk usage of the busiest data node, and whether the index " +
			"is read-only. Never fails: returns ok=false plus an explanation when the " +
			"store is unavailable, so you can tell the user what to fix instead of " +
			"retrying.",
	}, s.status)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_search",
		Description: "Search stored facts by text. Looks in fact values and notes " +
			"only — it does NOT match keys, so use memento_get for an exact key. " +
			"By default returns only trusted ('verified') facts; obsolete facts are " +
			"superseded and must not be treated as current.",
	}, s.search)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_get",
		Description: "Get one fact by its exact key. Returns found=false when there " +
			"is no such fact (that is not an error).",
	}, s.get)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_add",
		Description: "Store a verified fact: file locations, config and endpoint " +
			"values, behavior. Re-use the same key to correct a fact — the old " +
			"value moves to history. Re-adding the same value updates notes only, " +
			"without adding a history entry. Never store secrets (passwords, " +
			"tokens, keys): the store is plaintext.",
	}, s.add)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_list",
		Description: "List the facts stored for a project. Defaults to trusted " +
			"('verified') facts only.",
	}, s.list)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_obsolete",
		Description: "Mark a fact obsolete because its subject is gone (file " +
			"deleted, endpoint removed). Use memento_add instead when the value " +
			"merely changed — corrections move the old value to history. The value " +
			"is kept for reference; the reason is appended to notes.",
	}, s.obsolete)

	err := srv.Run(context.Background(), &mcp.StdioTransport{})
	if errors.Is(err, io.EOF) {
		// Normal shutdown: the client closed the connection.
		return nil
	}
	return err
}

// statusInput is empty: memento_status takes no arguments.
type statusInput struct{}

// status reports connectivity and effective configuration. It is deliberately
// error-free: it returns ok=false instead of failing, so an agent can relay the
// problem to the user in words. The payload is shared with `memento status`.
func (s *mcpServer) status(ctx context.Context, req *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, statusReport, error) {
	return nil, collectStatus(ctx, s.confPath), nil
}

// ── store tools ──────────────────────────────────────────────────────────────

// historyView is one past observation in the MCP-facing shape.
type historyView struct {
	Value      string `json:"value" jsonschema:"the earlier observation"`
	ObservedAt string `json:"observed_at" jsonschema:"RFC3339 when this observation was seen"`
	Source     string `json:"source" jsonschema:"where this observation came from"`
}

// factView is the MCP-facing shape of a fact: a stable contract for clients
// (RFC3339 timestamps, no internal fields) rather than the internal model.
type factView struct {
	Key        string        `json:"key" jsonschema:"stable fact key, e.g. proj:foo:loc:bar.py"`
	Value      string        `json:"value" jsonschema:"the fact itself"`
	Category   string        `json:"category" jsonschema:"location | config | endpoint | behavior | context_map"`
	Project    string        `json:"project" jsonschema:"project name, or 'global'"`
	Status     string        `json:"status" jsonschema:"verified | obsolete"`
	Source     string        `json:"source" jsonschema:"where it came from, e.g. tool:read_file, doc:README, user:confirmed"`
	Notes      string        `json:"notes,omitempty" jsonschema:"qualifiers: what was checked, how far, caveats"`
	ObservedAt string        `json:"observed_at" jsonschema:"RFC3339 when the current value was observed"`
	History    []historyView `json:"history,omitempty" jsonschema:"previous observations, oldest first"`
	CreatedAt  string        `json:"created_at" jsonschema:"RFC3339 creation time"`
	UpdatedAt  string        `json:"updated_at" jsonschema:"RFC3339 last update time"`
}

// rfc3339OrEmpty formats t for the MCP contract, or returns "" when t is the
// zero time — facts created before observed_at existed carry no date.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func newFactView(f model.Fact) factView {
	history := make([]historyView, 0, len(f.History))
	for _, h := range f.History {
		history = append(history, historyView{
			Value:      h.Value,
			ObservedAt: rfc3339OrEmpty(h.ObservedAt),
			Source:     h.Source,
		})
	}
	return factView{
		Key:        f.Key,
		Value:      f.Value,
		Category:   f.Category,
		Project:    f.Project,
		Status:     f.Status,
		Source:     f.Source,
		Notes:      f.Notes,
		ObservedAt: rfc3339OrEmpty(f.ObservedAt),
		History:    history,
		CreatedAt:  f.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  f.UpdatedAt.Format(time.RFC3339),
	}
}

func newFactViews(facts []model.Fact) []factView {
	views := make([]factView, 0, len(facts))
	for _, f := range facts {
		views = append(views, newFactView(f))
	}
	return views
}

// factsOutput is the list-shaped result shared by memento_search and memento_list.
type factsOutput struct {
	Count int        `json:"count" jsonschema:"number of facts returned"`
	Facts []factView `json:"facts" jsonschema:"matching facts, newest first is not guaranteed"`
}

// newClient returns a client for the configured store. Config is reloaded on
// every call so that a broken file or an unreachable server cannot kill the
// server process (see mcpServer). The index is ensured like the CLI does.
func (s *mcpServer) newClient(ctx context.Context) (*client.Client, error) {
	cfg, err := config.LoadConfigFrom(s.confPath)
	if err != nil {
		return nil, err
	}
	c, err := client.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.EnsureIndex(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// storeError marks a failure as an availability problem rather than an empty
// result, so an agent reports it instead of treating "nothing" as the answer.
func storeError(op string, err error) error {
	return fmt.Errorf("%s failed: %w. If the facts store is unreachable, call "+
		"memento_status and tell the user; do not retry in a loop", op, err)
}

// resolveStatus maps the status input to a filter. Empty means "trusted facts
// only" — obsolete facts are superseded and must not be mistaken for current;
// "any" disables the filter.
func resolveStatus(status string) string {
	switch status {
	case "":
		return "verified"
	case "any":
		return ""
	default:
		return status
	}
}

type searchInput struct {
	Query    string `json:"query" jsonschema:"text to look for in fact values and notes (keys are not searched)"`
	Project  string `json:"project,omitempty" jsonschema:"limit results to this project"`
	Category string `json:"category,omitempty" jsonschema:"limit results to this category"`
	Status   string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
}

func (s *mcpServer) search(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, factsOutput, error) {
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_search", err)
	}
	facts, err := c.Search(ctx, in.Query, in.Project, in.Category, resolveStatus(in.Status))
	if err != nil {
		return nil, factsOutput{}, storeError("memento_search", err)
	}
	return nil, factsOutput{Count: len(facts), Facts: newFactViews(facts)}, nil
}

type getInput struct {
	Key string `json:"key" jsonschema:"exact fact key"`
}

type getOutput struct {
	Found bool      `json:"found" jsonschema:"false when no fact has this key"`
	Fact  *factView `json:"fact,omitempty" jsonschema:"the fact, when found"`
}

func (s *mcpServer) get(ctx context.Context, req *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, getOutput{}, storeError("memento_get", err)
	}
	f, err := c.Get(ctx, in.Key)
	if errors.Is(err, client.ErrNotFound) {
		// Not an error: "there is no such fact" is a legitimate answer.
		return nil, getOutput{Found: false}, nil
	}
	if err != nil {
		return nil, getOutput{}, storeError("memento_get", err)
	}
	v := newFactView(*f)
	return nil, getOutput{Found: true, Fact: &v}, nil
}

type addInput struct {
	Key      string `json:"key" jsonschema:"stable fact key, e.g. proj:foo:loc:bar.py — re-use the same key to update"`
	Value    string `json:"value" jsonschema:"the fact text"`
	Category string `json:"category" jsonschema:"location | config | endpoint | behavior | context_map"`
	Project  string `json:"project,omitempty" jsonschema:"project name; defaults to 'global'"`
	Source   string `json:"source,omitempty" jsonschema:"where the fact came from (tool:read_file, doc:README, user:confirmed); defaults to tool:mcp"`
	Notes    string `json:"notes,omitempty" jsonschema:"qualifiers: what was checked, how far, caveats — carried to later sessions"`
}

type addOutput struct {
	Key     string `json:"key" jsonschema:"the stored key"`
	Created bool   `json:"created" jsonschema:"true when the fact was newly created, false when an existing fact was updated"`
	Status  string `json:"status" jsonschema:"status of the stored fact"`
}

func (s *mcpServer) add(ctx context.Context, req *mcp.CallToolRequest, in addInput) (*mcp.CallToolResult, addOutput, error) {
	if in.Project == "" {
		in.Project = "global"
	}
	if in.Source == "" {
		in.Source = "tool:mcp"
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, addOutput{}, storeError("memento_add", err)
	}
	f := model.NewFact(in.Key, in.Value, in.Category, in.Project, in.Source, in.Notes)
	created, err := c.Add(ctx, f)
	if err != nil {
		return nil, addOutput{}, storeError("memento_add", err)
	}
	return nil, addOutput{Key: in.Key, Created: created, Status: f.Status}, nil
}

type listInput struct {
	Project string `json:"project" jsonschema:"project name to list facts for"`
	Status  string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
}

func (s *mcpServer) list(ctx context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, factsOutput, error) {
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_list", err)
	}
	facts, err := c.List(ctx, in.Project)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_list", err)
	}
	// List has no server-side status filter, so apply it here to keep the
	// "trusted only by default" guarantee.
	if status := resolveStatus(in.Status); status != "" {
		kept := facts[:0]
		for _, f := range facts {
			if f.Status == status {
				kept = append(kept, f)
			}
		}
		facts = kept
	}
	return nil, factsOutput{Count: len(facts), Facts: newFactViews(facts)}, nil
}

type obsoleteInput struct {
	Key    string `json:"key" jsonschema:"key of the fact whose subject is gone"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is gone (file deleted, endpoint removed); appended to notes"`
}

type obsoleteOutput struct {
	Key    string `json:"key" jsonschema:"the key that was marked obsolete"`
	Status string `json:"status" jsonschema:"the new status"`
}

func (s *mcpServer) obsolete(ctx context.Context, req *mcp.CallToolRequest, in obsoleteInput) (*mcp.CallToolResult, obsoleteOutput, error) {
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, obsoleteOutput{}, storeError("memento_obsolete", err)
	}
	err = c.Obsolete(ctx, in.Key, in.Reason)
	if errors.Is(err, client.ErrNotFound) {
		return nil, obsoleteOutput{}, fmt.Errorf("no fact with key %q: nothing to mark obsolete", in.Key)
	}
	if err != nil {
		return nil, obsoleteOutput{}, storeError("memento_obsolete", err)
	}
	return nil, obsoleteOutput{Key: in.Key, Status: "obsolete"}, nil
}
