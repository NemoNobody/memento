package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

// maxFetchRedirects bounds redirect hops: more than a handful is a loop or a
// tracker chain, not a document.
const maxFetchRedirects = 5

// Via routes recorded on a doc: which path actually served the fetch.
const (
	ViaDirect = "direct"
	ViaProxy  = "proxy"
)

// ipBlockStatuses are the responses of sites that ban client IP ranges
// (403 forbidden, 422 unprocessable used by some frontends). The same client
// through a proxy is a different IP, so these statuses — unlike 404/401 —
// justify a proxy retry when fallback is on.
var ipBlockStatuses = map[int]bool{
	http.StatusForbidden:           true, // 403
	http.StatusUnprocessableEntity: true, // 422
}

// NormalizeURL turns user input into a canonical fetchable URL: a missing
// scheme becomes https, the fragment is dropped (it selects a viewport, not a
// document), the host is lowercased, and utm_* tracking parameters are cut —
// they carry no context and split the shared URL-cache into duplicates.
// Anything that is not a plain public http(s) URL is rejected before any
// request is made.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("fetch: empty url")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("fetch: parse url %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("fetch: scheme %q is not supported (http/https only)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("fetch: url %q has no host", raw)
	}
	if u.User != nil {
		// Credentials in a URL are a leak vector (they reach the target host);
		// the fetcher never sends them and refuses to accept them.
		return "", fmt.Errorf("fetch: url %q carries user info; remove credentials from the url", raw)
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path == "" {
		// "example.com" and "https://example.com/" are the same document;
		// an empty path must not split the shared URL-cache into two ids.
		u.Path = "/"
	}
	return u.String(), nil
}

// URLHash is the stable document id: first 16 hex chars of sha256 over the
// normalized URL. The URL cache is shared across all projects on purpose.
// Normalization makes "example.com" and "https://example.com/" land on the
// same doc.
func URLHash(normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))
	return hex.EncodeToString(sum[:])[:16]
}

// HostAllowed checks the URL host against the fetch allowlist. An empty list
// allows every host; a list entry matches exactly, and an entry with a leading
// dot matches its subdomains (".example.com" → "docs.example.com").
// Normalization must run first; HostAllowed validates the result.
func HostAllowed(cfg *config.Config, normalizedURL string) error {
	if len(cfg.FetchAllowHosts) == 0 {
		return nil
	}
	u, err := url.Parse(normalizedURL)
	if err != nil {
		return fmt.Errorf("fetch: parse url: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	for _, entry := range cfg.FetchAllowHosts {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) || host == entry[1:] {
				return nil
			}
			continue
		}
		if host == entry {
			return nil
		}
	}
	return fmt.Errorf("fetch: host %q is not in fetch_allow_hosts", host)
}

// fetchHTTP downloads the raw page body under the configured caps. It is the
// only network path for document input: bounded timeout, bounded body, bounded
// redirects, no cookies and no store credentials — requests are uncredentialed
// so fetching cannot leak the user's identity to the target host.
//
// Proxy semantics: with fetch_proxy_url set and fallback off, every request
// goes through the proxy. With fallback on, the request starts direct and
// retries through the proxy on a network error/timeout or an IP-ban status
// (403/422). 404/401 are final: the client, not the IP, is the problem.
func fetchHTTP(ctx context.Context, cfg *config.Config, normalizedURL string) (contentType string, body []byte, finalURL string, via string, err error) {
	timeout := time.Duration(cfg.FetchTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxBytes := cfg.FetchMaxBytes
	if maxBytes <= 0 {
		maxBytes = 2_000_000
	}

	var proxy *url.URL
	if cfg.FetchProxyURL != "" {
		proxy, err = url.Parse(cfg.FetchProxyURL)
		if err != nil {
			return "", nil, "", "", fmt.Errorf("fetch: parse fetch_proxy_url %q: %w", cfg.FetchProxyURL, err)
		}
	}

	attempt := func(p *url.URL) (string, []byte, string, int, error) {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if p != nil {
			tr.Proxy = func(*http.Request) (*url.URL, error) { return p, nil }
		}
		httpc := &http.Client{Timeout: timeout, Transport: tr}
		httpc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > maxFetchRedirects {
				return fmt.Errorf("fetch: more than %d redirects", maxFetchRedirects)
			}
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizedURL, nil)
		if err != nil {
			return "", nil, "", 0, fmt.Errorf("fetch: new request: %w", err)
		}
		req.Header.Set("Accept", "text/html, text/plain;q=0.8")
		ua := cfg.FetchUserAgent
		if ua == "" {
			ua = "memento/fetch (+OpenSearch docs cache)"
		}
		req.Header.Set("User-Agent", ua)

		resp, err := httpc.Do(req)
		if err != nil {
			return "", nil, "", 0, fmt.Errorf("fetch %s: %w", normalizedURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		final := resp.Request.URL.String()

		if resp.StatusCode >= 400 {
			return "", nil, final, resp.StatusCode,
				fmt.Errorf("fetch %s: HTTP %d", normalizedURL, resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		// Read one byte past the cap to tell "exactly at cap" from "over cap".
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
		if err != nil {
			return "", nil, final, resp.StatusCode, fmt.Errorf("fetch %s: read body: %w", normalizedURL, err)
		}
		if len(body) > maxBytes {
			return "", nil, final, resp.StatusCode, fmt.Errorf(
				"fetch %s: body exceeds fetch_max_bytes (%d); the page is too large to cache",
				normalizedURL, maxBytes)
		}
		return ct, body, final, resp.StatusCode, nil
	}

	// Proxy-only mode: fallback off and proxy configured → every request proxied.
	if proxy != nil && !cfg.FetchProxyFallback {
		ct, body, final, _, err := attempt(proxy)
		return ct, body, final, ViaProxy, err
	}

	ct, body, final, status, err := attempt(nil)
	if err == nil {
		return ct, body, final, ViaDirect, nil
	}
	// Fallback triggers: no HTTP response at all (network failure, timeout,
	// redirect limit — status 0), or an IP-ban status. Any real status is
	// final: 404/401 are not IP bans, and oversize (under a 200) is a property
	// of the document, so the proxy would fetch the same over-large page.
	isIPBlock := ipBlockStatuses[status]
	isNetwork := status == 0
	if !isIPBlock && !isNetwork {
		return "", nil, final, ViaDirect, err
	}
	if proxy == nil {
		return "", nil, final, ViaDirect, err
	}
	ct, body, final2, _, perr := attempt(proxy)
	if perr != nil {
		return "", nil, final2, ViaProxy, fmt.Errorf("%w (direct: %v)", perr, err)
	}
	return ct, body, final2, ViaProxy, nil
}

// FetchPage downloads a page and extracts it into a full Doc snapshot
// (without storing it). The raw HTML is carried for storage only: no read
// path of memento serves it (context economy — raw pages are curl's job).
//
// content-type routing: HTML goes through section+image extraction,
// text/plain is chunked directly; anything else (PDF, images, JSON feeds) is
// rejected — silent garbage extraction is worse than an explicit error.
func FetchPage(ctx context.Context, cfg *config.Config, normalizedURL string) (*model.Doc, error) {
	if err := CheckRobots(ctx, cfg, normalizedURL); err != nil {
		return nil, err
	}
	ct, body, finalURL, via, err := fetchHTTP(ctx, cfg, normalizedURL)
	if err != nil {
		return nil, err
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))

	var sections []model.Section
	var images []model.ImageRef
	var truncated bool
	switch media {
	case "text/html", "application/xhtml+xml", "":
		sections, images, truncated = ExtractSections(string(body))
	case "text/plain":
		sections, truncated = ExtractPlainText(string(body))
	default:
		return nil, fmt.Errorf("fetch: content type %q is not supported (text/html, text/plain only)", media)
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("fetch %s: no readable text extracted (content type %q)", normalizedURL, media)
	}

	title := ExtractTitle(string(body))
	if title == "" {
		title = normalizedURL
	}
	size := 0
	sum := sha256.New()
	for i := range sections {
		sections[i].Order = i
		size += len([]rune(sections[i].Text))
		sum.Write([]byte(sections[i].Text))
		sum.Write([]byte{'\n'})
	}
	images = resolveImageRefs(images, finalURL)

	host, _ := os.Hostname()
	now := time.Now().UTC()
	doc := &model.Doc{
		URL:         normalizedURL,
		URLHash:     URLHash(normalizedURL),
		FinalURL:    finalURL,
		ContentHash: hex.EncodeToString(sum.Sum(nil)),
		Title:       title,
		FetchedAt:   now,
		Size:        size,
		SizeRaw:     len(body),
		Truncated:   truncated,
		FetchedBy:   host,
		Via:         via,
		HTML:        string(body),
		Sections:    sections,
		Images:      images,
	}
	if cfg.DocsTTLDays > 0 {
		doc.ExpiresAt = now.AddDate(0, 0, cfg.DocsTTLDays)
	}
	return doc, nil
}

// resolveImageRefs resolves relative img srcs against the final URL, so the
// stored references are openable from anywhere. Unparseable or non-http(s)
// schemes (mailto, javascript, data) keep their original form.
func resolveImageRefs(images []model.ImageRef, baseURL string) []model.ImageRef {
	if len(images) == 0 {
		return images
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" {
		return images
	}
	for i := range images {
		u, err := url.Parse(images[i].URL)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(u)
		if abs.Scheme == "http" || abs.Scheme == "https" {
			images[i].URL = abs.String()
		}
	}
	return images
}
