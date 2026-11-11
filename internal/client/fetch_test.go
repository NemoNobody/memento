package client

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

// writeBody serves a fixture body; a write failure means the page never
// reached the client, so continuing the test would only produce noise.
func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("fixture write: %v", err)
	}
}

// TestFetchEnvProxySkipsDialGuard pins the proxy/guard boundary: an
// HTTP_PROXY env route is user infra (like fetch_proxy_url) — the dial
// target is the proxy (loopback proxies are legit), so the public-address
// guard must not fire on it. Without a proxy the guard must still block
// loopback targets at dial time.
//
// Note on the stdlib proxy cache: http.ProxyFromEnvironment parses the env
// once per process, so t.Setenv here populates a cache that later tests in
// this package inherit. The direct-path leg below still passes because
// httpproxy bypasses loopback targets by default — not because the cache is
// empty. A future non-loopback direct-path test in this package must not
// trust the ambient env: pass the proxy decision explicitly (as the render
// path does with renderEgressProxyWith) or run in a fresh process.
func TestFetchEnvProxySkipsDialGuard(t *testing.T) {
	cfg := &config.Config{FetchTimeoutSeconds: 5}

	// Proxied path: Go's ProxyFromEnvironment bypasses loopback targets, so
	// the proxied URL must be a non-localhost host; the discard port (9)
	// refuses the proxy dial fast, with no DNS and no network.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	_, err := fetchHTTP(context.Background(), cfg, "http://example.invalid/x", fetchCondition{})
	if err == nil {
		t.Fatal("want failure against the discard proxy")
	}
	if strings.Contains(err.Error(), "refusing connection to non-public address") {
		t.Errorf("env-proxy dial hit the target guard: %v", err)
	}

	// Direct path: no env proxy, loopback target → the dial guard fires.
	t.Setenv("HTTP_PROXY", "")
	_, err = fetchHTTP(context.Background(), cfg, "http://127.0.0.1:19/x", fetchCondition{})
	if err == nil {
		t.Fatal("want failure against loopback")
	}
	if !strings.Contains(err.Error(), "refusing connection to non-public address") {
		t.Errorf("direct loopback dial must hit the guard: %v", err)
	}
}

// TestGuardedDialContextAllow pins the guard's exemption hook: the dial is
// refused on blocked addresses unless allowAddr exempts that exact resolved
// address (the env proxy's IPs), and a public IP is never exempted.
func TestGuardedDialContextAllow(t *testing.T) {
	dial := guardedDialContext(nil)
	_, err := dial(context.Background(), "tcp", "127.0.0.1:19")
	if err == nil || !strings.Contains(err.Error(), "refusing connection to non-public address") {
		t.Errorf("loopback without exemption: %v", err)
	}

	allow := guardedDialContext(func(address string) bool { return address == "127.0.0.1:19" })
	_, err = allow(context.Background(), "tcp", "127.0.0.1:19")
	if err != nil && strings.Contains(err.Error(), "refusing connection") {
		t.Errorf("loopback proxy address must be exempted, got: %v", err)
	}

	deny := guardedDialContext(func(address string) bool { return address == "127.0.0.1:20" })
	_, err = deny(context.Background(), "tcp", "127.0.0.1:19")
	if err == nil || !strings.Contains(err.Error(), "refusing connection") {
		t.Errorf("non-matching address must stay guarded: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "https://example.com/"},
		{"example.com/x?utm_source=nl&a=1", "https://example.com/x?a=1"},
		{"HTTPS://Example.COM/p#frag", "https://example.com/p"},
		{"http://h:8080/a?b=2&utm_medium=x", "http://h:8080/a?b=2"},
		{"  https://a.com/path/  ", "https://a.com/path/"},
		// Default ports are not part of the document id: the shared URL cache
		// must not split "host:443" and "host" into two snapshots.
		{"https://example.com:443/x", "https://example.com/x"},
		{"http://example.com:80/x", "http://example.com/x"},
		{"https://example.com:8443/x", "https://example.com:8443/x"},
		{"http://example.com:443/", "http://example.com:443/"},
		// IPv6 literals keep their brackets after the default-port strip —
		// an unbracketed address re-parses as a bogus host+port.
		{"https://[2606:4700:4700::1111]:443/x", "https://[2606:4700:4700::1111]/x"},
		{"https://[2606:4700:4700::1111]/x", "https://[2606:4700:4700::1111]/x"},
	}
	for _, tc := range cases {
		got, err := NormalizeURL(tc.in, false)
		if err != nil {
			t.Errorf("NormalizeURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "ftp://example.com/x", "https://user:pass@host/x", "http://"} {
		if _, err := NormalizeURL(bad, false); err == nil {
			t.Errorf("NormalizeURL(%q): want error, got nil", bad)
		}
	}

	// The refusal messages must not echo the credentials: they reach agent
	// context and MCP client logs.
	for _, secretURL := range []string{"https://admin:hunter2@example.com/x", "http://u:p%zz@proxy:3128", "https://admin:hunter2@127.0.0.1/"} {
		_, err := NormalizeURL(secretURL, false)
		if err == nil {
			t.Fatalf("NormalizeURL(%q): want error, got nil", secretURL)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "p%zz") {
			t.Errorf("error echoes credentials: %v", err)
		}
	}

	// A literal '+' inside a signed query survives normalization untouched:
	// the Query()/Encode round-trip re-quotes literals and reorders
	// parameters, breaking signatures (finding 126).
	if got, err := NormalizeURL("https://ex.com/f?sig=a%2Bb+c&v=1", true); err != nil ||
		got != "https://ex.com/f?sig=a%2Bb+c&v=1" {
		t.Errorf("signed query must survive verbatim: %q, %v", got, err)
	}
	// utm_* cutting still works when a cut is actually due.
	if got, err := NormalizeURL("https://ex.com/f?utm_source=x&sig=a+b", true); err != nil ||
		strings.Contains(got, "utm_") || !strings.Contains(got, "sig=") {
		t.Errorf("utm cut broken: %q, %v", got, err)
	}
	// "utm_" inside a VALUE (a redirect target, a signature fragment) must
	// not trigger the rebuild: the exact encoding and order of a signed
	// query survive.
	if got, err := NormalizeURL("https://ex.com/p?next=%2Fsearch%3Futm_source%3Dx&sig=a+b", true); err != nil ||
		!strings.Contains(got, "sig=a+b") || !strings.Contains(got, "utm_source") {
		t.Errorf("utm-in-value must not rebuild the query: %q, %v", got, err)
	}
}

// The SSRF guard: fetch follows untrusted URLs, so internal addresses are
// refused by default (loopback, RFC1918, link-local incl. cloud metadata,
// multicast, 0/8, special-use domains).
func TestBlockedHostGuard(t *testing.T) {
	for _, h := range []string{
		"127.0.0.1", "127.1.2.3", "::1", "0.0.0.0", "0.1.2.3",
		"10.0.0.5", "172.16.0.1", "192.168.1.50", "169.254.169.254",
		"ff02::1", "224.0.0.1", "fc00::1", "fd12:3456::7",
		"localhost", "api.localhost", "router.local", "svc.internal",
		"box.home.arpa", "intranet.home", "a.test",
		// CGNAT (Tailscale's default pool), IETF reserved, documentation,
		// benchmarking, class-E: unicast-shaped but not global.
		"100.64.0.1", "100.127.255.255", "192.0.0.1", "192.0.2.1",
		"198.18.0.1", "198.19.9.9", "198.51.100.7", "203.0.113.9",
		"240.0.0.1", "255.255.255.255",
		// Tunnels that embed an IPv4: the embedded address is what counts.
		"2002:c0a8:0132::1",                       // 6to4 → 192.168.1.50
		"2001:0000:0000:0000:0000:0000:3f57:fecd", // Teredo → 192.168.1.50
		"64:ff9b::c0a8:0132",                      // NAT64 well-known → 192.168.1.50
		"64:ff9b::7f00:1",                         // NAT64 well-known → 127.0.0.1
		"64:ff9b:1::7f00:1",                       // NAT64 local /48 → 127.0.0.1
		// IPv4-compatible IPv6 (deprecated, still parsed): To4 decodes only
		// ::ffff-mapped, so these must be decoded by hand.
		"::7f00:1",    // ::127.0.0.1
		"::127.0.0.1", // same, dotted form
		"::a9fe:a9fe", // ::169.254.169.254 (cloud metadata)
	} {
		if !blockedHost(h) {
			t.Errorf("blockedHost(%q) = false, want blocked", h)
		}
	}
	for _, h := range []string{
		"example.com", "docs.gitlab.com", "8.8.8.8", "1.1.1.1",
		"93.184.216.34", "localedge.dev", "test.example.com", "notlocalhost.dev",
		"100.63.0.1", "100.128.0.1", "192.0.1.1", "198.17.0.1", // just outside the ranges
		"2002:8888:8888::1",                       // 6to4 → 136.136.136.136 (public)
		"2001:0000:0000:0000:0000:0000:8888:8888", // Teredo → 119.119.119.119
		"64:ff9b::808:808",                        // NAT64 well-known → 8.8.8.8 (public)
		"64:ff9b:1::808:808",                      // NAT64 local /48 → 8.8.8.8 (public)
		"::808:808",                               // IPv4-compatible → 8.8.8.8 (public)
	} {
		if blockedHost(h) {
			t.Errorf("blockedHost(%q) = true, want allowed", h)
		}
	}
}

func TestNormalizeURLBlocksNonPublic(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1:9200/_cat/indices",
		"http://localhost/x",
		"http://[::1]/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.50/docs",
		"http://router.local/config",
	} {
		if _, err := NormalizeURL(bad, false); err == nil {
			t.Errorf("NormalizeURL(%q): want refusal, got nil", bad)
		}
		// fetch_allow_private is a documented opt-in for self-hosted docs:
		// with it the name-level pre-guard must stand down (the authoritative
		// boundary is the dial-time check, gated by the same flag).
		if _, err := NormalizeURL(bad, true); err != nil {
			t.Errorf("NormalizeURL(%q, allowPrivate): %v, want accepted", bad, err)
		}
	}
	// The userinfo refusal is not gated by the opt-in: credentials in a URL
	// stay refused regardless of the target.
	if _, err := NormalizeURL("https://admin:hunter2@127.0.0.1/", true); err == nil {
		t.Error("allowPrivate must not accept userinfo URLs")
	}
}

func TestFetchRefusesLoopbackByDefault(t *testing.T) {
	ft := newFetchTarget(t)
	if _, err := FetchPage(context.Background(), config.DefaultConfig(), ft.srv.URL+"/page", nil); err == nil ||
		!strings.Contains(err.Error(), "non-public") {
		t.Errorf("loopback target must be refused by default, got: %v", err)
	}
}

func TestURLHashStableAcrossNormalizations(t *testing.T) {
	a, err := NormalizeURL("example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeURL("https://example.com/", false)
	if err != nil {
		t.Fatal(err)
	}
	ha, hb := URLHash(a), URLHash(b)
	if ha != hb {
		t.Errorf("hashes differ: %s vs %s", ha, hb)
	}
	if len(ha) != 16 {
		t.Errorf("hash length = %d, want 16", len(ha))
	}
}

func TestHostAllowed(t *testing.T) {
	cfg := &config.Config{FetchAllowHosts: []string{"example.com", ".docs.gitlab.com"}}
	allow := []string{
		"https://example.com/a",
		"https://docs.gitlab.com/x",
		"https://a.docs.gitlab.com/y",
	}
	deny := []string{
		"https://sub.example.com",
		"https://gitlab.com",
	}
	for _, u := range allow {
		if err := HostAllowed(cfg, u); err != nil {
			t.Errorf("HostAllowed(%q): %v, want allowed", u, err)
		}
	}
	for _, u := range deny {
		if err := HostAllowed(cfg, u); err == nil {
			t.Errorf("HostAllowed(%q): want denied", u)
		}
	}
	if err := HostAllowed(&config.Config{}, "https://anything.test"); err != nil {
		t.Errorf("empty allowlist must allow everything, got: %v", err)
	}
}

// fetchTarget is the document server the fetch tests point at.
type fetchTarget struct {
	srv    *httptest.Server
	ua     string   // User-Agent seen on the last request
	rlHits int      // requests to /ratelimit (429-then-200 counter)
	inm    []string // If-None-Match values /etag received, in order
}

func newFetchTarget(t *testing.T) *fetchTarget {
	t.Helper()
	ft := &fetchTarget{}
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		ft.ua = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		writeBody(t, w, `<html><head><title>Doc &amp; Co</title></head>`+
			`<body><h1>Alpha</h1><p>First paragraph.</p>`+
			`<h2>Beta</h2><p>Second paragraph.</p>`+
			`<img src="/img/a.png" alt="A"></body></html>`)
	})
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeBody(t, w, "One.\n\nTwo.")
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeBody(t, w, `{}`)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page", http.StatusFound)
	})
	mux.HandleFunc("/blocked", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/ratelimit", func(w http.ResponseWriter, r *http.Request) {
		ft.rlHits++
		if ft.rlHits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		writeBody(t, w, "<html><body><h1>Limited</h1><p>now served</p></body></html>")
	})
	mux.HandleFunc("/markdown", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		writeBody(t, w, "# Head\n\nSome *markdown* prose.\n")
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	// /etag answers with a validator and 304 on a matching If-None-Match:
	// the conditional-GET path the docs cache revalidates with.
	mux.HandleFunc("/etag", func(w http.ResponseWriter, r *http.Request) {
		ft.inm = append(ft.inm, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		writeBody(t, w, `<html><body><h1>Cacheable</h1><p>v1 body</p></body></html>`)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		writeBody(t, w, strings.Repeat("x", 2000))
	})
	ft.srv = httptest.NewServer(mux)
	t.Cleanup(ft.srv.Close)
	return ft
}

func newProxyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		writeBody(t, w, "<html><body><h1>Via proxy</h1><p>proxied page</p></body></html>")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchPageHTML(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()

	cfg.FetchAllowPrivate = true
	cfg.FetchUserAgent = "unit-agent"

	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/page", nil)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if ft.ua != "unit-agent" {
		t.Errorf("server saw UA %q, want the configured fetch_user_agent", ft.ua)
	}
	if doc.Title != "Doc & Co" {
		t.Errorf("title = %q", doc.Title)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(doc.Sections))
	}
	if doc.Sections[0].Heading != "Alpha" || doc.Sections[0].Text != "First paragraph." {
		t.Errorf("sections[0] = %+v", doc.Sections[0])
	}
	if len(doc.Images) != 1 || doc.Images[0].URL != ft.srv.URL+"/img/a.png" {
		t.Errorf("images = %+v, want the relative src resolved against the page", doc.Images)
	}
	if doc.Via != ViaDirect {
		t.Errorf("via = %q, want direct", doc.Via)
	}
	if doc.FinalURL != ft.srv.URL+"/page" {
		t.Errorf("final_url = %q", doc.FinalURL)
	}
	if !strings.Contains(doc.HTML, "<title>") {
		t.Error("raw html not stored on the snapshot")
	}
	if doc.SizeRaw == 0 || doc.Size == 0 || len(doc.ContentHash) != 64 {
		t.Errorf("size_raw=%d size=%d content_hash=%q", doc.SizeRaw, doc.Size, doc.ContentHash)
	}
	if doc.ExpiresAt.IsZero() {
		t.Error("expires_at must be set from docs_ttl_days")
	}
}

func TestFetchPagePlainTextAndRejects(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()

	cfg.FetchAllowPrivate = true
	ctx := context.Background()

	doc, err := FetchPage(ctx, cfg, ft.srv.URL+"/plain", nil)
	if err != nil {
		t.Fatalf("text/plain fetch: %v", err)
	}
	if len(doc.Sections) == 0 || doc.Sections[0].Heading != "Part" {
		t.Errorf("plain sections = %+v", doc.Sections)
	}

	if _, err := FetchPage(ctx, cfg, ft.srv.URL+"/json", nil); err == nil ||
		!strings.Contains(err.Error(), "not supported") {
		t.Errorf("json content type must be rejected with an explicit error, got: %v", err)
	}

	oversize := config.DefaultConfig()

	oversize.FetchAllowPrivate = true
	oversize.FetchMaxBytes = 100
	if _, err := FetchPage(ctx, oversize, ft.srv.URL+"/big", nil); err == nil ||
		!strings.Contains(err.Error(), "exceeds fetch_max_bytes") {
		t.Errorf("oversize body must fail with an explicit cap error, got: %v", err)
	}
}

func TestFetchPageRedirect(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/redirect", nil)
	if err != nil {
		t.Fatalf("redirect fetch: %v", err)
	}
	if doc.FinalURL != ft.srv.URL+"/page" {
		t.Errorf("final_url = %q, want the redirect target", doc.FinalURL)
	}
}

// TestFetchPagePolicyRefusalIsFinal (finding 124): an allowlist/redirect
// policy refusal is the operator's wall — the proxied attempt faces the
// SAME wall, so retrying through the proxy must not even be attempted
// (the tell: no "(direct:" composition, which only appears when the proxy
// attempt also failed).
func TestFetchPagePolicyRefusalIsFinal(t *testing.T) {
	ft := newFetchTarget(t)
	proxy := newProxyServer(t)
	fb := config.DefaultConfig()
	fb.FetchAllowPrivate = true
	fb.FetchProxyURL = proxy.URL
	fb.FetchProxyFallback = true
	fb.FetchAllowHosts = []string{"elsewhere.invalid"}
	_, err := FetchPage(context.Background(), fb, ft.srv.URL+"/redirect", nil)
	if err == nil {
		t.Fatal("a redirect outside the allowlist must fail")
	}
	if !strings.Contains(err.Error(), "redirect chain") {
		t.Errorf("want the policy refusal, got: %v", err)
	}
	if strings.Contains(err.Error(), "(direct:") {
		t.Errorf("policy refusal must not retry through the proxy: %v", err)
	}
}

// TestResolveImageRefsDropsInternalRefs (finding 125): an img href aimed
// at an internal host is SSRF bait, not documentation; the drop is counted
// (the caller folds it into truncated).
func TestResolveImageRefsDropsInternalRefs(t *testing.T) {
	mk := func() []model.ImageRef {
		return []model.ImageRef{
			{URL: "/a.png"},
			{URL: "http://127.0.0.1/b.png"},
			{URL: "http://intranet.home/c.png"},
			{URL: "https://cdn.example/d.png"},
		}
	}
	kept, dropped := resolveImageRefs(mk(), "https://docs.example/page", false)
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if len(kept) != 2 || kept[0].URL != "https://docs.example/a.png" || kept[1].URL != "https://cdn.example/d.png" {
		t.Errorf("kept = %+v, want the doc-relative and the public ref", kept)
	}
	kept2, dropped2 := resolveImageRefs(mk(), "https://docs.example/page", true)
	if dropped2 != 0 || len(kept2) != 4 {
		t.Errorf("fetch_allow_private must keep internal refs: kept=%d dropped=%d", len(kept2), dropped2)
	}
}

func TestFetchPageProxyFallback(t *testing.T) {
	ft := newFetchTarget(t)
	proxy := newProxyServer(t)
	ctx := context.Background()

	// IP-ban status with fallback on → the proxy route serves the page.
	fb := config.DefaultConfig()

	fb.FetchAllowPrivate = true
	fb.FetchProxyURL = proxy.URL
	fb.FetchProxyFallback = true
	doc, err := FetchPage(ctx, fb, ft.srv.URL+"/blocked", nil)
	if err != nil {
		t.Fatalf("403 with fallback: %v", err)
	}
	if doc.Via != ViaProxy {
		t.Errorf("via = %q, want proxy", doc.Via)
	}
	if len(doc.Sections) == 0 || doc.Sections[0].Heading != "Via proxy" {
		t.Errorf("sections from proxy: %+v", doc.Sections)
	}

	// 429 is an IP ban too: the proxy is a different IP (and the polite
	// direct retry has already spent its single attempt on the target).
	doc, err = FetchPage(ctx, fb, ft.srv.URL+"/ratelimit", nil)
	if err != nil {
		t.Fatalf("429 with fallback: %v", err)
	}
	if doc.Via != ViaProxy {
		t.Errorf("429 via = %q, want proxy", doc.Via)
	}

	// 404 is not an IP ban: no retry through the proxy.
	if _, err := FetchPage(ctx, fb, ft.srv.URL+"/missing", nil); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404 must be final, got: %v", err)
	}

	// Proxy-only mode (fallback off, proxy set): every request is proxied.
	only := config.DefaultConfig()

	only.FetchAllowPrivate = true
	only.FetchProxyURL = proxy.URL
	doc, err = FetchPage(ctx, only, ft.srv.URL+"/missing", nil)
	if err != nil {
		t.Fatalf("proxy-only mode: %v", err)
	}
	if doc.Via != ViaProxy {
		t.Errorf("via = %q, want proxy", doc.Via)
	}
}

// TestFetchPage429PoliteRetry: without a proxy the client gets exactly one
// polite retry after a short pause (no hammering; Retry-After is not
// honored — documented). The second attempt serves the page.
func TestFetchPage429PoliteRetry(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true

	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/ratelimit", nil)
	if err != nil {
		t.Fatalf("429 polite retry: %v", err)
	}
	if ft.rlHits != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one polite retry)", ft.rlHits)
	}
	if doc.Via != ViaDirect {
		t.Errorf("via = %q, want direct", doc.Via)
	}
}

// TestFetchPageMarkdown: text/markdown arrives as plain extracted text —
// documentation endpoints (raw READMEs) are documentation too.
func TestFetchPageMarkdown(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true

	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/markdown", nil)
	if err != nil {
		t.Fatalf("markdown fetch: %v", err)
	}
	if len(doc.Sections) == 0 || !strings.Contains(doc.Sections[0].Text, "*markdown* prose") {
		t.Errorf("sections = %+v", doc.Sections)
	}
}

// TestFetchPageHook: the pre-store hook runs after extraction and BEFORE
// finalization — size and content_hash describe the stored (redacted) text,
// and a hook error rejects the page.
func TestFetchPageHook(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true

	hook := func(d *model.Doc) ([]model.Redaction, error) {
		d.Sections[0].Text = "red"
		return []model.Redaction{{Field: "section 0", Pattern: "aws-access-key", Chars: 20}}, nil
	}
	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/page", hook)
	if err != nil {
		t.Fatalf("hooked fetch: %v", err)
	}
	if doc.Size != 3+len("Second paragraph.") {
		t.Errorf("size = %d, want it computed from the redacted text", doc.Size)
	}
	if len(doc.Redactions) != 1 || doc.Redactions[0].Pattern != "aws-access-key" {
		t.Errorf("redactions not carried: %+v", doc.Redactions)
	}
	sum := sha256.Sum256([]byte("red\nSecond paragraph.\n"))
	if doc.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("content_hash must be over the final text: %s", doc.ContentHash)
	}

	reject := func(d *model.Doc) ([]model.Redaction, error) {
		return nil, errors.New("guard: medium finding")
	}
	if _, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/page", reject); err == nil ||
		!strings.Contains(err.Error(), "medium finding") {
		t.Errorf("hook rejection must propagate: %v", err)
	}
}

// TestFetchPageDefaultUA pins the anti-fingerprinting default: an unconfigured
// client arrives as a current desktop browser, not as "memento/x.y.z".
func TestFetchPageDefaultUA(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true

	if _, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/page", nil); err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if ft.ua != config.DefaultFetchUserAgent {
		t.Errorf("server saw UA %q, want the Chrome default", ft.ua)
	}
	if !strings.Contains(ft.ua, "Chrome/145") {
		t.Errorf("default UA %q is not a current browser line", ft.ua)
	}
}

// TestFetchPageRateLimitedPoliteRetry pins the 429-without-proxy contract:
// one short polite retry on the same client, no hammering, no proxy needed.
func TestFetchPageRateLimitedPoliteRetry(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true

	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/ratelimit", nil)
	if err != nil {
		t.Fatalf("429 polite retry: %v", err)
	}
	if ft.rlHits != 2 {
		t.Errorf("target saw %d requests, want exactly 2 (one polite retry)", ft.rlHits)
	}
	if len(doc.Sections) == 0 || doc.Sections[0].Heading != "Limited" {
		t.Errorf("sections = %+v, want the page served after the retry", doc.Sections)
	}
}

// TestFetchPageEmptyContentTypeIsHTML pins the lenient branch: servers that
// send no Content-Type at all (httptest cannot produce this — Go sniffs — so
// a raw response is written on a bare listener) must be parsed as HTML.
func TestFetchPageEmptyContentTypeIsHTML(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Read the request headers only (the client waits for the response;
		// draining to EOF would deadlock).
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		body := "<html><head><title>NoCT</title></head><body><h1>Head</h1><p>body text here</p></body></html>"
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	}()

	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	doc, err := FetchPage(context.Background(), cfg, "http://"+ln.Addr().String()+"/noct", nil)
	if err != nil {
		t.Fatalf("empty Content-Type fetch: %v", err)
	}
	if doc.Title != "NoCT" {
		t.Errorf("title = %q, want the HTML parsed", doc.Title)
	}
}

func TestLooksLikeJSON(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"a":1}`, true},
		{"  \n [1,2,3] ", true},
		// An API error body arriving with no declared type is still JSON:
		// leniency here would store the error blob as a document.
		{`{"error":{"type":"index_not_found_exception"},"status":404}`, true},
		{`<html><body>{"looks":1}</body></html>`, false},
		{`{`, false}, // truncated: not valid JSON
		{`plain prose`, false},
		{`{invalid`, false},
		{`}`, false}, // closes without opening
		{`[]`, true},
		{``, false},
	} {
		if got := looksLikeJSON([]byte(tc.body)); got != tc.want {
			t.Errorf("looksLikeJSON(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestFetchPageUndeclaredJSONIsRefused pins the guard the empty-CT leniency
// must not swallow: an API endpoint answering with JSON and no Content-Type
// is a contract mismatch, not a malformed page. Parsing it as HTML would
// store an error blob (or raw API payload) as a document snapshot.
func TestFetchPageUndeclaredJSONIsRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		body := `{"error":{"type":"index_not_found_exception","reason":"no such index"},"status":404}`
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	}()

	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	_, err = FetchPage(context.Background(), cfg, "http://"+ln.Addr().String()+"/api", nil)
	if err == nil {
		t.Fatal("undeclared JSON body must be refused, not doc-parsed")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("refusal = %v, want it to name the JSON body", err)
	}
}

// TestFetchPageOversize pins the cap's failure mode: the page is an input,
// not a target; over the cap the fetch refuses with an actionable message.
func TestFetchPageOversize(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	cfg.FetchMaxBytes = 100

	_, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/big", nil)
	if err == nil {
		t.Fatal("want oversize refusal")
	}
	if !strings.Contains(err.Error(), "fetch_max_bytes") {
		t.Errorf("oversize error = %v, want fetch_max_bytes named", err)
	}
}

// TestFetchPageRefusesPrivateAddress pins the default privacy posture:
// loopback targets are refused unless fetch_allow_private is set on purpose.
func TestFetchPageRefusesPrivateAddress(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig() // fetch_allow_private defaults off

	_, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/page", nil)
	if err == nil {
		t.Fatal("loopback fetch must be refused by default")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Errorf("loopback refusal = %v, want the privacy-guard message", err)
	}
}

// TestFetchPageTruncatesSections pins that extraction limits are loud: the
// stored text is a prefix and doc.Truncated says so.
func TestFetchPageTruncatesSections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		writeBody(t, w, "<html><body><h1>Big</h1><p>"+strings.Repeat("x", MaxSectionChars+500)+"</p></body></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	doc, err := FetchPage(context.Background(), cfg, srv.URL+"/huge", nil)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if !doc.Truncated {
		t.Error("Truncated = false, want true (the section was capped)")
	}
	if len(doc.Sections) == 0 || utf8RuneLen(doc.Sections[0].Text) != MaxSectionChars {
		t.Errorf("section text length = %d, want capped at %d", len(doc.Sections[0].Text), MaxSectionChars)
	}
}

func utf8RuneLen(s string) int { return len([]rune(s)) }

// TestFetchPageStatusClasses pins the typed failure classes the caller
// branches on: render/proxy eligibility and CLI hints are driven by the
// class, not by message parsing.
func TestFetchPageStatusClasses(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true

	_, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/blocked", nil)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Status != http.StatusForbidden {
		t.Fatalf("403 without proxy: %v, want FetchError status 403", err)
	}
	if fe.Class != ClassStatus {
		t.Errorf("403 class = %q, want %q", fe.Class, ClassStatus)
	}
	// 404 is a status too, and never render-eligible: the renderer faces
	// the same missing document.
	_, err = FetchPage(context.Background(), cfg, ft.srv.URL+"/missing", nil)
	if !errors.As(err, &fe) || fe.Status != http.StatusNotFound {
		t.Fatalf("404: %v, want FetchError status 404", err)
	}
	if renderEligible(err) {
		t.Error("404 must not be render-eligible")
	}
	// 403 is render-eligible: a browser from the same IP may still get in.
	_, err = FetchPage(context.Background(), cfg, ft.srv.URL+"/blocked", nil)
	if !renderEligible(err) {
		t.Error("403 must be render-eligible")
	}
}

// TestRevalidatePage304KeepsCopy pins the conditional-GET contract: the stored
// copy's ETag goes out as If-None-Match, a 304 answers "unchanged" without a
// body, and the caller keeps its snapshot (only timestamps move).
func TestRevalidatePage304KeepsCopy(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	ctx := context.Background()
	url := ft.srv.URL + "/etag"

	doc, err := FetchPage(ctx, cfg, url, nil)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if doc.ETag != `"v1"` {
		t.Fatalf("ETag = %q, want \"v1\" captured from the origin", doc.ETag)
	}

	doc2, notModified, err := RevalidatePage(ctx, cfg, url, doc, nil)
	if err != nil {
		t.Fatalf("RevalidatePage: %v", err)
	}
	if !notModified || doc2 != nil {
		t.Fatalf("revalidate = (%v, %v), want (nil doc, notModified=true)", doc2, notModified)
	}
	if len(ft.inm) != 2 || ft.inm[0] != "" || ft.inm[1] != `"v1"` {
		t.Errorf("If-None-Match sequence = %q, want [\"\" \"v1\"] (only the revalidation is conditional)", ft.inm)
	}
}

// TestRevalidatePageSkipsValidatorsForRenderedCopy pins that HTTP validators are
// representation-specific: a copy captured through the renderer must re-fetch
// in full, or the origin's ETag would be applied to a body it never described.
func TestRevalidatePageSkipsValidatorsForRenderedCopy(t *testing.T) {
	ft := newFetchTarget(t)
	cfg := config.DefaultConfig()
	cfg.FetchAllowPrivate = true
	ctx := context.Background()

	prior := &model.Doc{Via: ViaObscura, ETag: `"v1"`}
	doc, notModified, err := RevalidatePage(ctx, cfg, ft.srv.URL+"/etag", prior, nil)
	if err != nil {
		t.Fatalf("RevalidatePage: %v", err)
	}
	if notModified || doc == nil {
		t.Fatalf("rendered prior must re-fetch in full, got notModified=%v doc=%v", notModified, doc)
	}
	if len(ft.inm) != 1 || ft.inm[0] != "" {
		t.Errorf("If-None-Match = %q, want none for a rendered copy", ft.inm)
	}
}
