package client

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// TestAuthorResolveIsProcessWide: the writer label is resolved once per store,
// not once per client. The MCP server builds a client per tool call, so a
// per-client cache would probe GET /_security/authinfo on every write.
func TestAuthorResolveIsProcessWide(t *testing.T) {
	probes := 0
	c1, _ := newFakeClientCfg(t, nil, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/_security/authinfo" {
			probes++
			return http.StatusOK, `{"user_name":"alice"}`
		}
		return http.StatusNotFound, `{"status":404}`
	})
	if got := c1.resolveAuthor(context.Background()); got.AuthorSource != AuthorSourceSecurity {
		t.Fatalf("first resolve = %q, want security", got.AuthorSource)
	}

	c2, err := NewClient(c1.cfg)
	if err != nil {
		t.Fatalf("second NewClient: %v", err)
	}
	if got := c2.resolveAuthor(context.Background()); got.AuthorSource != AuthorSourceSecurity {
		t.Fatalf("second resolve = %q, want security", got.AuthorSource)
	}
	if probes != 1 {
		t.Fatalf("authinfo probes = %d, want 1 (the cache is per store, not per client)", probes)
	}
}

// TestInfraGatesAreBounded: a day-period journal adds one pipeline key per day,
// so the claim map must not grow without bound.
func TestInfraGatesAreBounded(t *testing.T) {
	resetProcessCaches()
	for i := 0; i < maxInfraGates+10; i++ {
		infraGate("pipeline|store|history-" + strconv.Itoa(i))
	}
	infraGatesMu.Lock()
	n := len(infraGates)
	infraGatesMu.Unlock()
	if n > maxInfraGates {
		t.Fatalf("gates = %d, want at most %d", n, maxInfraGates)
	}
}
