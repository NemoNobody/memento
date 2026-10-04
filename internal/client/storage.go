package client

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// StorageState describes the health of the cluster that holds the facts.
//
// It reports facts, not verdicts. Whether a yellow cluster matters depends on the
// deployment — a single node is a normal personal install, while a multi-node
// cluster missing replicas is a different story. The client must not assume a
// cluster shape, so it reports what it sees and leaves interpretation to callers.
type StorageState struct {
	ClusterHealth string `json:"cluster_health" jsonschema:"cluster health: green | yellow | red"`
	IndexHealth   string `json:"index_health" jsonschema:"health of the memento index: green | yellow | red"`
	Unassigned    int    `json:"unassigned_shards" jsonschema:"number of unassigned shards in the cluster"`
	DataNodes     int    `json:"data_nodes" jsonschema:"number of data nodes in the cluster"`
	ReadOnlyIndex bool   `json:"index_read_only" jsonschema:"true when the index has a read-only block and rejects writes"`

	// WorstNode is the data node with the most used disk — the one that will hit
	// a watermark (and the read-only flood stage) first.
	WorstNode            string  `json:"worst_node,omitempty" jsonschema:"data node with the highest disk usage"`
	WorstNodeUsedPercent float64 `json:"worst_node_used_percent,omitempty" jsonschema:"disk usage of that node, in percent"`

	// Unavailable names what could not be collected and why. This is diagnostics:
	// a partial answer is still useful and must not fail status.
	Unavailable string `json:"unavailable,omitempty" jsonschema:"parts of the storage state that could not be collected"`
}

// StorageState gathers cluster health, disk usage and the index read-only flag.
// It never returns an error: problems are reported in Unavailable.
func (c *Client) StorageState(ctx context.Context) *StorageState {
	st := &StorageState{}
	var problems []string

	if health, err := c.api.Cluster.Health(ctx, nil); err != nil {
		problems = append(problems, "cluster health: "+err.Error())
	} else {
		st.ClusterHealth = health.Status
		st.Unassigned = health.UnassignedShards
		st.DataNodes = health.NumberOfDataNodes
	}

	idxReq := &opensearchapi.ClusterHealthReq{Indices: []string{c.cfg.IndexName}}
	if idxHealth, err := c.api.Cluster.Health(ctx, idxReq); err != nil {
		problems = append(problems, "index health: "+err.Error())
	} else {
		st.IndexHealth = idxHealth.Status
	}

	if stats, err := c.api.Nodes.Stats(ctx, &opensearchapi.NodesStatsReq{Metric: []string{"fs"}}); err != nil {
		problems = append(problems, "disk usage: "+err.Error())
	} else {
		st.WorstNode, st.WorstNodeUsedPercent = worstDiskUsage(stats)
	}

	flat := true
	get, err := c.api.Indices.Get(ctx, opensearchapi.IndicesGetReq{
		Indices: []string{c.cfg.IndexName},
		Params:  opensearchapi.IndicesGetParams{FlatSettings: &flat},
	})
	if err != nil {
		problems = append(problems, "index settings: "+err.Error())
	} else {
		st.ReadOnlyIndex = readOnlyFromSettings(get)
	}

	if len(problems) > 0 {
		st.Unavailable = strings.Join(problems, "; ")
	}
	return st
}

// worstDiskUsage returns the data node with the highest disk usage. Nodes that
// hold no data are ignored: they cannot hit a data watermark. An empty name means
// no data node could be inspected.
func worstDiskUsage(stats *opensearchapi.NodesStatsResp) (string, float64) {
	if stats == nil {
		return "", 0
	}
	var (
		worstName string
		worstPct  float64
		found     bool
	)
	for id, node := range stats.Nodes {
		if !isDataNode(node.Roles) {
			continue
		}
		total := float64(node.FS.Total.TotalInBytes)
		if total <= 0 {
			continue
		}
		used := (total - float64(node.FS.Total.AvailableInBytes)) / total * 100
		if !found || used > worstPct {
			name := node.Name
			if name == "" {
				name = id
			}
			worstName, worstPct, found = name, used, true
		}
	}
	return worstName, worstPct
}

// isDataNode reports whether the node roles include a data role.
func isDataNode(roles []string) bool {
	for _, r := range roles {
		if r == "data" || strings.HasPrefix(r, "data_") {
			return true
		}
	}
	return false
}

// readOnlyFromSettings reports whether the index currently carries a read-only
// block, i.e. writes are rejected. Flat settings put the flag at the top level,
// but nested form is accepted too.
func readOnlyFromSettings(resp *opensearchapi.IndicesGetResp) bool {
	if resp == nil || resp.IndicesGetRespData == nil {
		return false
	}
	for _, idx := range *resp.IndicesGetRespData {
		var flat map[string]json.RawMessage
		if err := json.Unmarshal(idx.Settings, &flat); err == nil {
			if settingTrue(flat["index.blocks.read_only"]) ||
				settingTrue(flat["index.blocks.read_only_allow_delete"]) {
				return true
			}
		}

		var nested struct {
			Index struct {
				Blocks struct {
					ReadOnly            json.RawMessage `json:"read_only"`
					ReadOnlyAllowDelete json.RawMessage `json:"read_only_allow_delete"`
				} `json:"blocks"`
			} `json:"index"`
		}
		if err := json.Unmarshal(idx.Settings, &nested); err == nil {
			if settingTrue(nested.Index.Blocks.ReadOnly) ||
				settingTrue(nested.Index.Blocks.ReadOnlyAllowDelete) {
				return true
			}
		}
	}
	return false
}

// settingTrue reports whether a JSON setting holds a truthy value ("true" or "1",
// quoted or bare).
func settingTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == "true" || s == "1"
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	return false
}
