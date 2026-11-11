package client

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

// replicasBase answers the fixed parts of a reconcile pass: the facts index
// exists (HEAD), its settings report `stamped` replicas, and settings writes
// succeed. healthDataNodes < 0 makes the health probe fail.
func replicasBase(stamped string, healthDataNodes int) func(recordedRequest) (int, string) {
	return func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/memento":
			return http.StatusOK, ""
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			if healthDataNodes < 0 {
				return http.StatusInternalServerError, `{"status":500,"error":{"type":"cluster_exception"}}`
			}
			return http.StatusOK, `{"status":"green","number_of_data_nodes":` + strconv.Itoa(healthDataNodes) + `}`
		case r.Method == http.MethodGet && r.Path == "/memento/_settings":
			return http.StatusOK, `{"memento":{"settings":{"index.number_of_replicas":` + stamped + `}}}`
		case r.Method == http.MethodGet && r.Path == "/memento-docs/_settings":
			return http.StatusNotFound, `{"status":404,"error":{"type":"index_not_found_exception"}}`
		case r.Method == http.MethodPut && r.Path == "/memento/_settings":
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
		cfg.DocsIndexName = "memento-docs"
	}, replicasBase(`"0"`, 2))
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/memento/_settings")
	if len(puts) != 1 {
		t.Fatalf("settings puts = %d, want 1", len(puts))
	}
	if !strings.Contains(puts[0].Body, `"index.number_of_replicas":1`) {
		t.Errorf("put body = %s, want replicas 1", puts[0].Body)
	}
	// The docs index is missing (404): nothing to reconcile, no write.
	if got := fake.requestsFor(http.MethodPut, "/memento-docs/_settings"); len(got) != 0 {
		t.Errorf("missing docs index must not be written to, got %d puts", len(got))
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
	if puts := fake.requestsFor(http.MethodPut, "/memento/_settings"); len(puts) != 0 {
		t.Errorf("puts = %d, want 0 on unknown topology", len(puts))
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
	puts := fake.requestsFor(http.MethodPut, "/memento/_settings")
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
	if puts := fake.requestsFor(http.MethodPut, "/memento/_settings"); len(puts) != 0 {
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
			return http.StatusOK, `{"memento-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodGet && r.Path == "/_index_template/memento-journal":
			return http.StatusOK, `{"index_templates":[{"name":"memento-journal","index_template":{"template":{"settings":{"index.number_of_replicas":"0"}}}}]}`
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"status":"green","number_of_data_nodes":3}`
		case r.Method == http.MethodPut && r.Path == "/_index_template/memento-journal":
			tplPut = r.Body
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && r.Path == "/memento-history-2026.11/_doc":
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
			return http.StatusOK, `{"memento-journal-ts":{"processors":[]}}`
		case r.Method == http.MethodGet && r.Path == "/_index_template/memento-journal":
			return http.StatusOK, `{"index_templates":[{"name":"memento-journal","index_template":{"template":{"settings":{"index.number_of_replicas":1}}}}]}`
		case r.Method == http.MethodGet && r.Path == "/_cluster/health":
			return http.StatusOK, `{"status":"green","number_of_data_nodes":3}`
		case r.Method == http.MethodPut && r.Path == "/_index_template/memento-journal":
			tplPuts++
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/_settings"):
			return http.StatusOK, `{"acknowledged":true}`
		case r.Method == http.MethodPost && r.Path == "/memento-history-2026.11/_doc":
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
