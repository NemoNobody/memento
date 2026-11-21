package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const testBundle = `{"id":"a","type":"dashboard","attributes":{"title":"A"},"references":[]}
{"id":"b","type":"index-pattern","attributes":{"title":"velruna"}}
`

// writeIndexResponder serves a well-formed global-tenant setup: one alias, one
// write index, PUTs accepted, refresh fine. It fails loudly on anything else so
// a wrong request path shows up as a failed assertion, not a silent pass.
func writeIndexResponder(t *testing.T) func(recordedRequest) (int, string) {
	t.Helper()
	return func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			// Without format=json the cluster answers in a text table and the
			// decode fails on a live install.
			if got := r.Query.Get("format"); got != "json" {
				t.Errorf("_cat format = %q, want json", got)
			}
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_2","is_write_index":"true"}]`
		// Writes go through the ALIAS, not the resolved index: that is what
		// survives a concurrent .kibana_N migration.
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/.kibana/_doc/"):
			return http.StatusOK, `{"result":"updated"}`
		case r.Method == http.MethodPost && r.Path == "/.kibana/_refresh":
			return http.StatusOK, `{"_shards":{"total":1,"successful":1,"failed":0}}`
		}
		t.Errorf("unexpected request %s %s", r.Method, r.Path)
		return http.StatusInternalServerError, `{"error":"unexpected request"}`
	}
}

func TestPushDashboards_ResolvesAliasAndWritesThroughIt(t *testing.T) {
	c, fake := newFakeClient(t, writeIndexResponder(t))

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Index != ".kibana_2" || !res.Global {
		t.Errorf("Index/Global = %q/%v, want the resolved write index and Global", res.Index, res.Global)
	}
	if res.Indexed != 2 || res.Failed != 0 {
		t.Errorf("Indexed/Failed = %d/%d, want 2/0 (errors: %v)", res.Indexed, res.Failed, res.Errors)
	}

	docs := map[string]recordedRequest{}
	var refreshed bool
	probed := false
	for _, r := range fake.requests {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/.kibana/_doc/"):
			docs[strings.TrimPrefix(r.Path, "/.kibana/_doc/")] = r
		case r.Method == http.MethodPost && r.Path == "/.kibana/_refresh":
			refreshed = true
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			probed = true
		case r.Method == http.MethodHead:
			t.Errorf("a HEAD probe was issued although the target came from the alias")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.Path)
		}
	}
	if !probed {
		t.Error("the .kibana alias was never probed")
	}
	if !refreshed {
		t.Error("the index was never refreshed: the objects stay invisible")
	}
	for _, want := range []string{"dashboard:a", "index-pattern:b"} {
		doc, ok := docs[want]
		if !ok {
			t.Fatalf("no document written for %q (written: %v)", want, keys(docs))
		}
		if !strings.Contains(doc.Body, `"migrationVersion"`) {
			t.Errorf("%s: document has no migrationVersion: %s", want, doc.Body)
		}
	}
}

// A re-run must overwrite the same ids: duplicates would double every panel.
func TestPushDashboards_IsIdempotentOnRerun(t *testing.T) {
	c, fake := newFakeClient(t, writeIndexResponder(t))

	for run := 1; run <= 2; run++ {
		if _, err := c.PushDashboards(context.Background(), []byte(testBundle), ""); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}

	seen := map[string]int{}
	for _, r := range fake.requests {
		if r.Method == http.MethodPut {
			seen[r.Path]++
		}
	}
	if len(seen) != 2 {
		t.Errorf("wrote %d distinct ids, want 2 (paths: %v)", len(seen), seen)
	}
	for path, n := range seen {
		if n != 2 {
			t.Errorf("%s written %d times across two runs, want 2 (the same path each run)", path, n)
		}
	}
}

func TestPushDashboards_ExplicitIndexIsProbedNotAliased(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/.kibana_custom/_mapping":
			return http.StatusOK, `{".kibana_custom":{"mappings":{"properties":{"type":{"type":"keyword"},"updated_at":{"type":"date"},"migrationVersion":{"type":"object"}}}}}`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/.kibana_custom/_doc/"):
			return http.StatusOK, `{"result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/.kibana_custom/_refresh":
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_custom")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Index != ".kibana_custom" || res.Global {
		t.Errorf("Index/Global = %q/%v, want .kibana_custom/false", res.Index, res.Global)
	}
	if res.Indexed != 2 {
		t.Errorf("Indexed = %d, want 2", res.Indexed)
	}
	for _, r := range fake.requests {
		if strings.HasPrefix(r.Path, "/_cat/") {
			t.Errorf("alias was probed despite --index: %s %s", r.Method, r.Path)
		}
	}
}

// Without the probe a typo lands in a freshly auto-created index and every PUT
// returns 2xx: a green run that installed nothing anyone can see.
func TestPushDashboards_MissingExplicitIndexFails(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/_mapping") {
			return http.StatusNotFound, `{"error":{"type":"index_not_found_exception","reason":"no such index"}}`
		}
		return http.StatusOK, `{}`
	})

	_, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_9")
	if err == nil {
		t.Fatal("PushDashboards wrote into a non-existent index")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the index does not exist", err)
	}
	for _, r := range fake.requests {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			t.Errorf("wrote to a missing index: %s %s", r.Method, r.Path)
		}
	}
}

func TestPushDashboards_AmbiguousAliasIsRefused(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana" {
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_1","is_write_index":"-"},
			                            {"alias":".kibana","index":".kibana_2","is_write_index":"-"}]`
		}
		return http.StatusOK, `{}`
	})

	_, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err == nil {
		t.Fatal("PushDashboards guessed between two indices")
	}
	if !strings.Contains(err.Error(), "--index") {
		t.Errorf("error = %v, want it to offer --index", err)
	}
}

func TestPushDashboards_MultipleWriteIndicesAreRefused(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana" {
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_1","is_write_index":"true"},
			                            {"alias":".kibana","index":".kibana_2","is_write_index":"true"}]`
		}
		return http.StatusOK, `{}`
	})

	if _, err := c.PushDashboards(context.Background(), []byte(testBundle), ""); err == nil {
		t.Fatal("PushDashboards accepted two write indices")
	}
}

func TestPushDashboards_SingleEntryWithoutWriteFlagIsUsed(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_9","is_write_index":"-"}]`
		case r.Method == http.MethodPut, r.Method == http.MethodPost:
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Index != ".kibana_9" {
		t.Errorf("Index = %q, want .kibana_9", res.Index)
	}
}

// A missing alias is normally 200 + [] (covered below); a proxy or an older
// gateway may answer 404/400 instead. Both must send the operator to Dashboards.
func TestPushDashboards_MissingAliasIsExplained(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest} {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			return status, `{"error":{"type":"aliases_not_found_exception","reason":"aliases [.kibana] missing"}}`
		})
		_, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
		if err == nil {
			t.Fatalf("HTTP %d: PushDashboards succeeded without an alias", status)
		}
		if !strings.Contains(err.Error(), "Dashboards") {
			t.Errorf("HTTP %d: error = %v, want it to point at Dashboards", status, err)
		}
	}
}

// A write-only role cannot read mappings, and a proxy may not; the caller named
// the index, so the probe must not turn a legitimate target into a refusal — it
// reports the run as unverified instead.
func TestPushDashboards_ProbeRefusalDoesNotBlockExplicitIndex(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusMethodNotAllowed} {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/_mapping"):
				return status, ""
			case r.Method == http.MethodHead:
				// The fallback existence probe: the index is there.
				return http.StatusOK, ""
			case r.Method == http.MethodPut, r.Method == http.MethodPost:
				return http.StatusOK, `{}`
			}
			return http.StatusInternalServerError, `{}`
		})
		res, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_1")
		if err != nil {
			t.Fatalf("mapping probe %d blocked a named index: %v", status, err)
		}
		if res.Indexed != 2 {
			t.Errorf("probe %d: Indexed = %d, want 2", status, res.Indexed)
		}
		if !res.Unverified {
			t.Errorf("probe %d: Unverified = false, want true (the run could not check the index)", status)
		}
	}
}

// When the mapping cannot be read, the existence probe still catches a typo:
// auto-create would otherwise turn it into a green, empty run.
func TestPushDashboards_UnreadableMappingStillRejectsMissingIndex(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/_mapping"):
			return http.StatusForbidden, ""
		case r.Method == http.MethodHead:
			return http.StatusNotFound, ""
		}
		return http.StatusOK, `{}`
	})
	_, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_7")
	if err == nil {
		t.Fatal("a missing index slipped through when the mapping was unreadable")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the index does not exist", err)
	}
}

func TestPushDashboards_RejectsWildcardIndex(t *testing.T) {
	for _, bad := range []string{".kibana*", ".kibana?", ".kibana_1,.kibana_2", "_all", "-.kibana"} {
		c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
			return http.StatusOK, `{}`
		})
		if _, err := c.PushDashboards(context.Background(), []byte(testBundle), bad); err == nil {
			t.Errorf("PushDashboards accepted %q", bad)
		}
		if len(fake.requests) != 0 {
			t.Errorf("%q was probed or written: %+v", bad, fake.requests)
		}
	}
}

// A rejected password must be named once, not repeated 22 times as PUT errors.
func TestPushDashboards_Probe401IsFatal(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusUnauthorized, `{"error":"unauthorized"}`
	})
	_, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_1")
	if err == nil {
		t.Fatal("a 401 probe was ignored")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error = %v, want it to name the credentials", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("issued %d requests after a 401, want just the probe", len(fake.requests))
	}
}

// is_write_index=false means the index rejects writes: taking it as the target
// would fail every PUT after a successful probe.
func TestPushDashboards_AliasMarkedNotWritableIsRefused(t *testing.T) {
	for _, flag := range []string{"false", "TRUE", "true "} {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_1","is_write_index":"` + flag + `"}]`
		})
		if _, err := c.PushDashboards(context.Background(), []byte(testBundle), ""); err == nil {
			t.Errorf("is_write_index=%q was accepted as the write target", flag)
		}
	}
}

// Any existing index passes an existence check and the facts index has a
// dynamic mapping: a typo that happens to match a real index would receive 22
// saved objects with a green "indexed 22". The mapping, not the name, decides.
func TestPushDashboards_RefusesNonSavedObjectsIndex(t *testing.T) {
	// A facts index: dynamic mapping, no type/updated_at/migrationVersion.
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, `{"velruna":{"mappings":{"properties":{"key":{"type":"keyword"},"value":{"type":"text"}}}}}`
	})
	if _, err := c.PushDashboards(context.Background(), []byte(testBundle), "velruna"); err == nil {
		t.Fatal("PushDashboards wrote into the facts index")
	}
	for _, r := range fake.requests {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			t.Errorf("wrote to a non-saved-objects index: %s %s", r.Method, r.Path)
		}
	}
}

// All three marker fields are required: an index with only two of them is not a
// saved-objects store, and each field must be pinned by its own case.
func TestPushDashboards_RequiresEverySavedObjectField(t *testing.T) {
	bodies := map[string]string{
		"type+updated_at":       `{"x":{"mappings":{"properties":{"type":{"type":"keyword"},"updated_at":{"type":"date"}}}}}`,
		"type+migrationVersion": `{"x":{"mappings":{"properties":{"type":{"type":"keyword"},"migrationVersion":{"type":"object"}}}}}`,
		"updated_at+migration":  `{"x":{"mappings":{"properties":{"updated_at":{"type":"date"},"migrationVersion":{"type":"object"}}}}}`,
	}
	for name, body := range bodies {
		c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
			return http.StatusOK, body
		})
		if _, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_1"); err == nil {
			t.Errorf("%s: an index without all three fields was accepted", name)
		}
	}
}

// The task-manager index lives in the same cluster and is .kibana-prefixed: a
// name check would have written 22 objects nobody reads.
// The real .kibana mapping is hundreds of KB: reading it with a snippet-sized
// limit truncated the JSON, and a valid saved-objects index was rejected.
func TestPushDashboards_LargeMappingIsAccepted(t *testing.T) {
	props := `"type":{"type":"keyword"},"updated_at":{"type":"date"},"migrationVersion":{"type":"object"}`
	for i := 0; i < 600; i++ {
		props += fmt.Sprintf(`,"field_%03d":{"type":"text","fields":{"keyword":{"type":"keyword"}}}`, i)
	}
	mapping := `{".kibana_1":{"mappings":{"properties":{` + props + `}}}}`
	if len(mapping) < 32<<10 {
		t.Fatalf("test mapping is only %d bytes, it must exceed the snippet limit", len(mapping))
	}

	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/_mapping"):
			return http.StatusOK, mapping
		case r.Method == http.MethodPut, r.Method == http.MethodPost:
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_1")
	if err != nil {
		t.Fatalf("a large saved-objects mapping was rejected: %v", err)
	}
	if res.Indexed != 2 {
		t.Errorf("Indexed = %d, want 2", res.Indexed)
	}
}

func TestPushDashboards_RefusesTaskManagerIndex(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusOK, `{".kibana_task_manager_1":{"mappings":{"properties":{"task":{"type":"object"},"type":{"type":"keyword"}}}}}`
	})
	if _, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana_task_manager_1"); err == nil {
		t.Fatal("PushDashboards wrote into the task-manager index")
	}
}

// Naming the alias is the same request as the default.
func TestPushDashboards_ExplicitAliasIsGlobal(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_3","is_write_index":"true"}]`
		case r.Method == http.MethodPut, r.Method == http.MethodPost:
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{}`
	})
	res, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kibana")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if !res.Global || res.Index != ".kibana_3" {
		t.Errorf("Global/Index = %v/%q, want true/.kibana_3", res.Global, res.Index)
	}
}

func TestPushDashboards_NoDashboardsIndexIsExplained(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana" {
			return http.StatusOK, `[]`
		}
		return http.StatusInternalServerError, `{}`
	})

	_, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err == nil {
		t.Fatal("PushDashboards succeeded without a saved-objects index")
	}
	if !strings.Contains(err.Error(), "Dashboards") {
		t.Errorf("error = %v, want it to send the operator to Dashboards", err)
	}
}

// The generic "is it up?" hint sends an operator with a wrong password to
// reinstall the UI; the status code says otherwise.
func TestPushDashboards_AuthFailureNamesCredentials(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusForbidden, `{"error":{"type":"security_exception","reason":"no permissions"}}`
	})

	_, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err == nil {
		t.Fatal("PushDashboards ignored a 403")
	}
	if !strings.Contains(err.Error(), "credentials") && !strings.Contains(err.Error(), "privilege") {
		t.Errorf("error = %v, want it to mention credentials or privileges", err)
	}
}

// A rejected refresh still means the objects were written: the counts must come
// back so the caller can report them and a retry is recognisably safe.
func TestPushDashboards_RefreshFailureReturnsCounts(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_2","is_write_index":"true"}]`
		case r.Method == http.MethodPut:
			return http.StatusOK, `{}`
		case r.Method == http.MethodPost:
			return http.StatusForbidden, `{"error":"no refresh privilege"}`
		}
		return http.StatusInternalServerError, `{}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err == nil {
		t.Fatal("PushDashboards ignored a failed refresh")
	}
	if res == nil || res.Indexed != 2 {
		t.Fatalf("result = %+v, want the two indexed objects reported alongside the error", res)
	}
	if !strings.Contains(err.Error(), "refresh") {
		t.Errorf("error = %v, want it to name the refresh step", err)
	}
}

func TestPushDashboards_CountsPerObjectFailures(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_2","is_write_index":"true"}]`
		case r.Method == http.MethodPut && strings.Contains(r.Path, "dashboard"):
			return http.StatusBadRequest, `{"error":"mapping_mismatch"}`
		case r.Method == http.MethodPut, r.Method == http.MethodPost:
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Indexed != 1 || res.Failed != 1 {
		t.Errorf("Indexed/Failed = %d/%d, want 1/1", res.Indexed, res.Failed)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "dashboard:a") {
		t.Errorf("Errors = %v, want one entry naming the object", res.Errors)
	}
}

func TestPushDashboards_UnsupportedTypeIsReportedNotFatal(t *testing.T) {
	c, _ := newFakeClient(t, writeIndexResponder(t))

	bundle := `{"id":"x","type":"lens","attributes":{}}` + "\n" +
		`{"id":"a","type":"dashboard","attributes":{"title":"A"}}` + "\n"
	res, err := c.PushDashboards(context.Background(), []byte(bundle), "")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Indexed != 1 || res.Failed != 1 {
		t.Errorf("Indexed/Failed = %d/%d, want 1/1", res.Indexed, res.Failed)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "unsupported saved object type") {
		t.Errorf("Errors = %v, want the unsupported type named", res.Errors)
	}
}

func keys(m map[string]recordedRequest) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
