package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigFetchFields(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DocsIndexName != "memento-docs" {
		t.Errorf("DocsIndexName = %q, want memento-docs", cfg.DocsIndexName)
	}
	if cfg.FetchTimeoutSeconds != 30 {
		t.Errorf("FetchTimeoutSeconds = %d, want 30", cfg.FetchTimeoutSeconds)
	}
	if cfg.FetchMaxBytes != 2_000_000 {
		t.Errorf("FetchMaxBytes = %d, want 2000000", cfg.FetchMaxBytes)
	}
	if cfg.DocsTTLDays != 90 {
		t.Errorf("DocsTTLDays = %d, want 90", cfg.DocsTTLDays)
	}
	if cfg.FetchUserAgent == "" {
		t.Error("FetchUserAgent must have a default")
	}
	if cfg.FetchRespectRobots {
		t.Error("FetchRespectRobots must default to off (user-initiated fetches)")
	}
	if cfg.FetchProxyFallback {
		t.Error("FetchProxyFallback must default to off")
	}
	if cfg.LocalRepoScanDepth != 2 {
		t.Errorf("LocalRepoScanDepth = %d, want 2", cfg.LocalRepoScanDepth)
	}
	if len(cfg.FetchAllowHosts) != 0 || len(cfg.LocalRepoRoots) != 0 {
		t.Error("allowlist and repo roots must default to empty")
	}
}

func TestLoadConfigFetchEnv(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	t.Setenv("MEMENTO_DOCS_INDEX", "docsx")
	t.Setenv("MEMENTO_FETCH_TIMEOUT", "7")
	t.Setenv("MEMENTO_FETCH_MAX_BYTES", "999")
	t.Setenv("MEMENTO_DOCS_TTL_DAYS", "0")
	t.Setenv("MEMENTO_FETCH_HOSTS", "a.com, .b.com ,")
	t.Setenv("MEMENTO_FETCH_USER_AGENT", "UA/1")
	t.Setenv("MEMENTO_FETCH_RESPECT_ROBOTS", "true")
	t.Setenv("MEMENTO_FETCH_PROXY_URL", "http://p:3128")
	t.Setenv("MEMENTO_FETCH_PROXY_FALLBACK", "true")
	t.Setenv("MEMENTO_LOCAL_REPO_ROOTS", "/r1, /r2")
	t.Setenv("MEMENTO_REPO_SCAN_DEPTH", "4")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DocsIndexName != "docsx" || cfg.FetchTimeoutSeconds != 7 || cfg.FetchMaxBytes != 999 {
		t.Errorf("fetch numeric overrides: %+v", cfg)
	}
	if cfg.DocsTTLDays != 0 {
		t.Errorf("DocsTTLDays = %d, want 0 (explicit: never expire)", cfg.DocsTTLDays)
	}
	if len(cfg.FetchAllowHosts) != 2 || cfg.FetchAllowHosts[0] != "a.com" || cfg.FetchAllowHosts[1] != ".b.com" {
		t.Errorf("FetchAllowHosts = %v, want [a.com .b.com]", cfg.FetchAllowHosts)
	}
	if cfg.FetchUserAgent != "UA/1" || !cfg.FetchRespectRobots {
		t.Errorf("UA/robots: %q respect=%v", cfg.FetchUserAgent, cfg.FetchRespectRobots)
	}
	if cfg.FetchProxyURL != "http://p:3128" || !cfg.FetchProxyFallback {
		t.Errorf("proxy: %q fallback=%v", cfg.FetchProxyURL, cfg.FetchProxyFallback)
	}
	if len(cfg.LocalRepoRoots) != 2 || cfg.LocalRepoRoots[0] != "/r1" || cfg.LocalRepoRoots[1] != "/r2" {
		t.Errorf("LocalRepoRoots = %v", cfg.LocalRepoRoots)
	}
	if cfg.LocalRepoScanDepth != 4 {
		t.Errorf("LocalRepoScanDepth = %d, want 4", cfg.LocalRepoScanDepth)
	}
}

func TestLoadConfigFetchFileFields(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	body := `{
		"docs_index_name": "fd",
		"fetch_timeout_seconds": 5,
		"fetch_max_bytes": 123,
		"docs_ttl_days": 7,
		"fetch_allow_hosts": ["x.com", ".y.com"],
		"fetch_user_agent": "FA/2",
		"fetch_respect_robots": true,
		"fetch_proxy_url": "http://px:8080",
		"fetch_proxy_fallback": true,
		"local_repo_roots": ["/a", "/b"],
		"local_repo_scan_depth": 3
	}`
	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DocsIndexName != "fd" || cfg.FetchTimeoutSeconds != 5 || cfg.FetchMaxBytes != 123 || cfg.DocsTTLDays != 7 {
		t.Errorf("file fetch fields not applied: %+v", cfg)
	}
	if len(cfg.FetchAllowHosts) != 2 || cfg.FetchAllowHosts[1] != ".y.com" {
		t.Errorf("allowlist = %v", cfg.FetchAllowHosts)
	}
	if cfg.FetchUserAgent != "FA/2" || !cfg.FetchRespectRobots {
		t.Errorf("UA/robots = %q %v", cfg.FetchUserAgent, cfg.FetchRespectRobots)
	}
	if cfg.FetchProxyURL != "http://px:8080" || !cfg.FetchProxyFallback {
		t.Errorf("proxy = %q %v", cfg.FetchProxyURL, cfg.FetchProxyFallback)
	}
	if len(cfg.LocalRepoRoots) != 2 || cfg.LocalRepoScanDepth != 3 {
		t.Errorf("repo roots/depth = %v %d", cfg.LocalRepoRoots, cfg.LocalRepoScanDepth)
	}
}

// TestValidateProxyURL pins the load-time contract (#9): a value the client
// cannot dial is refused at load, never surfaced as a fetch-time network
// error (which would steer the obscura render into direct egress).
func TestValidateProxyURL(t *testing.T) {
	ok := []string{"", "http://p:3128", "https://p.example:8443", "socks5://127.0.0.1:1080", "socks5h://p:1080"}
	for _, v := range ok {
		if err := validateProxyURL(v); err != nil {
			t.Errorf("validateProxyURL(%q) = %v, want ok", v, err)
		}
	}
	bad := []string{
		"proxy.internal:3128",   // scheme-less: dials as a bogus-scheme network error
		"http://user:pw@p:3128", // credentials: never forwarded to argv
		"ftp://p:21",            // unsupported scheme
		"http://",               // no host
		"not a url ://",         // syntax
	}
	for _, v := range bad {
		if err := validateProxyURL(v); err == nil {
			t.Errorf("validateProxyURL(%q) = nil, want refusal", v)
		}
	}
}

// TestLoadConfigRejectsBadProxyURL: the contract fires at load, so a bad
// fetch_proxy_url never reaches a fetch.
func TestLoadConfigRejectsBadProxyURL(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	t.Setenv("MEMENTO_FETCH_PROXY_URL", "proxy.internal:3128")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("a scheme-less proxy URL must fail at config load")
	}
}

func TestLoadConfigFetchBadEnvFails(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	for _, tc := range []struct{ key, val string }{
		{"MEMENTO_FETCH_TIMEOUT", "abc"},
		{"MEMENTO_FETCH_PROXY_FALLBACK", "nope"},
		{"MEMENTO_REPO_SCAN_DEPTH", "deep"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := LoadConfig(); err == nil {
				t.Errorf("%s=%q: want loud failure", tc.key, tc.val)
			}
		})
	}
}
