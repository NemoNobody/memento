package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
)

// writeBody serves a fixture body; a write failure means the page never
// reached the client, so continuing the test would only produce noise.
func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("fixture write: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "https://example.com/"},
		{"example.com/x?utm_source=nl&a=1", "https://example.com/x?a=1"},
		{"HTTPS://Example.COM/p#frag", "https://example.com/p"},
		{"http://h:8080/a?b=2&utm_medium=x", "http://h:8080/a?b=2"},
		{"  https://a.com/path/  ", "https://a.com/path/"},
	}
	for _, tc := range cases {
		got, err := NormalizeURL(tc.in)
		if err != nil {
			t.Errorf("NormalizeURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "ftp://example.com/x", "https://user:pass@host/x", "http://"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("NormalizeURL(%q): want error, got nil", bad)
		}
	}
}

func TestURLHashStableAcrossNormalizations(t *testing.T) {
	a, err := NormalizeURL("example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeURL("https://example.com/")
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
	srv *httptest.Server
	ua  string // User-Agent seen on the last request
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
	cfg.FetchUserAgent = "unit-agent"

	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/page")
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
	ctx := context.Background()

	doc, err := FetchPage(ctx, cfg, ft.srv.URL+"/plain")
	if err != nil {
		t.Fatalf("text/plain fetch: %v", err)
	}
	if len(doc.Sections) == 0 || doc.Sections[0].Heading != "Part" {
		t.Errorf("plain sections = %+v", doc.Sections)
	}

	if _, err := FetchPage(ctx, cfg, ft.srv.URL+"/json"); err == nil ||
		!strings.Contains(err.Error(), "not supported") {
		t.Errorf("json content type must be rejected with an explicit error, got: %v", err)
	}

	oversize := config.DefaultConfig()
	oversize.FetchMaxBytes = 100
	if _, err := FetchPage(ctx, oversize, ft.srv.URL+"/big"); err == nil ||
		!strings.Contains(err.Error(), "exceeds fetch_max_bytes") {
		t.Errorf("oversize body must fail with an explicit cap error, got: %v", err)
	}
}

func TestFetchPageRedirect(t *testing.T) {
	ft := newFetchTarget(t)
	doc, err := FetchPage(context.Background(), config.DefaultConfig(), ft.srv.URL+"/redirect")
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
	fb.FetchProxyURL = proxy.URL
	fb.FetchProxyFallback = true
	doc, err := FetchPage(ctx, fb, ft.srv.URL+"/blocked")
	if err != nil {
		t.Fatalf("403 with fallback: %v", err)
	}
	if doc.Via != ViaProxy {
		t.Errorf("via = %q, want proxy", doc.Via)
	}
	if len(doc.Sections) == 0 || doc.Sections[0].Heading != "Via proxy" {
		t.Errorf("sections from proxy: %+v", doc.Sections)
	}

	// 404 is not an IP ban: no retry through the proxy.
	if _, err := FetchPage(ctx, fb, ft.srv.URL+"/missing"); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404 must be final, got: %v", err)
	}

	// Proxy-only mode (fallback off, proxy set): every request is proxied.
	only := config.DefaultConfig()
	only.FetchProxyURL = proxy.URL
	doc, err = FetchPage(ctx, only, ft.srv.URL+"/missing")
	if err != nil {
		t.Fatalf("proxy-only mode: %v", err)
	}
	if doc.Via != ViaProxy {
		t.Errorf("via = %q, want proxy", doc.Via)
	}
}
