package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/memento/internal/model"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// The journal is the append-only log of accepted fact values. Every accepted
// write appends one entry (op_type=create, so appends never race: two racing
// writers both land), and the main fact doc keeps current_ref = the journal
// id of its current value, so "the journal contains the current value" is a
// fact, not an assumption about write order.
//
// Journal indices are date-partitioned: <prefix>-history-<period-date>
// (memento-history-2026.11 by default). The period is config because index
// proliferation is cluster load, and the same knob is the admin's pruning
// granularity: delete a whole old index, history for every key ages out.

// journalIndexName derives the journal index for a period and a time. The
// date is part of the name (an undated index is one infinitely growing
// index, which defeats the point). Names: 2026-W02 (ISO week), 2026-Q1,
// 2026.11, 2026.11.09, 2026.
func journalIndexName(prefix, period string, t time.Time) string {
	var date string
	switch period {
	case "week":
		y, w := t.UTC().ISOWeek()
		date = fmt.Sprintf("%d-W%02d", y, w)
	case "quarter":
		date = fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
	case "month":
		date = t.UTC().Format("2006.01")
	case "day":
		date = t.UTC().Format("2006.01.02")
	case "year":
		date = strconv.Itoa(t.Year())
	default:
		// Config validates the period at load; a raw call with a bad period
		// must not invent a naming scheme: month is the documented default.
		date = t.UTC().Format("2006.01")
	}
	return prefix + "-history-" + date
}

// journalWildcard matches every journal index of a prefix. Reads carry
// ignore_unavailable + allow_no_indices so deleting old indices (the admin
// pruning path) never breaks reads.
func journalWildcard(prefix string) string { return prefix + "-history-*" }

// journalTemplateName is the composable index template applied to every
// journal index at auto-creation. It is app-managed and idempotent: explicit
// mappings stop a first write from auto-mapping created_at as text.
func journalTemplateName(prefix string) string { return prefix + "-journal" }

// journalPipelineName is the ingest pipeline stamping entries with the
// server clock. Writer clocks skew — on a shared store the writers are
// different machines — so entry ordering follows the store's clock, not the
// clients'. The template wires it as the index default_pipeline.
func journalPipelineName(prefix string) string { return prefix + "-journal-ts" }

// buildJournalTemplateBody builds the composable index template: explicit
// mappings (keyword for grouping fields, text for searched values, date for
// timestamps) and the ts pipeline. replicas 0: single-node personal installs
// are the primary shape, and a journal replica would sit unassigned forever.
func buildJournalTemplateBody(prefix string) map[string]interface{} {
	return map[string]interface{}{
		"index_patterns": []string{prefix + "-history-*"},
		"priority":       10,
		"template": map[string]interface{}{
			"settings": map[string]interface{}{
				"index.default_pipeline":   journalPipelineName(prefix),
				"index.number_of_replicas": 0,
			},
			"mappings": map[string]interface{}{
				"properties": map[string]interface{}{
					"fact_key":      map[string]interface{}{"type": "keyword"},
					"fact_id":       map[string]interface{}{"type": "keyword"},
					"project":       map[string]interface{}{"type": "keyword"},
					"category":      map[string]interface{}{"type": "keyword"},
					"op":            map[string]interface{}{"type": "keyword"},
					"value":         map[string]interface{}{"type": "text"},
					"notes":         map[string]interface{}{"type": "text"},
					"source":        map[string]interface{}{"type": "keyword"},
					"created_at":    map[string]interface{}{"type": "date"},
					"ingest_at":     map[string]interface{}{"type": "date"},
					"author":        map[string]interface{}{"type": "keyword"},
					"author_source": map[string]interface{}{"type": "keyword"},
					"model":         map[string]interface{}{"type": "keyword"},
					"host":          map[string]interface{}{"type": "keyword"},
					"session":       map[string]interface{}{"type": "keyword"},
				},
			},
		},
	}
}

// buildJournalPipelineBody stamps entries with the store clock on ingest.
func buildJournalPipelineBody(prefix string) map[string]interface{} {
	return map[string]interface{}{
		"description": "stamp " + prefix + " journal entries with the server clock",
		"processors": []map[string]interface{}{
			{"set": map[string]interface{}{
				"field": "ingest_at",
				"value": "{{_ingest.timestamp}}",
			}},
		},
	}
}

// ensureJournalInfra installs the journal pipeline and index template.
// It runs once per process (the journal write path is the only caller):
// PUT pipeline and the template existence check are idempotent, and the
// create-race between processes is benign.
func (c *Client) ensureJournalInfra(ctx context.Context) error {
	var err error
	c.journalOnce.Do(func() {
		pipeline := journalPipelineName(c.cfg.IndexName)
		_, getErr := c.api.Ingest.Get(ctx, &opensearchapi.IngestGetReq{PipelineIDs: []string{pipeline}})
		if getErr != nil {
			body, _ := json.Marshal(buildJournalPipelineBody(c.cfg.IndexName))
			if _, mkErr := c.api.Ingest.Create(ctx, opensearchapi.IngestCreateReq{
				PipelineID: pipeline,
				Body:       strings.NewReader(string(body)),
			}); mkErr != nil && !statusIs(mkErr, 409) { // lost the create race: it exists now
				err = wrapWriteError("create journal pipeline", mkErr)
			}
		}

		tpl := journalTemplateName(c.cfg.IndexName)
		_, exErr := c.api.IndexTemplate.Exists(ctx, opensearchapi.IndexTemplateExistsReq{IndexTemplate: tpl})
		if exErr != nil {
			body, _ := json.Marshal(buildJournalTemplateBody(c.cfg.IndexName))
			if _, mkErr := c.api.IndexTemplate.Create(ctx, opensearchapi.IndexTemplateCreateReq{
				IndexTemplate: tpl,
				Body:          strings.NewReader(string(body)),
			}); mkErr != nil {
				err = wrapWriteError("create journal template", mkErr)
			}
		}
	})
	return err
}

// statusIs reports whether a typed API error carries an HTTP status code
// (StringError/StructError expose it; transport errors do not).
func statusIs(err error, code int) bool {
	var se *opensearch.StringError
	if errors.As(err, &se) {
		return se.Status == code
	}
	var ste *opensearch.StructError
	if errors.As(err, &ste) {
		return ste.Status == code
	}
	return false
}

// AppendJournal appends one accepted entry (op_type=create: appends never
// overwrite each other) and returns the entry id.
func (c *Client) AppendJournal(ctx context.Context, e *model.JournalEntry) (string, error) {
	if !model.ValidOp(e.Op) {
		return "", fmt.Errorf("journal: unknown op %q", e.Op)
	}
	if err := c.ensureJournalInfra(ctx); err != nil {
		return "", err
	}
	index := journalIndexName(c.cfg.IndexName, c.cfg.HistoryIndexPeriod, e.CreatedAt)
	body, _ := json.Marshal(e)
	resp, err := c.api.Index(ctx, opensearchapi.IndexReq{
		Index: index,
		Body:  strings.NewReader(string(body)),
		Params: opensearchapi.IndexParams{
			Refresh: "wait_for",
			OpType:  "create",
		},
	})
	if err != nil {
		return "", wrapWriteError("journal append", err)
	}
	return resp.ID, nil
}

// buildJournalEntriesBody builds the journal read: one fact key, newest
// first, bounded size.
func buildJournalEntriesBody(key string, limit int) map[string]interface{} {
	size := limit
	if size <= 0 || size > NavSizeCap {
		size = NavSizeCap
	}
	return map[string]interface{}{
		"query": map[string]interface{}{
			"term": map[string]interface{}{"fact_key": key},
		},
		"sort": []map[string]interface{}{
			{"created_at": map[string]interface{}{"order": "desc"}},
		},
		"size": size,
	}
}

// journalSearch runs a journal query across the wildcard with the flags
// that make old-index deletes invisible to readers. A 404 (no journal index
// exists yet: a store that never appended) means "empty journal", not an
// error — the same answer the wildcard+allow_no_indices flags give in the
// real cluster.
func (c *Client) journalSearch(ctx context.Context, body map[string]interface{}) (*opensearchapi.SearchResp, error) {
	data, _ := json.Marshal(body)
	yes := true
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{journalWildcard(c.cfg.IndexName)},
		Body:    strings.NewReader(string(data)),
		Params: opensearchapi.SearchParams{
			IgnoreUnavailable: &yes,
			AllowNoIndices:    &yes,
		},
	})
	if err != nil {
		if isNotFound(err) {
			return &opensearchapi.SearchResp{}, nil
		}
		return nil, fmt.Errorf("journal read: %w", err)
	}
	return resp, nil
}

// JournalEntries reads the newest journal entries for a key. total is the
// cluster-side entry count (the honest truncation signal).
func (c *Client) JournalEntries(ctx context.Context, key string, limit int) ([]model.JournalEntry, int, bool, error) {
	resp, err := c.journalSearch(ctx, buildJournalEntriesBody(key, limit))
	if err != nil {
		return nil, 0, false, err
	}
	total := resp.Hits.Total.Value
	entries := make([]model.JournalEntry, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var e model.JournalEntry
		if err := json.Unmarshal(hit.Source, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, total, total > len(entries), nil
}

// buildJournalInfoBody builds the per-key history aggregation: entry counts
// per operation and the surviving journal's time range. size 0: the answer
// is the aggregation, not the documents.
func buildJournalInfoBody(key string) map[string]interface{} {
	return map[string]interface{}{
		"size": 0,
		"query": map[string]interface{}{
			"term": map[string]interface{}{"fact_key": key},
		},
		"aggs": map[string]interface{}{
			"writes": map[string]interface{}{
				"filter": map[string]interface{}{"term": map[string]interface{}{"op": model.OpWrite}},
			},
			"rediscoveries": map[string]interface{}{
				"filter": map[string]interface{}{"term": map[string]interface{}{"op": model.OpRediscovery}},
			},
			"first": map[string]interface{}{"min": map[string]interface{}{"field": "created_at"}},
			"last":  map[string]interface{}{"max": map[string]interface{}{"field": "created_at"}},
		},
	}
}

// HistoryInfo answers the agent's fast-overview question: has this fact
// changed, how often, was it re-discovered, and what is the surviving
// journal's time range. Counts are LOWER BOUNDS: an admin deleting old
// journal indices removes entries the app cannot know about.
func (c *Client) HistoryInfo(ctx context.Context, key string) (*model.HistoryInfo, error) {
	info := &model.HistoryInfo{Key: key}
	fact, err := c.Get(ctx, key)
	if err == nil {
		info.Category = fact.Category
		info.CurrentRef = fact.CurrentRef
	} else if !strings.Contains(err.Error(), ErrNotFound.Error()) {
		return nil, err
	}

	resp, err := c.journalSearch(ctx, buildJournalInfoBody(key))
	if err != nil {
		return nil, fmt.Errorf("history info: %w", err)
	}
	aggs := map[string]struct {
		DocCount float64  `json:"doc_count"`
		Value    *float64 `json:"value"`
	}{}
	if len(resp.Aggregations) > 0 {
		if err := json.Unmarshal(resp.Aggregations, &aggs); err != nil {
			return nil, fmt.Errorf("history info: decode aggregations: %w", err)
		}
	}
	info.Writes = int(aggs["writes"].DocCount)
	info.Rediscoveries = int(aggs["rediscoveries"].DocCount)
	if v := aggs["first"].Value; v != nil {
		t := time.UnixMilli(int64(*v)).UTC()
		info.FirstAt = &t
	}
	if v := aggs["last"].Value; v != nil {
		t := time.UnixMilli(int64(*v)).UTC()
		info.LastAt = &t
	}
	return info, nil
}

// planJournalPrune decides which journal entries to drop. ids arrive
// newest-first; the newest `keep` are retained, and the entry whose id ==
// currentRef is ALWAYS retained (it is the journal's proof of the current
// value; deleting it breaks the "journal contains the current" invariant
// even when it is older than the kept window).
func planJournalPrune(ids []string, currentRef string, keep int) []string {
	if len(ids) <= keep {
		return nil
	}
	drop := make([]string, 0, len(ids)-keep)
	for i, id := range ids {
		if i < keep || id == currentRef {
			continue
		}
		drop = append(drop, id)
	}
	return drop
}

// PruneJournal drops journal entries beyond the newest keep for a key,
// always sparing the current_ref entry. It returns the number of deleted
// entries. Destructive: the CLI (--yes) / MCP (confirm) barrier must
// precede it.
func (c *Client) PruneJournal(ctx context.Context, key string, keep int) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("prune: keep must be >= 0, got %d", keep)
	}
	resp, err := c.journalSearch(ctx, buildJournalEntriesBody(key, NavSizeCap))
	if err != nil {
		return 0, err
	}
	if resp.Hits.Total.Value > len(resp.Hits.Hits) {
		return 0, fmt.Errorf("prune journal: %d entries for %q exceeds the readable cap %d", resp.Hits.Total.Value, key, NavSizeCap)
	}
	if len(resp.Hits.Hits) <= keep {
		return 0, nil
	}
	ref := ""
	if fact, err := c.Get(ctx, key); err == nil {
		ref = fact.CurrentRef
	}

	ids := make([]string, 0, len(resp.Hits.Hits))
	indexByID := map[string]string{}
	for _, h := range resp.Hits.Hits {
		ids = append(ids, h.ID)
		indexByID[h.ID] = h.Index
	}
	drop := planJournalPrune(ids, ref, keep)
	if len(drop) == 0 {
		return 0, nil
	}

	var ndjson strings.Builder
	for _, id := range drop {
		line, _ := json.Marshal(map[string]interface{}{
			"delete": map[string]interface{}{"_index": indexByID[id], "_id": id},
		})
		ndjson.Write(append(line, '\n'))
	}
	bulk, err := c.api.Bulk(ctx, opensearchapi.BulkReq{
		Body:   strings.NewReader(ndjson.String()),
		Params: opensearchapi.BulkParams{Refresh: "wait_for"},
	})
	if err != nil {
		return 0, wrapWriteError("prune journal", err)
	}
	if bulk.Errors {
		return 0, fmt.Errorf("prune journal: some deletes failed")
	}
	return len(drop), nil
}

// journalOnce lives on the Client (see opensearch.go); journal infra is
// installed idempotently on the first append.

// MigrateFact moves one fact's in-doc history array into the journal
// (big-bang migration, one-time): history entries become op=write entries
// in oldest-first order, the current value gets its own entry, current_ref
// points at it, and the array is removed from _source. It returns the
// number of journal entries appended.
func (c *Client) MigrateFact(ctx context.Context, f *model.Fact, now time.Time) (int, error) {
	if f.CurrentRef != "" {
		return 0, nil // already migrated (journal-first facts have a ref)
	}
	appended := 0
	for _, h := range f.History {
		ts := h.ObservedAt
		if ts.IsZero() {
			ts = f.UpdatedAt
		}
		if ts.IsZero() {
			ts = now
		}
		e := model.NewJournalEntry(f.Key, f.FactID, f.Project, f.Category, model.OpWrite, h.Value, "", h.Source, ts)
		if _, err := c.AppendJournal(ctx, e); err != nil {
			return appended, fmt.Errorf("migrate %q: history entry: %w", f.Key, err)
		}
		appended++
	}
	ts := f.ObservedAt
	if ts.IsZero() {
		ts = f.UpdatedAt
	}
	if ts.IsZero() {
		ts = now
	}
	cur := model.NewJournalEntry(f.Key, f.FactID, f.Project, f.Category, model.OpWrite, f.Value, f.Notes, f.Source, ts)
	ref, err := c.AppendJournal(ctx, cur)
	if err != nil {
		return appended, fmt.Errorf("migrate %q: current entry: %w", f.Key, err)
	}
	appended++

	body, _ := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang": "painless",
			"source": `
ctx._source.current_ref = params.ref;
ctx._source.remove('history');
ctx._source.updated_at = params.updated_at;
`,
			"params": map[string]interface{}{
				"ref":        ref,
				"updated_at": now.UTC().Format(time.RFC3339),
			},
		},
	})
	if _, err := c.api.Update(ctx, opensearchapi.UpdateReq{
		Index:      c.cfg.IndexName,
		DocumentID: f.Key,
		Body:       strings.NewReader(string(body)),
		Params:     opensearchapi.UpdateParams{Refresh: "wait_for"},
	}); err != nil {
		return appended, fmt.Errorf("migrate %q: link current_ref: %w", f.Key, err)
	}
	return appended, nil
}

// ScanAllFacts returns every fact (size cap NavSizeCap) for migration
// planning and execution; project == "" scans all projects.
func (c *Client) ScanAllFacts(ctx context.Context, project string) ([]model.Fact, SearchMeta, error) {
	return c.search(ctx, buildProjectBody(project, ""))
}
