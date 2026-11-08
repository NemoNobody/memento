package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
)

func TestUAToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"memento/fetch (+OpenSearch docs cache)", "memento"},
		{"Googlebot/2.1 (+http://www.google.com/bot.html)", "googlebot"},
		{"  Bot-Name 1.0 ", "bot-name"},
	}
	for _, tc := range cases {
		if got := uaToken(tc.in); got != tc.want {
			t.Errorf("uaToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRobots(t *testing.T) {
	body := `
# top comment
User-agent: memento
Disallow: /secret
Allow: /secret/public

User-agent: *
Disallow: /private
`
	groups := parseRobots(body)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.agent != "memento" || len(g.disallow) != 1 || g.disallow[0] != "/secret" ||
		len(g.allow) != 1 || g.allow[0] != "/secret/public" {
		t.Errorf("group[0] = %+v", g)
	}
	if groups[1].agent != "*" || len(groups[1].disallow) != 1 || groups[1].disallow[0] != "/private" {
		t.Errorf("group[1] = %+v", groups[1])
	}
}

func TestRobotsAllowed(t *testing.T) {
	e := robotsEntry{groups: []robotsGroup{
		{agent: "*", disallow: []string{"/private"}},
		{agent: "memento", disallow: []string{"/secret", "/"}, allow: []string{"/secret/public"}},
		{agent: "friendly", disallow: []string{}},
	}}
	cases := []struct {
		agent, path string
		want        bool
	}{
		{"memento", "/secret/one", false},
		{"memento", "/secret/public/x", true}, // Allow beats Disallow on longer match
		{"memento", "/public", false},         // bare Disallow: / blocks the rest
		{"memento", "/", false},
		{"other", "/private/x", false},
		{"other", "/public", true},
		{"friendly", "/anything", true}, // empty Disallow list: no restrictions
	}
	for _, tc := range cases {
		if got := robotsAllowed(e, tc.agent, tc.path); got != tc.want {
			t.Errorf("robotsAllowed(%q,%q) = %v, want %v", tc.agent, tc.path, got, tc.want)
		}
	}
}

// newRobotsServer serves robots.txt (counted) plus /secret and /public.
func newRobotsServer(t *testing.T, robotsBody string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt" && robotsBody != "":
			atomic.AddInt32(hits, 1)
			w.Header().Set("Content-Type", "text/plain")
			writeBody(t, w, robotsBody)
		default:
			w.Header().Set("Content-Type", "text/html")
			writeBody(t, w, "<html><body><h1>Page</h1><p>text</p></body></html>")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func robotsCfg() *config.Config {
	cfg := config.DefaultConfig()

	cfg.FetchAllowPrivate = true
	cfg.FetchRespectRobots = true
	cfg.FetchUserAgent = "memento/test (robots test)"
	return cfg
}

func TestCheckRobots(t *testing.T) {
	var hits int32
	srv := newRobotsServer(t, "User-agent: memento\nDisallow: /secret\n", &hits)
	ctx := context.Background()
	cfg := robotsCfg()

	// Unique port per test isolates the in-process robots cache.
	if err := CheckRobots(ctx, cfg, srv.URL+"/secret"); err == nil {
		t.Error("disallowed path must be rejected")
	} else if !strings.Contains(err.Error(), "robots.txt") {
		t.Errorf("error should mention robots.txt: %v", err)
	}
	if err := CheckRobots(ctx, cfg, srv.URL+"/public"); err != nil {
		t.Errorf("allowed path: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (24h cache)", n)
	}
	// Disabled feature: no robots traffic, no restriction.
	off := config.DefaultConfig()

	off.FetchAllowPrivate = true
	off.FetchUserAgent = "memento/test"
	if err := CheckRobots(ctx, off, srv.URL+"/secret"); err != nil {
		t.Errorf("robots off: %v", err)
	}
}

func TestCheckRobots404FailsOpen(t *testing.T) {
	var hits int32
	srv := newRobotsServer(t, "", &hits) // no robots.txt handler: 404
	if err := CheckRobots(context.Background(), robotsCfg(), srv.URL+"/secret"); err != nil {
		t.Errorf("404 robots must fail open, got: %v", err)
	}
}

// A cancelled robots fetch is the caller's deadline, not the host's verdict:
// caching it would let one cancelled request poison (or blind) the host's
// rules for the whole 24h TTL.
func TestRobotsCancelledNotCached(t *testing.T) {
	var hits int32
	srv := newRobotsServer(t, "User-agent: memento\nDisallow: /secret\n", &hits)
	cfg := robotsCfg()
	u, err := url.Parse(srv.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if e := robotsFor(canceled, cfg, "http", u.Host); !e.bad {
		t.Fatal("cancelled robots fetch must report failure")
	}
	if e := robotsFor(context.Background(), cfg, "http", u.Host); e.bad {
		t.Error("the cancelled verdict was cached: the fresh call must refetch")
	}
	// The cancelled fetch died before the network (0 hits); the fresh call
	// fetched (1 hit) and its verdict is real: /secret is disallowed.
	if err := CheckRobots(context.Background(), cfg, srv.URL+"/secret"); err == nil {
		t.Error("the re-fetched ruleset must enforce the disallow")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (the cancelled fetch must not be cached as bad)", n)
	}
}
