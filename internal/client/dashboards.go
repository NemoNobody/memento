package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/NemoNobody/velruna/internal/dashboards"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// DashboardPush is the outcome of installing the Dashboards bundle.
type DashboardPush struct {
	// Index is the saved-objects write index the bundle went into.
	Index string
	// Indexed and Failed count objects; Failed objects also appear in Errors.
	Indexed int
	Failed  int
	Errors  []string
}

// kibanaWriteIndex resolves the write index behind the .kibana alias — the
// global tenant's saved-object storage.
//
// The saved-objects REST API is deliberately not used: on OSD 3.x the
// "security_tenant: global" header is ignored for basic-auth requests (the
// tenant follows the browser cookie), so an API import silently lands in the
// caller's personal tenant index. Writing the same documents the UI writes is
// the only scripted way into the global tenant.
func (c *Client) kibanaWriteIndex(ctx context.Context) (string, error) {
	resp, err := c.api.Cat.Aliases(ctx, &opensearchapi.CatAliasesReq{Aliases: []string{".kibana"}})
	if err != nil {
		return "", fmt.Errorf("resolve the .kibana alias at %s: %w (is OpenSearch reachable and has "+
			"Dashboards ever started against this cluster?)", c.cfg.OpenSearchURL, err)
	}
	for _, a := range resp.Aliases {
		if a.IsWriteIndex == "true" {
			return a.Index, nil
		}
	}
	if len(resp.Aliases) > 0 {
		// Non-UI clients sometimes create the alias without a write index;
		// the first entry is still the intended target.
		return resp.Aliases[0].Index, nil
	}
	return "", fmt.Errorf("the .kibana alias resolves to no index at %s: start OpenSearch Dashboards "+
		"once against this cluster, or pass --index", c.cfg.OpenSearchURL)
}

// PushDashboards writes the bundle into the global Dashboards tenant.
//
// index overrides the resolved write index ("" = resolve). Per-object failures
// are counted and reported instead of aborting the run: one bad object must
// not hide the twenty that would install fine. The returned error is fatal
// (bundle unreadable, alias unresolved, final refresh failed).
func (c *Client) PushDashboards(ctx context.Context, bundle []byte, index string) (*DashboardPush, error) {
	objs, err := dashboards.Parse(bundle)
	if err != nil {
		return nil, err
	}
	if index == "" {
		index, err = c.kibanaWriteIndex(ctx)
		if err != nil {
			return nil, err
		}
	}

	res := &DashboardPush{Index: index}
	now := time.Now()
	for _, o := range objs {
		doc, derr := o.Document(now)
		if derr != nil {
			res.Failed++
			res.Errors = append(res.Errors, derr.Error())
			continue
		}
		path := "/" + url.PathEscape(index) + "/_doc/" + url.PathEscape(o.DocumentID())
		if werr := c.putDocument(ctx, path, doc); werr != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", o.DocumentID(), werr))
			continue
		}
		res.Indexed++
	}

	// One refresh at the end: refreshing per document is a refresh storm on a
	// live cluster, and the caller only needs the objects visible when the
	// command returns.
	if rerr := c.refreshSavedObjects(ctx, index); rerr != nil {
		return res, rerr
	}
	return res, nil
}

// putDocument indexes one saved object in place (PUT on its stable id), so a
// re-run overwrites the same document instead of duplicating it.
func (c *Client) putDocument(ctx context.Context, path string, doc []byte) error {
	status, body, err := c.doSavedObjectRequest(ctx, http.MethodPut, path, doc)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("HTTP %d: %s", status, bodySnippet(body))
	}
	return nil
}

// refreshSavedObjects makes the writes visible to the next search.
func (c *Client) refreshSavedObjects(ctx context.Context, index string) error {
	path := "/" + url.PathEscape(index) + "/_refresh"
	status, body, err := c.doSavedObjectRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("refresh %s: %w", index, err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("refresh %s: HTTP %d: %s", index, status, bodySnippet(body))
	}
	return nil
}

// doSavedObjectRequest issues a request through the configured transport, so
// it inherits the same address, credentials and TLS settings as every other
// call (including the insecure_skip_verify used with demo certificates).
//
// Stream, not Perform: the raw Perform path is deprecated in opensearch-go v4
// and removed in v5 — Stream is the supported way to send a request the typed
// helpers do not model (the saved-objects documents here).
func (c *Client) doSavedObjectRequest(ctx context.Context, method, path string, body []byte) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rdr)
	if err != nil {
		return 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.api.Client.Stream(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(raw), nil
}

// bodySnippet keeps an error readable: a cluster response can be long, and the
// first line usually says what went wrong.
func bodySnippet(body string) string {
	b := bytes.TrimSpace([]byte(body))
	if len(b) == 0 {
		return "(empty response)"
	}
	var sb bytes.Buffer
	for _, line := range bytes.SplitN(b, []byte("\n"), 2) {
		sb.Write(line)
	}
	if sb.Len() > 400 {
		return string(sb.Bytes()[:400]) + "..."
	}
	return sb.String()
}
