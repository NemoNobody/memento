package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-") && strings.HasSuffix(r.Path, "/_doc"):
		return http.StatusOK, `{"_id":"journal-entry-1","result":"created"}`
	case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-") && strings.HasSuffix(r.Path, "/_search"):
		return http.StatusOK, `{"took":1,"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`
	}
	return journalInfraOK(r)
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
		{"week", day, "memento-history-2026-W46"},
		{"week", isoWeekFlip, "memento-history-2026-W53"},
		{"month", day, "memento-history-2026.11"},
		{"quarter", day, "memento-history-2026-Q4"},
		{"quarter", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "memento-history-2026-Q1"},
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

func TestAddCorrectionAppendsJournalBeforeMain(t *testing.T) {
	var journalID = "j-42"
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if code, body := journalInfraOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v1","category":"config","status":"verified"}}`
		case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-"):
			return http.StatusOK, `{"_id":"` + journalID + `","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	})

	f := model.NewFact("k", "v2", "config", "p", "tool:read_file", "")
	created, err := c.Add(context.Background(), f)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if created {
		t.Error("created = true, want false (key existed)")
	}

	// The journal entry for v2 must exist, and the main update must point at it.
	appends := fake.requestsFor(http.MethodPost, "/memento-history-2026.11/_doc")
	if len(appends) == 0 {
		appends = fake.requestsFor(http.MethodPost, "/memento-history-"+time.Now().UTC().Format("2006.01")+"/_doc")
	}
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
	if !strings.Contains(updates[0].Body, journalID) {
		t.Errorf("main update must set current_ref=%q, body=%s", journalID, updates[0].Body)
	}
	if strings.Contains(updates[0].Body, "history.add") {
		t.Error("main script must not append to the in-doc array anymore (the journal is the history)")
	}

	// Order: the journal append precedes the main write.
	appendedIdx, updatedIdx := -1, -1
	fake.mu.Lock()
	for i, r := range fake.requests {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-") {
			appendedIdx = i
		}
		if r.Method == http.MethodPost && r.Path == "/memento/_update/k" {
			updatedIdx = i
		}
	}
	fake.mu.Unlock()
	if appendedIdx < 0 || updatedIdx < 0 || appendedIdx > updatedIdx {
		t.Errorf("journal append (idx %d) must precede the main write (idx %d)", appendedIdx, updatedIdx)
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
		case r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-"):
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
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/memento-history-") {
			switch r.Path {
			case "/memento-history-2026.10/_doc":
				oct++
			case "/memento-history-2026.11/_doc":
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
	created, err := c.Add(context.Background(), f)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !created {
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
