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
func TestFetchEnvProxySkipsDialGuard(t *testing.T) {
	cfg := &config.Config{FetchTimeoutSeconds: 5}

	// Proxied path: Go's ProxyFromEnvironment bypasses loopback targets, so
	// the proxied URL must be a non-localhost host; the discard port (9)
	// refuses the proxy dial fast, with no DNS and no network.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	_, _, _, _, err := fetchHTTP(context.Background(), cfg, "http://example.invalid/x")
	if err == nil {
		t.Fatal("want failure against the discard proxy")
	}
	if strings.Contains(err.Error(), "refusing connection to non-public address") {
		t.Errorf("env-proxy dial hit the target guard: %v", err)
	}

	// Direct path: no env proxy, loopback target → the dial guard fires.
	t.Setenv("HTTP_PROXY", "")
	_, _, _, _, err = fetchHTTP(context.Background(), cfg, "http://127.0.0.1:19/x")
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
	ua     string // User-Agent seen on the last request
	rlHits int    // requests to /ratelimit (429-then-200 counter)
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
