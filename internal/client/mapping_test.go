package client

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
)

// mappingCase drives one mapping-reconcile scenario: the facts index exists,
// replicas are pinned (so the replica reconcile stays silent), and the mapping
// PUT answers with failFirst failures before succeeding.
func mappingCase(t *testing.T, failFirst int) (*Client, *fakeOpenSearch, *int) {
	t.Helper()
	puts := 0
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.IndexReplicas = 0
	}, func(r recordedRequest) (int, string) {
		switch {
		case r.Method == http.MethodHead && r.Path == "/memento":
			return http.StatusOK, ""
		case r.Method == http.MethodPut && r.Path == "/memento/_mapping":
			puts++
			if puts <= failFirst {
				return http.StatusInternalServerError, `{"status":500,"error":"boom"}`
			}
			return http.StatusOK, `{"acknowledged":true}`
		}
		return http.StatusInternalServerError, `{"error":"unexpected request: ` + r.Method + ` ` + r.Path + `"}`
	})
	return c, fake, &puts
}

// TestReconcileMappingOncePerProcess pins the caching half of the additive
// mapping reconcile: EnsureIndex runs on every command path, and a long-lived
// MCP server must not PUT the mapping before every call.
func TestReconcileMappingOncePerProcess(t *testing.T) {
	c, fake, _ := mappingCase(t, 0)
	for i := 0; i < 3; i++ {
		if err := c.EnsureIndex(context.Background()); err != nil {
			t.Fatalf("EnsureIndex #%d: %v", i, err)
		}
	}
	puts := fake.requestsFor(http.MethodPut, "/memento/_mapping")
	if len(puts) != 1 {
		t.Fatalf("mapping puts = %d, want 1 (cached after success)", len(puts))
	}
	if !strings.Contains(puts[0].Body, `"keywords":{"type":"keyword"}`) {
		t.Errorf("mapping body lacks the keywords field: %s", puts[0].Body)
	}
}

// TestReconcileMappingRetriesAfterFailure pins the other half: a refused PUT
// must not mark the reconcile done, or a long-lived process keeps the degraded
// dynamic mapping until it restarts.
func TestReconcileMappingRetriesAfterFailure(t *testing.T) {
	c, fake, _ := mappingCase(t, 1)
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex must stay best effort: %v", err)
	}
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex retry: %v", err)
	}
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex after success: %v", err)
	}
	puts := fake.requestsFor(http.MethodPut, "/memento/_mapping")
	if len(puts) != 2 {
		t.Fatalf("mapping puts = %d, want 2 (one failure, one retry, then cached)", len(puts))
	}
}

// TestMappingReconcileIsProcessWide: the MCP server builds a client per tool
// call, so the "already reconciled" claim has to outlive the client. Two
// clients for the same store must send the mapping PUT once.
func TestMappingReconcileIsProcessWide(t *testing.T) {
	c1, fake, _ := mappingCase(t, 0)
	if err := c1.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("first EnsureIndex: %v", err)
	}
	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if err := c2.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("second EnsureIndex: %v", err)
	}
	if puts := fake.requestsFor(http.MethodPut, "/memento/_mapping"); len(puts) != 1 {
		t.Fatalf("mapping puts = %d, want 1 (the claim is per store, not per client)", len(puts))
	}
}
