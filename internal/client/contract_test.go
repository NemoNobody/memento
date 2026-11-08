package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"

	"github.com/opensearch-project/opensearch-go/v4"
)

// recordedRequest is one HTTP request the client made to the fake cluster.
type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

// fakeOpenSearch stands in for a real cluster. It lets tests assert the HTTP
// contract — which endpoints are called and with what parameters — which is
// exactly where the read-after-write and read-only bugs lived.
type fakeOpenSearch struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	respond func(r recordedRequest) (status int, body string)
}

// newFakeClient starts a fake cluster and returns a client pointed at it.
func newFakeClient(t *testing.T, respond func(r recordedRequest) (int, string)) (*Client, *fakeOpenSearch) {
	return newFakeClientCfg(t, nil, respond)
}

// newFakeClientCfg is newFakeClient with a hook to override config values
// (e.g. HistoryLimit) after the defaults are applied.
func newFakeClientCfg(t *testing.T, mutate func(*config.Config),
	respond func(r recordedRequest) (int, string)) (*Client, *fakeOpenSearch) {
	t.Helper()

	fake := &fakeOpenSearch{respond: respond}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Body:   string(body),
		}

		fake.mu.Lock()
		fake.requests = append(fake.requests, rec)
		fake.mu.Unlock()

		status, respBody := respond(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(fake.Close)

	cfg := &config.Config{
		OpenSearchURL:      fake.URL,
		IndexName:          "memento",
		OpenSearchPassword: "testpw",
		InsecureSkipVerify: true,
	}
	if mutate != nil {
		mutate(cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, fake
}

// requestsFor returns recorded requests matching a method and path.
func (f *fakeOpenSearch) requestsFor(method, path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// sawAnyPrefix reports whether any request path starts with prefix.
func (f *fakeOpenSearch) sawAnyPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.HasPrefix(r.Path, prefix) {
			return true
		}
	}
	return false
}

// TestIsNotFound covers both error shapes OpenSearch uses for a missing document.
// The first fix only handled StringError and silently missed the structured form,
// so both are pinned here.
func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"string error 404",
			&opensearch.StringError{Status: 404, Err: "not found"},
			true,
		},
		{
			"struct error 404",
			&opensearch.StructError{
				Status: 404,
				Err:    opensearch.Err{Type: "document_missing_exception", Reason: "document missing"},
			},
			true,
		},
		{"string error 500", &opensearch.StringError{Status: 500, Err: "boom"}, false},
		{"struct error 429", &opensearch.StructError{Status: 429, Err: opensearch.Err{Type: "cluster_block_exception"}}, false},
		{"plain error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		if got := isNotFound(tc.err); got != tc.want {
			t.Errorf("%s: isNotFound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEnsureIndexDoesNotWriteWhenIndexExists pins the fix for a bug found while
// testing read-only behaviour: creating the index unconditionally meant a
// read-only index broke read commands too, because the failed create happened
// before any read.
func TestEnsureIndexDoesNotWriteWhenIndexExists(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodHead && r.Path == "/memento" {
			return http.StatusOK, ""
		}
		return http.StatusTooManyRequests,
			`{"error":{"type":"cluster_block_exception","reason":"index is read-only"},"status":429}`
	})

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex on an existing index: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/memento"); len(puts) != 0 {
		t.Errorf("EnsureIndex wrote to an existing index (%d PUT requests); "+
			"a read-only index would break read commands", len(puts))
	}
}

// TestEnsureIndexCreatesWhenMissing is the counterpart: a missing index must
// still be created.
func TestEnsureIndexCreatesWhenMissing(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead:
			return http.StatusNotFound, `{"status":404,"error":"index_not_found_exception"}`
		case r.Method == http.MethodPut && r.Path == "/memento":
			return http.StatusOK, `{"acknowledged":true}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/memento"); len(puts) != 1 {
		t.Fatalf("expected exactly one create, got %d", len(puts))
	}
}

// TestEnsureIndexToleratesCreateRace pins the TOCTOU fix: two fresh clients
// (two Zed windows, CLI + MCP) can both see HEAD 404 and race to create; the
// loser's resource_already_exists_exception is a success, not a tool failure.
func TestEnsureIndexToleratesCreateRace(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead:
			return http.StatusNotFound, `{"status":404,"error":"index_not_found_exception"}`
		case r.Method == http.MethodPut && r.Path == "/memento":
			return http.StatusBadRequest,
				`{"error":{"type":"resource_already_exists_exception","reason":"index [memento/uuid] already exists"},"status":400}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("losing the create race must be success, got: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/memento"); len(puts) != 1 {
		t.Fatalf("expected exactly one create attempt, got %d", len(puts))
	}
}

// TestGetUsesRealtimeDocumentAPI pins the read-after-write fix: reads must use the
// realtime document endpoint, not a search (which lags behind the refresh).
func TestGetUsesRealtimeDocumentAPI(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/memento/_doc/some:key" {
			return http.StatusOK, `{"_index":"memento","_id":"some:key","found":true,
				"_source":{"key":"some:key","value":"v","category":"config","project":"p","status":"verified"}}`
		}
		return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
	})

	f, err := c.Get(context.Background(), "some:key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if f.Value != "v" {
		t.Errorf("value = %q, want v", f.Value)
	}
	if fake.sawAnyPrefix("/memento/_search") {
		t.Error("Get used the search API; it must use the realtime document API " +
			"or reads right after a write will miss the document")
	}
}

// TestAddCorrectionUsesScriptedUpdate pins the history mechanism: a value change
// must not overwrite the old value — it is appended to history by a scripted
// update, in one atomic request. A PUT would silently lose the old observation.
func TestAddCorrectionUsesScriptedUpdate(t *testing.T) {
	var updateBody string
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"fact_id":"existing-id","key":"k","value":"old",
				"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z",
				"observed_at":"2020-01-01T00:00:00Z"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	created, err := c.Add(context.Background(), model.NewFact("k", "new value", "config", "p", "test:contract", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if created {
		t.Error("created = true when the fact already existed")
	}

	if puts := fake.requestsFor(http.MethodPut, "/memento/_doc/k"); len(puts) != 0 {
		t.Errorf("value change used PUT (%d); the old value would be lost — must be a scripted update", len(puts))
	}
	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("expected one scripted update, got %d", len(updates))
	}
	if got := updates[0].Query.Get("refresh"); got != "wait_for" {
		t.Errorf("refresh = %q, want wait_for", got)
	}

	var req map[string]any
	if err := json.Unmarshal([]byte(updateBody), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, updateBody)
	}
	script, ok := req["script"].(map[string]any)
	if !ok {
		t.Fatalf("update body has no script: %s", updateBody)
	}
	src, _ := script["source"].(string)
	if !strings.Contains(src, "history.add") {
		t.Errorf("script does not append to history: %s", src)
	}
	params, _ := script["params"].(map[string]any)
	if params["value"] != "new value" {
		t.Errorf("params.value = %v, want \"new value\"", params["value"])
	}
}

// TestAddSameValueUpdatesNotesOnly pins the second branch: re-adding the same
// value must not add a history entry and must not move observed_at — only the
// qualifiers are new.
func TestAddSameValueUpdatesNotesOnly(t *testing.T) {
	var updateBody string
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"fact_id":"existing-id","key":"k","value":"same",
				"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z",
				"observed_at":"2020-01-01T00:00:00Z"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	created, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:contract", "retouched"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if created {
		t.Error("created = true for an existing fact")
	}

	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("expected one update, got %d", len(updates))
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(updateBody), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, updateBody)
	}
	doc, ok := req["doc"].(map[string]any)
	if !ok {
		t.Fatalf("same-value update must use a partial doc (no script), got: %s", updateBody)
	}
	if _, has := doc["value"]; has {
		t.Error("same-value update must not touch value")
	}
	if _, has := doc["observed_at"]; has {
		t.Error("same-value update must not touch observed_at — the observation is old, only the note is new")
	}
	if doc["notes"] != "retouched" {
		t.Errorf("notes = %v, want retouched", doc["notes"])
	}
	if got := updates[0].Query.Get("refresh"); got != "wait_for" {
		t.Errorf("refresh = %q, want wait_for", got)
	}
}

// TestAddEmptyNotesPreservesNotes pins the notes-retention contract: the CLI
// flag and the MCP field default to empty, and notes never enter history, so
// a correction (and a same-value add) without notes must leave the existing
// qualifiers alone — writing "" over them is irreversible erasure.
func TestAddEmptyNotesPreservesNotes(t *testing.T) {
	var updateBody string
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"fact_id":"existing-id","key":"k","value":"same","notes":"dev config only",
				"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z",
				"observed_at":"2020-01-01T00:00:00Z"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	if _, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:contract", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// An empty notes field must be absent from the partial doc, not "".
	if strings.Contains(updateBody, `"notes"`) {
		t.Errorf("empty notes must not be written over the stored notes: %s", updateBody)
	}

	// The scripted correction path guards the same way (params.notes only
	// replaces when non-empty).
	updateBody = ""
	c2, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"fact_id":"existing-id","key":"k","value":"old","notes":"dev config only",
				"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z",
				"observed_at":"2020-01-01T00:00:00Z"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})
	if _, err := c2.Add(context.Background(), model.NewFact("k", "new value", "config", "p", "test:contract", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.Contains(updateBody, "params.notes != ''") {
		t.Errorf("correction script must guard notes replacement: %s", updateBody)
	}
}

// TestAddSendsRefreshWaitForForNewFacts checks the same refresh guarantee on the
// create path, where no previous document exists.
func TestAddSendsRefreshWaitForForNewFacts(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/new":
			return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
		case r.Method == http.MethodPut && r.Path == "/memento/_doc/new":
			return http.StatusCreated, `{"_index":"memento","_id":"new","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	created, err := c.Add(context.Background(), model.NewFact("new", "v", "config", "p", "test:contract", ""))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !created {
		t.Error("created = false for a brand-new fact")
	}
	puts := fake.requestsFor(http.MethodPut, "/memento/_doc/new")
	if len(puts) != 1 {
		t.Fatalf("expected one write, got %d", len(puts))
	}
	if got := puts[0].Query.Get("refresh"); got != "wait_for" {
		t.Errorf("refresh = %q, want wait_for", got)
	}
	if got := puts[0].Query.Get("op_type"); got != "create" {
		t.Errorf("op_type = %q, want create (a plain index lets a concurrent creator overwrite silently)", got)
	}
}

// TestAddCreateRaceFallsToCorrection pins the concurrency contract: when a
// concurrent process creates the same key between the Get and the create, the
// version conflict must not fail and must not overwrite — the winner's value
// goes to history via the scripted correction.
func TestAddCreateRaceFallsToCorrection(t *testing.T) {
	gets := 0
	var updateBody string
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			gets++
			if gets == 1 {
				return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
			}
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"fact_id":"winner-id","key":"k","value":"winner",
				"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z",
				"observed_at":"2020-01-01T00:00:00Z"}}`
		case r.Method == http.MethodPut && r.Path == "/memento/_doc/k":
			return http.StatusConflict, `{"error":{"type":"version_conflict_engine_exception","reason":"[k]: version conflict, document already exists"},"status":409}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	created, err := c.Add(context.Background(), model.NewFact("k", "mine", "config", "p", "test:contract", ""))
	if err != nil {
		t.Fatalf("Add lost the create race: %v", err)
	}
	if created {
		t.Error("created = true; the fact was created by the concurrent winner")
	}
	if gets != 2 {
		t.Errorf("gets = %d, want 2 (miss, then re-read of the winner)", gets)
	}
	if len(fake.requestsFor(http.MethodPost, "/memento/_update/k")) != 1 {
		t.Fatalf("no scripted correction after the create conflict: %+v", fake.requests)
	}
	// The correction script appends the server-side winner value to history
	// before replacing it (ctx._source.history), so the first value cannot
	// vanish.
	for _, want := range []string{"painless", "ctx._source.history"} {
		if !strings.Contains(updateBody, want) {
			t.Errorf("update body lacks %q: %s", want, updateBody)
		}
	}
}

// TestAddBlockedWriteIsActionable is the integration counterpart of the
// wrapWriteError unit test: a cluster block must reach the caller as a message
// saying what happened and that retrying will not help.
func TestAddBlockedWriteIsActionable(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet {
			return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
		}
		return http.StatusTooManyRequests,
			`{"error":{"type":"cluster_block_exception","reason":"disk usage exceeded flood-stage watermark"},"status":429}`
	})

	_, err := c.Add(context.Background(), model.NewFact("k", "v", "config", "p", "test:contract", ""))
	if err == nil {
		t.Fatal("Add succeeded against a blocking cluster")
	}
	msg := err.Error()
	for _, want := range []string{"blocking writes", "retrying will not help"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// TestAddCorrectionCarriesHistoryLimit pins the per-key retention contract:
// the correction script trims the oldest history beyond params.history_limit,
// so the limit value must ship with every scripted update. Without it, state
// keys corrected daily would grow one key's history without bound.
func TestAddCorrectionCarriesHistoryLimit(t *testing.T) {
	var updateBody string
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) { cfg.HistoryLimit = 10 },
		func(r recordedRequest) (int, string) {
			switch {
			case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
				return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
					"fact_id":"existing-id","key":"k","value":"old",
					"created_at":"2020-01-01T00:00:00Z","updated_at":"2020-01-01T00:00:00Z",
					"observed_at":"2020-01-01T00:00:00Z"}}`
			case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
				updateBody = r.Body
				return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
		})

	if _, err := c.Add(context.Background(), model.NewFact("k", "new value", "config", "p", "test:contract", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("expected one scripted update, got %d", len(updates))
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(updateBody), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, updateBody)
	}
	script, ok := req["script"].(map[string]any)
	if !ok {
		t.Fatalf("update body has no script: %s", updateBody)
	}
	params, _ := script["params"].(map[string]any)
	if params["history_limit"] != float64(10) {
		t.Errorf("params.history_limit = %v, want 10", params["history_limit"])
	}
	src, _ := script["source"].(string)
	if !strings.Contains(src, "history_limit") {
		t.Errorf("script does not apply the retention limit: %s", src)
	}
}

// TestPruneScriptedUpdate pins the prune contract: it is a scripted update
// that keeps the newest tail of history, touches neither value nor
// observed_at, refreshes for read-after-write, and reports the removed count.
func TestPruneScriptedUpdate(t *testing.T) {
	var updateBody string
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v","category":"config","project":"p","status":"verified",
				"history":[{"value":"h1"},{"value":"h2"},{"value":"h3"},{"value":"h4"},{"value":"h5"}]}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	removed, err := c.Prune(context.Background(), "k", 2)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}

	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("expected one scripted update, got %d", len(updates))
	}
	if got := updates[0].Query.Get("refresh"); got != "wait_for" {
		t.Errorf("refresh = %q, want wait_for", got)
	}

	var req map[string]any
	if err := json.Unmarshal([]byte(updateBody), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, updateBody)
	}
	script, ok := req["script"].(map[string]any)
	if !ok {
		t.Fatalf("update body has no script: %s", updateBody)
	}
	src, _ := script["source"].(string)
	if !strings.Contains(src, "subList") {
		t.Errorf("script does not keep the newest tail of history: %s", src)
	}
	params, _ := script["params"].(map[string]any)
	if params["keep"] != float64(2) {
		t.Errorf("params.keep = %v, want 2", params["keep"])
	}
	// The current value is never touched: only history and updated_at change.
	if strings.Contains(src, "value = ") {
		t.Errorf("prune script must not overwrite the value: %s", src)
	}
}

// TestPruneNoWriteWhenWithinLimit: within the limit there is nothing to drop,
// so nothing is written — updated_at must not move on a no-op.
func TestPruneNoWriteWhenWithinLimit(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/memento/_doc/k" {
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v","category":"config","project":"p","status":"verified",
				"history":[{"value":"h1"},{"value":"h2"}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	removed, err := c.Prune(context.Background(), "k", 5)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if updates := fake.requestsFor(http.MethodPost, "/memento/_update/k"); len(updates) != 0 {
		t.Errorf("prune within the limit wrote %d times; a no-op must not touch the document", len(updates))
	}
}

// TestPruneMissingFactIsNotFound keeps the handlers' errors.Is checks working:
// a missing key must arrive as ErrNotFound, not as a write error.
func TestPruneMissingFactIsNotFound(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
	})

	if _, err := c.Prune(context.Background(), "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestObsoleteKeepsValueAndAppendsReason pins the obsolete contract: the value
// and observed_at are left untouched (there is no new observation), and the
// reason is appended to notes so earlier qualifiers survive.
func TestObsoleteKeepsValueAndAppendsReason(t *testing.T) {
	var updateBody string
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
				"key":"k","value":"v","notes":"old note","status":"verified"}}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected"}`
	})

	if err := c.Obsolete(context.Background(), "k", "file deleted"); err != nil {
		t.Fatalf("Obsolete: %v", err)
	}

	updates := fake.requestsFor(http.MethodPost, "/memento/_update/k")
	if len(updates) != 1 {
		t.Fatalf("expected one update, got %d", len(updates))
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(updateBody), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, updateBody)
	}
	doc, ok := req["doc"].(map[string]any)
	if !ok {
		t.Fatalf("update body has no doc: %s", updateBody)
	}
	if doc["status"] != "obsolete" {
		t.Errorf("status = %v, want obsolete", doc["status"])
	}
	if want := "old note\nfile deleted"; doc["notes"] != want {
		t.Errorf("notes = %v, want %q", doc["notes"], want)
	}
	if _, has := doc["value"]; has {
		t.Error("obsolete must not overwrite the value — there is no new observation")
	}
	if _, has := doc["observed_at"]; has {
		t.Error("obsolete must not touch observed_at")
	}
}

// TestObsoleteMissingFactIsNotFound keeps the MCP handler's errors.Is check
// working: a missing fact must arrive as ErrNotFound, not as a write error.
func TestObsoleteMissingFactIsNotFound(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
	})

	err := c.Obsolete(context.Background(), "nope", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestListPrefixScope pins the prefix contract: the clause reaches the wire,
// and an invalid prefix fails before any request is made.
func TestListPrefixScope(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/memento/_search" {
			return http.StatusOK, `{"hits":{"hits":[]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	if _, _, err := c.List(context.Background(), "memento", "memento:state:*"); err != nil {
		t.Fatalf("List: %v", err)
	}
	reqs := fake.requestsFor(http.MethodPost, "/memento/_search")
	if len(reqs) != 1 {
		t.Fatalf("expected one search request, got %d", len(reqs))
	}
	var body struct {
		Query struct {
			Bool struct {
				Filter []map[string]interface{} `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var prefix string
	for _, f := range body.Query.Bool.Filter {
		if p, ok := f["prefix"]; ok {
			prefix = p.(map[string]interface{})["key"].(string)
		}
	}
	if prefix != "memento:state:" {
		t.Errorf("prefix on wire = %q, want memento:state:", prefix)
	}

	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()

	if _, _, err := c.List(context.Background(), "memento", "a*b"); err == nil {
		t.Error("List with middle * must fail")
	}
	if len(fake.requestsFor(http.MethodPost, "/memento/_search")) != 0 {
		t.Error("invalid prefix must not produce a request")
	}
}

// TestSearchMetaTotalAndSkipped pins the SearchMeta contract: Total is the
// cluster-side match count (the honest truncation signal), and hits whose
// _source fails to decode must surface as Skipped, never vanish silently.
func TestSearchMetaTotalAndSkipped(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/memento/_search" {
			return http.StatusOK, `{"hits":{"total":{"value":9,"relation":"eq"},"hits":[` +
				`{"_id":"1","_source":{"key":"a:one","value":"v1","category":"config","project":"p","status":"verified"}},` +
				`{"_id":"2","_source":{"key":123}}]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	facts, meta, err := c.Search(context.Background(), "q", "", "", "", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want 1 (the decodable hit): %+v", len(facts), facts)
	}
	if facts[0].Key != "a:one" {
		t.Errorf("facts[0].Key = %q, want a:one", facts[0].Key)
	}
	if meta.Total != 9 {
		t.Errorf("meta.Total = %d, want 9 (the cluster-side total, not the page size)", meta.Total)
	}
	if meta.Skipped != 1 {
		t.Errorf("meta.Skipped = %d, want 1 (the undecodable hit must be counted)", meta.Skipped)
	}
}

// TestKeysGuards pins the key-navigation contract: at least one of
// prefix/match is required, and both clauses reach the wire sorted by key.
func TestKeysGuards(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPost && r.Path == "/memento/_search" {
			return http.StatusOK, `{"hits":{"hits":[]}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	if _, _, err := c.Keys(context.Background(), "", "", "", ""); err == nil {
		t.Error("Keys with no prefix and no match must fail (context flood guard)")
	}
	if len(fake.requestsFor(http.MethodPost, "/memento/_search")) != 0 {
		t.Error("empty Keys query must not produce a request")
	}

	if _, _, err := c.Keys(context.Background(), "zed:", "state", "memento", "verified"); err != nil {
		t.Fatalf("Keys: %v", err)
	}
	reqs := fake.requestsFor(http.MethodPost, "/memento/_search")
	if len(reqs) != 1 {
		t.Fatalf("expected one search request, got %d", len(reqs))
	}
	var body struct {
		Query struct {
			Bool struct {
				Filter []map[string]interface{} `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
		Sort []map[string]map[string]string `json:"sort"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(body.Query.Bool.Filter) != 4 {
		t.Errorf("filters = %d, want 4", len(body.Query.Bool.Filter))
	}
	if len(body.Sort) != 1 || body.Sort[0]["key"]["order"] != "asc" {
		t.Errorf("sort = %v, want key asc", body.Sort)
	}
}
