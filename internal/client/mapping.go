package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// reconcileMapping adds mapping properties an existing index was created
// without. PUT _mapping is additive and idempotent for fields that are already
// there with the same type; the point is the fields added after the index
// existed (ttl_days, keywords), which would otherwise be typed by the first
// write's dynamic mapping — a `keywords` array would become text+keyword and
// an exact tag filter would silently stop matching tags the analyzer splits.
//
// It runs once per store per process. The MCP server builds a fresh client per
// tool call, so a per-client flag would pay the PUT on every call; the claim
// below is process-wide and is released on failure, so a refused PUT (a field
// already exists with a conflicting type) is retried by the next caller.
// The failure is deliberately not fatal, matching the replica reconcile: reads
// and writes stay correct, only the type best-effort degrades, and an operator
// can see the real types in GET _mapping.
var reconciledMappings sync.Map

func (c *Client) reconcileMapping(ctx context.Context) error {
	key := c.cfg.OpenSearchURL + "|" + c.cfg.IndexName
	if _, loaded := reconciledMappings.LoadOrStore(key, struct{}{}); loaded {
		return nil
	}
	body, err := json.Marshal(map[string]interface{}{
		"properties": buildIndexProperties(),
	})
	if err != nil {
		reconciledMappings.Delete(key)
		return fmt.Errorf("reconcile index mapping %s: %w", c.cfg.IndexName, err)
	}
	if _, err := c.api.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
		Indices: []string{c.cfg.IndexName},
		Body:    bytes.NewReader(body),
	}); err != nil {
		reconciledMappings.Delete(key)
		return fmt.Errorf("reconcile index mapping %s: %w", c.cfg.IndexName, err)
	}
	return nil
}
