package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// reconcileMapping adds mapping properties an existing index was created
// without. PUT _mapping is additive and idempotent for fields that are already
// there with the same type; the point is the fields added after the index
// existed (ttl_days, keywords), which would otherwise be typed by the first
// write's dynamic mapping — a `keywords` array would become text+keyword and
// an exact tag filter would silently stop matching tags the analyzer splits.
//
// It runs once per process (EnsureIndex is on every command path), so the
// probe is cached — but only on success: a refused PUT (a field already exists
// with a conflicting type) leaves the guard open, and the next command retries.
// The failure is deliberately not fatal, matching the replica reconcile: reads
// and writes stay correct, only the type best-effort degrades, and an operator
// can see the real types in GET _mapping.
func (c *Client) reconcileMapping(ctx context.Context) error {
	c.mappingMu.Lock()
	defer c.mappingMu.Unlock()
	if c.mappingOK {
		return nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"properties": buildIndexProperties(),
	})
	if err != nil {
		return fmt.Errorf("reconcile index mapping %s: %w", c.cfg.IndexName, err)
	}
	if _, err := c.api.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(body),
	}); err != nil {
		return fmt.Errorf("reconcile index mapping %s: %w", c.cfg.IndexName, err)
	}
	c.mappingOK = true
	return nil
}
