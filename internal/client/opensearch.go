package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
}

// NewClient creates a new OpenSearch client.
func NewClient(cfg *config.Config) (*Client, error) {
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
		return nil, fmt.Errorf("reach OpenSearch at %s: %w", c.cfg.OpenSearchURL, err)
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
				// Stored, never indexed: history is read with the fact, not searched.
				"history": map[string]interface{}{"type": "object", "enabled": false},
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
		return wrapWriteError("create index", err)
	}
	return nil
}

// addCorrectionScript records a value change: the current observation is
// appended to history before the new value replaces it. Facts created before
// observed_at/history existed have neither, so the script falls back to
// updated_at and initialises the array.
const addCorrectionScript = `
def obs = ctx._source.observed_at != null ? ctx._source.observed_at : ctx._source.updated_at;
def src = ctx._source.source != null ? ctx._source.source : '';
if (ctx._source.history == null) { ctx._source.history = new ArrayList(); }
ctx._source.history.add(['value': ctx._source.value, 'observed_at': obs, 'source': src]);
ctx._source.value = params.value;
ctx._source.observed_at = params.observed_at;
ctx._source.source = params.source;
ctx._source.notes = params.notes;
ctx._source.status = 'verified';
ctx._source.updated_at = params.updated_at;
`

// Add stores a fact under its key. Three cases: the key is new (created); the
// value is unchanged (only notes/source/status move — observed_at must not, the
// observation is old and only the qualifier is new); the value changed (the old
// observation is appended to history and the new one becomes current — this
// also resurrects an obsolete fact).
//
// It reports whether the fact was newly created. Writes wait for a refresh, so
// the fact is immediately visible to subsequent Search/List calls.
func (c *Client) Add(ctx context.Context, f *model.Fact) (created bool, err error) {
	existing, err := c.Get(ctx, f.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		body, _ := json.Marshal(f)
		_, err = c.api.Index(ctx, opensearchapi.IndexReq{
			Index:      c.cfg.IndexName,
			DocumentID: f.Key,
			Body:       bytes.NewReader(body),
			Params:     opensearchapi.IndexParams{Refresh: "wait_for"},
		})
		if err != nil {
			return false, wrapWriteError("add fact", err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("add fact: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	if existing.Value == f.Value {
		body, _ := json.Marshal(map[string]interface{}{
			"doc": map[string]interface{}{
				"notes":      f.Notes,
				"source":     f.Source,
				"status":     "verified",
				"updated_at": now,
			},
		})
		_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
			Index:      c.cfg.IndexName,
			DocumentID: f.Key,
			Body:       bytes.NewReader(body),
			Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
		})
		if err != nil {
			return false, wrapWriteError("add fact", err)
		}
		return false, nil
	}

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": addCorrectionScript,
			"params": map[string]interface{}{
				"value":       f.Value,
				"observed_at": f.ObservedAt.UTC().Format(time.RFC3339),
				"source":      f.Source,
				"notes":       f.Notes,
				"updated_at":  now,
			},
		},
	})
	_, err = c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		return false, wrapWriteError("add fact", err)
	}
	return false, nil
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

// Search performs a keyword search.
func (c *Client) Search(ctx context.Context, query string, project, category, status string) ([]model.Fact, error) {
	return c.search(ctx, buildSearchBody(query, project, category, status))
}

// List returns all facts for a project.
func (c *Client) List(ctx context.Context, project string) ([]model.Fact, error) {
	return c.searchByProject(ctx, project)
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

// buildSearchBody builds the search query body for a keyword search.
func buildSearchBody(query string, project, category, status string) map[string]interface{} {
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
	return map[string]interface{}{
		"query": map[string]interface{}{"bool": boolQuery},
	}
}

// buildProjectBody builds the search query body for a project lookup.
func buildProjectBody(project string) map[string]interface{} {
	return map[string]interface{}{
		"query": map[string]interface{}{
			"term": map[string]interface{}{"project": project},
		},
	}
}

// searchByProject returns all facts for a project.
func (c *Client) searchByProject(ctx context.Context, project string) ([]model.Fact, error) {
	return c.search(ctx, buildProjectBody(project))
}

// search executes a search and returns the matching facts.
func (c *Client) search(ctx context.Context, body map[string]interface{}) ([]model.Fact, error) {
	data, _ := json.Marshal(body)
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	var results []model.Fact
	for _, hit := range resp.Hits.Hits {
		var f model.Fact
		if err := json.Unmarshal(hit.Source, &f); err != nil {
			continue
		}
		results = append(results, f)
	}
	return results, nil
}
