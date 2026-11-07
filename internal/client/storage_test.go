package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestWorstDiskUsage(t *testing.T) {
	stats := &opensearchapi.NodesStatsResp{}
	stats.Nodes = map[string]opensearchapi.NodesStats{}

	mk := func(name string, roles []string, total, avail int) opensearchapi.NodesStats {
		var n opensearchapi.NodesStats
		n.Name = name
		n.Roles = roles
		n.FS.Total.TotalInBytes = total
		n.FS.Total.AvailableInBytes = avail
		return n
	}

	stats.Nodes["n1"] = mk("node-a", []string{"data", "ingest"}, 100, 30) // 70% used
	stats.Nodes["n2"] = mk("node-b", []string{"data"}, 100, 10)           // 90% used
	stats.Nodes["n3"] = mk("coordinator", []string{"ingest"}, 100, 1)     // not a data node

	name, pct := worstDiskUsage(stats)
	if name != "node-b" {
		t.Errorf("worst node = %q, want node-b", name)
	}
	if pct < 89.9 || pct > 90.1 {
		t.Errorf("worst pct = %.2f, want ~90", pct)
	}
}

func TestWorstDiskUsage_NoDataNodes(t *testing.T) {
	stats := &opensearchapi.NodesStatsResp{}
	stats.Nodes = map[string]opensearchapi.NodesStats{}
	var n opensearchapi.NodesStats
	n.Name = "coordinator"
	n.Roles = []string{"cluster_manager"}
	n.FS.Total.TotalInBytes = 100
	stats.Nodes["n1"] = n

	if name, pct := worstDiskUsage(stats); name != "" || pct != 0 {
		t.Errorf("worstDiskUsage = (%q, %v), want empty", name, pct)
	}
}

func TestWorstDiskUsage_Nil(t *testing.T) {
	if name, pct := worstDiskUsage(nil); name != "" || pct != 0 {
		t.Errorf("worstDiskUsage(nil) = (%q, %v), want empty", name, pct)
	}
}

func TestReadOnlyFromSettings(t *testing.T) {
	cases := map[string]string{
		"flat true":     `{"index.blocks.read_only_allow_delete":"true"}`,
		"nested true":   `{"index":{"blocks":{"read_only_allow_delete":"true"}}}`,
		"nested bare":   `{"index":{"blocks":{"read_only":true}}}`,
		"read_only=1":   `{"index.blocks.read_only":"1"}`,
		"absent":        `{"index":{"number_of_replicas":"1"}}`,
		"false":         `{"index.blocks.read_only":"false"}`,
		"nested false":  `{"index":{"blocks":{"read_only_allow_delete":"false"}}}`,
		"unrelated key": `{"index":{"blocks":{"write":"true"}}}`,
	}
	want := map[string]bool{
		"flat true": true, "nested true": true, "nested bare": true, "read_only=1": true,
		"absent": false, "false": false, "nested false": false, "unrelated key": false,
	}

	for name, settings := range cases {
		data := opensearchapi.IndicesGetRespData{
			"memento": {Settings: json.RawMessage(settings)},
		}
		resp := &opensearchapi.IndicesGetResp{IndicesGetRespData: &data}
		if got := readOnlyFromSettings(resp); got != want[name] {
			t.Errorf("%s: readOnlyFromSettings = %v, want %v", name, got, want[name])
		}
	}
}

func TestReadOnlyFromSettings_NilAndGarbage(t *testing.T) {
	if readOnlyFromSettings(nil) {
		t.Error("nil response must not report read-only")
	}
	var resp opensearchapi.IndicesGetResp
	if readOnlyFromSettings(&resp) {
		t.Error("empty response must not report read-only")
	}
	data := opensearchapi.IndicesGetRespData{"memento": {Settings: json.RawMessage(`not json`)}}
	resp.IndicesGetRespData = &data
	if readOnlyFromSettings(&resp) {
		t.Error("unparseable settings must not report read-only")
	}
}

func TestIsClusterBlock(t *testing.T) {
	blocked := &opensearch.StructError{
		Status: 403,
		Err: opensearch.Err{
			Type:   "cluster_block_exception",
			Reason: "[FORBIDDEN/12/index read-only / allow delete (api)]",
		},
	}
	if !isClusterBlock(blocked) {
		t.Error("cluster_block_exception not detected")
	}

	readOnlyString := &opensearch.StringError{Status: 403, Err: "index is read-only"}
	if !isClusterBlock(readOnlyString) {
		t.Error("string error mentioning read-only not detected")
	}

	missing := &opensearch.StructError{
		Status: 404,
		Err:    opensearch.Err{Type: "document_missing_exception", Reason: "document missing"},
	}
	if isClusterBlock(missing) {
		t.Error("document_missing_exception must not be treated as a cluster block")
	}
}

func TestWrapWriteError_MentionsClusterAndNoRetry(t *testing.T) {
	blocked := &opensearch.StructError{
		Status: 403,
		Err:    opensearch.Err{Type: "cluster_block_exception", Reason: "read-only"},
	}
	err := wrapWriteError("add fact", blocked)
	msg := err.Error()

	for _, want := range []string{"add fact", "cluster is blocking writes", "retrying will not help"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	// The original error must stay wrapped so callers can still inspect it.
	if !isClusterBlock(err) {
		t.Error("wrapped error no longer detected as a cluster block")
	}
}

func TestWrapWriteError_PlainErrorUnchanged(t *testing.T) {
	plain := &opensearch.StructError{
		Status: 500,
		Err:    opensearch.Err{Type: "some_other_error", Reason: "boom"},
	}
	msg := wrapWriteError("add fact", plain).Error()
	if strings.Contains(msg, "blocking writes") {
		t.Errorf("plain error got the block message: %s", msg)
	}
	if !strings.Contains(msg, "boom") {
		t.Errorf("original error text lost: %s", msg)
	}
}

// The health probe used to call _cluster/health/<index> directly. When the index
// does not exist, that request blocks server-side until its own timeout (~30 s),
// which hung `memento status` on a fresh install. These tests pin the contract:
// existence is probed first, and a missing index is reported, not queried.
func TestStorageState_IndexAbsent(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"cluster_name":"test","status":"yellow","number_of_data_nodes":1,"unassigned_shards":1}`
		case r.Method == http.MethodHead && r.Path == "/memento":
			return http.StatusNotFound, ""
		case r.Method == http.MethodGet && r.Path == "/_nodes/stats/fs":
			return http.StatusOK, `{"cluster_name":"test","nodes":{"n1":{"name":"node-a","roles":["data"],"fs":{"total":{"total_in_bytes":100,"available_in_bytes":30}}}}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	st := c.StorageState(context.Background())

	if !st.IndexAbsent {
		t.Errorf("IndexAbsent = false, want true (state: %+v)", st)
	}
	if st.IndexHealth != "" {
		t.Errorf("IndexHealth = %q, want empty for an absent index", st.IndexHealth)
	}
	if st.Unavailable != "" {
		t.Errorf("Unavailable = %q, want empty: an absent index is not a problem", st.Unavailable)
	}
	if st.ClusterHealth != "yellow" {
		t.Errorf("ClusterHealth = %q, want yellow", st.ClusterHealth)
	}
	if st.WorstNode != "node-a" {
		t.Errorf("WorstNode = %q, want node-a (disk usage must still be collected)", st.WorstNode)
	}
	// The expensive queries must not have been issued at all.
	for _, r := range fake.requests {
		if strings.HasPrefix(r.Path, "/_cluster/health/memento") {
			t.Errorf("scoped index health was queried for an absent index: %s %s", r.Method, r.Path)
		}
		if r.Method == http.MethodGet && r.Path == "/memento" {
			t.Errorf("index settings were queried for an absent index: %s %s", r.Method, r.Path)
		}
	}
}

func TestStorageState_IndexPresent(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"cluster_name":"test","status":"green","number_of_data_nodes":1,"unassigned_shards":0}`
		case r.Method == http.MethodHead && r.Path == "/memento":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/_cluster/health/memento":
			return http.StatusOK, `{"cluster_name":"test","status":"yellow","number_of_data_nodes":1,"unassigned_shards":1}`
		case r.Method == http.MethodGet && r.Path == "/_nodes/stats/fs":
			return http.StatusOK, `{"cluster_name":"test","nodes":{}}`
		case r.Method == http.MethodGet && r.Path == "/memento":
			return http.StatusOK, `{"memento":{"settings":{"index":{"number_of_shards":"1"}}}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	st := c.StorageState(context.Background())

	if st.IndexAbsent {
		t.Errorf("IndexAbsent = true, want false for an existing index (state: %+v)", st)
	}
	if st.IndexHealth != "yellow" {
		t.Errorf("IndexHealth = %q, want yellow", st.IndexHealth)
	}
	if st.Unavailable != "" {
		t.Errorf("Unavailable = %q, want empty", st.Unavailable)
	}
}

func TestStorageState_ExistsProbeFails(t *testing.T) {
	c, fake := newFakeClient(t, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"cluster_name":"test","status":"green","number_of_data_nodes":1,"unassigned_shards":0}`
		case r.Method == http.MethodHead && r.Path == "/memento":
			return http.StatusUnauthorized, `{"status":401,"error":"unauthorized"}`
		case r.Method == http.MethodGet && r.Path == "/_nodes/stats/fs":
			return http.StatusOK, `{"cluster_name":"test","nodes":{}}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})

	st := c.StorageState(context.Background())

	// A failed probe is "could not check", not "absent": the index is not
	// declared missing, and the hanging scoped health must not be queried.
	if st.IndexAbsent {
		t.Errorf("IndexAbsent = true, want false when the probe failed (state: %+v)", st)
	}
	if !strings.Contains(st.Unavailable, "index existence") {
		t.Errorf("Unavailable = %q, want a mention of the failed existence probe", st.Unavailable)
	}
	for _, r := range fake.requests {
		if strings.HasPrefix(r.Path, "/_cluster/health/memento") {
			t.Errorf("scoped index health was queried after a failed existence probe: %s %s", r.Method, r.Path)
		}
	}
}
