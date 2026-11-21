package client

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const testBundle = `{"id":"a","type":"dashboard","attributes":{"title":"A"},"references":[]}
{"id":"b","type":"index-pattern","attributes":{"title":"velruna"}}
`

func TestPushDashboards_ResolvesAliasAndWritesEveryObject(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_1","is_write_index":"true"}]`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/.kibana_1/_doc/"):
			return http.StatusOK, `{"result":"updated"}`
		case r.Method == http.MethodPost && r.Path == "/.kibana_1/_refresh":
			return http.StatusOK, `{"_shards":{"total":1,"successful":1,"failed":0}}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected request"}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Index != ".kibana_1" {
		t.Errorf("Index = %q, want the alias write index", res.Index)
	}
	if res.Indexed != 2 || res.Failed != 0 {
		t.Errorf("Indexed/Failed = %d/%d, want 2/0 (errors: %v)", res.Indexed, res.Failed, res.Errors)
	}

	docs := map[string]recordedRequest{}
	var refreshed bool
	for _, r := range fake.requests {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/.kibana_1/_doc/"):
			id, uerr := url.PathUnescape(strings.TrimPrefix(r.Path, "/.kibana_1/_doc/"))
			if uerr != nil {
				t.Fatalf("document id %q is not path-escaped: %v", r.Path, uerr)
			}
			docs[id] = r
		case r.Method == http.MethodPost && r.Path == "/.kibana_1/_refresh":
			refreshed = true
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_cat/aliases"):
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.Path)
		}
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
		if strings.Contains(doc.Query.Get("refresh"), "") && doc.Query.Get("refresh") != "" {
			t.Errorf("%s: refresh=%q, want the single trailing refresh", want, doc.Query.Get("refresh"))
		}
	}
}

func TestPushDashboards_IndexOverrideSkipsAliasProbe(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/custom/_doc/"):
			return http.StatusOK, `{"result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/custom/_refresh":
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{"error":"alias must not be probed"}`
	})

	res, err := c.PushDashboards(context.Background(), []byte(testBundle), "custom")
	if err != nil {
		t.Fatalf("PushDashboards: %v", err)
	}
	if res.Index != "custom" || res.Indexed != 2 {
		t.Errorf("Index/Indexed = %q/%d, want custom/2", res.Index, res.Indexed)
	}
	for _, r := range fake.requests {
		if strings.HasPrefix(r.Path, "/_cat/") {
			t.Errorf("alias was probed despite --index: %s %s", r.Method, r.Path)
		}
	}
}

func TestPushDashboards_AliasWithoutWriteFlagUsesFirstEntry(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_9","is_write_index":"-"}]`
		case r.Method == http.MethodPut && strings.HasPrefix(r.Path, "/.kibana_9/_doc/"):
			return http.StatusOK, `{}`
		case r.Method == http.MethodPost && r.Path == "/.kibana_9/_refresh":
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

func TestPushDashboards_AliasProbeFailureIsExplained(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"error":"boom"}`
	})

	_, err := c.PushDashboards(context.Background(), []byte(testBundle), "")
	if err == nil {
		t.Fatal("PushDashboards succeeded although the alias probe failed")
	}
	if !strings.Contains(err.Error(), ".kibana") {
		t.Errorf("error = %v, want it to name the alias", err)
	}
}

// One rejected object must not hide the others: the counts are the operator's
// only clue about what is half-installed.
func TestPushDashboards_CountsPerObjectFailures(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_1","is_write_index":"true"}]`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "dashboard%3Aa"),
			r.Method == http.MethodPut && strings.HasSuffix(r.Path, "dashboard:a"):
			return http.StatusBadRequest, `{"error":"mapping_mismatch"}`
		case r.Method == http.MethodPut:
			return http.StatusOK, `{}`
		case r.Method == http.MethodPost && r.Path == "/.kibana_1/_refresh":
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
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cat/aliases/.kibana":
			return http.StatusOK, `[{"alias":".kibana","index":".kibana_1","is_write_index":"true"}]`
		case r.Method == http.MethodPut:
			return http.StatusOK, `{}`
		case r.Method == http.MethodPost && r.Path == "/.kibana_1/_refresh":
			return http.StatusOK, `{}`
		}
		return http.StatusInternalServerError, `{}`
	})

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
