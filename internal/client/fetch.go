package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

// maxFetchRedirects bounds redirect hops: more than a handful is a loop or a
// tracker chain, not a document.
const maxFetchRedirects = 5

// Via routes recorded on a doc: which path actually served the fetch.
const (
	ViaDirect  = "direct"
	ViaProxy   = "proxy"
	ViaObscura = "obscura"
)

// FetchError classes why a fetch failed, so the render fallback can decide
// on the REASON (the raw status 0 is too wide: it hides "the host is not
// allowed" behind "the network is down"). The classes that justify handing
// the URL to a browser renderer are the ones where the client — not the
// document — is the problem: network/DNS/timeout/TLS failures and the
// IP-ban statuses.
type FetchError struct {
	Class  string // network | status | redirect | policy | oversize
	Status int    // set for Class==status
	Err    error
}

func (e *FetchError) Error() string { return e.Err.Error() }
func (e *FetchError) Unwrap() error { return e.Err }

// Fetch failure classes.
const (
	ClassNetwork  = "network"  // no HTTP response: DNS, connect, timeout, TLS
	ClassStatus   = "status"   // an HTTP status >= 400 (see Status)
	ClassRedirect = "redirect" // the redirect chain hit its bound
	ClassPolicy   = "policy"   // allowlist / SSRF refusals: the proxy and the renderer see the same wall
	ClassOversize = "oversize" // the document is larger than fetch_max_bytes
)

// Sentinels wrapped into CheckRedirect refusals so the class survives
// net/http's url.Error wrapping.
var (
	ErrRedirectLimit = errors.New("redirect limit")
	ErrFetchPolicy   = errors.New("fetch policy")
)

// renderEligible answers: should a failed plain fetch be retried through the
// configured browser renderer? Network failures and IP-ban statuses are
// eligible (a real browser from a new route may get in). Redirect limits,
// policy refusals, 404/401 and oversize are not: the renderer faces the
// same wall, the same document, or a wall the operator put up on purpose.
func renderEligible(err error) bool {
	var fe *FetchError
	if !errors.As(err, &fe) {
		return false
	}
	switch fe.Class {
	case ClassNetwork:
		return true
	case ClassStatus:
		return ipBlockStatuses[fe.Status]
	}
	return false
}

// ipBlockStatuses are the responses of sites that ban client IP ranges
// (403 forbidden, 422 unprocessable used by some frontends). The same client
// through a proxy is a different IP, so these statuses — unlike 404/401 —
// justify a proxy retry when fallback is on.
var ipBlockStatuses = map[int]bool{
	http.StatusForbidden:           true, // 403
	http.StatusUnprocessableEntity: true, // 422
	http.StatusTooManyRequests:     true, // 429: the proxy path is a new IP; without a proxy one polite retry
}

// blockedIP reports addresses a fetch must never connect to. Fetch follows
// untrusted URLs and agent-supplied links, so the default must never reach
// internal services: loopback, RFC1918, link-local (cloud metadata sits at
// 169.254.169.254), multicast, the unspecified address, 0.0.0.0/8 (which
// routes to loopback on Linux), CGNAT (100.64.0.0/10 — Tailscale's default
// pool: on a tailnet machine these reach real hosts), the IETF reserved and
// documentation ranges, the benchmarking range, and class-E. Tunnels that
// embed an IPv4 inside an IPv6 address (6to4, Teredo) are decoded so the
// embedded v4 goes through the same checks. fetch_allow_private is the
// explicit opt-in for self-hosted targets.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 {
			return true
		}
		return inReservedV4(v4)
	}
	// 6to4 (2002::/16) embeds the IPv4 in bytes 2..5; Teredo (2001:0::/32)
	// stores the peer v4 one-complemented in the last four bytes; the NAT64
	// well-known prefix (64:ff9b::/96) and its local /48 variant
	// (64:ff9b:1::/48, RFC 8215) carry the v4 in the last four bytes.
	// Arbitrary RFC 6052 prefixes are not decoded — the documented residual.
	if b := ip.To16(); b != nil {
		if b[0] == 0x20 && b[1] == 0x02 {
			return blockedIP(net.IP(append([]byte(nil), b[2:6]...)))
		}
		if b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x00 && b[3] == 0x00 {
			v4 := net.IP{^b[12], ^b[13], ^b[14], ^b[15]}
			return blockedIP(v4)
		}
		if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && b[4] == 0x00 {
			// /96 (b[5]==0) and the local /48 (b[4..5]==00 01) share the head.
			if b[5] == 0x00 || b[5] == 0x01 {
				return blockedIP(net.IP(append([]byte(nil), b[12:16]...)))
			}
		}
	}
	return false
}

// reservedV4 are the non-global unicast v4 ranges net.IP.IsPrivate does not
// cover (RFC1918 only): CGNAT, IETF protocol assignments, documentation
// ranges, benchmarking, and class-E/broadcast.
var reservedV4 = []*net.IPNet{
	{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},
	{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},
	{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)},
	{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)},
	{IP: net.IPv4(198, 51, 100, 0), Mask: net.CIDRMask(24, 32)},
	{IP: net.IPv4(203, 0, 113, 0), Mask: net.CIDRMask(24, 32)},
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},
}

func inReservedV4(v4 net.IP) bool {
	for _, n := range reservedV4 {
		if n.Contains(v4) {
			return true
		}
	}
	return false
}

// blockedHost is the cheap pre-DNS guard: literal IPs are checked directly,
// and special-use domains that resolve loopback (localhost, .local, .internal,
// .home, .home.arpa, .test) are refused by name. The authoritative check is
// guardedDialContext, which sees the resolved address.
func blockedHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return blockedIP(ip)
	}
	switch {
	case host == "localhost",
		strings.HasSuffix(host, ".localhost"),
		strings.HasSuffix(host, ".local"),
		strings.HasSuffix(host, ".internal"),
		strings.HasSuffix(host, ".home"),
		strings.HasSuffix(host, ".home.arpa"),
		strings.HasSuffix(host, ".test"):
		return true
	}
	return false
}

// guardedDialContext refuses connections whose resolved address is not public
// (SSRF/DNS-rebinding defense). It is installed on direct (non-proxied)
// transports only: with a proxy configured, the user chose where the traffic
// exits, and the proxy — not memento — enforces the target policy.
//
// allowAddr (nil = nothing allowed) exempts specific dial addresses: the env
// proxy's resolved IPs, so a loopback HTTP_PROXY keeps working while every
// direct hop (including NO_PROXY redirect targets) stays guarded. The dial
// target is checked after resolution, so DNS rebinding cannot slip past it.
func guardedDialContext(allowAddr func(address string) bool) func(ctx context.Context, network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	d.Control = func(network, address string, c syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("fetch: guard: %w", err)
		}
		if blockedIP(net.ParseIP(host)) {
			if allowAddr != nil && allowAddr(address) {
				return nil // proxied hop: the dial target is the user's proxy
			}
			return fmt.Errorf("fetch: refusing connection to non-public address %s", host)
		}
		return nil
	}
	return d.DialContext
}

// NormalizeURL turns user input into a canonical fetchable URL: a missing
// scheme becomes https, the fragment is dropped (it selects a viewport, not a
// document), the host is lowercased, and utm_* tracking parameters are cut —
// they carry no context and split the shared URL-cache into duplicates.
// Anything that is not a plain public http(s) URL is rejected before any
// request is made. allowPrivate mirrors fetch_allow_private: without it the
// name-level pre-guard refuses literal private IPs and special-use names
// (the authoritative check is guardedDialContext on the resolved address);
// with it the operator opted in to self-hosted targets, and the name check
// must not defeat that documented opt-in.
func NormalizeURL(raw string, allowPrivate bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("fetch: empty url")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The parse error echoes the input, so a URL with credentials in it
		// must never be quoted: the message reaches agent context and logs.
		if strings.Contains(raw, "@") {
			return "", fmt.Errorf("fetch: url rejected: invalid syntax (the url carries user info; its text is not echoed)")
		}
		return "", fmt.Errorf("fetch: parse url %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("fetch: scheme %q is not supported (http/https only)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("fetch: url %q has no host", config.RedactURL(raw))
	}
	if u.User != nil {
		// Credentials in a URL are a leak vector (they reach the target host);
		// the fetcher never sends them and refuses to accept them. The message
		// carries the REDACTED url: raw contains the password itself. This check
		// must precede the blocked-host one: u.Host embeds the userinfo
		// ("admin:secret@host"), so its message would echo the password.
		return "", fmt.Errorf("fetch: url %q carries user info; remove credentials from the url", config.RedactURL(raw))
	}
	if !allowPrivate && blockedHost(strings.ToLower(u.Hostname())) {
		return "", fmt.Errorf(
			"fetch: host %q is not a public address; fetch refuses loopback/private/link-local targets "+
				"(opt in with fetch_allow_private for self-hosted docs)", u.Host)
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
	if p := u.Port(); (p == "80" && u.Scheme == "http") || (p == "443" && u.Scheme == "https") {
		// The default port is not part of the document id: a shared URL
		// cache must not split "host:443" and "host" into two snapshots.
		// Hostname() strips the brackets of IPv6 literals; restore them —
		// an unbracketed address re-parses as a bogus host+port.
		h := u.Hostname()
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		u.Host = h
	}
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
			// The parse error echoes the input: a proxy url with credentials
			// in it must not be quoted into agent-visible output.
			if strings.Contains(cfg.FetchProxyURL, "@") {
				return "", nil, "", "", fmt.Errorf("fetch: parse fetch_proxy_url: invalid syntax (it carries user info; its text is not echoed)")
			}
			return "", nil, "", "", fmt.Errorf("fetch: parse fetch_proxy_url %q: %w", cfg.FetchProxyURL, err)
		}
	}

	attempt := func(p *url.URL) (string, []byte, string, int, error) {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// The MCP server is long-lived and every fetch clones a fresh transport;
		// idle keep-alive connections and their goroutines would leak. Registered
		// first so LIFO runs it last, after the response body is closed.
		defer tr.CloseIdleConnections()
		if p != nil {
			tr.Proxy = func(*http.Request) (*url.URL, error) { return p, nil }
		} else if !cfg.FetchAllowPrivate {
			// Direct fetch: enforce the public-address rule on the resolved IP
			// (closes DNS rebinding: the hostname check alone is not enough).
			// An env proxy (HTTP_PROXY, honored by the cloned transport) is
			// user infra like fetch_proxy_url: on a proxied hop the dial target
			// is the proxy (loopback proxies are legit). The proxy decision is
			// re-evaluated per hop and only the proxy's resolved addresses are
			// exempt — every direct hop, including NO_PROXY-exempt redirect
			// targets, stays guarded.
			envProxy := tr.Proxy
			var mu sync.Mutex
			var proxyIPs []net.IP
			tr.Proxy = func(req *http.Request) (*url.URL, error) {
				u, perr := envProxy(req)
				mu.Lock()
				proxyIPs = nil
				if perr == nil && u != nil && u.Hostname() != "" {
					if ips, aerr := net.LookupIP(u.Hostname()); aerr == nil {
						proxyIPs = ips
					}
				}
				mu.Unlock()
				return u, perr
			}
			tr.DialContext = guardedDialContext(func(address string) bool {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return false
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return false
				}
				mu.Lock()
				defer mu.Unlock()
				for _, a := range proxyIPs {
					if a.Equal(ip) {
						return true
					}
				}
				return false
			})
		}
		httpc := &http.Client{Timeout: timeout, Transport: tr}
		httpc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > maxFetchRedirects {
				return fmt.Errorf("fetch: more than %d redirects: %w", maxFetchRedirects, ErrRedirectLimit)
			}
			// The allowlist must hold for every hop: an allowed host that
			// 302s to a loopback/link-local target must not be fetched
			// (SSRF via redirect would silently break the privacy guarantee).
			if err := HostAllowed(cfg, req.URL.String()); err != nil {
				return fmt.Errorf("fetch: redirect chain: %v: %w", err, ErrFetchPolicy)
			}
			if !cfg.FetchAllowPrivate && blockedHost(strings.ToLower(req.URL.Hostname())) {
				// Hostname(), not Host: a Location header can carry userinfo,
				// and the message reaches agent context.
				return fmt.Errorf("fetch: redirect chain: %s is not a public address: %w", req.URL.Hostname(), ErrFetchPolicy)
			}
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizedURL, nil)
		if err != nil {
			return "", nil, "", 0, fmt.Errorf("fetch: new request: %w", err)
		}
		req.Header.Set("Accept", "text/html, text/plain;q=0.8, text/markdown;q=0.6")
		ua := cfg.FetchUserAgent
		if ua == "" {
			ua = "memento/fetch (+OpenSearch docs cache)"
		}
		req.Header.Set("User-Agent", ua)

		resp, err := httpc.Do(req)
		if err != nil {
			// Classify the no-response failures for the render fallback; the
			// message keeps the target for operators.
			switch {
			case errors.Is(err, ErrRedirectLimit):
				return "", nil, "", 0, &FetchError{Class: ClassRedirect, Err: fmt.Errorf("fetch %s: %w", normalizedURL, err)}
			case errors.Is(err, ErrFetchPolicy):
				return "", nil, "", 0, &FetchError{Class: ClassPolicy, Err: fmt.Errorf("fetch %s: %w", normalizedURL, err)}
			default:
				return "", nil, "", 0, &FetchError{Class: ClassNetwork, Err: fmt.Errorf("fetch %s: %w", normalizedURL, err)}
			}
		}
		defer func() { _ = resp.Body.Close() }()
		final := resp.Request.URL.String()

		if resp.StatusCode >= 400 {
			return "", nil, final, resp.StatusCode,
				&FetchError{Class: ClassStatus, Status: resp.StatusCode,
					Err: fmt.Errorf("fetch %s: HTTP %d", normalizedURL, resp.StatusCode)}
		}
		ct := resp.Header.Get("Content-Type")
		// Read one byte past the cap to tell "exactly at cap" from "over cap".
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
		if err != nil {
			return "", nil, final, resp.StatusCode, &FetchError{Class: ClassNetwork, Err: fmt.Errorf("fetch %s: read body: %w", normalizedURL, err)}
		}
		if len(body) > maxBytes {
			return "", nil, final, resp.StatusCode, &FetchError{Class: ClassOversize, Err: fmt.Errorf(
				"fetch %s: body exceeds fetch_max_bytes (%d); the page is too large to cache",
				normalizedURL, maxBytes)}
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
	// 429 without a proxy means a rate limit on this IP: one polite retry
	// (a single short pause, no hammering). Retry-After is deliberately not
	// honored: a hostile server could park the client for hours — the
	// documented tradeoff.
	if status == http.StatusTooManyRequests && proxy == nil {
		select {
		case <-ctx.Done():
			return "", nil, final, ViaDirect, err
		case <-time.After(2 * time.Second):
		}
		ct, body, final, status, err = attempt(nil)
		if err == nil {
			return ct, body, final, ViaDirect, nil
		}
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
// text/plain and markdown are chunked directly; anything else (PDF, images,
// JSON feeds) is rejected — silent garbage extraction is worse than an
// explicit error.
//
// hook runs after extraction and BEFORE the document is finalized
// (size/content_hash computed from the final, possibly redacted text): the
// write-time secret guard rewrites agent-visible fields here, so the stored
// metadata describes exactly what the agent-visible content is. A hook
// error rejects the page (the guard's policy lives in the caller; the
// client only owns the timing).
func FetchPage(ctx context.Context, cfg *config.Config, normalizedURL string,
	hook func(*model.Doc) ([]model.Redaction, error)) (*model.Doc, error) {
	if err := CheckRobots(ctx, cfg, normalizedURL); err != nil {
		return nil, err
	}
	ct, body, finalURL, via, err := fetchHTTP(ctx, cfg, normalizedURL)
	if err != nil {
		// Render fallback (opt-in): only for failures the renderer can
		// plausibly beat (network walls, IP-ban statuses). On render failure
		// the answer names BOTH reasons: the plain wall the operator hit, and
		// what the renderer said.
		if cfg.FetchRender == config.RenderObscura && renderEligible(err) {
			rbody, rerr := renderObscura(ctx, cfg, normalizedURL)
			if rerr != nil {
				return nil, fmt.Errorf("fetch %s: plain fetch: %v; obscura render: %w", normalizedURL, err, rerr)
			}
			body, ct, finalURL, via = rbody, "text/html", normalizedURL, ViaObscura
		} else {
			return nil, err
		}
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))

	var sections []model.Section
	var images []model.ImageRef
	var truncated bool
	switch media {
	case "text/html", "application/xhtml+xml", "":
		sections, images, truncated = ExtractSections(string(body))
	case "text/plain", "text/markdown", "text/x-markdown":
		sections, truncated = ExtractPlainText(string(body))
	default:
		return nil, fmt.Errorf("fetch: content type %q is not supported (text/html, text/plain, text/markdown only)", media)
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("fetch %s: no readable text extracted (content type %q)", normalizedURL, media)
	}

	title := ExtractTitle(string(body))
	if title == "" {
		title = normalizedURL
	}
	images = resolveImageRefs(images, finalURL)

	host, _ := os.Hostname()
	now := time.Now().UTC()
	doc := &model.Doc{
		URL:       normalizedURL,
		URLHash:   URLHash(normalizedURL),
		FinalURL:  finalURL,
		Title:     title,
		FetchedAt: now,
		SizeRaw:   len(body),
		Truncated: truncated,
		FetchedBy: host,
		Via:       via,
		HTML:      string(body),
		Sections:  sections,
		Images:    images,
	}
	if hook != nil {
		redactions, herr := hook(doc)
		if herr != nil {
			return nil, herr
		}
		doc.Redactions = redactions
	}

	// Finalize after the hook: size and content_hash describe the text that
	// will actually be stored (a redacted span must not be counted as if it
	// shipped whole).
	size := 0
	sum := sha256.New()
	for i := range doc.Sections {
		doc.Sections[i].Order = i
		size += len([]rune(doc.Sections[i].Text))
		sum.Write([]byte(doc.Sections[i].Text))
		sum.Write([]byte{'\n'})
	}
	doc.Size = size
	doc.ContentHash = hex.EncodeToString(sum.Sum(nil))
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
