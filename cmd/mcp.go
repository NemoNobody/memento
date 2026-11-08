package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
	"github.com/NemoNobody/memento/internal/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

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

Config resolution: --conf flag, then MEMENTO_CONF, then next to the binary,
then ~/.memento.conf. The memento.conf in the working directory is ignored on
purpose: the editor starts this server from the cwd of an arbitrary project,
and a cloned repository's config must not hijack the store endpoint.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if err := runMCPServer(mcpFlags.conf); err != nil {
				_, _ = io.WriteString(os.Stderr, "memento mcp: "+err.Error()+"\n")
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
		Version: version.Version,
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
			"superseded and must not be treated as current. History is omitted " +
			"unless include_history=true. Answers carry age_days/stale: a stale fact " +
			"must be re-verified against the source before being trusted.",
	}, s.search)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_get",
		Description: "Get one fact by its exact key. Returns found=false when there " +
			"is no such fact (that is not an error). History is omitted unless " +
			"include_history=true. Answers carry age_days/stale: a stale fact must be " +
			"re-verified against the source before being trusted.",
	}, s.get)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_add",
		Description: "Store a verified fact: file locations, config and endpoint " +
			"values, behavior. Re-use the same key to correct a fact — the old " +
			"value moves to history. Re-adding the same value updates notes only, " +
			"without adding a history entry. Never store secrets (passwords, " +
			"tokens, keys): the store is plaintext and a secret_guard rejects " +
			"credential-like text on writes.",
	}, s.add)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_list",
		Description: "List the facts stored for a project. Defaults to trusted " +
			"('verified') facts only. prefix keeps only keys starting with it " +
			"(trailing * allowed; keys conventionally start with the project " +
			"name, e.g. prefix '<project>:state:'). History is omitted unless " +
			"include_history=true.",
	}, s.list)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_keys",
		Description: "Find facts by key across all projects: prefix (key start, " +
			"e.g. 'proj:foo:loc:') and/or match (key substring; * and ? wildcards). " +
			"At least one is required; results are sorted by key. The key-level " +
			"navigation tool — use it to scope before memento_list/get; project is " +
			"an optional extra filter. Defaults to trusted ('verified') facts only.",
	}, s.keys)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_prune",
		Description: "Drop old history entries under a key, keeping the newest " +
			"'keep' (the current value is never touched). DESTRUCTIVE: dropped " +
			"observations are gone for good. Without confirm=true nothing is " +
			"written — the call returns the plan. Show the plan to the user and " +
			"re-call with confirm=true only after their explicit approval.",
	}, s.prune)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_obsolete",
		Description: "Mark a fact obsolete because its subject is gone (file " +
			"deleted, endpoint removed). Use memento_add instead when the value " +
			"merely changed — corrections move the old value to history. The value " +
			"is kept for reference; the reason is appended to notes.",
	}, s.obsolete)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_fetch_doc",
		Description: "Fetch a web page, store its full snapshot in the docs index, " +
			"and return the outline (hash, title, sections). A second fetch of the " +
			"same URL returns the stored snapshot (cached=true) until its TTL " +
			"expires; refresh=true forces a new download. Section text is read with " +
			"memento_get_doc; the raw page is never returned (context economy). " +
			"Requests send no cookies and no credentials; a host outside " +
			"fetch_allow_hosts is rejected before the request. GitHub URLs that " +
			"match a local clone resolve to a local path (local=true) with no " +
			"network and no store write.",
	}, s.fetchDoc)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_get_doc",
		Description: "Read a stored document snapshot by hash: the outline by " +
			"default, section=N for one section's text, full=true for every section " +
			"(prefer one section: full bloats the context). Never touches the " +
			"network: stale=true means the snapshot is past its TTL — refresh it " +
			"with memento_fetch_doc before quoting.",
	}, s.getDoc)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "memento_search_docs",
		Description: "BM25 search over stored page snapshots (section text only; " +
			"the raw html is write-only and never searched). Returns hash+url+title+" +
			"snippet per doc; pull full text with memento_get_doc. Complements " +
			"memento_search, which searches verified facts, not pages.",
	}, s.searchDocs)

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
// problem to the user in words. The payload is shared with `memento status`,
// but resolved through the MCP NoCWD chain: the report must describe the
// config the store tools actually use, never a cwd file the tools ignore.
func (s *mcpServer) status(ctx context.Context, req *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, statusReport, error) {
	return nil, collectStatus(ctx, s.confPath, true), nil
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
	AgeDays    int           `json:"age_days" jsonschema:"whole days since the observation; -1 when observed_at is unknown"`
	Stale      bool          `json:"stale" jsonschema:"true when age passes the category TTL (config/endpoint 14d, location/behavior 30d, context_map never) — re-verify against the source before trusting"`
	History    []historyView `json:"history,omitempty" jsonschema:"previous observations, oldest first; present only when include_history=true"`
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

// newFactView renders a fact for the MCP contract. History ships only when
// asked for: corrections are frequent (state keys get re-observed daily) and
// shipping every past observation bloats the agent's context on every read.
// Staleness is computed at read time — the store provides the evidence
// (observed_at), the signal (stale/age_days) tells the agent to re-verify.
func newFactView(f model.Fact, includeHistory bool) factView {
	v := factView{
		Key:        f.Key,
		Value:      f.Value,
		Category:   f.Category,
		Project:    f.Project,
		Status:     f.Status,
		Source:     f.Source,
		Notes:      f.Notes,
		ObservedAt: rfc3339OrEmpty(f.ObservedAt),
		AgeDays:    f.AgeDays(time.Now().UTC()),
		Stale:      f.Stale(time.Now().UTC()),
		CreatedAt:  rfc3339OrEmpty(f.CreatedAt),
		UpdatedAt:  rfc3339OrEmpty(f.UpdatedAt),
	}
	if includeHistory {
		history := make([]historyView, 0, len(f.History))
		for _, h := range f.History {
			history = append(history, historyView{
				Value:      h.Value,
				ObservedAt: rfc3339OrEmpty(h.ObservedAt),
				Source:     h.Source,
			})
		}
		v.History = history
	}
	return v
}

func newFactViews(facts []model.Fact, includeHistory bool) []factView {
	views := make([]factView, 0, len(facts))
	for _, f := range facts {
		views = append(views, newFactView(f, includeHistory))
	}
	return views
}

// factsOutput is the list-shaped result shared by memento_search and memento_list.
type factsOutput struct {
	Count int        `json:"count" jsonschema:"number of facts returned"`
	Facts []factView `json:"facts" jsonschema:"matching facts, newest first is not guaranteed"`
	// Truncated is the honest page signal: the store matched more than the
	// page delivered. For list/keys the page is the navigation cap (1000),
	// for search it is the requested limit — the advice differs, and an
	// agent told to "narrow the query" when the limit cut the output would
	// answer from a subset it cannot see.
	Truncated bool `json:"truncated,omitempty" jsonschema:"true when the store matched more facts than this page (the requested limit for search; the navigation cap 1000 for list/keys): for search raise the limit (max 50) before narrowing, for list/keys narrow the query (prefix/project/status); absent means complete"`
	Skipped   int  `json:"skipped,omitempty" jsonschema:"hits the store returned whose content failed to decode (index corruption): report to the user, never silently drop"`
}

// loadConfig resolves the config for a tool call: the explicit --conf wins;
// without one the working directory is ignored (the editor opens MCP with
// the cwd of an arbitrary, possibly untrusted, project — its memento.conf
// must not hijack the endpoint).
func (s *mcpServer) loadConfig() (*config.Config, error) {
	if s.confPath != "" {
		return config.LoadConfigFrom(s.confPath)
	}
	return config.LoadConfigNoCWD()
}

// newClient returns a client for the configured store. Config is reloaded on
// every call so that a broken file or an unreachable server cannot kill the
// server process (see mcpServer). The index is ensured like the CLI does.
func (s *mcpServer) newClient(ctx context.Context) (*client.Client, error) {
	c, _, err := s.newClientWithConfig(ctx)
	return c, err
}

// clientFromConfig builds the client for an already-loaded config, so a tool
// that needs config values (the guard, prune's default keep) loads the config
// once: the keyring behind a locked wallet is prompted at most once per call.
func (s *mcpServer) clientFromConfig(ctx context.Context, cfg *config.Config) (*client.Client, error) {
	c, err := client.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.EnsureIndex(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// newClientWithConfig is newClient plus the effective config, for tools that
// need config values beyond the connection (memento_prune's default keep).
func (s *mcpServer) newClientWithConfig(ctx context.Context) (*client.Client, *config.Config, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	c, err := s.clientFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return c, cfg, nil
}

// storeError marks a failure as an availability problem rather than an empty
// result, so an agent reports it instead of treating "nothing" as the answer.
func storeError(op string, err error) error {
	return fmt.Errorf("%s failed: %w. If the facts store is unreachable, call "+
		"memento_status and tell the user; do not retry in a loop", op, err)
}

// resolveStatus maps the status input to a filter. Empty means "trusted facts
// only" — obsolete facts are superseded and must not be mistaken for current;
// "any" disables the filter. Statuses are stored lowercase, so the input is
// canonicalized: a case-sensitive miss would silently return zero matches.
func resolveStatus(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "":
		return "verified"
	case "any":
		return ""
	default:
		return s
	}
}

type searchInput struct {
	Query          string `json:"query" jsonschema:"text to look for in fact values and notes (keys are not searched)"`
	Project        string `json:"project,omitempty" jsonschema:"limit results to this project"`
	Category       string `json:"category,omitempty" jsonschema:"limit results to this category"`
	Status         string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
	Limit          int    `json:"limit,omitempty" jsonschema:"max hits to return (default 10, cap 50): the context budget for search results"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only)"`
}

func (s *mcpServer) search(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, factsOutput, error) {
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_search", err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	// Write edges store the lowercase canonical category; the term filter is
	// case-sensitive, so canonicalize the filter too.
	in.Category = strings.ToLower(in.Category)
	facts, meta, err := c.Search(ctx, in.Query, in.Project, in.Category, resolveStatus(in.Status), limit)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_search", err)
	}
	return nil, factsOutput{
		Count:     len(facts),
		Truncated: meta.Total > len(facts),
		Skipped:   meta.Skipped,
		Facts:     newFactViews(facts, in.IncludeHistory),
	}, nil
}

type getInput struct {
	Key            string `json:"key" jsonschema:"exact fact key"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only)"`
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
	v := newFactView(*f, in.IncludeHistory)
	return nil, getOutput{Found: true, Fact: &v}, nil
}

type addInput struct {
	Key      string `json:"key" jsonschema:"stable fact key, e.g. proj:foo:loc:bar.py — re-use the same key to update"`
	Value    string `json:"value" jsonschema:"the fact text"`
	Category string `json:"category" jsonschema:"location | config | endpoint | behavior | context_map"`
	Project  string `json:"project,omitempty" jsonschema:"project name; defaults to 'global'"`
	Source   string `json:"source,omitempty" jsonschema:"where the fact came from (tool:read_file, doc:README, user:confirmed); defaults to tool:mcp"`
	Notes    string `json:"notes,omitempty" jsonschema:"qualifiers: what was checked, how far, caveats — carried to later sessions"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask. It is the agent's
	// barrier: show the masked finding to the user first, set this only after
	// their explicit approval. At reject it has no effect.
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type addOutput struct {
	Key     string `json:"key" jsonschema:"the stored key"`
	Created bool   `json:"created" jsonschema:"true when the fact was newly created, false when an existing fact was updated"`
	Status  string `json:"status" jsonschema:"status of the stored fact"`
}

func (s *mcpServer) add(ctx context.Context, req *mcp.CallToolRequest, in addInput) (*mcp.CallToolResult, addOutput, error) {
	// Validate the shape before guard and store: an empty key produces a
	// cryptic store error, and an unknown category never ages (silent rot).
	if strings.TrimSpace(in.Key) == "" {
		return nil, addOutput{}, errors.New("memento_add: key is required")
	}
	if strings.TrimSpace(in.Value) == "" {
		return nil, addOutput{}, errors.New("memento_add: value is required")
	}
	if !model.ValidCategory(strings.ToLower(in.Category)) {
		return nil, addOutput{}, fmt.Errorf("memento_add: unknown category %q (want location | config | endpoint | behavior | context_map)", in.Category)
	}
	// Canonicalize for storage: the TTL table and the search category filter
	// are case-sensitive, so a mixed-case write would silently never age and
	// never match category filters.
	in.Category = strings.ToLower(in.Category)
	if in.Project == "" {
		in.Project = "global"
	}
	if in.Source == "" {
		in.Source = "tool:mcp"
	}
	// The guard runs before any store contact: a rejected write must be
	// rejected even when OpenSearch is down. The config is loaded once and
	// reused by the client (a locked wallet is not prompted twice per write).
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, addOutput{}, storeError("memento_add", cerr)
	}
	if err := guardCheck(cfg.SecretGuard, in.ConfirmSecret,
		guardField{"key", in.Key},
		guardField{"value", in.Value},
		guardField{"notes", in.Notes},
		guardField{"source", in.Source}); err != nil {
		return nil, addOutput{}, err
	}
	c, err := s.clientFromConfig(ctx, cfg)
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
	Project        string `json:"project" jsonschema:"project name to list facts for"`
	Prefix         string `json:"prefix,omitempty" jsonschema:"keep only keys starting with this prefix (trailing * allowed), e.g. '<project>:state:'"`
	Status         string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only)"`
}

func (s *mcpServer) list(ctx context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, factsOutput, error) {
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_list", err)
	}
	facts, meta, err := c.List(ctx, in.Project, in.Prefix)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_list", err)
	}
	// The truncation signal is the cluster-side total: the status filter
	// below shrinks the page, and truncation must survive the filtering.
	// rawLen >= cap used to fire on an exactly-full page (a complete result).
	rawLen := meta.Total
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
	return nil, factsOutput{Count: len(facts), Truncated: rawLen > client.NavSizeCap, Skipped: meta.Skipped, Facts: newFactViews(facts, in.IncludeHistory)}, nil
}

type keysInput struct {
	Prefix         string `json:"prefix,omitempty" jsonschema:"key prefix, e.g. 'proj:foo:loc:'; trailing * allowed"`
	Match          string `json:"match,omitempty" jsonschema:"key substring; * and ? work as wildcards"`
	Project        string `json:"project,omitempty" jsonschema:"limit to this project (default: all projects)"`
	Status         string `json:"status,omitempty" jsonschema:"limit results to this status: verified (default), obsolete, or any"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"include the full history chain (default: current value only)"`
}

// keys is the key-level navigation tool: prefix/match on the key field, with
// project as an optional filter. The empty-query guard lives in the client so
// CLI and MCP share one contract.
func (s *mcpServer) keys(ctx context.Context, req *mcp.CallToolRequest, in keysInput) (*mcp.CallToolResult, factsOutput, error) {
	// Validate the query shape before any store contact: a bad query is the
	// caller's problem, not an availability problem.
	if in.Prefix == "" && in.Match == "" {
		return nil, factsOutput{}, fmt.Errorf("memento_keys: provide a key prefix and/or a match pattern")
	}
	if _, err := client.NormalizePrefix(in.Prefix); err != nil {
		return nil, factsOutput{}, fmt.Errorf("memento_keys: %w", err)
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, factsOutput{}, storeError("memento_keys", err)
	}
	facts, meta, err := c.Keys(ctx, in.Prefix, in.Match, in.Project, resolveStatus(in.Status))
	if err != nil {
		return nil, factsOutput{}, storeError("memento_keys", err)
	}
	return nil, factsOutput{Count: len(facts), Truncated: meta.Total > client.NavSizeCap, Skipped: meta.Skipped, Facts: newFactViews(facts, in.IncludeHistory)}, nil
}

type obsoleteInput struct {
	Key    string `json:"key" jsonschema:"key of the fact whose subject is gone"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is gone (file deleted, endpoint removed); appended to notes"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask (see addInput).
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type obsoleteOutput struct {
	Key    string `json:"key" jsonschema:"the key that was marked obsolete"`
	Status string `json:"status" jsonschema:"the new status"`
}

func (s *mcpServer) obsolete(ctx context.Context, req *mcp.CallToolRequest, in obsoleteInput) (*mcp.CallToolResult, obsoleteOutput, error) {
	// The config is the guard's policy: a load failure must fail the write
	// (as in add), never silently skip the scan of a credential-like reason.
	// Loaded once and reused: a locked wallet is not prompted twice per write.
	cfg, cerr := s.loadConfig()
	if cerr != nil {
		return nil, obsoleteOutput{}, storeError("memento_obsolete", cerr)
	}
	if err := guardCheck(cfg.SecretGuard, in.ConfirmSecret,
		guardField{"reason", in.Reason}); err != nil {
		return nil, obsoleteOutput{}, err
	}
	c, err := s.clientFromConfig(ctx, cfg)
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

type pruneInput struct {
	Key     string `json:"key" jsonschema:"key of the fact whose history to prune"`
	Keep    int    `json:"keep,omitempty" jsonschema:"number of newest history entries to keep; 0 = history_limit from config"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"must be true to write; without it only the plan is returned"`
}

type pruneOutput struct {
	Key          string `json:"key" jsonschema:"the pruned key"`
	HistoryTotal int    `json:"history_total" jsonschema:"history entries before the call"`
	Keep         int    `json:"keep" jsonschema:"history entries that survive"`
	Removed      int    `json:"removed" jsonschema:"entries deleted (0 when nothing was written)"`
	Pruned       bool   `json:"pruned" jsonschema:"true when history was actually rewritten"`
}

// prune is destructive, so it carries the same barrier as the CLI: without
// confirm=true the plan is returned and nothing is written. An agent must
// relay the plan to the user and re-call only after explicit approval.
func (s *mcpServer) prune(ctx context.Context, req *mcp.CallToolRequest, in pruneInput) (*mcp.CallToolResult, pruneOutput, error) {
	c, cfg, err := s.newClientWithConfig(ctx)
	if err != nil {
		return nil, pruneOutput{}, storeError("memento_prune", err)
	}
	keep := in.Keep
	if keep <= 0 {
		keep = cfg.HistoryLimit
	}
	if keep <= 0 {
		// history_limit: 0 means "keep everything": silently collapsing it
		// to 1 would destroy every history entry. Demand an explicit keep.
		return nil, pruneOutput{}, fmt.Errorf("memento_prune: history_limit is 0 (keep everything) and no keep value was given: pass an explicit keep (e.g. keep: 5)")
	}
	f, err := c.Get(ctx, in.Key)
	if errors.Is(err, client.ErrNotFound) {
		return nil, pruneOutput{}, fmt.Errorf("no fact with key %q: nothing to prune", in.Key)
	}
	if err != nil {
		return nil, pruneOutput{}, storeError("memento_prune", err)
	}
	total := len(f.History)
	if total <= keep {
		return nil, pruneOutput{Key: in.Key, HistoryTotal: total, Keep: total, Removed: 0}, nil
	}
	if !in.Confirm {
		// The barrier: describe the plan, write nothing.
		return nil, pruneOutput{}, fmt.Errorf("memento_prune is destructive: key %q has %d history "+
			"entries; keeping the newest %d would drop %d observations for good. Nothing was written. "+
			"Show this plan to the user and re-call with confirm=true only after their approval",
			in.Key, total, keep, total-keep)
	}
	removed, err := c.Prune(ctx, in.Key, keep)
	if err != nil {
		return nil, pruneOutput{}, storeError("memento_prune", err)
	}
	return nil, pruneOutput{Key: in.Key, HistoryTotal: total, Keep: keep, Removed: removed, Pruned: true}, nil
}

// --- page snapshots (docs index) ---

type fetchDocInput struct {
	URL     string `json:"url" jsonschema:"page url to fetch; a missing scheme becomes https, utm_* parameters are stripped"`
	Project string `json:"project,omitempty" jsonschema:"tag the document with this project"`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"force a fresh download even when a stored snapshot exists"`

	// ConfirmSecret bypasses a guard hit at secret_guard=ask, the same
	// contract as memento_add: page text is untrusted and can quote
	// credential-like strings.
	ConfirmSecret bool `json:"confirm_secret,omitempty" jsonschema:"confirm a guard hit is a false positive (secret_guard=ask only); set only after the user approved the exact finding"`
}

type docSectionOutline struct {
	Order   int    `json:"order" jsonschema:"section number for memento_get_doc"`
	Heading string `json:"heading"`
	Chars   int    `json:"chars" jsonschema:"text length in runes"`
}

func newDocOutline(d *model.Doc) []docSectionOutline {
	out := make([]docSectionOutline, 0, len(d.Sections))
	for _, s := range d.Sections {
		out = append(out, docSectionOutline{Order: s.Order, Heading: s.Heading, Chars: len([]rune(s.Text))})
	}
	return out
}

type fetchDocOutput struct {
	Hash      string              `json:"hash,omitempty" jsonschema:"document id for memento_get_doc"`
	URL       string              `json:"url,omitempty"`
	Title     string              `json:"title,omitempty"`
	Project   string              `json:"project,omitempty"`
	FetchedAt string              `json:"fetched_at,omitempty"`
	ExpiresAt string              `json:"expires_at,omitempty"`
	Stale     bool                `json:"stale,omitempty" jsonschema:"past TTL: re-fetch with memento_fetch_doc before quoting"`
	Cached    bool                `json:"cached,omitempty" jsonschema:"true when the stored snapshot was returned without a network request"`
	Truncated bool                `json:"truncated,omitempty" jsonschema:"extraction hit a limit: the stored text is a prefix"`
	Sections  []docSectionOutline `json:"sections,omitempty" jsonschema:"outline: pull a section with memento_get_doc(hash, section)"`

	// GitHub local-clone resolution: the answer is a file on this machine.
	Local bool   `json:"local,omitempty" jsonschema:"true when the url resolved to a file in a local git clone: Path is the file; nothing was fetched"`
	Path  string `json:"path,omitempty" jsonschema:"local clone file path when local=true"`
}

func (s *mcpServer) fetchDoc(ctx context.Context, req *mcp.CallToolRequest, in fetchDocInput) (*mcp.CallToolResult, fetchDocOutput, error) {
	// Validate before any store contact: a bad url, a denied host, a robots
	// verdict is the caller's problem, not an availability problem.
	rawURL, err := client.NormalizeURL(in.URL)
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	cfg, err := s.loadConfig()
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	if err := client.HostAllowed(cfg, rawURL); err != nil {
		return nil, fetchDocOutput{}, err
	}
	if p, ok := client.GitHubLocal(cfg, rawURL); ok {
		// The content is in a local clone: hand out the path. No fetch, no
		// store write: the repo mapping is machine-specific and must never
		// enter the shared index.
		return nil, fetchDocOutput{Local: true, Path: p}, nil
	}
	c, err := client.NewClient(cfg) // FetchDoc ensures the docs index itself
	if err != nil {
		return nil, fetchDocOutput{}, storeError("memento_fetch_doc", err)
	}
	doc, cached, err := c.FetchDoc(ctx, rawURL, in.Project, in.Refresh,
		func(d *model.Doc) error {
			// Guard after extraction, before the store write.
			return guardFetch(d, cfg.SecretGuard, in.ConfirmSecret)
		})
	if err != nil {
		return nil, fetchDocOutput{}, err
	}
	now := time.Now().UTC()
	out := fetchDocOutput{
		Hash:      doc.URLHash,
		URL:       doc.URL,
		Title:     doc.Title,
		Project:   doc.Project,
		FetchedAt: rfc3339OrEmpty(doc.FetchedAt),
		ExpiresAt: rfc3339OrEmpty(doc.ExpiresAt),
		Stale:     doc.Stale(now),
		Cached:    cached,
		Truncated: doc.Truncated,
		Sections:  newDocOutline(doc),
	}
	return nil, out, nil
}

type getDocInput struct {
	Hash string `json:"hash" jsonschema:"document id from memento_fetch_doc/memento_search_docs"`

	// Section is a pointer: 0 is a valid section number, and its absence
	// must mean "outline only".
	Section *int `json:"section,omitempty" jsonschema:"section number to return in full (from the outline); omit for the outline only"`
	Full    bool `json:"full,omitempty" jsonschema:"return every section (bloats the context; prefer one section)"`
}

type getDocOutput struct {
	Found     bool                `json:"found" jsonschema:"false when no stored doc has this hash"`
	Hash      string              `json:"hash,omitempty"`
	URL       string              `json:"url,omitempty"`
	Title     string              `json:"title,omitempty"`
	Stale     bool                `json:"stale,omitempty" jsonschema:"past TTL: refresh with memento_fetch_doc before quoting"`
	Truncated bool                `json:"truncated,omitempty" jsonschema:"extraction hit a limit: the stored text is a prefix"`
	Sections  []docSectionOutline `json:"sections,omitempty" jsonschema:"outline: heading + length per section"`

	SectionText string `json:"section_text,omitempty" jsonschema:"the text of the requested section"`
	FullText    string `json:"full_text,omitempty" jsonschema:"every section, delimited by headers (full=true)"`
}

func (s *mcpServer) getDoc(ctx context.Context, req *mcp.CallToolRequest, in getDocInput) (*mcp.CallToolResult, getDocOutput, error) {
	if strings.TrimSpace(in.Hash) == "" {
		return nil, getDocOutput{}, errors.New("memento_get_doc: hash is required")
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, getDocOutput{}, storeError("memento_get_doc", err)
	}
	doc, err := c.GetDocByHash(ctx, in.Hash)
	if errors.Is(err, client.ErrNoDoc) {
		return nil, getDocOutput{Found: false}, nil
	}
	if err != nil {
		return nil, getDocOutput{}, storeError("memento_get_doc", err)
	}
	now := time.Now().UTC()
	out := getDocOutput{
		Found:     true,
		Hash:      doc.URLHash,
		URL:       doc.URL,
		Title:     doc.Title,
		Stale:     doc.Stale(now),
		Truncated: doc.Truncated,
		Sections:  newDocOutline(doc),
	}
	if in.Full {
		var b strings.Builder
		for _, sec := range doc.Sections {
			b.WriteString("--- [")
			b.WriteString(strconv.Itoa(sec.Order))
			b.WriteString("] ")
			b.WriteString(sec.Heading)
			b.WriteString(" ---\n")
			b.WriteString(sec.Text)
			b.WriteString("\n")
		}
		out.FullText = b.String()
	}
	if in.Section != nil {
		for _, sec := range doc.Sections {
			if sec.Order == *in.Section {
				out.SectionText = sec.Text
				return nil, out, nil
			}
		}
		return nil, getDocOutput{}, fmt.Errorf("memento_get_doc: doc %s has no section %d (the outline lists %d sections)",
			doc.URLHash, *in.Section, len(doc.Sections))
	}
	return nil, out, nil
}

type searchDocsInput struct {
	Query   string `json:"query" jsonschema:"text to search in stored document sections (the raw html is never searched)"`
	Project string `json:"project,omitempty" jsonschema:"limit to documents tagged with this project"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max documents to return (default 5, cap 20): the context budget for search results"`
}

type docHitView struct {
	Hash    string `json:"hash" jsonschema:"document id for memento_get_doc"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Stale   bool   `json:"stale" jsonschema:"past TTL: refresh with memento_fetch_doc before quoting"`
	Snippet string `json:"snippet,omitempty" jsonschema:"first section, whitespace-collapsed and capped"`
}

type docsHitsOutput struct {
	Count   int          `json:"count"`
	Docs    []docHitView `json:"docs"`
	Skipped int          `json:"skipped,omitempty" jsonschema:"stored documents whose content failed to decode (index corruption): report to the user"`
}

func (s *mcpServer) searchDocs(ctx context.Context, req *mcp.CallToolRequest, in searchDocsInput) (*mcp.CallToolResult, docsHitsOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, docsHitsOutput{}, errors.New("memento_search_docs: query is required")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > client.DocsSearchCap {
		limit = client.DocsSearchCap
	}
	c, err := s.newClient(ctx)
	if err != nil {
		return nil, docsHitsOutput{}, storeError("memento_search_docs", err)
	}
	docs, meta, err := c.SearchDocs(ctx, in.Query, in.Project, limit)
	if err != nil {
		return nil, docsHitsOutput{}, storeError("memento_search_docs", err)
	}
	now := time.Now().UTC()
	hits := make([]docHitView, 0, len(docs))
	for _, d := range docs {
		h := docHitView{Hash: d.URLHash, URL: d.URL, Title: d.Title, Stale: d.Stale(now)}
		if len(d.Sections) > 0 {
			h.Snippet = snippet(d.Sections[0].Text, 300)
		}
		hits = append(hits, h)
	}
	return nil, docsHitsOutput{Count: len(hits), Docs: hits, Skipped: meta.Skipped}, nil
}
