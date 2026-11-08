package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/NemoNobody/memento/internal/model"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ErrNoDoc is returned when a document with the given hash does not exist.
var ErrNoDoc = errors.New("document not found")

// DocsIndexName returns the configured documents index name.
func (c *Client) DocsIndexName() string { return c.cfg.DocsIndexName }

// buildDocsIndexBody returns the docs index mappings. The docs index lives
// apart from the facts index on purpose: page text is bulk, facts are a small
// trusted set, and mixing them would slow fact search and bloat every read.
//
// sections are plain objects (not nested): they are searched for hits and read
// back whole; per-section positional queries are not needed.
//
// html is stored (re-extraction source, shared cache) and deliberately NOT
// indexed: BM25 runs over extracted sections, never over markup, and no read
// path serves html (see htmlExclude) — raw pages are curl's job, not context.
func buildDocsIndexBody() map[string]interface{} {
	return map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"url":          map[string]interface{}{"type": "keyword"},
				"final_url":    map[string]interface{}{"type": "keyword"},
				"url_hash":     map[string]interface{}{"type": "keyword"},
				"content_hash": map[string]interface{}{"type": "keyword"},
				"title":        map[string]interface{}{"type": "text"},
				"project":      map[string]interface{}{"type": "keyword"},
				"fetched_at":   map[string]interface{}{"type": "date"},
				"expires_at":   map[string]interface{}{"type": "date"},
				"size":         map[string]interface{}{"type": "integer"},
				"size_raw":     map[string]interface{}{"type": "integer"},
				"truncated":    map[string]interface{}{"type": "boolean"},
				"fetched_by":   map[string]interface{}{"type": "keyword"},
				"via":          map[string]interface{}{"type": "keyword"},
				"html":         map[string]interface{}{"type": "text", "index": false},
				"sections": map[string]interface{}{
					"properties": map[string]interface{}{
						"order":   map[string]interface{}{"type": "integer"},
						"heading": map[string]interface{}{"type": "text"},
						"text":    map[string]interface{}{"type": "text"},
					},
				},
				"images": map[string]interface{}{
					"properties": map[string]interface{}{
						"url":   map[string]interface{}{"type": "keyword"},
						"alt":   map[string]interface{}{"type": "text"},
						"title": map[string]interface{}{"type": "text"},
					},
				},
			},
		},
	}
}

// htmlExclude is the standing _source exclusion every docs read applies: the
// snapshot is kept in the store, and the agent's context never sees it.
var htmlExclude = []string{"html"}

// EnsureDocsIndex creates the docs index when it is missing, with the same
// read-first shape as EnsureIndex (a read-only index must not break reads).
func (c *Client) EnsureDocsIndex(ctx context.Context) error {
	resp, err := c.api.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{c.cfg.DocsIndexName},
	})
	switch {
	case err == nil:
		return nil
	case resp != nil && resp.StatusCode == http.StatusNotFound:
	default:
		return fmt.Errorf("check index %s: %w", c.cfg.DocsIndexName, err)
	}

	body, _ := json.Marshal(buildDocsIndexBody())
	if _, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: c.cfg.DocsIndexName,
		Body:  bytes.NewReader(body),
	}); err != nil {
		if isAlreadyExists(err) {
			return nil // lost the create race to a concurrent process: the index is there
		}
		return wrapWriteError("create docs index", err)
	}
	return nil
}

// PutDoc stores a document under its URL hash, replacing any older version
// (a refresh is a correction of the snapshot).
func (c *Client) PutDoc(ctx context.Context, doc *model.Doc) error {
	body, _ := json.Marshal(doc)
	_, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: doc.URLHash,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.IndexParams{Refresh: "wait_for"},
	})
	if err != nil {
		return wrapWriteError("store doc", err)
	}
	return nil
}

// GetDocByHash reads one document with the realtime GET API, so a document
// stored a moment ago is visible immediately. html is excluded from the
// response by design (context economy).
func (c *Client) GetDocByHash(ctx context.Context, hash string) (*model.Doc, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: hash,
		Params:     opensearchapi.DocumentGetParams{SourceExcludes: htmlExclude},
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrNoDoc, hash)
		}
		return nil, fmt.Errorf("get doc: %w", err)
	}
	if !resp.Found {
		return nil, fmt.Errorf("%w: %q", ErrNoDoc, hash)
	}
	var doc model.Doc
	if err := json.Unmarshal(resp.Source, &doc); err != nil {
		return nil, fmt.Errorf("get doc: decode %q: %w", hash, err)
	}
	return &doc, nil
}

// DocsSearchCap is the result budget for docs searches: page hits are
// research input, not a target — the agent reads snippets and pulls a
// section by hash, it never ingests a wall of documents.
const DocsSearchCap = 20

// buildDocSearchBody builds the docs search: text match over title and section
// text, optionally scoped to a project, capped to a result budget.
func buildDocSearchBody(query, project string, limit int) map[string]interface{} {
	if limit <= 0 {
		limit = 5
	}
	if limit > DocsSearchCap {
		limit = DocsSearchCap
	}
	filters := []map[string]interface{}{}
	if project != "" {
		filters = append(filters, map[string]interface{}{"term": map[string]interface{}{"project": project}})
	}
	boolQuery := map[string]interface{}{
		"must": map[string]interface{}{
			"multi_match": map[string]interface{}{
				"query":  query,
				"fields": []string{"title", "sections.text"},
			},
		},
	}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}
	return map[string]interface{}{
		"query":   map[string]interface{}{"bool": boolQuery},
		"size":    limit,
		"_source": map[string]interface{}{"excludes": htmlExclude},
	}
}

// SearchDocs finds stored documents whose title or text matches the query.
// Results are relevance-ordered and capped; callers ship snippets, not whole
// documents, into the agent's context.
func (c *Client) SearchDocs(ctx context.Context, query, project string, limit int) ([]model.Doc, SearchMeta, error) {
	data, _ := json.Marshal(buildDocSearchBody(query, project, limit))
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{c.cfg.DocsIndexName},
		Body:    bytes.NewReader(data),
	})
	if err != nil {
		return nil, SearchMeta{}, fmt.Errorf("search docs: %w", err)
	}
	meta := SearchMeta{Total: resp.Hits.Total.Value}
	var docs []model.Doc
	for _, hit := range resp.Hits.Hits {
		var d model.Doc
		if err := json.Unmarshal(hit.Source, &d); err != nil {
			meta.Skipped++
			continue
		}
		docs = append(docs, d)
	}
	return docs, meta, nil
}

// FetchDoc is the full fetch pipeline: cache check → download → extract →
// guard → store. cached=true means the stored snapshot was fresh and the
// network was not touched at all. A stale snapshot is refetched (stale is an
// instruction to re-verify, not to discard); refresh=true forces the refetch.
//
// beforeStore (may be nil) inspects the extracted doc before it is written —
// this is where the secret guard runs: page text is untrusted input, and a
// rejected doc must never reach the plaintext store. Validation of the URL
// itself (normalize, allowlist) belongs to the callers: bad input is rejected
// before the store is touched at all.
func (c *Client) FetchDoc(ctx context.Context, rawURL, project string, refresh bool,
	beforeStore func(*model.Doc) error) (doc *model.Doc, cached bool, err error) {
	if err := c.EnsureDocsIndex(ctx); err != nil {
		return nil, false, err
	}
	hash := URLHash(rawURL)
	if !refresh {
		existing, err := c.GetDocByHash(ctx, hash)
		switch {
		case err == nil && !existing.Stale(time.Now().UTC()):
			return existing, true, nil
		case err != nil && !errors.Is(err, ErrNoDoc):
			return nil, false, fmt.Errorf("fetch doc: %w", err)
		}
	}
	doc, err = FetchPage(ctx, c.cfg, rawURL)
	if err != nil {
		return nil, false, err
	}
	// The project tag is part of the doc the guard inspects: it must be set
	// before the pre-store hook, never after.
	doc.Project = project
	if beforeStore != nil {
		if err := beforeStore(doc); err != nil {
			return nil, false, err
		}
	}
	if err := c.PutDoc(ctx, doc); err != nil {
		return nil, false, err
	}
	return doc, false, nil
}
