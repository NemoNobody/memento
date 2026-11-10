package cmd

import (
	"context"
	"strings"
	"testing"
)

func TestAddBatchShapeLimits(t *testing.T) {
	s := &mcpServer{}
	if _, _, err := s.addBatch(context.Background(), nil, addBatchInput{}); err == nil {
		t.Error("empty batch must be refused")
	}
	many := make([]addInput, maxAddBatchItems+1)
	if _, _, err := s.addBatch(context.Background(), nil, addBatchInput{Items: many}); err == nil {
		t.Error("over-cap batch must be refused")
	}
}

// Duplicate keys inside one batch are ambiguous by construction (the second
// item corrects the first); the comparison runs on the TRIMMED key — what the
// store actually keys on — so " k " and "k" collide loudly.
func TestAddBatchDuplicateKeysRejected(t *testing.T) {
	s := &mcpServer{}
	in := addBatchInput{Items: []addInput{
		{Key: "k", Value: "v", Category: "config"},
		{Key: " k ", Value: "w", Category: "config"},
	}}
	if _, _, err := s.addBatch(context.Background(), nil, in); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want duplicate-key refusal, got %v", err)
	}
}

// The guard must run before any store contact: with an unreachable store and
// a sentinel value, the per-item errors must name the secret guard, not the
// store — proving rejection happens offline.
func TestAddBatchGuardRunsBeforeStore(t *testing.T) {
	t.Setenv("MEMENTO_SECRET_GUARD", "reject")
	t.Setenv("MEMENTO_OPENSEARCH_URL", "http://127.0.0.1:9")
	s := &mcpServer{}
	in := addBatchInput{Items: []addInput{
		{Key: "k1", Value: "clean value", Category: "config"},
		{Key: "k2", Value: "leaked: " + guardSentinel, Category: "config"},
	}}
	_, out, err := s.addBatch(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("batch-level: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(out.Items))
	}
	if !strings.Contains(out.Items[1].Error, "secret") {
		t.Errorf("guard item error = %q, want the secret guard to fire", out.Items[1].Error)
	}
	// The clean item reaches the (broken) store path: a store error, not a
	// guard error — both paths are per item, the batch itself never fails.
	if strings.Contains(out.Items[0].Error, "secret") {
		t.Errorf("clean item error = %q, want a store error", out.Items[0].Error)
	}
}
