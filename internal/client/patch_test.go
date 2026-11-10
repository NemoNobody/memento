package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/config"
)

// patchRespond serves the routes Patch touches: doc read, journal routes
// (infra probes + append), and the scripted main-doc update (body captured).
func patchRespond(source string, updateBody *string) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
			return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":` + source + `}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
			*updateBody = r.Body
			return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Method + " " + r.Path + `"}`
	}
}

// patchParams digs params out of the captured _update body (scripted form).
func patchParams(t *testing.T, body string) map[string]any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("update body is not JSON: %v (%s)", err, body)
	}
	script, ok := req["script"].(map[string]any)
	if !ok {
		t.Fatalf("update body has no script (patch must be scripted): %s", body)
	}
	params, ok := script["params"].(map[string]any)
	if !ok {
		t.Fatalf("script has no params: %s", body)
	}
	return params
}

// TestPatchNotesAppendIsTouch pins the cheap-update contract: a dated note
// appends atomically (inside the script, so a concurrent add cannot drop it),
// touches the fact (Changed=false), and writes NO journal entry — the journal
// records accepted values, and the value did not move.
func TestPatchNotesAppendIsTouch(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified","notes":"old"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{NotesAppend: "checked in prod"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if res.Changed {
		t.Error("Changed = true for a notes-only patch; it is a touch")
	}
	params := patchParams(t, body)
	wantLine := time.Now().UTC().Format("2006-01-02") + ": checked in prod"
	if params["notes_append"] != wantLine {
		t.Errorf("notes_append = %v, want %q", params["notes_append"], wantLine)
	}
	if v, ok := params["value"]; !ok || v != nil {
		t.Errorf("value must be null (untouched), got %v", v)
	}
	if v, ok := params["notes"]; !ok || v != nil {
		t.Errorf("notes must be null (the script appends), got %v", v)
	}
	if fake.sawAnyPrefix("/memento-history-") {
		t.Error("a notes-only patch must not write the journal")
	}
}

// TestPatchValueChangeJournalsAndClearsSummary pins the correction path:
// the new value is journaled, observed_at and current_ref move, and the old
// summary is CLEARED (a table of contents for the old value is a lie).
func TestPatchValueChangeJournalsAndClearsSummary(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v1","summary":"old contents","category":"config","project":"p","status":"verified"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Value: "v2"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed {
		t.Error("Changed = false for a value change")
	}
	appends := fake.requestsFor(http.MethodPost, "/memento-history-"+time.Now().UTC().Format("2006.01")+"/_doc")
	if len(appends) != 1 {
		t.Fatalf("journal appends = %d, want 1", len(appends))
	}
	var je map[string]any
	if err := json.Unmarshal([]byte(appends[0].Body), &je); err != nil {
		t.Fatalf("journal body: %v", err)
	}
	if je["value"] != "v2" {
		t.Errorf("journal value = %v, want v2 (the journal records accepted state)", je["value"])
	}
	params := patchParams(t, body)
	if params["value"] != "v2" {
		t.Errorf("params.value = %v, want v2", params["value"])
	}
	if s, ok := params["summary"]; !ok || s != "" {
		t.Errorf("params.summary = %v, want \"\" (the old summary must be cleared with the value)", params["summary"])
	}
	if v, ok := params["observed_at"]; !ok || v == nil {
		t.Error("params.observed_at must move on a value change")
	}
	if params["current_ref"] != "journal-entry-1" {
		t.Errorf("current_ref = %v, want journal-entry-1", params["current_ref"])
	}
}

// TestPatchLongValueRequiresNewSummary keeps add's summary rule on the patch
// path: a long new value without a summary is refused before any write.
func TestPatchLongValueRequiresNewSummary(t *testing.T) {
	var body string
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) { cfg.SummaryMinLen = 10 }, patchRespond(
		`{"key":"k","value":"short","category":"config","project":"p","status":"verified"}`,
		&body))

	long := strings.Repeat("x", 20)
	_, err := c.Patch(context.Background(), "k", PatchFields{Value: long})
	if err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("err = %v, want a summary-required refusal", err)
	}
	if body != "" {
		t.Error("a refused patch must not write the document")
	}
	if fake.sawAnyPrefix("/memento-history-") {
		t.Error("a refused patch must not touch the journal")
	}
}

// TestPatchResurrectSetsObservedAt pins resurrection: declaring a dead fact
// alive again IS a fresh verification — observed_at moves and the transition
// is journaled.
func TestPatchResurrectSetsObservedAt(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"obsolete","observed_at":"2020-01-01T00:00:00Z"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Status: "verified"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed || res.Status != "verified" {
		t.Errorf("res = %+v, want Changed=true Status=verified", res)
	}
	if fake.sawAnyPrefix("/memento-history-") == false {
		t.Error("the status transition must be journaled")
	}
	params := patchParams(t, body)
	if v, ok := params["observed_at"]; !ok || v == nil {
		t.Error("resurrection must set observed_at=now")
	}
	if params["status"] != "verified" {
		t.Errorf("params.status = %v, want verified", params["status"])
	}
}

// TestPatchObsoleteWithReasonJournals: reason folds into notes and the
// transition is journaled (audit symmetry with memento_obsolete).
func TestPatchObsoleteWithReasonJournals(t *testing.T) {
	var body string
	c, fake := newFakeClient(t, patchRespond(
		`{"key":"k","value":"v","category":"config","project":"p","status":"verified","notes":"n"}`,
		&body))

	res, err := c.Patch(context.Background(), "k", PatchFields{Status: "obsolete", Reason: "file deleted"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if !res.Changed || res.Status != "obsolete" {
		t.Errorf("res = %+v, want Changed=true Status=obsolete", res)
	}
	params := patchParams(t, body)
	if params["notes"] != "n\nfile deleted" {
		t.Errorf("notes = %v, want the reason folded in", params["notes"])
	}
	if !fake.sawAnyPrefix("/memento-history-") {
		t.Error("obsolete via patch must journal the transition")
	}
}

// TestPatchValidation covers the pre-store refusals: empty patch, reason
// without status, unknown status, and a missing fact.
func TestPatchValidation(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"status":500,"error":"must not reach the store"}`
	})
	ctx := context.Background()

	if _, err := c.Patch(ctx, "k", PatchFields{}); err == nil ||
		!strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("empty patch: err = %v", err)
	}
	if _, err := c.Patch(ctx, "k", PatchFields{Reason: "why"}); err == nil ||
		!strings.Contains(err.Error(), "status=obsolete") {
		t.Errorf("reason without status: err = %v", err)
	}
	if _, err := c.Patch(ctx, "k", PatchFields{Status: "bogus"}); err == nil ||
		!strings.Contains(err.Error(), "verified|obsolete") {
		t.Errorf("bad status: err = %v", err)
	}

	notFound, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
	})
	if _, err := notFound.Patch(ctx, "nope", PatchFields{NotesAppend: "x"}); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("missing fact: err = %v, want a not-found refusal", err)
	}
}
