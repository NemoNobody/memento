package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// defaultRequestTimeout bounds each HTTP round-trip so the CLI fails fast
// instead of hanging forever when OpenSearch is unreachable.
const defaultRequestTimeout = 30 * time.Second

// ErrNotFound is returned when a fact with the given key does not exist.
var ErrNotFound = errors.New("fact not found")

// Client wraps the OpenSearch client.
type Client struct {
	api *opensearchapi.Client
	cfg *config.Config

	// journalInfraMu guards journal infra installation (pipeline +
	// template): the idempotent check runs until it succeeds — a failed
	// probe must not mark the infra as installed (see ensureJournalInfra).
	journalInfraMu sync.Mutex
	journalInfraOK bool

	// journalIdxPipeline tracks journal indices whose default_pipeline has
	// been pinned in this process (see ensureJournalIndexPipeline).
	journalIdxPipeline map[string]bool

	// authorMu guards the writer-label cache: a security-resolved identity
	// lives for the process; a degraded chain is re-probed with backoff
	// (see resolveAuthor).
	authorMu       sync.Mutex
	authorResolved bool
	authorRetryAt  time.Time
	author         AuthorInfo
}

// NewClient creates a new OpenSearch client.
func NewClient(cfg *config.Config) (*Client, error) {
	if cfg.OpenSearchPassword == "" {
		// Fail fast with the migration path spelled out: a 401 from the server
		// would not tell the operator where credentials come from now.
		return nil, fmt.Errorf("OpenSearch password is empty: store one with 'memento creds set' " +
			"(OS keyring), export OPENSEARCH_PASSWORD, or set allow_plaintext=true in the config")
	}
	api, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses:          []string{cfg.OpenSearchURL},
			Username:           cfg.OpenSearchUser,
			Password:           cfg.OpenSearchPassword,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			RequestTimeout:     defaultRequestTimeout,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create opensearch client: %w", err)
	}
	return &Client{api: api, cfg: cfg}, nil
}

// isNotFound reports whether err is an HTTP 404 from OpenSearch. The client
// returns different error types depending on the shape of the response body:
// a plain string error, or a structured one with type/reason/root_cause.
func isNotFound(err error) bool {
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return se.Status == http.StatusNotFound
	}
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Status == http.StatusNotFound
	}
	return false
}

// isAlreadyExists reports OpenSearch's "the index you are creating is already
// there". Two memento processes on a fresh cluster (two Zed windows, CLI +
// MCP) both pass the HEAD check and race to create the index; the loser must
// treat the collision as success, not as a tool failure.
func isAlreadyExists(err error) bool {
	var ste *opensearch.StructError
	return errors.As(err, &ste) && ste.Err.Type == "resource_already_exists_exception"
}

// isVersionConflict reports OpenSearch's optimistic-concurrency rejection:
// op_type=create on an existing document (or an if_seq_no mismatch).
func isVersionConflict(err error) bool {
	var ste *opensearch.StructError
	return errors.As(err, &ste) && ste.Err.Type == "version_conflict_engine_exception"
}

// isClusterBlock reports whether the error is OpenSearch refusing a write
// because of a cluster or index block — typically the disk flood stage turning
// indices read-only, or an explicit read-only block.
func isClusterBlock(err error) bool {
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Err.Type == "cluster_block_exception" ||
			strings.Contains(ste.Err.Reason, "read-only")
	}
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return strings.Contains(se.Err, "cluster_block_exception") ||
			strings.Contains(se.Err, "read-only")
	}
	return false
}

// wrapWriteError turns a cluster-side block into an actionable message: the
// caller must know this is not a memento bug and that retrying will not help. The
// guidance comes first and the raw cluster error last, because the raw dump is
// long (type + reason + root_cause) and would otherwise bury the one thing the
// reader needs to act on.
func wrapWriteError(op string, err error) error {
	if isClusterBlock(err) {
		return fmt.Errorf("%s: the cluster is blocking writes. This is a cluster-side "+
			"block — usually the disk flood stage (a data node above the flood "+
			"watermark) or an explicit read-only block. Free space on the data nodes "+
			"or ask the cluster owner; retrying will not help. Cluster said: %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// Info checks connectivity and returns cluster information. It is the cheapest
// way to tell whether the store is usable.
func (c *Client) Info(ctx context.Context) (*opensearchapi.InfoResp, error) {
	resp, err := c.api.Info(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reach OpenSearch at %s: %w", config.RedactURL(c.cfg.OpenSearchURL), err)
	}
	return resp, nil
}

// URL returns the configured OpenSearch URL.
func (c *Client) URL() string { return c.cfg.OpenSearchURL }

// IndexName returns the configured index name.
func (c *Client) IndexName() string { return c.cfg.IndexName }

// buildIndexBody returns the index creation body (mappings) used by EnsureIndex.
func buildIndexBody() map[string]interface{} {
	return map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"fact_id":     map[string]interface{}{"type": "keyword"},
				"key":         map[string]interface{}{"type": "keyword"},
				"value":       map[string]interface{}{"type": "text"},
				"category":    map[string]interface{}{"type": "keyword"},
				"project":     map[string]interface{}{"type": "keyword"},
				"status":      map[string]interface{}{"type": "keyword"},
				"created_at":  map[string]interface{}{"type": "date"},
				"updated_at":  map[string]interface{}{"type": "date"},
				"observed_at": map[string]interface{}{"type": "date"},
				"source":      map[string]interface{}{"type": "keyword"},
				"notes":       map[string]interface{}{"type": "text"},
				// Summary is the table-of-contents rendering of value: searchable
				// text, so listings and BM25 can work from it.
				"summary": map[string]interface{}{"type": "text"},
				// Stored, never indexed: legacy history arrays predate the journal
				// and are read with the fact, not searched.
				"history": map[string]interface{}{"type": "object", "enabled": false},
				// Journal entry id of the current value (see model.Fact.CurrentRef).
				"current_ref": map[string]interface{}{"type": "keyword"},
				// Authorship labels (self-reported; see model.Fact): exact-match
				// fields for "who wrote what" queries, never full-text.
				"author":        map[string]interface{}{"type": "keyword"},
				"author_source": map[string]interface{}{"type": "keyword"},
				"model":         map[string]interface{}{"type": "keyword"},
				"host":          map[string]interface{}{"type": "keyword"},
				"session":       map[string]interface{}{"type": "keyword"},
				"updated_by":    map[string]interface{}{"type": "keyword"},
			},
		},
	}
}

// EnsureIndex creates the index when it is missing.
//
// Existence is checked with a read-only request first, and the create is skipped
// when the index is already there. Creating unconditionally would need a write
// and would therefore fail on a read-only index — breaking read commands
// (`get`, `search`, `list`), which must keep working even when the cluster has
// blocked writes.
func (c *Client) EnsureIndex(ctx context.Context) error {
	existsReq := opensearchapi.IndicesExistsReq{Indices: []string{c.cfg.IndexName}}
	resp, err := c.api.Indices.Exists(ctx, existsReq)
	switch {
	case err == nil:
		return nil
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		// The index is missing: create it below. The status code is checked
		// instead of the error type because a HEAD response has no body, so the
		// client cannot parse it into a typed error.
	default:
		return fmt.Errorf("check index %s: %w", c.cfg.IndexName, err)
	}

	body, _ := json.Marshal(buildIndexBody())
	if _, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: c.cfg.IndexName,
		Body:  bytes.NewReader(body),
	}); err != nil {
		if isAlreadyExists(err) {
			return nil // lost the create race to a concurrent process: the index is there
		}
		return wrapWriteError("create index", err)
	}
	return nil
}

// addCorrectionScript records a value change on the main fact doc: the new
// value replaces the old, and current_ref points at the journal entry that
// was appended for it (the journal append happens BEFORE this script: the
// journal is the log, the main doc is the projection, and an orphan journal
// entry from a failed main write is a harmless truth, never a lost value).
// updated_by moves to the last toucher; the birth author labels are
// write-once and deliberately absent from this script.
//
// The in-doc history array is no longer maintained: the journal index is the
// history. Facts created before the journal keep their array until
// 'memento migrate-history' moves it.
const addCorrectionScript = `
ctx._source.value = params.value;
ctx._source.category = params.category;
ctx._source.observed_at = params.observed_at;
ctx._source.source = params.source;
if (params.notes != null && params.notes != '') { ctx._source.notes = params.notes; }
ctx._source.status = 'verified';
ctx._source.summary = params.summary;
ctx._source.current_ref = params.current_ref;
ctx._source.updated_by = params.updated_by;
ctx._source.updated_at = params.updated_at;
`

// Add stores a fact under its key. Three cases: the key is new (created, then
// journaled and linked); the value is unchanged (only notes/source/status
// move — observed_at must not, the observation is old and only the qualifier
// is new, and the journal is untouched); the value changed (a journal entry
// is appended for the new value, then the main doc is corrected to point at
// it — this also resurrects an obsolete fact).
//
// The journal append precedes the main write: the journal is the accepted
// log, the main doc is the projection. A failed main write leaves an orphan
// entry (truth: the value was accepted, the projection lags and is fixed by
// the next write); the reverse order could lose values from the log.
//
// Notes are only replaced by a non-empty new note: the CLI flag and the MCP
// field default to empty, and a correction without --notes must not silently
// erase the qualifiers (they are not kept in history, so erasure is irreversible).
//
// The category moves with the correction on both update paths: the write edge
// validates and canonicalizes it, and a fact stored with the wrong category
// must not keep the wrong staleness TTL forever (category drives stale).
//
// The summary moves with the value unconditionally: the summary describes the
// current value, and a correction to a short value (no summary required)
// erasing the old summary is the honest state — an old table of contents for
// a new value is a lie.
//
// It reports whether the fact was newly created. Writes wait for a refresh,
// so the fact is immediately visible to subsequent Search/List calls.
func (c *Client) Add(ctx context.Context, f *model.Fact) (created bool, err error) {
	if err := c.checkSummaryCap(f); err != nil {
		return false, err
	}
	existing, err := c.Get(ctx, f.Key)
	if errors.Is(err, ErrNotFound) {
		// Summary bounds are enforced on accepted values only (create and
		// correction): a same-value notes touch must not fail over a summary
		// the pre-summary fact never had.
		if err := c.checkSummaryRequired(f); err != nil {
			return false, err
		}
		// Birth labels are stamped once (write-once): who/what/where created
		// the fact. Later writes move only updated_by.
		c.resolveAuthor(ctx).stampFact(f)
		f.UpdatedBy = f.Author
		created, err = c.createFact(ctx, f)
		if err != nil {
			return false, err
		}
		if created {
			// Journal the birth value, then link the fact to it. A journal
			// failure leaves the fact in the pre-journal shape (migrate-history
			// catches it), not a broken fact.
			ref, jErr := c.journalAppend(ctx, f, f.FactID, f.ObservedAt)
			if jErr != nil {
				return true, jErr
			}
			if lErr := c.linkCurrentRef(ctx, f.Key, ref); lErr != nil {
				return true, lErr
			}
			return true, nil
		}
		// op_type=create lost the race to a concurrent Add of the same key
		// (a plain index would last-write-win and drop the winner's value
		// with no journal entry): re-read and apply as a correction below.
		existing, err = c.Get(ctx, f.Key)
		if err != nil {
			return false, fmt.Errorf("add fact: %w", err)
		}
	} else if err != nil {
		return false, fmt.Errorf("add fact: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	if existing.Value == f.Value {
		doc := map[string]interface{}{
			"source":     f.Source,
			"category":   f.Category,
			"status":     "verified",
			"updated_by": c.resolveAuthor(ctx).Author,
			"updated_at": now,
		}
		if f.Notes != "" {
			doc["notes"] = f.Notes
		}
		body, _ := json.Marshal(map[string]interface{}{
			"doc": doc,
		})
		_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
			Index:      c.cfg.IndexName,
			DocumentID: f.Key,
			Body:       strings.NewReader(string(body)),
			Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
		})
		if err != nil {
			return false, wrapWriteError("add fact", err)
		}
		return false, nil
	}

	if err := c.checkSummaryRequired(f); err != nil {
		return false, err
	}

	// The journal entry carries the doc's fact_id (not the caller's fresh
	// uuid): entries group by the identity that survives, and the doc's
	// fact_id is that identity.
	ref, err := c.journalAppend(ctx, f, existing.FactID, f.ObservedAt)
	if err != nil {
		return false, err
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": addCorrectionScript,
			"params": map[string]interface{}{
				"value":       f.Value,
				"category":    f.Category,
				"observed_at": f.ObservedAt.UTC().Format(time.RFC3339),
				"source":      f.Source,
				"notes":       f.Notes,
				"summary":     f.Summary,
				"updated_at":  now,
				"current_ref": ref,
				"updated_by":  c.resolveAuthor(ctx).Author,
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       strings.NewReader(string(body)),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		return false, wrapWriteError("add fact", err)
	}
	return false, nil
}

// journalAppend appends the journal entry for an accepted fact value and
// returns the entry id. The caller links the fact to it: the correction
// script carries current_ref, the create path links in a follow-up update.
// The entry carries its own author labels: "who wrote this version" is a
// per-entry fact of the log; factID is the identity the entry belongs to
// (the doc's fact_id, which survives key renames).
func (c *Client) journalAppend(ctx context.Context, f *model.Fact, factID string, ts time.Time) (string, error) {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	e := model.NewJournalEntry(f.Key, factID, f.Project, f.Category, model.OpWrite, f.Value, f.Notes, f.Source, ts)
	c.resolveAuthor(ctx).applyTo(e)
	return c.AppendJournal(ctx, e)
}

// linkCurrentRef points a fact at its journal entry. Used on the create path
// (the birth write happens before the journal id exists); the correction
// script sets current_ref inside the same write.
func (c *Client) linkCurrentRef(ctx context.Context, key, ref string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"doc": map[string]interface{}{"current_ref": ref},
	})
	if _, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       strings.NewReader(string(body)),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	}); err != nil {
		return wrapWriteError("link current_ref", err)
	}
	return nil
}

// checkSummaryCap rejects an over-long summary in any write: a provided
// summary that is longer than the cap stops being a summary. The cap lives
// on the shape, not on the accepted value, so it runs before the store
// read.
func (c *Client) checkSummaryCap(f *model.Fact) error {
	if c.cfg.SummaryMaxLen > 0 {
		if n := utf8.RuneCountInString(f.Summary); n > c.cfg.SummaryMaxLen {
			return fmt.Errorf("add fact: summary is %d characters, over summary_max_len=%d: keep it a table of contents, not a second value", n, c.cfg.SummaryMaxLen)
		}
	}
	return nil
}

// checkSummaryRequired enforces the table-of-contents contract on an
// accepted value (create and correction paths): a value over summary_min_len
// must ship a summary, so store listings stay skimmable. Lengths count
// runes: the store is text-first and byte counts would punish non-Latin
// scripts.
func (c *Client) checkSummaryRequired(f *model.Fact) error {
	if c.cfg.SummaryMinLen > 0 && f.Summary == "" {
		if n := utf8.RuneCountInString(f.Value); n > c.cfg.SummaryMinLen {
			return fmt.Errorf("add fact: value is %d characters, over summary_min_len=%d: a summary is required (CLI --summary, MCP summary) — it keeps store listings skimmable", n, c.cfg.SummaryMinLen)
		}
	}
	return nil
}

// createFact stores a brand-new fact with op_type=create: OpenSearch refuses
// the write when the document exists, so two concurrent creators of the same
// key cannot silently overwrite each other (last-write-wins would lose the
// first value with no history entry). It reports created=true on success and
// created=false on a version conflict, leaving the caller to re-read the
// winner and apply the fact as a correction.
func (c *Client) createFact(ctx context.Context, f *model.Fact) (bool, error) {
	body, _ := json.Marshal(f)
	_, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.IndexParams{Refresh: "wait_for", OpType: "create"},
	})
	if err == nil {
		return true, nil
	}
	if isVersionConflict(err) {
		return false, nil
	}
	return false, wrapWriteError("add fact", err)
}

// Get gets a fact by key. It uses the realtime document GET API, so it sees
// the fact immediately after Add (unlike a search, which waits for a refresh).
func (c *Client) Get(ctx context.Context, key string) (*model.Fact, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, fmt.Errorf("get fact: %w", err)
	}
	if !resp.Found {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	var f model.Fact
	if err := json.Unmarshal(resp.Source, &f); err != nil {
		return nil, fmt.Errorf("get fact: decode %q: %w", key, err)
	}
	return &f, nil
}

// Search performs a keyword search, capped to limit hits (limit <= 0 uses the
// server default of 10). Relevance order is OpenSearch's.
func (c *Client) Search(ctx context.Context, query string, project, category, status string, limit int) ([]model.Fact, SearchMeta, error) {
	return c.search(ctx, buildSearchBody(query, project, category, status, limit))
}

// List returns all facts for a project whose key starts with prefix.
// An empty prefix lists the whole project. A prefix may carry the trailing
// '*' sugar ("state:*" == "state:"); a '*' anywhere else is rejected.
func (c *Client) List(ctx context.Context, project, prefix string) ([]model.Fact, SearchMeta, error) {
	p, err := NormalizePrefix(prefix)
	if err != nil {
		return nil, SearchMeta{}, err
	}
	return c.search(ctx, buildProjectBody(project, p))
}

// Keys returns facts whose key matches a prefix and/or a substring, across
// all projects (project == "" keeps the scope global). It is the key-level
// navigation tool: prefix answers "what is under this subtree", match
// answers "which keys mention this token". At least one of them is required:
// an unbounded key query is a context flood, not a search.
//
// match is a Lucene keyword pattern: '*' and '?' inside it are wildcards.
func (c *Client) Keys(ctx context.Context, prefix, match, project, status string) ([]model.Fact, SearchMeta, error) {
	p, err := NormalizePrefix(prefix)
	if err != nil {
		return nil, SearchMeta{}, err
	}
	if p == "" && match == "" {
		return nil, SearchMeta{}, fmt.Errorf("keys: provide a key prefix and/or a match pattern")
	}
	return c.search(ctx, buildKeysBody(p, match, project, status))
}

// NormalizePrefix validates a key prefix and strips the optional trailing
// '*'. It is exported so CLI and MCP can reject bad prefixes before touching
// the store.
func NormalizePrefix(prefix string) (string, error) {
	p := strings.TrimSpace(prefix)
	p = strings.TrimSuffix(p, "*")
	if strings.Contains(p, "*") {
		return "", fmt.Errorf("key prefix %q: only a trailing * is supported", prefix)
	}
	return p, nil
}

// Obsolete marks a fact obsolete because its subject is gone. The value and
// observed_at are intentionally left untouched — there is no new observation.
// The reason is appended to Notes so earlier qualifiers are not lost.
func (c *Client) Obsolete(ctx context.Context, key, reason string) error {
	existing, err := c.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("obsolete fact: %w", err)
	}
	notes := existing.Notes
	if reason != "" {
		if notes == "" {
			notes = reason
		} else {
			notes = notes + "\n" + reason
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"doc": map[string]interface{}{
			"status":     "obsolete",
			"notes":      notes,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("obsolete %w: %q", ErrNotFound, key)
		}
		return wrapWriteError("obsolete fact", err)
	}
	return nil
}

// pruneHistoryScript keeps only the newest params.keep history entries in
// the legacy in-doc array. It serves facts that predate the journal until
// 'memento migrate-history' moves their array; journal pruning is
// PruneJournal (the journal index is the history for journal-era facts).
const pruneHistoryScript = `
def h = ctx._source.history;
if (h != null && h.size() > params.keep) {
  ctx._source.history = new ArrayList(h.subList(h.size() - params.keep, h.size()));
}
ctx._source.updated_at = params.updated_at;
`

// JournalEntriesForPrune exposes the journal entry count for a key (the
// prune planning path shows the journal, not the legacy array, once a key
// has entries there).
func (c *Client) JournalCount(ctx context.Context, key string) (int, error) {
	_, total, _, err := c.JournalEntries(ctx, key, 1)
	return total, err
}

// Prune drops history beyond the newest keep. It is journal-first: a key
// with journal entries is pruned in the journal (the legacy in-doc array is
// ignored for such keys — migrate-history removes it); keys that predate the
// journal fall back to the legacy array so live stores prune cleanly between
// release and migration. It returns the number of entries removed. Like every
// destructive call it must sit behind an explicit user confirmation — the
// barrier lives in the CLI (--yes) and the MCP tool (confirm=true).
func (c *Client) Prune(ctx context.Context, key string, keep int) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("prune: keep must be >= 0, got %d", keep)
	}
	total, err := c.JournalCount(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("prune fact: %w", err)
	}
	if total > 0 {
		return c.PruneJournal(ctx, key, keep)
	}

	existing, err := c.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("prune fact: %w", err)
	}
	removed := len(existing.History) - keep
	if removed <= 0 {
		return 0, nil // nothing beyond the limit: no write, no updated_at move
	}
	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": pruneHistoryScript,
			"params": map[string]interface{}{
				"keep":       keep,
				"updated_at": time.Now().UTC().Format(time.RFC3339),
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("prune %w: %q", ErrNotFound, key)
		}
		return 0, wrapWriteError("prune fact", err)
	}
	return removed, nil
}

// buildSearchBody builds the search query body for a keyword search.
// The result budget matters as soon as one index holds bulk text (docs):
// an uncapped search ships every hit into the caller's context.
func buildSearchBody(query string, project, category, status string, limit int) map[string]interface{} {
	filters := []map[string]interface{}{}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	if category != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"category": category}})
	}
	if status != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"status": status}})
	}
	boolQuery := map[string]interface{}{
		"must": map[string]interface{}{
			// multi_match treats the query as text. query_string would parse
			// Lucene syntax, so keys like "proj:foo" or "foo(bar" would either
			// match the wrong field or fail with a 400.
			"multi_match": map[string]interface{}{
				"query":  query,
				"fields": []string{"value", "notes"},
			},
		},
	}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}
	body := map[string]interface{}{
		"query": map[string]interface{}{"bool": boolQuery},
	}
	if limit > 0 {
		body["size"] = limit
	} else {
		// "no cap": size is mandatory for predictability; the server default
		// of 10 silently truncates limit=0 queries.
		body["size"] = NavSizeCap
	}
	return body
}

// NavSizeCap is the explicit size for navigation queries (list/keys) and for
// "no cap" searches: without it OpenSearch returns 10 hits and callers
// silently receive an arbitrary subset.
const NavSizeCap = 1000

// buildProjectBody builds the search query body for a project lookup,
// optionally narrowed to a key prefix.
func buildProjectBody(project, prefix string) map[string]interface{} {
	filters := []map[string]interface{}{
		{"term": map[string]interface{}{"project": project}},
	}
	if prefix != "" {
		filters = append(filters, map[string]interface{}{
			"prefix": map[string]interface{}{"key": prefix},
		})
	}
	return map[string]interface{}{
		"query": map[string]interface{}{"bool": map[string]interface{}{"filter": filters}},
		"size":  NavSizeCap,
	}
}

// buildKeysBody builds the key-navigation query: AND of the given filters,
// sorted by key so the navigation output is stable.
func buildKeysBody(prefix, match, project, status string) map[string]interface{} {
	filters := []map[string]interface{}{}
	if prefix != "" {
		filters = append(filters, map[string]interface{}{
			"prefix": map[string]interface{}{"key": prefix},
		})
	}
	if match != "" {
		filters = append(filters, map[string]interface{}{
			"wildcard": map[string]interface{}{"key": "*" + match + "*"},
		})
	}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	if status != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"status": status}})
	}
	return map[string]interface{}{
		"query": map[string]interface{}{"bool": map[string]interface{}{"filter": filters}},
		"sort":  []map[string]interface{}{{"key": map[string]interface{}{"order": "asc"}}},
		"size":  NavSizeCap,
	}
}

// SearchMeta carries query diagnostics that a facts-only return hides:
// Total is the cluster-side match count (the honest truncation signal: a
// page exactly filling the cap is a complete result), Skipped counts hits
// whose _source failed to decode — index corruption must never be silent.
type SearchMeta struct {
	Total   int
	Skipped int
}

// search executes a search and returns the matching facts with meta.
func (c *Client) search(ctx context.Context, body map[string]interface{}) ([]model.Fact, SearchMeta, error) {
	data, _ := json.Marshal(body)
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		return nil, SearchMeta{}, fmt.Errorf("search: %w", err)
	}
	meta := SearchMeta{Total: resp.Hits.Total.Value}
	var results []model.Fact
	for _, hit := range resp.Hits.Hits {
		var f model.Fact
		if err := json.Unmarshal(hit.Source, &f); err != nil {
			meta.Skipped++
			continue
		}
		results = append(results, f)
	}
	return results, meta, nil
}
