package client

import (
	"encoding/json"
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
