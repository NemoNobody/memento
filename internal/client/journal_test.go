package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/model"
)

// journalInfraOK answers the infra probes as "pipeline and template already
// exist" so a test focuses on the journal call under test, not the setup.
func journalInfraOK(r recordedRequest) (int, string) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
		return http.StatusOK, `{"memento-journal-ts":{"processors":[]}}`
	case r.Method == http.MethodHead && strings.HasPrefix(r.Path, "/_index_template/"):
		return http.StatusOK, ""
	case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
		return http.StatusOK, `{"acknowledged":true}`
	}
	return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
}

// journalRoutesOK answers every journal route (infra probes, append, search)
// with neutral results — infra exists, append creates a fixed entry, search
// is empty — so main-index contract tests can pin main-doc behavior without
// spelling out the journal plumbing. A 500 sentinel means "not a journal
// route": the caller's own respond takes over.
func journalRoutesOK(r recordedRequest) (int, string) {
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-") && strings.Contains(r.Path, "/_doc"):
		// auto-id appends (POST /_doc) and preallocated-id appends
		// (POST /_doc/<id>, op_type=create — the doc-first write paths)
		return http.StatusOK, `{"_id":"journal-entry-1","result":"created"}`
	case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/memento-history-"):
		// deterministic-id appends (migration): PUT /<index>/_doc/<id>
		return http.StatusOK, `{"_id":"journal-entry-1","result":"created"}`
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-") && strings.HasSuffix(r.Path, "/_search"):
		return http.StatusOK, `{"took":1,"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`
	}
	return journalInfraOK(r)
}

// journalAppends returns the POST writes to ANY journal dated index.
// Pinning one month name flakes when a test run crosses the month boundary:
// the write lands in the next period's index.
// journalAppends returns writes to ANY journal dated index: POST /_doc
// (auto id) and PUT /_doc/<id> (deterministic id — migration and the
// doc-first write paths). Pinning one month name flakes when a test run
// crosses the month boundary: the write lands in the next period's index.
func journalAppends(fake *fakeOpenSearch) []recordedRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []recordedRequest
	for _, r := range fake.requests {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) &&
			strings.HasPrefix(r.Path, "/memento-history-") &&
			strings.Contains(r.Path, "/_doc") {
			out = append(out, r)
		}
	}
	return out
}

func TestJournalIndexNamePeriods(t *testing.T) {
	day := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	isoWeekFlip := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC) // ISO 2026-W53
	tests := []struct {
		period string
		at     time.Time
		want   string
	}{
		{"day", day, "memento-history-2026.11.09"},
		{"week", day, "memento-history-2026-w46"},
		{"week", isoWeekFlip, "memento-history-2026-w53"},
		{"month", day, "memento-history-2026.11"},
		{"quarter", day, "memento-history-2026-q4"},
		{"quarter", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "memento-history-2026-q1"},
		// quarter/year must partition on UTC, not the writer's zone: 00:30
		// at UTC+3 on Oct 1 is Sep 30 21:30 UTC — the previous quarter.
		{"quarter", time.Date(2026, 10, 1, 0, 30, 0, 0, time.FixedZone("UTC+3", 3*3600)), "memento-history-2026-q3"},
		{"year", day, "memento-history-2026"},
		{"bogus", day, "memento-history-2026.11"}, // config validates; raw calls default to month
	}
	for _, tc := range tests {
		if got := journalIndexName("memento", tc.period, tc.at); got != tc.want {
			t.Errorf("period %s at %s: got %s, want %s", tc.period, tc.at, got, tc.want)
		}
	}
}

func TestAppendJournalWritesToDatedIndexWithCreate(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/memento-history-2026.11/_doc" {
			return http.StatusOK, `{"_id":"entry-1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	ts := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	e := model.NewJournalEntry("k", "id-1", "proj", "config", model.OpWrite, "v", "notes", "tool:read_file", ts)
	id, err := c.AppendJournal(context.Background(), e)
	if err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if id != "entry-1" {
		t.Errorf("id = %q, want entry-1", id)
	}
	writes := fake.requestsFor(http.MethodPost, "/memento-history-2026.11/_doc")
	if len(writes) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(writes))
	}
	q := writes[0].Query
	if q.Get("op_type") != "create" {
		t.Errorf("op_type = %q, want create (appends must never overwrite)", q.Get("op_type"))
	}
	if q.Get("refresh") != "wait_for" {
		t.Errorf("refresh = %q, want wait_for (reads-after-write must see the entry)", q.Get("refresh"))
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(writes[0].Body), &body); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if body["fact_key"] != "k" || body["op"] != model.OpWrite || body["value"] != "v" {
		t.Errorf("journal body = %v", body)
	}
}

func TestJournalReadsCarryUnavailableFlags(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/memento-history-*/_search" {
			return http.StatusOK, `{"hits":{"total":{"value":1},"hits":[
				{"_index":"memento-history-2026.11","_id":"e1","_source":{
					"fact_key":"k","op":"write","value":"v","created_at":"2026-11-09T10:00:00Z"}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	entries, total, truncated, err := c.JournalEntries(context.Background(), "k", 5)
	if err != nil {
		t.Fatalf("JournalEntries: %v", err)
	}
	if total != 1 || truncated || len(entries) != 1 || entries[0].Value != "v" {
		t.Fatalf("entries=%v total=%d truncated=%v", entries, total, truncated)
	}
	searches := fake.requestsFor(http.MethodPost, "/memento-history-*/_search")
	if len(searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(searches))
	}
	if searches[0].Query.Get("ignore_unavailable") != "true" ||
		searches[0].Query.Get("allow_no_indices") != "true" {
		t.Error("journal search must carry ignore_unavailable+allow_no_indices (admin index deletes must not break reads)")
	}
}

func TestJournalMissingIndexReadsAsEmpty(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		return http.StatusNotFound, `{"status":404,"error":{"type":"index_not_found_exception"}}`
	})

	entries, total, _, err := c.JournalEntries(context.Background(), "k", 5)
	if err != nil {
		t.Fatalf("a store without a journal index must read as empty, got %v", err)
	}
	if total != 0 || len(entries) != 0 {
		t.Fatalf("entries=%v total=%d, want empty", entries, total)
	}
}

func TestHistoryInfoParsesAggregations(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v","category":"config","current_ref":"e2"}}`
		case r.Method == http.MethodPost && r.Path == "/memento-history-*/_search":
			return http.StatusOK, `{"hits":{"total":{"value":3},"hits":[]},
				"aggregations":{
					"writes":{"doc_count":2},
					"rediscoveries":{"doc_count":1},
					"first":{"value":1762665600000},
					"last":{"value":1762752000000}}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	info, err := c.HistoryInfo(context.Background(), "k")
	if err != nil {
		t.Fatalf("HistoryInfo: %v", err)
	}
	if info.Writes != 2 || info.Rediscoveries != 1 {
		t.Errorf("writes=%d rediscoveries=%d, want 2/1", info.Writes, info.Rediscoveries)
	}
	if info.CurrentRef != "e2" || info.Category != "config" {
		t.Errorf("current_ref=%q category=%q, want e2/config", info.CurrentRef, info.Category)
	}
	if info.FirstAt == nil || info.LastAt == nil {
		t.Fatal("first_at/last_at must be present when aggregations answer")
	}
}

func TestPlanJournalPrune(t *testing.T) {
	ids := []string{"new", "mid", "old", "cur"}
	// keep=1: the newest survives; the currentRef entry is ALWAYS spared even
	// when it is older than the kept window (it proves the current value).
	drop := planJournalPrune(ids, "cur", 1)
	want := []string{"mid", "old"}
	if len(drop) != len(want) || drop[0] != want[0] || drop[1] != want[1] {
		t.Errorf("drop = %v, want %v", drop, want)
	}
	if d := planJournalPrune(ids, "", 4); d != nil {
		t.Errorf("len <= keep must drop nothing, got %v", d)
	}
}

// TestAddCorrectionUpdatesMainBeforeJournaling pins the doc-first order:
// the scripted update lands first, the journal entry is indexed only after
// it was accepted — a refused update must leave no journal trace (the
// journal is a history of accepted state; phantom entries are an audit lie).
// The doc->entry link survives via the preallocated entry id.
func TestAddCorrectionUpdatesMainBeforeJournaling(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v1","category":"config","status":"verified"}}`
		case (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Path, "/memento-history-"):
			return http.StatusOK, `{"_id":"ignored","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})

	f := model.NewFact("k", "v2", "config", "p", "tool:read_file", "")
	res, err := c.Add(context.Background(), f)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Created {
		t.Error("created = true, want false (key existed)")
	}

	// The journal entry for v2 must exist, and the main update must point at it.
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]interface{}
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["value"] != "v2" || je["op"] != model.OpWrite {
		t.Errorf("journal entry = %v, want op=write value=v2", je)
	}

	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("main updates = %d, want 1", len(updates))
	}
	// The link: the update carries current_ref = the preallocated id, and
	// the entry is indexed under that same id (op_type=create).
	id := appends[0].Path[strings.LastIndex(appends[0].Path, "/")+1:]
	if id == "" || !strings.Contains(updates[0].Body, id) {
		t.Errorf("main update must set current_ref to the entry id %q, body=%s", id, updates[0].Body)
	}
	if strings.Contains(updates[0].Body, "history.add") {
		t.Error("main script must not append to the in-doc array anymore (the journal is the history)")
	}

	// Order: the main write precedes the journal append.
	appendedIdx, updatedIdx := -1, -1
	fake.mu.Lock()
	for i, r := range fake.requests {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Path, "/memento-history-") {
			appendedIdx = i
		}
		if r.Method == http.MethodPost && r.Path == "/memento/_update/k" {
			updatedIdx = i
		}
	}
	fake.mu.Unlock()
	if appendedIdx < 0 || updatedIdx < 0 || appendedIdx < updatedIdx {
		t.Errorf("journal append (idx %d) must follow the main write (idx %d)", appendedIdx, updatedIdx)
	}
}

func TestAddSameValueDoesNotAppendJournal(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v1","category":"config","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v1", "config", "p", "tool:read_file", "n")
	if _, err := c.Add(context.Background(), f); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if fake.sawAnyPrefix("/memento-history-") {
		t.Error("same-value add must not touch the journal (no new observation)")
	}
}

func TestMigrateFactMovesArrayToJournal(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/memento-history-"):
			return http.StatusOK, `{"_id":"entry","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v3", "config", "p", "s", "")
	f.History = []model.HistoryEntry{
		{Value: "v1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "a"},
		{Value: "v2", ObservedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Source: "b"},
	}
	appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MigrateFact: %v", err)
	}
	if appended != 3 {
		t.Errorf("appended = %d, want 3 (2 history + 1 current)", appended)
	}

	var oct, nov int
	fake.mu.Lock()
	for _, r := range fake.requests {
		if r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/memento-history-") {
			switch {
			case strings.HasPrefix(r.Path, "/memento-history-2026.10/_doc/"):
				oct++
			case strings.HasPrefix(r.Path, "/memento-history-2026.11/_doc/"):
				nov++
			}
		}
	}
	fake.mu.Unlock()
	if oct != 1 || nov != 2 {
		t.Errorf("entries by month: oct=%d nov=%d, want 1/2 (observed_at decides the index)", oct, nov)
	}

	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("fact updates = %d, want 1", len(updates))
	}
	if !strings.Contains(updates[0].Body, "remove('history')") || !strings.Contains(updates[0].Body, "current_ref") {
		t.Errorf("migration update must link current_ref and drop the array, body=%s", updates[0].Body)
	}
	// The link must be conditional: a concurrent Add that already set
	// current_ref carries a fresher ref; overwriting it would point the new
	// value at the old journal entry.
	if !strings.Contains(updates[0].Body, "current_ref == null || ctx._source.current_ref == ''") {
		t.Errorf("migration link must only set current_ref while empty, body=%s", updates[0].Body)
	}
}

func TestAddCreatesIndexThenJournalsNewFact(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusNotFound, `{"status":404,"error":{"type":"document_missing_exception"}}`
		case r.Method == http.MethodPut && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"created"}`
		case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-"):
			return http.StatusOK, `{"_id":"j1","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	f := model.NewFact("k", "v1", "config", "p", "s", "")
	res, err := c.Add(context.Background(), f)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !res.Created {
		t.Error("created = false, want true")
	}
	if !fake.sawAnyPrefix("/memento-history-") {
		t.Error("a new fact's birth value must be journaled")
	}
	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 || !strings.Contains(updates[0].Body, "current_ref") {
		t.Error("the created fact must be linked to its journal entry (current_ref)")
	}
}

// TestEnsureJournalInfraRetriesAfterFailure pins the infra-install caching
// contract: only SUCCESS is remembered. A failed first attempt (403,
// network blip) must not mark the infra as installed for the rest of the
// process — a long-lived MCP server would append journal entries forever
// without the template's mappings and the server-clock pipeline.
func TestEnsureJournalInfraRetriesAfterFailure(t *testing.T) {
	tplPuts := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusOK, `{"memento-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodHead && strings.HasPrefix(r.Path, "/_index_template/"):
			return http.StatusNotFound, "" // template missing
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/_index_template/"):
			tplPuts++
			if tplPuts == 1 {
				return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception","reason":"no permission"}}`
			}
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && r.Path == "/memento-history-2026.11/_doc":
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err == nil {
		t.Fatal("first append must fail while the template cannot be installed")
	}
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("second append must re-attempt the infra install: %v", err)
	}
	if tplPuts != 2 {
		t.Errorf("template PUTs = %d, want 2: a failed install is not a success", tplPuts)
	}
}

// TestPruneJournalFailsOnFactReadFailure pins that a transient Get failure
// must abort the prune: swallowing it reads as "no current_ref to spare",
// silently disarming the protection of the entry that carries the current
// value.
func TestPruneJournalFailsOnFactReadFailure(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodPost && r.Path == "/memento-history-*/_search":
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":2,"relation":"eq"},"hits":[
				{"_index":"memento-history-2026.11","_id":"a","_source":{"fact_key":"k","op":"write","value":"v1","created_at":"2026-11-01T10:00:00Z"}},
				{"_index":"memento-history-2026.11","_id":"b","_source":{"fact_key":"k","op":"write","value":"v0","created_at":"2026-10-01T10:00:00Z"}}]}}`
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusInternalServerError, `{"status":500,"error":"transient"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	if _, err := c.PruneJournal(context.Background(), "k", 1); err == nil {
		t.Fatal("a transient fact-read failure must not disarm the current_ref protection")
	} else if !strings.Contains(err.Error(), "read fact") {
		t.Errorf("the error must name the fact read: %v", err)
	}
	if fake.sawAnyPrefix("/_bulk") {
		t.Error("no bulk deletes may be issued when the fact read failed")
	}
}

// TestPruneJournalCapMessageIsHonestOnGTE: track_total_hits clips the
// reported total at 10000 (relation=gte); the cap message must not report
// the clipped number as an exact count.
func TestPruneJournalCapMessageIsHonestOnGTE(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/memento-history-*/_search" {
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":10000,"relation":"gte"},"hits":[
				{"_index":"memento-history-2026.11","_id":"a","_source":{"fact_key":"k","op":"write","value":"v1","created_at":"2026-11-01T10:00:00Z"}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	_, err := c.PruneJournal(context.Background(), "k", 1)
	if err == nil {
		t.Fatal("exceeding the readable cap must be an error")
	}
	if !strings.Contains(err.Error(), "at least 10000") {
		t.Errorf("a gte total must be reported as a bound: %v", err)
	}
}

// TestMigrateFactRerunCollidesOnDeterministicIDs pins the partial-failure
// recovery contract: the migration appends under ids derived from the entry
// content, so the rerun the CLI instructs collides with the entries the
// first run wrote (409 = success) instead of duplicating them.
func TestMigrateFactRerunCollidesOnDeterministicIDs(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	updateCalls := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/memento-history-"):
			mu.Lock()
			defer mu.Unlock()
			if seen[r.Path] {
				return http.StatusConflict, `{"error":{"type":"version_conflict_engine_exception","reason":"document already exists"},"status":409}`
			}
			seen[r.Path] = true
			return http.StatusOK, `{"_id":"x","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateCalls++
			if updateCalls == 1 {
				return http.StatusInternalServerError, `{"status":500,"error":"transient link failure"}`
			}
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	mk := func() *model.Fact {
		f := model.NewFact("k", "v3", "config", "p", "s", "")
		f.ObservedAt = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		f.History = []model.HistoryEntry{
			{Value: "v1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "a"},
		}
		return f
	}
	// First run: the appends land, the link update fails (partial failure).
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("first run must report the link failure")
	}
	// The rerun re-appends the same logical entries: the ids collide, and
	// the collision is success, not duplication.
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("rerun must succeed on collisions: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("journal docs = %d, want 2: the rerun must not duplicate entries", len(seen))
	}
}

// TestMigrateFactZeroTimestampRerunCollides: legacy facts with NO
// timestamps at all (observed_at/updated_at both zero) must stay rerun-safe
// too: the id hashes a zero-time marker and the target index is derived
// from the SAME stable time — not the rerun's clock — so the second run
// collides with the first in the same doc of the same index even when the
// runs land in different periods.
func TestMigrateFactZeroTimestampRerunCollides(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	updateCalls := 0
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/memento-history-"):
			mu.Lock()
			defer mu.Unlock()
			if seen[r.Path] {
				return http.StatusConflict, `{"error":{"type":"version_conflict_engine_exception","reason":"document already exists"},"status":409}`
			}
			seen[r.Path] = true
			return http.StatusOK, `{"_id":"x","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateCalls++
			if updateCalls == 1 {
				return http.StatusInternalServerError, `{"status":500,"error":"transient link failure"}`
			}
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	mk := func() *model.Fact {
		f := model.NewFact("k", "v2", "config", "p", "s", "")
		f.ObservedAt = time.Time{}
		f.UpdatedAt = time.Time{}
		f.History = []model.HistoryEntry{{Value: "v1", Source: "a"}} // no timestamp
		return f
	}
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("first run must report the link failure")
	}
	// A rerun after the month flipped: the display ts (now-fallback) moved
	// to a new period, the stable id and index must not have.
	if _, err := c.MigrateFact(context.Background(), mk(), time.Date(2026, 12, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("rerun must succeed on collisions: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("journal docs = %d, want 2: zero-timestamp rerun across a period boundary must not duplicate", len(seen))
	}
}

// TestAddCorrectionJournalsDocFactID: the journal entry of a correction
// carries the doc's fact_id (the identity that survives), not the fresh
// uuid of the caller's in-memory fact.
func TestAddCorrectionJournalsDocFactID(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","fact_id":"doc-1","value":"old","category":"config","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Path + `"}`
	})
	f := model.NewFact("k", "new", "config", "p", "s", "")
	if _, err := c.Add(context.Background(), f); err != nil {
		t.Fatalf("Add: %v", err)
	}
	writes := journalAppends(fake)
	if len(writes) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(writes))
	}
	if !strings.Contains(writes[0].Body, `"fact_id":"doc-1"`) {
		t.Errorf("the correction entry must carry the doc's fact_id, body=%s", writes[0].Body)
	}
}

// TestJournalEntriesSortedByStoreClockFirst: the pipeline stamps ingest_at
// with the store clock so ordering survives writer clock skew; reads must
// honor that clock, not the clients'.
func TestJournalEntriesSortedByStoreClockFirst(t *testing.T) {
	body := buildJournalEntriesBody("k", 5)
	sortKeys, ok := body["sort"].([]map[string]interface{})
	if !ok || len(sortKeys) != 2 {
		t.Fatalf("sort = %v, want two keys", body["sort"])
	}
	if _, ok := sortKeys[0]["ingest_at"]; !ok {
		t.Errorf("first sort key must be the pipeline's server clock: %v", sortKeys[0])
	}
	if _, ok := sortKeys[1]["created_at"]; !ok {
		t.Errorf("second sort key must be created_at: %v", sortKeys[1])
	}
}

// TestAppendJournalPinsPipelineOnExistingIndex: an append pins the ts
// pipeline on the journal index via one idempotent settings PUT — the heal
// path for indices auto-created while infra install was broken (entries
// without ingest_at sort last and become the first prune victims). One PUT
// per index per process.
func TestAppendJournalPinsPipelineOnExistingIndex(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		if r.Method == http.MethodPost && r.Path == "/memento-history-2026.11/_doc" {
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal #2: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/memento-history-2026.11/_settings")
	if len(puts) != 1 {
		t.Errorf("settings PUTs = %d, want 1 (pinned once per index per process)", len(puts))
	}
	if !strings.Contains(puts[0].Body, "memento-journal-ts") {
		t.Errorf("settings body must wire the ts pipeline: %s", puts[0].Body)
	}
}

// TestScanAllFactsEmptyProjectScansAll: the default migrate-history scope
// must scan every project. A term{"project":""} filter matches no document
// (writers always store a non-empty project), so an empty project omits the
// scope filter instead of emptying it — otherwise the one-time migration
// silently no-ops and reports "scanned: 0".
func TestScanAllFactsEmptyProjectScansAll(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/memento/_search" {
			return http.StatusOK, `{"took":1,"hits":{"total":{"value":1,"relation":"eq"},"hits":[
				{"_index":"memento","_id":"k","_source":{"fact_id":"i","key":"k","project":"p1","value":"v","category":"config","status":"verified"}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	facts, _, err := c.ScanAllFacts(context.Background(), "")
	if err != nil {
		t.Fatalf("ScanAllFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want 1: an empty project must scan all projects", len(facts))
	}
	reqs := fake.requestsFor(http.MethodPost, "/memento/_search")
	if len(reqs) != 1 {
		t.Fatalf("searches = %d, want 1", len(reqs))
	}
	if strings.Contains(reqs[0].Body, `"project"`) {
		t.Errorf("empty-project scan must omit the project term filter, body=%s", reqs[0].Body)
	}
	if !strings.Contains(reqs[0].Body, "match_all") {
		t.Errorf("empty-project scan must match all documents, body=%s", reqs[0].Body)
	}
	if _, _, err := c.ScanAllFacts(context.Background(), "p1"); err != nil {
		t.Fatalf("ScanAllFacts scoped: %v", err)
	}
	reqs = fake.requestsFor(http.MethodPost, "/memento/_search")
	if len(reqs) != 2 {
		t.Fatalf("searches = %d, want 2", len(reqs))
	}
	if !strings.Contains(reqs[1].Body, `"term":{"project":"p1"}`) {
		t.Errorf("scoped scan must keep the project term filter, body=%s", reqs[1].Body)
	}
}

// TestMigrateFactLinkedWithArrayAppendsOnlyHistory: a fact that already
// carries current_ref but still carries the legacy array gets the array
// entries and nothing else — no fabricated duplicate of the current value,
// no re-link (the script keeps the live ref) — and its array is removed.
func TestMigrateFactLinkedWithArrayAppendsOnlyHistory(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/memento-history-"):
			return http.StatusOK, `{"_id":"entry","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})
	f := model.NewFact("k", "v-current", "config", "p", "s", "")
	f.CurrentRef = "live-ref"
	f.History = []model.HistoryEntry{
		{Value: "v1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "a"},
	}
	appended, err := c.MigrateFact(context.Background(), f, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MigrateFact: %v", err)
	}
	if appended != 1 {
		t.Errorf("appended = %d, want 1 (array entry only, the linked current value is already journaled)", appended)
	}
	appends := journalAppends(fake)
	if len(appends) != 1 {
		t.Fatalf("journal writes = %d, want 1", len(appends))
	}
	if strings.Contains(appends[0].Body, "v-current") {
		t.Errorf("must not re-journal the linked current value, body=%s", appends[0].Body)
	}
	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("fact updates = %d, want 1 (the array must still be removed)", len(updates))
	}
	if !strings.Contains(updates[0].Body, "current_ref == null") {
		t.Errorf("link script must stay conditional (live ref untouched), body=%s", updates[0].Body)
	}

	// A fully migrated fact (linked, no array) is a no-op: no further
	// appends, no further update request.
	full := model.NewFact("k2", "v", "config", "p", "s", "")
	full.CurrentRef = "live-ref"
	if n, err := c.MigrateFact(context.Background(), full, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err != nil || n != 0 {
		t.Errorf("fully migrated fact: n=%d err=%v, want 0/no error", n, err)
	}
	if got := len(journalAppends(fake)); got != 1 {
		t.Errorf("journal writes after no-op = %d, want still 1", got)
	}
	if got := len(fake.requestsFor(http.MethodPost, "/memento/_update/k")); got != 1 {
		t.Errorf("updates after no-op = %d, want still 1", got)
	}
}
