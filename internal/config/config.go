package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NemoNobody/memento/internal/keyring"
)

// Secret guard levels: how the write paths treat credential-like text.
// The store is plaintext, so guarding writes is the only place a secret can
// still be stopped. See internal/secretguard for the detector.
const (
	GuardReject = "reject" // never store credential-like text; no confirmation exists
	GuardAsk    = "ask"    // store only after an explicit confirmation flag
	GuardAllow  = "allow"  // deliberate opt-out: scan nothing
)

// Config holds configuration for the memento CLI.
type Config struct {
	OpenSearchURL      string `json:"opensearch_url"`
	OpenSearchUser     string `json:"opensearch_user"`
	OpenSearchPassword string `json:"opensearch_password"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	IndexName          string `json:"index_name"`

	// HistoryLimit bounds per-key history on every correction: after the old
	// value is appended, the oldest entries beyond this limit are dropped.
	// 0 means "keep everything".
	HistoryLimit int `json:"history_limit"`

	// SecretGuard is one of GuardReject / GuardAsk / GuardAllow. It applies to
	// every write path (CLI add/obsolete, MCP memento_add/memento_obsolete).
	SecretGuard string `json:"secret_guard"`

	// DocsIndexName is the index that holds fetched documents. It is separate
	// from the facts index on purpose: page text is bulk, and facts must stay
	// a small trusted set that searches cheaply.
	DocsIndexName string `json:"docs_index_name"`

	// FetchTimeoutSeconds bounds one document download (connect+body).
	FetchTimeoutSeconds int `json:"fetch_timeout_seconds"`

	// FetchMaxBytes caps the downloaded body: a page is input, not a target,
	// and a runaway response must not exhaust the process.
	FetchMaxBytes int `json:"fetch_max_bytes"`

	// DocsTTLDays is how long a fetched document is trusted before it must be
	// re-fetched. 0 means the docs never expire.
	DocsTTLDays int `json:"docs_ttl_days"`

	// FetchAllowHosts is a privacy allowlist: exact hosts and (with a leading
	// dot) their subdomains. Empty means "any public host".
	FetchAllowHosts []string `json:"fetch_allow_hosts"`

	// FetchUserAgent is sent verbatim on document fetches. Some sites reject
	// generic clients; the identity is config, not hardcoded, and it is never
	// a fingerprint: no cookies, no headers beyond Accept/UA.
	FetchUserAgent string `json:"fetch_user_agent"`

	// FetchRespectRobots turns on polite crawler mode: robots.txt of the
	// target host is consulted before fetching, matched by the
	// fetch_user_agent token. Off by default: v1 fetches single
	// user-initiated pages (browser semantics); on is the courteous setting
	// for public/shared infrastructure. v2 browser-render ignores robots.
	FetchRespectRobots bool `json:"fetch_respect_robots"`

	// FetchProxyURL routes fetches through a proxy (http/socks5). Empty means
	// direct (plus the standard HTTP_PROXY env, honored by the transport).
	FetchProxyURL string `json:"fetch_proxy_url"`

	// FetchProxyFallback retries a failed/timed-out direct fetch through the
	// proxy. Off by default: fallback is an explicit operator choice.
	FetchProxyFallback bool `json:"fetch_proxy_fallback"`

	// LocalRepoRoots are directories scanned for local git clones: GitHub
	// URLs that match a clone (with a matching ref) are answered with a local
	// path instead of being fetched and indexed. Machine-specific — never in
	// the shared store.
	LocalRepoRoots []string `json:"local_repo_roots"`

	// LocalRepoScanDepth bounds the directory scan (1 = <root>/*/.git).
	// Default 2 covers <root>/<org>/<repo>; deeper trees raise it in config.
	LocalRepoScanDepth int `json:"local_repo_scan_depth"`

	// AllowPlaintext makes the plaintext config-file password a valid
	// credential source. Off by default: the password lives in the OS keyring
	// (memento creds set) or the environment (containers, headless); a file
	// carrying a credential is the legacy shape, opt in explicitly.
	AllowPlaintext bool `json:"allow_plaintext"`

	// PasswordSource says where OpenSearchPassword came from: env|keyring|
	// file|none. Runtime-only and never serialized: status lines explain how
	// credentials resolve without echoing anything secret.
	PasswordSource string `json:"-"`

	// KeyringError carries a keyring provider failure (bus down, wallet
	// closed) that is NOT "entry not found". Surfacing it keeps a keyring
	// outage from degrading silently into "no password".
	KeyringError string `json:"-"`

	// FileHasPlaintext records that the config file carried a password that
	// was refused (allow_plaintext off) — the status line turns that into the
	// migration hint.
	FileHasPlaintext bool `json:"-"`
}

// KeyringService is the service name under which OpenSearch credentials live
// in the OS keyring; the account is the host[:port] of the effective URL.
const KeyringService = "memento"

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		OpenSearchURL:       "https://127.0.0.1:9200",
		OpenSearchUser:      "admin",
		OpenSearchPassword:  "",
		InsecureSkipVerify:  true,
		IndexName:           "memento",
		HistoryLimit:        20,
		SecretGuard:         GuardReject,
		DocsIndexName:       "memento-docs",
		FetchTimeoutSeconds: 30,
		FetchMaxBytes:       2_000_000,
		DocsTTLDays:         90,
		FetchUserAgent:      "memento/fetch (+OpenSearch docs cache)",
		LocalRepoScanDepth:  2,
	}
}

// LoadConfig loads configuration using the default lookup chain.
func LoadConfig() (*Config, error) {
	return LoadConfigFrom("")
}

// LoadConfigFrom loads configuration, preferring an explicit config file path
// when one is given; otherwise it uses the default lookup chain. Environment
// variables always override values from the file.
//
// The default lookup chain (see FindConfigPath) is:
//  1. MEMENTO_CONF env var (explicit path)
//  2. memento.conf in the current working directory
//  3. memento.conf next to the executable
//  4. ~/.memento.conf
//
// Env vars override the file: OPENSEARCH_URL, OPENSEARCH_USER,
// OPENSEARCH_PASSWORD, OPENSEARCH_INSECURE_SKIP_VERIFY, MEMENTO_INDEX,
// MEMENTO_HISTORY_LIMIT, MEMENTO_SECRET_GUARD, MEMENTO_DOCS_INDEX,
// MEMENTO_FETCH_TIMEOUT, MEMENTO_FETCH_MAX_BYTES, MEMENTO_DOCS_TTL_DAYS,
// MEMENTO_FETCH_HOSTS (comma-separated), MEMENTO_FETCH_USER_AGENT,
// MEMENTO_FETCH_PROXY_URL, MEMENTO_FETCH_PROXY_FALLBACK,
// MEMENTO_LOCAL_REPO_ROOTS (comma-separated), MEMENTO_REPO_SCAN_DEPTH.
func LoadConfigFrom(explicitPath string) (*Config, error) {
	cfg := DefaultConfig()
	path := explicitPath
	if path == "" {
		path = FindConfigPath()
	}
	if path != "" {
		if err := loadFile(cfg, path); err != nil {
			return nil, err
		}
	}
	if url := os.Getenv("OPENSEARCH_URL"); url != "" {
		cfg.OpenSearchURL = url
	}
	if user := os.Getenv("OPENSEARCH_USER"); user != "" {
		cfg.OpenSearchUser = user
	}
	if pass := os.Getenv("OPENSEARCH_PASSWORD"); pass != "" {
		cfg.OpenSearchPassword = pass
	}
	if v := os.Getenv("OPENSEARCH_INSECURE_SKIP_VERIFY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("OPENSEARCH_INSECURE_SKIP_VERIFY: %w", err)
		}
		cfg.InsecureSkipVerify = b
	}
	if idx := os.Getenv("MEMENTO_INDEX"); idx != "" {
		cfg.IndexName = idx
	}
	if v := os.Getenv("MEMENTO_HISTORY_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_HISTORY_LIMIT: %w", err)
		}
		cfg.HistoryLimit = n
	}
	if v := os.Getenv("MEMENTO_SECRET_GUARD"); v != "" {
		g, err := normalizeGuard(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_SECRET_GUARD: %w", err)
		}
		cfg.SecretGuard = g
	}
	if v := os.Getenv("MEMENTO_ALLOW_PLAINTEXT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_ALLOW_PLAINTEXT: %w", err)
		}
		cfg.AllowPlaintext = b
	}
	if v := os.Getenv("MEMENTO_DOCS_INDEX"); v != "" {
		cfg.DocsIndexName = v
	}
	if v := os.Getenv("MEMENTO_FETCH_TIMEOUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_TIMEOUT: %w", err)
		}
		cfg.FetchTimeoutSeconds = n
	}
	if v := os.Getenv("MEMENTO_FETCH_MAX_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_MAX_BYTES: %w", err)
		}
		cfg.FetchMaxBytes = n
	}
	if v := os.Getenv("MEMENTO_DOCS_TTL_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_DOCS_TTL_DAYS: %w", err)
		}
		cfg.DocsTTLDays = n
	}
	if v := os.Getenv("MEMENTO_FETCH_HOSTS"); v != "" {
		cfg.FetchAllowHosts = splitHosts(v)
	}
	if v := os.Getenv("MEMENTO_FETCH_USER_AGENT"); v != "" {
		cfg.FetchUserAgent = v
	}
	if v := os.Getenv("MEMENTO_FETCH_RESPECT_ROBOTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_RESPECT_ROBOTS: %w", err)
		}
		cfg.FetchRespectRobots = b
	}
	if v := os.Getenv("MEMENTO_FETCH_PROXY_URL"); v != "" {
		cfg.FetchProxyURL = v
	}
	if v := os.Getenv("MEMENTO_FETCH_PROXY_FALLBACK"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_PROXY_FALLBACK: %w", err)
		}
		cfg.FetchProxyFallback = b
	}
	if v := os.Getenv("MEMENTO_LOCAL_REPO_ROOTS"); v != "" {
		cfg.LocalRepoRoots = splitCSV(v)
	}
	if v := os.Getenv("MEMENTO_REPO_SCAN_DEPTH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_REPO_SCAN_DEPTH: %w", err)
		}
		cfg.LocalRepoScanDepth = n
	}
	resolvePassword(cfg)
	return cfg, nil
}

// resolvePassword finalizes the OpenSearch credential: environment first
// (containers, headless, explicit overrides), then the OS keyring, then the
// plaintext file — and the file only when allow_plaintext is on. A missing
// password is not a config-load error: commands that need the store report
// it with the migration hint, and status stays usable.
func resolvePassword(cfg *Config) {
	if os.Getenv("OPENSEARCH_PASSWORD") != "" {
		cfg.PasswordSource = "env"
		return
	}
	if pw, err := keyring.Get(KeyringService, keyringAccount(cfg.OpenSearchURL)); err == nil && pw != "" {
		cfg.OpenSearchPassword = pw
		cfg.PasswordSource = "keyring"
		return
	} else if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		// Provider outage, not a missing entry: record it so status surfaces
		// the outage instead of blaming the user's config.
		cfg.KeyringError = err.Error()
	}
	if cfg.FileHasPlaintext && cfg.AllowPlaintext {
		cfg.PasswordSource = "file"
		return
	}
	cfg.OpenSearchPassword = ""
	cfg.PasswordSource = "none"
}

// keyringAccount derives the keyring account (host[:port]) from the URL so
// one keyring serves several OpenSearch deployments.
func keyringAccount(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// fileConfig mirrors Config for JSON decoding. Fields are optional: a missing
// key must leave the default in place. InsecureSkipVerify is a *bool so that
// "absent" (keep default true) is distinguishable from an explicit false.
type fileConfig struct {
	OpenSearchURL       string   `json:"opensearch_url"`
	OpenSearchUser      string   `json:"opensearch_user"`
	OpenSearchPassword  string   `json:"opensearch_password"`
	InsecureSkipVerify  *bool    `json:"insecure_skip_verify"`
	IndexName           string   `json:"index_name"`
	HistoryLimit        *int     `json:"history_limit"`
	SecretGuard         *string  `json:"secret_guard"`
	DocsIndexName       string   `json:"docs_index_name"`
	FetchTimeoutSeconds *int     `json:"fetch_timeout_seconds"`
	FetchMaxBytes       *int     `json:"fetch_max_bytes"`
	DocsTTLDays         *int     `json:"docs_ttl_days"`
	FetchAllowHosts     []string `json:"fetch_allow_hosts"`
	FetchUserAgent      string   `json:"fetch_user_agent"`
	FetchRespectRobots  *bool    `json:"fetch_respect_robots"`
	FetchProxyURL       string   `json:"fetch_proxy_url"`
	FetchProxyFallback  *bool    `json:"fetch_proxy_fallback"`
	LocalRepoRoots      []string `json:"local_repo_roots"`
	LocalRepoScanDepth  *int     `json:"local_repo_scan_depth"`
	AllowPlaintext      *bool    `json:"allow_plaintext"`
}

// splitHosts parses a comma-separated host list, dropping empty entries.
func splitHosts(v string) []string {
	parts := strings.Split(v, ",")
	hosts := make([]string, 0, len(parts))
	for _, h := range parts {
		if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// splitCSV parses a comma-separated list, trimming spaces and dropping
// empty entries.
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadFile reads the config file at path into cfg.
func loadFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var c fileConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.OpenSearchURL != "" {
		cfg.OpenSearchURL = c.OpenSearchURL
	}
	if c.OpenSearchUser != "" {
		cfg.OpenSearchUser = c.OpenSearchUser
	}
	if c.OpenSearchPassword != "" {
		cfg.OpenSearchPassword = c.OpenSearchPassword
		cfg.FileHasPlaintext = true
	}
	if c.InsecureSkipVerify != nil {
		cfg.InsecureSkipVerify = *c.InsecureSkipVerify
	}
	if c.IndexName != "" {
		cfg.IndexName = c.IndexName
	}
	if c.HistoryLimit != nil {
		cfg.HistoryLimit = *c.HistoryLimit
	}
	if c.SecretGuard != nil {
		g, err := normalizeGuard(*c.SecretGuard)
		if err != nil {
			return fmt.Errorf("config %s: secret_guard: %w", path, err)
		}
		cfg.SecretGuard = g
	}
	if c.DocsIndexName != "" {
		cfg.DocsIndexName = c.DocsIndexName
	}
	if c.FetchTimeoutSeconds != nil {
		cfg.FetchTimeoutSeconds = *c.FetchTimeoutSeconds
	}
	if c.FetchMaxBytes != nil {
		cfg.FetchMaxBytes = *c.FetchMaxBytes
	}
	if c.DocsTTLDays != nil {
		cfg.DocsTTLDays = *c.DocsTTLDays
	}
	if len(c.FetchAllowHosts) > 0 {
		cfg.FetchAllowHosts = c.FetchAllowHosts
	}
	if c.FetchUserAgent != "" {
		cfg.FetchUserAgent = c.FetchUserAgent
	}
	if c.FetchRespectRobots != nil {
		cfg.FetchRespectRobots = *c.FetchRespectRobots
	}
	if c.FetchProxyURL != "" {
		cfg.FetchProxyURL = c.FetchProxyURL
	}
	if c.FetchProxyFallback != nil {
		cfg.FetchProxyFallback = *c.FetchProxyFallback
	}
	if len(c.LocalRepoRoots) > 0 {
		cfg.LocalRepoRoots = splitCSV(strings.Join(c.LocalRepoRoots, ","))
	}
	if c.LocalRepoScanDepth != nil {
		cfg.LocalRepoScanDepth = *c.LocalRepoScanDepth
	}
	if c.AllowPlaintext != nil {
		cfg.AllowPlaintext = *c.AllowPlaintext
	}
	return nil
}

// normalizeGuard maps a configured level to its canonical form, rejecting
// typos loudly: a silently ignored guard value is a guard that is not there.
func normalizeGuard(v string) (string, error) {
	g := strings.ToLower(strings.TrimSpace(v))
	switch g {
	case GuardReject, GuardAsk, GuardAllow:
		return g, nil
	}
	return "", fmt.Errorf("must be one of reject|ask|allow, got %q", v)
}

// FindConfigPath returns the path of the config file to use, or "" when none
// exists. Lookup order:
//  1. MEMENTO_CONF env var (explicit path)
//  2. memento.conf in the current working directory
//  3. memento.conf next to the executable
//  4. ~/.memento.conf
func FindConfigPath() string {
	if p := os.Getenv("MEMENTO_CONF"); p != "" {
		return p
	}
	if _, err := os.Stat("memento.conf"); err == nil {
		return "memento.conf"
	}
	if execPath, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(execPath), "memento.conf")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".memento.conf")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
