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
	"unicode/utf8"

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
func buildDocsIndexBody(replicas int) map[string]interface{} {
	return map[string]interface{}{
		"settings": map[string]interface{}{
			"index.number_of_replicas": replicas,
		},
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"url":                  map[string]interface{}{"type": "keyword"},
				"final_url":            map[string]interface{}{"type": "keyword"},
				"url_hash":             map[string]interface{}{"type": "keyword"},
				"content_hash":         map[string]interface{}{"type": "keyword"},
				"etag":                 map[string]interface{}{"type": "keyword"},
				"last_modified":        map[string]interface{}{"type": "keyword"},
				"title":                map[string]interface{}{"type": "text"},
				"project":              map[string]interface{}{"type": "keyword"},
				"fetched_at":           map[string]interface{}{"type": "date"},
				"expires_at":           map[string]interface{}{"type": "date"},
				"size":                 map[string]interface{}{"type": "integer"},
				"size_raw":             map[string]interface{}{"type": "integer"},
				"full_chars":           map[string]interface{}{"type": "integer"},
				"truncated":            map[string]interface{}{"type": "boolean"},
				"guard_medium_ignored": map[string]interface{}{"type": "integer"},
				"fetched_by":           map[string]interface{}{"type": "keyword"},
				"via":                  map[string]interface{}{"type": "keyword"},
				"content_type":         map[string]interface{}{"type": "keyword"},
				"html":                 map[string]interface{}{"type": "text", "index": false},
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

	body, _ := json.Marshal(buildDocsIndexBody(c.indexReplicas(ctx)))
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
	return c.putDocCAS(ctx, doc, 0, 0)
}

// putDocCAS writes a snapshot, optionally conditioned on the version pair from
// the read that produced it (0,0 = unconditional). A concurrent writer makes the
// stale write fail instead of silently replacing newer content.
func (c *Client) putDocCAS(ctx context.Context, doc *model.Doc, seqNo, primaryTerm int) error {
	params := opensearchapi.IndexParams{Refresh: "wait_for"}
	if seqNo != 0 || primaryTerm != 0 {
		params.IfSeqNo = &seqNo
		params.IfPrimaryTerm = &primaryTerm
	}
	return c.indexDoc(ctx, doc, params)
}

// createDoc writes a snapshot only if the hash has no document yet: two first
// fetches of one URL must not overwrite each other's copy.
func (c *Client) createDoc(ctx context.Context, doc *model.Doc) error {
	return c.indexDoc(ctx, doc, opensearchapi.IndexParams{Refresh: "wait_for", OpType: "create"})
}

func (c *Client) indexDoc(ctx context.Context, doc *model.Doc, params opensearchapi.IndexParams) error {
	body, _ := json.Marshal(doc)
	_, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: doc.URLHash,
		Body:       bytes.NewReader(body),
		Params:     params,
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
	return c.getDocByHash(ctx, hash, false)
}

// getDocByHash reads a document, optionally with the stored raw html. Only the
// re-extraction path asks for the body: it is not context, and it must not
// travel with a normal read.
func (c *Client) getDocByHash(ctx context.Context, hash string, withHTML bool) (*model.Doc, error) {
	doc, _, _, err := c.getDocByHashVersion(ctx, hash, withHTML)
	return doc, err
}

// getDocByHashVersion is getDocByHash plus the document's version pair, which a
// conditional write needs.
func (c *Client) getDocByHashVersion(ctx context.Context, hash string, withHTML bool) (*model.Doc, int, int, error) {
	params := opensearchapi.DocumentGetParams{SourceExcludes: htmlExclude}
	if withHTML {
		params = opensearchapi.DocumentGetParams{}
	}
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: hash,
		Params:     params,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, 0, fmt.Errorf("%w: %q", ErrNoDoc, hash)
		}
		return nil, 0, 0, fmt.Errorf("get doc: %w", err)
	}
	if !resp.Found {
		return nil, 0, 0, fmt.Errorf("%w: %q", ErrNoDoc, hash)
	}
	var doc model.Doc
	if err := json.Unmarshal(resp.Source, &doc); err != nil {
		return nil, 0, 0, fmt.Errorf("get doc: decode %q: %w", hash, err)
	}
	return &doc, resp.SeqNo, resp.PrimaryTerm, nil
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
		"query":            map[string]interface{}{"bool": boolQuery},
		"size":             limit,
		"track_total_hits": true,
		"_source":          map[string]interface{}{"excludes": htmlExclude},
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
		if statusIs(err, http.StatusNotFound) && strings.Contains(err.Error(), "index_not_found_exception") {
			// The docs index is created lazily on the first fetch: a missing
			// index means "no stored documents yet", not a failure. Same posture
			// as the journal wildcard reads (ignore_unavailable).
			return nil, SearchMeta{}, nil
		}
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

// revalidateDoc moves a stored snapshot's cache metadata after a 304 without
// rewriting the document. The caller's copy came from GetDocByHash, which
// excludes html on purpose; a full PutDoc would silently delete the raw body
// the snapshot keeps for re-extraction (and, in a shared cache, one host's
// 304 would erase what another host stored).
func (c *Client) revalidateDoc(ctx context.Context, hash string, fetchedAt, expiresAt time.Time, project, etag, lastModified string) error {
	fields := map[string]interface{}{
		"fetched_at": fetchedAt.Format(time.RFC3339Nano),
	}
	if expiresAt.IsZero() {
		// TTL disabled (or never configured): a fresh fetch under this config
		// stores no expiry, so a revalidated copy must lose the old one too —
		// otherwise the store keeps calling it stale while the caller says it
		// never expires, and every read revalidates again. A null in a partial
		// update removes the field.
		fields["expires_at"] = nil
	} else {
		fields["expires_at"] = expiresAt.Format(time.RFC3339Nano)
	}
	if project != "" {
		fields["project"] = project
	}
	if etag != "" {
		fields["etag"] = etag
	}
	if lastModified != "" {
		fields["last_modified"] = lastModified
	}
	body, _ := json.Marshal(map[string]interface{}{"doc": fields})
	_, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: hash,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		return wrapWriteError("revalidate doc", err)
	}
	return nil
}

// tagDocProject rewrites only the project field of a stored doc (the cache-hit
// retag). The doc id in the docs index is the url hash.
func (c *Client) tagDocProject(ctx context.Context, hash, project string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"doc": map[string]interface{}{"project": project},
	})
	_, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: hash,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	})
	if err != nil {
		return wrapWriteError("tag doc", err)
	}
	return nil
}

// storedText joins a snapshot's blocks into their raw text: blocks are contiguous
// slices, so concatenating them (no separator) gives back the page text whatever
// the block size was. The never-shrink rule compares these strings, so it must not
// depend on where the split fell.
func storedText(doc *model.Doc) string {
	var b strings.Builder
	for _, s := range doc.Sections {
		b.WriteString(s.Text)
	}
	return b.String()
}

// keepsStored reports whether the stored snapshot already holds at least as much
// of the same page as the candidate: the candidate is a truncated prefix of it
// and the revisions agree. Requiring Truncated means a shorter but complete page
// (the content really got smaller) still replaces the old text, and comparing the
// hashes keeps a shared page head from passing for the same revision.
func keepsStored(stored, candidate *model.Doc) bool {
	if stored == nil || candidate == nil || len(candidate.Sections) == 0 || !candidate.Truncated {
		return false
	}
	// A snapshot written before full_chars existed carries the previous hash
	// scheme: its hash can never equal a new one, so the hash gate is skipped for
	// that generation and the textual rule decides.
	legacy := stored.FullChars == 0 || candidate.FullChars == 0
	if !legacy && stored.ContentHash != "" && candidate.ContentHash != "" && stored.ContentHash != candidate.ContentHash {
		return false
	}
	s, c := storedText(stored), storedText(candidate)
	return len(c) < len(s) && strings.HasPrefix(s, c)
}

// wantsMore reports whether the caller's budget could hold more of the page than
// the snapshot does. Truncation from dropped image refs, or a first block larger
// than the budget, leaves size == full_chars: there is no text to add, and
// re-extracting would only re-parse the html.
func wantsMore(doc *model.Doc, limits ExtractLimits) bool {
	if !doc.Truncated {
		return false
	}
	if doc.FullChars > 0 && doc.Size >= doc.FullChars {
		return false
	}
	return limits.MaxChars == 0 || limits.MaxChars > doc.Size
}

// mergeRedactions unions two audit lists without repeating an entry. The identity
// is the detector and the span size, not the field name: re-extraction renumbers
// blocks, and the same finding must not be counted twice.
func mergeRedactions(old, extra []model.Redaction) []model.Redaction {
	if len(extra) == 0 {
		return old
	}
	type key struct {
		pattern string
		chars   int
	}
	seen := make(map[key]bool, len(old))
	for _, r := range old {
		seen[key{r.Pattern, r.Chars}] = true
	}
	out := old
	for _, r := range extra {
		k := key{r.Pattern, r.Chars}
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// FetchDoc is FetchDocWithin with the configured limits.
func (c *Client) FetchDoc(ctx context.Context, rawURL, project string, refresh bool,
	hook func(*model.Doc) ([]model.Redaction, error)) (doc *model.Doc, cached, revalidated bool, err error) {
	return c.FetchDocWithin(ctx, rawURL, project, refresh,
		ExtractLimits{BlockChars: c.cfg.FetchBlockChars, MaxChars: c.cfg.FetchDocChars}, hook)
}

// FetchDocWithin is FetchDoc with explicit extraction limits. Two rules keep a
// shared snapshot from losing text: a stored copy is never shrunk while its text
// is a prefix of what is already there (two agents fetching one URL with
// different budgets would otherwise race the larger copy away), and a truncated
// copy is re-extracted from its stored html when the caller asks for more — no
// network. The re-extraction re-runs the caller's hook: a credential split by
// markup is invisible to the html pass and is reconstructed by extraction.
func (c *Client) FetchDocWithin(ctx context.Context, rawURL, project string, refresh bool, limits ExtractLimits,
	hook func(*model.Doc) ([]model.Redaction, error)) (doc *model.Doc, cached, revalidated bool, err error) {
	limits = limits.normalized()
	if err := c.EnsureDocsIndex(ctx); err != nil {
		return nil, false, false, err
	}
	hash := URLHash(rawURL)
	var prior *model.Doc
	if !refresh {
		existing, err := c.GetDocByHash(ctx, hash)
		switch {
		case err == nil && !existing.Stale(time.Now().UTC()):
			if project != "" && existing.Project != project {
				// The first fetch's tag must not be permanent: projects share
				// the URL cache on purpose, and the requested tag is part of
				// the tool contract ("tag the document with this project").
				// Silently ignoring it would hide the doc from the new
				// project's searches forever.
				if err := c.tagDocProject(ctx, hash, project); err != nil {
					return nil, false, false, err
				}
				existing.Project = project
			}
			if wantsMore(existing, limits) {
				// The caller asks for more than the stored prefix: rebuild it from
				// the raw html the snapshot keeps, under the same guard a fetch
				// would run. A failure falls back to the stored copy — the fetch
				// itself succeeded.
				if grown, changed, gerr := c.ReextractDoc(ctx, hash, limits, hook); gerr == nil && changed {
					grown.HTML = ""
					existing = grown
				}
			}
			return existing, true, false, nil
		case err != nil && !errors.Is(err, ErrNoDoc):
			return nil, false, false, fmt.Errorf("fetch doc: %w", err)
		case err == nil:
			// Stale: revalidate with this copy's validators instead of
			// re-downloading it (a 304 costs headers only).
			prior = existing
		}
	} else if existing, gerr := c.GetDocByHash(ctx, hash); gerr == nil {
		// Forced refresh still revalidates: 304 is the cheapest possible
		// correct answer, and the caller asked for freshness, not for bytes.
		prior = existing
	}
	// The project tag is part of the doc the guard inspects: the hook must
	// see it, and the hook runs before the doc is finalized (see FetchPage).
	doc, meta, err := RevalidatePageWithin(ctx, c.cfg, rawURL, prior, limits, func(d *model.Doc) ([]model.Redaction, error) {
		d.Project = project
		if hook == nil {
			return nil, nil
		}
		return hook(d)
	})
	if err != nil {
		return nil, false, false, err
	}
	if meta.notModified {
		if prior == nil {
			// No validators were sent without a cached copy, so this is a
			// protocol surprise, not a normal path.
			return nil, false, false, fmt.Errorf("fetch doc: origin answered 304 for %s without a cached copy", rawURL)
		}
		// The origin confirmed the stored copy: move the timestamps (and the
		// validators it may have rotated) only. A full rewrite is wrong here:
		// the copy was read with _source excludes (html is not loaded), and
		// PutDoc would silently drop the raw body the snapshot stores for
		// re-extraction.
		if err := c.touchDoc(ctx, hash, prior, project, meta); err != nil {
			return nil, false, false, err
		}
		if wantsMore(prior, limits) {
			// The origin says the page did not change, so the stored html is the
			// source for more text; a 304 carries no body to extract from.
			if grown, changed, gerr := c.ReextractDoc(ctx, hash, limits, hook); gerr == nil && changed {
				grown.HTML = ""
				return grown, true, true, nil
			}
		}
		return prior, true, true, nil
	}
	// A smaller candidate must not shrink the snapshot: another writer may have
	// stored more of the same page since it was read. Re-read once, right before
	// the write, with the version pair the conditional write needs.
	var seqNo, primaryTerm int
	create := false
	if current, seq, pt, gerr := c.getDocByHashVersion(ctx, hash, false); gerr == nil {
		seqNo, primaryTerm = seq, pt
		if keepsStored(current, doc) {
			if err := c.touchDoc(ctx, hash, current, project, meta); err != nil {
				return nil, false, false, err
			}
			return current, false, false, nil
		}
	} else if errors.Is(gerr, ErrNoDoc) {
		// Nothing stored yet: a create-conditioned write keeps two first fetches
		// of one URL from overwriting each other.
		create = true
	} else {
		// A read failure must not degrade into an unconditional write: the copy
		// it would replace is unknown.
		return nil, false, false, fmt.Errorf("fetch doc: re-read before write: %w", gerr)
	}
	if keepsStored(prior, doc) {
		if err := c.touchDoc(ctx, hash, prior, project, meta); err != nil {
			return nil, false, false, err
		}
		return prior, false, false, nil
	}
	var werr error
	if create {
		werr = c.createDoc(ctx, doc)
	} else {
		werr = c.putDocCAS(ctx, doc, seqNo, primaryTerm)
	}
	if werr != nil {
		if isVersionConflict(werr) {
			// A writer landed between the re-read and the write: its snapshot is
			// newer, so answer with that one instead of overwriting it.
			if current, gerr := c.GetDocByHash(ctx, hash); gerr == nil {
				return current, true, false, nil
			}
		}
		return nil, false, false, werr
	}
	return doc, false, false, nil
}

// touchDoc moves a stored snapshot's cache metadata after a revalidation or a
// same-content fetch with a smaller budget: timestamps, validators and the
// project tag. The content (html, sections, redactions, guard counters) is kept
// as stored.
func (c *Client) touchDoc(ctx context.Context, hash string, doc *model.Doc, project string, meta fetchMeta) error {
	now := time.Now().UTC()
	var expires time.Time
	if c.cfg.DocsTTLDays > 0 {
		expires = now.AddDate(0, 0, c.cfg.DocsTTLDays)
	}
	if err := c.revalidateDoc(ctx, hash, now, expires, project, meta.etag, meta.lastModified); err != nil {
		return err
	}
	doc.FetchedAt = now
	doc.ExpiresAt = expires
	if meta.etag != "" {
		doc.ETag = meta.etag
	}
	if meta.lastModified != "" {
		doc.LastModified = meta.lastModified
	}
	if project != "" {
		doc.Project = project
	}
	return nil
}

// ReextractDoc rebuilds a stored document's blocks from the raw html kept in the
// snapshot, without touching the network. A snapshot is never shrunk: when the
// stored text already holds at least as much as the new limits allow, it is
// returned unchanged.
//
// hook (may be nil) is the caller's write-time guard, and it runs again here. The
// stored html was redacted by the html pass, but a credential split by markup is
// invisible to that pass and is reconstructed by extraction; skipping the guard
// would put it back into the agent-visible text. The audit lists are merged, so
// the earlier findings stay on record.
func (c *Client) ReextractDoc(ctx context.Context, hash string, limits ExtractLimits,
	hook func(*model.Doc) ([]model.Redaction, error)) (*model.Doc, bool, error) {
	limits = limits.normalized()
	doc, seqNo, primaryTerm, err := c.getDocByHashVersion(ctx, hash, true)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(doc.HTML) == "" {
		return nil, false, fmt.Errorf("doc %s carries no raw html: re-extraction needs a fresh fetch", hash)
	}
	var sections []model.Section
	var truncated bool
	var full string
	switch doc.ContentType {
	case "text/html", "application/xhtml+xml", "":
		sections, _, truncated, full = ExtractSectionsWithin(doc.HTML, limits)
	case "text/plain", "text/markdown", "text/x-markdown":
		sections, truncated, full = ExtractPlainTextWithin(doc.HTML, limits)
	default:
		return nil, false, fmt.Errorf("doc %s has content type %q: re-extraction is not supported", hash, doc.ContentType)
	}
	if len(sections) == 0 {
		return nil, false, fmt.Errorf("doc %s re-extracted to no readable text", hash)
	}
	size := 0
	for i := range sections {
		sections[i].Order = i
		size += utf8.RuneCountInString(sections[i].Text)
	}
	fullChars := utf8.RuneCountInString(full)
	if size <= doc.Size && fullChars <= doc.FullChars {
		return doc, false, nil
	}
	if !truncated && doc.Truncated && (doc.FullChars == 0 || doc.Size >= doc.FullChars) {
		// The stored text was complete, so the earlier flag came from image refs
		// dropped as SSRF bait; re-extraction does not resolve them again.
		truncated = true
	}
	oldRedactions := doc.Redactions
	oldMedium := doc.GuardMediumIgnored
	sizeBeforeHook := size
	doc.Sections, doc.Size, doc.Truncated, doc.FullChars = sections, size, truncated, fullChars
	if hook != nil {
		// The scan replaces the counter rather than adding to it: the same
		// medium findings are re-seen on every growth step. A hook that does not
		// scan (e.g. secret_guard=allow) leaves it at zero, and the stored count
		// still describes the unchanged text.
		doc.GuardMediumIgnored = 0
		extra, herr := hook(doc)
		if herr != nil {
			return nil, false, herr
		}
		if doc.GuardMediumIgnored == 0 {
			doc.GuardMediumIgnored = oldMedium
		}
		doc.Redactions = mergeRedactions(oldRedactions, extra)
		size = 0
		for i := range doc.Sections {
			size += utf8.RuneCountInString(doc.Sections[i].Text)
		}
		doc.Size = size
		// A redaction shrinks the stored text; full_chars describes the same
		// post-redaction text, so a complete snapshot still reports
		// size == full_chars.
		doc.FullChars = fullChars - (sizeBeforeHook - size)
		if doc.FullChars < size {
			doc.FullChars = size
		}
	}
	// Re-read right before the write: a concurrent fetch may have stored a
	// larger copy, or a different revision, while this one was extracting. A
	// revision change aborts the write — sections from the old html next to a
	// newer html/content_hash would be a mixed snapshot. The update itself is
	// conditioned on the version it read, so a writer landing in between fails
	// the write instead of being overwritten.
	if current, seq, pt, gerr := c.getDocByHashVersion(ctx, hash, false); gerr == nil {
		seqNo, primaryTerm = seq, pt
		if current.ContentHash != "" && doc.ContentHash != "" && current.ContentHash != doc.ContentHash {
			return current, false, nil
		}
		if keepsStored(current, doc) {
			return current, false, nil
		}
	}
	if err := c.updateDocText(ctx, hash, doc, seqNo, primaryTerm); err != nil {
		if isVersionConflict(err) {
			if current, gerr := c.GetDocByHash(ctx, hash); gerr == nil {
				return current, false, nil
			}
		}
		return nil, false, err
	}
	return doc, true, nil
}

// updateDocText replaces the stored blocks and their accounting (and the audit
// lists the re-extraction touched), leaving the raw html and the cache metadata
// untouched.
func (c *Client) updateDocText(ctx context.Context, hash string, doc *model.Doc, seqNo, primaryTerm int) error {
	fields := map[string]interface{}{
		"sections":             doc.Sections,
		"size":                 doc.Size,
		"full_chars":           doc.FullChars,
		"truncated":            doc.Truncated,
		"redactions":           doc.Redactions,
		"guard_medium_ignored": doc.GuardMediumIgnored,
	}
	body, _ := json.Marshal(map[string]interface{}{"doc": fields})
	params := opensearchapi.UpdateParams{Refresh: "wait_for"}
	if seqNo != 0 || primaryTerm != 0 {
		params.IfSeqNo = &seqNo
		params.IfPrimaryTerm = &primaryTerm
	}
	_, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.DocsIndexName,
		DocumentID: hash,
		Body:       bytes.NewReader(body),
		Params:     params,
	})
	if err != nil {
		return wrapWriteError("update doc text", err)
	}
	return nil
}
