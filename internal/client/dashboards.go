package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/velruna/internal/dashboards"
)

// kibanaAlias is the alias OpenSearch Dashboards creates over its
// saved-objects indices. Writing through the alias (not through the resolved
// index) is deliberate: a concurrent .kibana_1 -> .kibana_2 migration moves the
// write index atomically, and a resolved index name would keep pointing at the
// index that is being retired.
const kibanaAlias = ".kibana"

// DashboardPush is the outcome of installing the Dashboards bundle.
type DashboardPush struct {
	// Index is the saved-objects index the bundle went into.
	Index string
	// Global reports whether Index was resolved from the .kibana alias (the
	// global tenant) rather than passed by the caller with --index.
	Global bool
	// Objects is how many objects the bundle contains; a run where Failed
	// equals Objects wrote nothing at all.
	Objects int
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
//
// The probe is a raw _cat request rather than the typed Cat helper: the helper
// turns a 401/403 body into a JSON-unmarshal error and loses the status code,
// which is exactly the part an operator needs to tell "wrong password" from
// "Dashboards never started".
func (c *Client) kibanaWriteIndex(ctx context.Context) (string, error) {
	path := "/_cat/aliases/" + url.PathEscape(kibanaAlias) + "?format=json"
	status, body, err := c.doSavedObjectRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("probe the %s alias at %s: %w%s",
			kibanaAlias, c.cfg.OpenSearchURL, err, transportHint(err))
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("probe the %s alias at %s: HTTP %d — the credentials were rejected "+
			"(check the password and the cat/aliases privilege)", kibanaAlias, c.cfg.OpenSearchURL, status)
	case http.StatusNotFound, http.StatusBadRequest:
		// Defensive: a live OpenSearch answers a named _cat alias that does not
		// exist with 200 + [] (see below), but a proxy or an older gateway may
		// turn that into 404 — either way it means "Dashboards never
		// initialized here".
		return "", fmt.Errorf("no %s alias at %s (HTTP %d): start OpenSearch Dashboards once against "+
			"this cluster, or pass --index", kibanaAlias, c.cfg.OpenSearchURL, status)
	default:
		return "", fmt.Errorf("probe the %s alias at %s: HTTP %d: %s",
			kibanaAlias, c.cfg.OpenSearchURL, status, bodySnippet(body))
	}

	var rows []struct {
		Alias        string `json:"alias"`
		Index        string `json:"index"`
		IsWriteIndex string `json:"is_write_index"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		return "", fmt.Errorf("decode the %s alias list from %s: %w",
			kibanaAlias, c.cfg.OpenSearchURL, err)
	}

	var write []string
	for _, a := range rows {
		if a.IsWriteIndex == "true" {
			write = append(write, a.Index)
		}
	}
	switch {
	case len(write) == 1:
		return write[0], nil
	case len(write) > 1:
		// Two write indices is a broken cluster state; guessing would split the
		// bundle across both.
		return "", fmt.Errorf("the %s alias has %d write indices (%s): fix the alias or pass --index",
			kibanaAlias, len(write), strings.Join(write, ", "))
	case len(rows) == 1:
		// Dashboards creates the alias without an explicit write flag on a
		// single index; one entry cannot be ambiguous. An explicit "false" is
		// different: that index is marked as NOT writable and every PUT would
		// fail after the probe.
		switch rows[0].IsWriteIndex {
		case "", "-":
			return rows[0].Index, nil
		case "false":
			return "", fmt.Errorf("the %s alias points at %s, which is marked is_write_index=false: "+
				"fix the alias or pass --index", kibanaAlias, rows[0].Index)
		}
		return "", fmt.Errorf("the %s alias has an unrecognised write flag %q on %s: pass --index",
			kibanaAlias, rows[0].IsWriteIndex, rows[0].Index)
	case len(rows) > 1:
		idx := make([]string, 0, len(rows))
		for _, r := range rows {
			idx = append(idx, r.Index)
		}
		return "", fmt.Errorf("the %s alias points at %d indices (%s) and none is marked as the write "+
			"index: pass --index to choose one", kibanaAlias, len(rows), strings.Join(idx, ", "))
	}
	return "", fmt.Errorf("the %s alias resolves to no index at %s: start OpenSearch Dashboards "+
		"once against this cluster, or pass --index", kibanaAlias, c.cfg.OpenSearchURL)
}

// transportHint adds the one sentence that a transport-level failure needs; a
// rejected credential or a 500 is reported by the caller, which knows the code.
func transportHint(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return " (network or TLS failure: check opensearch_url, the certificates and insecure_skip_verify)"
	}
	return ""
}

// PushDashboards writes the bundle into the global Dashboards tenant.
//
// index overrides the resolved write index ("" = resolve from the alias) and
// switches the target from the global tenant to whatever the caller names.
//
// Per-object failures are counted and reported instead of aborting the run: one
// bad object must not hide the twenty that would install fine. The returned
// error is fatal (bundle unreadable, alias unresolved, target index missing,
// final refresh failed) — and the result is returned alongside it, because a
// failed refresh still means the documents were written.
func (c *Client) PushDashboards(ctx context.Context, bundle []byte, index string) (*DashboardPush, error) {
	objs, err := dashboards.Parse(bundle)
	if err != nil {
		return nil, err
	}

	res := &DashboardPush{Index: index, Global: index == "", Objects: len(objs)}
	if index == kibanaAlias {
		// Naming the alias explicitly is the same request as the default: it
		// targets the global tenant, so classify it that way instead of calling
		// it a "requested index" and dropping the tenant hint.
		index = ""
		res.Global = true
	}
	// Writes go through the alias when the target was resolved from it: the
	// alias routes to the current write index even if Dashboards migrates the
	// index mid-run.
	target := index
	if index == "" {
		resolved, rerr := c.kibanaWriteIndex(ctx)
		if rerr != nil {
			return nil, rerr
		}
		res.Index = resolved
		target = kibanaAlias
	} else {
		// A wildcard, a comma list or a reserved alias would pass the existence
		// probe and then fail (or write to several indices) on every PUT; the
		// flag names one saved-objects index.
		if strings.ContainsAny(index, "*?,") || index == "_all" || strings.HasPrefix(index, "-") {
			return nil, fmt.Errorf("--index must name a single saved-objects index, not a pattern: %q", index)
		}
		// Any existing index passes the probe, and the facts index has a dynamic
		// mapping: a typo that happens to match a real index would get 22 saved
		// objects with a green "indexed 22". Saved-objects indices are .kibana*.
		if !strings.HasPrefix(index, ".kibana") {
			return nil, fmt.Errorf("--index %q is not a saved-objects index (expected .kibana*): omit "+
				"--index to resolve the .kibana alias, or name the index Dashboards actually uses", index)
		}
		ok, herr := c.savedObjectsIndexExists(ctx, index)
		if herr != nil {
			return nil, herr
		}
		if !ok {
			// Without this check a typo lands in a brand-new empty index
			// (action.auto_create_index is on by default) and every PUT returns
			// 2xx: the run looks successful and installs nothing anyone can see.
			return nil, fmt.Errorf("index %s does not exist at %s: has Dashboards ever started against this "+
				"cluster, or is --index mistyped?", index, c.cfg.OpenSearchURL)
		}
	}

	now := time.Now()
	for _, o := range objs {
		doc, derr := o.Document(now)
		if derr != nil {
			res.Failed++
			res.Errors = append(res.Errors, derr.Error())
			continue
		}
		path := "/" + url.PathEscape(target) + "/_doc/" + url.PathEscape(o.DocumentID())
		if werr := c.putDocument(ctx, path, doc); werr != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %s: %v", target, o.DocumentID(), werr))
			continue
		}
		res.Indexed++
	}

	// One refresh at the end: refreshing per document is a refresh storm on a
	// live cluster, and the caller only needs the objects visible when the
	// command returns.
	if rerr := c.refreshSavedObjects(ctx, target); rerr != nil {
		return res, rerr
	}
	return res, nil
}

// savedObjectsIndexExists probes the caller-supplied index before anything is
// written to it. Only 404 means "missing": a write-only role legitimately
// cannot call HEAD (403), and a proxy may not allow it (405) — in those cases
// the caller named the index explicitly and the PUTs are the real test.
func (c *Client) savedObjectsIndexExists(ctx context.Context, index string) (bool, error) {
	status, body, err := c.doSavedObjectRequest(ctx, http.MethodHead, "/"+url.PathEscape(index), nil)
	if err != nil {
		return false, fmt.Errorf("probe index %s: %w", index, err)
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized:
		// 401 is a credential failure, not an authorization subtlety: letting it
		// through would turn one clear message into N identical PUT errors.
		return false, fmt.Errorf("probe index %s: HTTP 401 — the credentials were rejected "+
			"(check the password)", index)
	case http.StatusMethodNotAllowed, http.StatusForbidden:
		// Write-only roles and proxies may forbid HEAD; the caller named the
		// index, so the PUTs are the real test.
		return true, nil
	}
	return false, fmt.Errorf("probe index %s: HTTP %d: %s", index, status, bodySnippet(body))
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
	if resp != nil {
		// Stream documents resp != nil together with err (a cancelled context
		// during retry backoff); the body still has to be closed.
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return 0, "", err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return resp.StatusCode, "", err
	}
	// Drain what the limit cut off (bounded) so the connection can be reused:
	// a failing push otherwise opens a fresh TLS connection per object.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(raw), nil
}

// bodySnippet keeps an error readable: the first line of a cluster response
// usually says what went wrong, and the rest is noise in a terminal.
func bodySnippet(body string) string {
	b := strings.TrimSpace(body)
	if b == "" {
		return "(empty response)"
	}
	if i := strings.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	const maxSnippet = 400
	if len(b) > maxSnippet {
		b = b[:maxSnippet]
		// A cut inside a multi-byte rune would print as a replacement char.
		for len(b) > 0 && !utf8.ValidString(b) {
			b = b[:len(b)-1]
		}
		b += "..."
	}
	return b
}
