package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// replicasBase answers the fixed parts of a reconcile pass: the facts index
// exists (HEAD), its settings report `stamped` replicas, and settings writes
// succeed. healthDataNodes < 0 makes the health probe fail.
func replicasBase(stamped string, healthDataNodes int) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/velruna":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			if healthDataNodes < 0 {
				return http.StatusInternalServerError, `{"status":500,"error":{"type":"cluster_exception"}}`
			}
			return http.StatusOK, `{"status":"green","number_of_data_nodes":` + strconv.Itoa(healthDataNodes) + `}`
		case r.Method == http.MethodGet && r.Path == "/velruna/_settings":
			return http.StatusOK, `{"velruna":{"settings":{"index.number_of_replicas":` + stamped + `}}}`
		case r.Method == http.MethodGet && r.Path == "/velruna-docs/_settings":
			return http.StatusNotFound, `{"status":404,"error":{"type":"index_not_found_exception"}}`
		case r.Method == http.MethodGet && r.Path == "/velruna-history-*/_settings":
			return http.StatusOK, `{"velruna-history-2026.10":{"settings":{"index.number_of_replicas":"0"}},"velruna-history-2026.11":{"settings":{"index.number_of_replicas":"1"}}}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	}
}

func TestReconcileRaisesReplicasOnGrownCluster(t *testing.T) {
	// auto mode, two data nodes, the index still carries replicas "0" from
	// its single-node birth: exactly one corrective PUT with 1.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
		cfg.DocsIndexName = "velruna-docs"
	}, replicasBase(`"0"`, 2))
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/velruna/_settings")
	if len(puts) != 1 {
		t.Fatalf("settings puts = %d, want 1", len(puts))
	}
	if !strings.Contains(puts[0].Body, `"index.number_of_replicas":1`) {
		t.Errorf("put body = %s, want replicas 1", puts[0].Body)
	}
	// The docs index is missing (404): nothing to reconcile, no write.
	if got := fake.requestsFor(http.MethodPut, "/velruna-docs/_settings"); len(got) != 0 {
		t.Errorf("missing docs index must not be written to, got %d puts", len(got))
	}
	// History period indices: only the drifted period is corrected, the
	// already-correct one is left alone.
	if got := fake.requestsFor(http.MethodPut, "/velruna-history-2026.10/_settings"); len(got) != 1 {
		t.Errorf("history 2026.10 puts = %d, want 1", len(got))
	}
	if got := fake.requestsFor(http.MethodPut, "/velruna-history-2026.11/_settings"); len(got) != 0 {
		t.Errorf("history 2026.11 puts = %d, want 0 (already at desired)", len(got))
	}
}

func TestReconcileSkippedWhenProbeFails(t *testing.T) {
	// auto mode with a failed health probe: the topology is unknown, and
	// guessing 0 must never strip replicas from a possibly real cluster.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, replicasBase(`"1"`, -1))
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna/_settings"); len(puts) != 0 {
		t.Errorf("puts = %d, want 0 on unknown topology", len(puts))
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna-history-2026.10/_settings"); len(puts) != 0 {
		t.Errorf("history puts = %d, want 0 on unknown topology", len(puts))
	}
}

func TestReconcileAppliesExplicitCount(t *testing.T) {
	// Explicit index_replicas=2: reconciled even on a single-node cluster,
	// and no health probe is needed.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = 2
	}, replicasBase(`"0"`, 1))
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/velruna/_settings")
	if len(puts) != 1 || !strings.Contains(puts[0].Body, `"index.number_of_replicas":2`) {
		t.Fatalf("puts = %+v, want one put with replicas 2", puts)
	}
}

func TestReconcileNoWriteOnMatch(t *testing.T) {
	// Settings already agree (note the bare-number form: settings may arrive
	// as "1" or 1 depending on the version): zero writes.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, replicasBase(`1`, 2))
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna/_settings"); len(puts) != 0 {
		t.Errorf("puts = %d, want 0 on match", len(puts))
	}
}

func TestJournalTemplateDriftReinstalled(t *testing.T) {
	// The template stamps every NEW history index; on a grown cluster a stale
	// replicas=0 must be replaced before the next write inherits it.
	var tplPut string
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusOK, `{"velruna-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodGet && r.Path == "/_index_template/velruna-journal":
			return http.StatusOK, `{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index":{"number_of_replicas":"0","default_pipeline":"velruna-journal-ts"}}}}}]}`
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"status":"green","number_of_data_nodes":3}`
		case r.Method == http.MethodPut && r.Path == "/_index_template/velruna-journal":
			tplPut = r.Body
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && r.Path == "/velruna-history-2026.11/_doc":
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if !strings.Contains(tplPut, `"index.number_of_replicas":1`) {
		t.Errorf("template re-PUT body = %s, want replicas 1", tplPut)
	}
}

func TestJournalTemplateNoReinstallOnMatch(t *testing.T) {
	// Stamped count already matches the topology: no template write.
	tplPuts := 0
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/_ingest/pipeline/"):
			return http.StatusOK, `{"velruna-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodGet && r.Path == "/_index_template/velruna-journal":
			return http.StatusOK, `{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index":{"number_of_replicas":1}}}}}]}`
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"status":"green","number_of_data_nodes":3}`
		case r.Method == http.MethodPut && r.Path == "/_index_template/velruna-journal":
			tplPuts++
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && r.Path == "/velruna-history-2026.11/_doc":
			return http.StatusOK, `{"_id":"e1","result":"created"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected: ` + r.Method + " " + r.Path + `"}`
	})
	e := model.NewJournalEntry("k", "id", "p", "config", model.OpWrite, "v", "", "s", time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if _, err := c.AppendJournal(context.Background(), e); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if tplPuts != 0 {
		t.Errorf("template puts = %d, want 0 on match", tplPuts)
	}
}

func TestTemplateStampedReplicas_Forms(t *testing.T) {
	// GET /_index_template returns NESTED settings in production (the old flat
	// test body was the bug); the flat key stays as a fallback.
	cases := map[string]int{
		`{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index":{"number_of_replicas":"0","default_pipeline":"p"}}}}}]}`: 0,
		`{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index":{"number_of_replicas":1}}}}}]}`:                          1,
		`{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index.number_of_replicas":"2"}}}}]}`:                            2,
		`{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":{"index":{"number_of_shards":"1"}}}}}]}`:                          -1,
		`{"index_templates":[{"name":"velruna-journal","index_template":{"template":{"settings":null}}}]}`:                                                        -1,
		`{"index_templates":[]}`: -1,
	}
	for body, want := range cases {
		var resp opensearchapi.IndexTemplateGetResp
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		if got := templateStampedReplicas(&resp); got != want {
			t.Errorf("body %s: templateStampedReplicas = %d, want %d", body, got, want)
		}
	}
	if got := templateStampedReplicas(nil); got != -1 {
		t.Errorf("nil response = %d, want -1", got)
	}
}

func TestReconcileZeroDataNodesUnknown(t *testing.T) {
	// A healthy-looking probe that reports zero data nodes is not a trustworthy
	// topology: reconcile must never write toward 0 off it.
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, replicasBase(`1`, 0))
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna/_settings"); len(puts) != 0 {
		t.Errorf("puts = %d, want 0 on a zero-node topology", len(puts))
	}
}

func TestReconcileHistoryWildcardMissing(t *testing.T) {
	// No history indices yet (or the role cannot read them): the wildcard GET
	// 404s — reconcile must succeed and write nothing for history.
	base := replicasBase(`1`, 2)
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/velruna-history-*/_settings" {
			return http.StatusNotFound, `{"status":404,"error":{"type":"index_not_found_exception"}}`
		}
		return base(r)
	})
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/velruna/_settings"); len(puts) != 0 {
		t.Errorf("facts puts = %d, want 0 on match", len(puts))
	}
}

func TestReconcilePutFailureBestEffort(t *testing.T) {
	// A failed corrective settings write (read-only store) must not fail the
	// operation the caller asked for.
	base := replicasBase(`"0"`, 2)
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = -1
	}, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings") {
			return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception"}}`
		}
		return base(r)
	})
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex must survive a failed replica PUT: %v", err)
	}
}
