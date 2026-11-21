package client

import (
	"context"
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
		case r.Method == http.MethodHead && r.Path == "/custom":
			return http.StatusOK, ""
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/custom/_doc/"):
			return http.StatusOK, `{"result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/custom/_refresh":
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "custom")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Index != "custom" || res.Global {
		t.Errorf("Index/Global = %q/%v, want custom/false", res.Index, res.Global)
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
		if r.Method == http.MethodHead {
			return http.StatusNotFound, ""
		}
		return http.StatusOK, `{}`
	})

	_, err := c.PushDashboards(context.Background(), []byte(testBundle), ".kiban_1")
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
