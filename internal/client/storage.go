package client

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// IndexStat is one index's size and shard layout as reported by _cat/indices.
type IndexStat struct {
	Index     string `json:"index" jsonschema:"index name"`
	Health    string `json:"health" jsonschema:"green | yellow | red"`
	Documents int    `json:"documents" jsonschema:"stored documents"`
	SizeBytes int64  `json:"size_bytes" jsonschema:"store size of all shard copies, bytes"`
	Primaries int    `json:"primaries" jsonschema:"primary shards"`
	Replicas  int    `json:"replicas" jsonschema:"configured replicas per primary"`
}

// indexStatCap bounds the reported index page: a store with a long journal
// history has many period indices, and status must stay one cheap call.
const indexStatCap = 50

// StorageState describes the health of the cluster that holds the facts.
//
// It reports facts, not verdicts. Whether a yellow cluster matters depends on the
// deployment — a single node is a normal personal install, while a multi-node
// cluster missing replicas is a different story. The client must not assume a
// cluster shape, so it reports what it sees and leaves interpretation to callers.
type StorageState struct {
	ClusterHealth string `json:"cluster_health" jsonschema:"cluster health: green | yellow | red"`
	IndexHealth   string `json:"index_health" jsonschema:"health of the velruna index: green | yellow | red; empty when the index does not exist yet"`

	// IndexAbsent means the facts index does not exist yet. It is a normal
	// state on a fresh install: the index is created on the first write.
	// It must be reported explicitly: index_health alone cannot say "absent"
	// from "not collected".
	IndexAbsent   bool `json:"index_absent,omitempty" jsonschema:"true when the facts index does not exist yet: it is created on the first write"`
	Unassigned    int  `json:"unassigned_shards" jsonschema:"number of unassigned shards in the cluster"`
	DataNodes     int  `json:"data_nodes" jsonschema:"number of data nodes in the cluster"`
	ReadOnlyIndex bool `json:"index_read_only" jsonschema:"true when the index has a read-only block and rejects writes"`

	// WorstNode is the data node with the most used disk — the one that will hit
	// a watermark (and the read-only flood stage) first.
	WorstNode            string  `json:"worst_node,omitempty" jsonschema:"data node with the highest disk usage"`
	WorstNodeUsedPercent float64 `json:"worst_node_used_percent,omitempty" jsonschema:"disk usage of that node, in percent"`

	// Indices lists the velruna indices (facts, docs, journal periods) with
	// their size and shard layout: the "is the store growing, and how" signal.
	// Sorted by name; capped so a store with many history periods stays cheap.
	Indices          []IndexStat `json:"indices,omitempty" jsonschema:"velruna indices with documents, size and shard layout (sorted by name)"`
	IndicesTruncated bool        `json:"indices_truncated,omitempty" jsonschema:"true when more indices exist than the reported page"`

	// Replica drift for the facts index: the configured replica count versus
	// what the current topology asks for. A drift means the reconcile did not
	// take (or could not: a single node cannot host a replica).
	DesiredReplicas *int `json:"desired_replicas,omitempty" jsonschema:"replicas the current topology asks for (config index_replicas, or the auto rule)"`
	ActualReplicas  *int `json:"actual_replicas,omitempty" jsonschema:"replicas configured on the facts index"`
	ReplicaDrift    bool `json:"replica_drift,omitempty" jsonschema:"true when actual differs from desired: the replica reconcile did not converge"`

	// Unavailable names what could not be collected and why. This is diagnostics:
	// a partial answer is still useful and must not fail status.
	Unavailable string `json:"unavailable,omitempty" jsonschema:"parts of the storage state that could not be collected"`
}

// StorageState gathers cluster health, disk usage and the index read-only flag.
// It never returns an error: problems are reported in Unavailable.
func (c *Client) StorageState(ctx context.Context) *StorageState {
	st := &StorageState{}
	var problems []string

	// Probe index existence before asking anything index-scoped. A GET on
	// _cluster/health/<index> blocks server-side until its own timeout (≈30 s)
	// when the index is missing, so status used to hang on a fresh install.
	// HEAD on the index answers immediately.
	exists, err := c.indexExists(ctx)
	if err != nil {
		problems = append(problems, "index existence: "+err.Error())
	}

	if health, err := c.api.Cluster.Health(ctx, nil); err != nil {
		problems = append(problems, "cluster health: "+err.Error())
	} else {
		st.ClusterHealth = health.Status
		st.Unassigned = health.UnassignedShards
		st.DataNodes = health.NumberOfDataNodes
	}

	switch {
	case err != nil:
		// Existence unknown: do not risk the blocking index-scoped query.
	case !exists:
		st.IndexAbsent = true
	default:
		idxReq := &opensearchapi.ClusterHealthReq{Indices: []string{c.cfg.IndexName}}
		if idxHealth, err := c.api.Cluster.Health(ctx, idxReq); err != nil {
			problems = append(problems, "index health: "+err.Error())
		} else {
			st.IndexHealth = idxHealth.Status
		}
	}

	if stats, err := c.api.Nodes.Stats(ctx, &opensearchapi.NodesStatsReq{Metric: []string{"fs"}}); err != nil {
		problems = append(problems, "disk usage: "+err.Error())
	} else {
		st.WorstNode, st.WorstNodeUsedPercent = worstDiskUsage(stats)
	}

	if exists && err == nil {
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
	}

	// _cat/indices covers the facts index, the docs index and every journal
	// period in one call: sizes are what says whether the store is growing
	// and where. bytes=b keeps store.size an exact integer, not "1.2gb".
	if cat, err := c.api.Cat.Indices(ctx, &opensearchapi.CatIndicesReq{
		Indices: []string{c.cfg.IndexName + "*"},
		Params:  opensearchapi.CatIndicesParams{Bytes: "b"},
	}); err != nil {
		problems = append(problems, "index sizes: "+err.Error())
	} else {
		for _, idx := range cat.Indices {
			si := IndexStat{Index: idx.Index, Health: idx.Health}
			if idx.DocsCount != nil {
				si.Documents = *idx.DocsCount
			}
			if idx.Primary != nil {
				si.Primaries = *idx.Primary
			}
			if idx.Replica != nil {
				si.Replicas = *idx.Replica
			}
			if idx.StoreSize != nil {
				// A malformed size drops the number, not the row: the index is
				// still useful to see.
				if n, perr := strconv.ParseInt(*idx.StoreSize, 10, 64); perr == nil {
					si.SizeBytes = n
				}
			}
			st.Indices = append(st.Indices, si)
			if idx.Index == c.cfg.IndexName && idx.Replica != nil {
				rep := *idx.Replica
				st.ActualReplicas = &rep
			}
		}
		sort.Slice(st.Indices, func(i, j int) bool { return st.Indices[i].Index < st.Indices[j].Index })
		if len(st.Indices) > indexStatCap {
			st.Indices = st.Indices[:indexStatCap]
			st.IndicesTruncated = true
		}
	}

	if st.ActualReplicas != nil {
		desired := c.indexReplicas(ctx)
		// A failed auto probe returns 0 and must not be read as "desired 0":
		// that would report a drift the operator cannot act on.
		if c.cfg.IndexReplicas >= 0 || c.replicasProbeOK {
			st.DesiredReplicas = &desired
			st.ReplicaDrift = desired != *st.ActualReplicas
		}
	}

	if len(problems) > 0 {
		st.Unavailable = strings.Join(problems, "; ")
	}
	return st
}

// indexExists probes the facts index with HEAD /<index>. It returns found ==
// false with a nil error only when OpenSearch answered 404, i.e. the index
// genuinely does not exist. Any other failure (network, auth) is returned as
// an error so the caller reports "could not check" rather than "absent".
func (c *Client) indexExists(ctx context.Context) (bool, error) {
	resp, err := c.api.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{c.cfg.IndexName},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
