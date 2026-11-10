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
	"unicode/utf8"

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

// Journal index period names (config history_index_period). A period typo
// would silently split the journal across two naming schemes, so load
// rejects anything outside this set loudly.
const (
	PeriodDay     = "day"
	PeriodWeek    = "week"
	PeriodMonth   = "month"
	PeriodQuarter = "quarter"
	PeriodYear    = "year"
)

// Fetch render backends (config fetch_render): what happens when a plain
// HTTP fetch is refused by bot protection. A backend typo would silently
// disable the fallback the operator believes is active, so load rejects
// anything outside this set loudly.
const (
	RenderOff     = "off"     // plain HTTP only: a blocked fetch is final
	RenderObscura = "obscura" // fallback: the external obscura browser-render binary
)

// DefaultFetchUserAgent is a current desktop Chrome UA. Sites that gate on
// User-Agent sniffing (a bare Go UA reads as a bot to commodity WAFs) let it
// through; the fetch itself stays uncredentialed (no cookies, only
// Accept/UA), so the identity is a generic browser, not the user's real
// fingerprint. The value is config (fetch_user_agent), never personal.
const DefaultFetchUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

// Config holds configuration for the memento CLI.
type Config struct {
	OpenSearchURL      string `json:"opensearch_url"`
	OpenSearchUser     string `json:"opensearch_user"`
	OpenSearchPassword string `json:"opensearch_password"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	IndexName          string `json:"index_name"`

	// HistoryLimit is the default keep-N for `prune`. It no longer trims on
	// write: corrections append to the journal index without a bound
	// (write-forever by design — disk growth equals write count, cleanup is
	// the admin's per-period index delete). 0 means "no default": prune then
	// demands an explicit --keep.
	HistoryLimit int `json:"history_limit"`

	// HistoryIndexPeriod is the date granularity of the journal indices:
	// day | week | month | quarter | year (default month). The journal lives
	// in <index>-history-<period-with-date> indices: one index per period is
	// the hygiene tradeoff (too many indices = shard overhead on the cluster,
	// too coarse a period = the admin cannot prune history by date).
	HistoryIndexPeriod string `json:"history_index_period"`

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

	// FetchAllowPrivate relaxes the SSRF guard for self-hosted targets: it
	// permits loopback, private (RFC1918, CGNAT), link-local, reserved and
	// multicast addresses. Off by default: fetch follows untrusted URLs and
	// agent-supplied links, so the default must never touch internal services
	// (cloud metadata, OpenSearch itself, routers, tailnets).
	FetchAllowPrivate bool `json:"fetch_allow_private"`

	// FetchUserAgent is sent verbatim on document fetches. Some sites reject
	// generic clients; the identity is config, not hardcoded, and it is never
	// a fingerprint: no cookies, no headers beyond Accept/UA.
	FetchUserAgent string `json:"fetch_user_agent"`

	// FetchRespectRobots turns on polite crawler mode: robots.txt of the
	// target host is consulted before fetching, matched by the
	// fetch_user_agent token. Off by default: v1 fetches single
	// user-initiated pages (browser semantics); on is the courteous setting
	// for public/shared infrastructure. The robots verdict gates the whole
	// fetch: the plain path AND the browser render (one contract).
	FetchRespectRobots bool `json:"fetch_respect_robots"`

	// FetchProxyURL routes fetches through a proxy (http/socks5). Empty means
	// direct (plus the standard HTTP_PROXY env, honored by the transport).
	FetchProxyURL string `json:"fetch_proxy_url"`

	// FetchProxyFallback retries a failed/timed-out direct fetch through the
	// proxy. Off by default: fallback is an explicit operator choice.
	FetchProxyFallback bool `json:"fetch_proxy_fallback"`

	// FetchRender selects the fallback backend when a plain fetch is refused
	// by bot protection: "off" (default) ends with the error; "obscura" runs
	// the external obscura binary (real browser engine, JS executes, Chrome
	// TLS-fingerprint under --stealth). Opt-in by design: obscura is a
	// separate third-party download that executes untrusted page JS
	// in-process, so installing and enabling it is the operator's call.
	FetchRender string `json:"fetch_render"`

	// FetchObscuraPath is the obscura binary: an absolute path, or a name
	// resolved through PATH (default "obscura"). obscura and obscura-worker
	// must stay in one directory (upstream layout).
	FetchObscuraPath string `json:"fetch_obscura_path"`

	// FetchObscuraStealth passes --stealth to obscura (Chrome TLS/HTTP2
	// fingerprint, browser header hints). On by default: stealth is the
	// point of the renderer. It requires upstream's -stealth build; the
	// plain build rejects the flag, and the chain surfaces that error.
	FetchObscuraStealth bool `json:"fetch_obscura_stealth"`

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

	// Author labels who wrote a fact: author, the model that recorded it, the
	// host it was recorded on, the agent session. SELF-REPORTED by design —
	// audit metadata for a shared store, not an authentication factor: the
	// caller supplies the labels, so they are only as trustworthy as the
	// caller. The author chain is store-authenticated user → `author` → OS
	// user, and author_source records which one won.
	Author  string `json:"author"`
	Model   string `json:"model"`
	Host    string `json:"host"`
	Session string `json:"session"`

	// SummaryMinLen is the value length (in runes) above which a summary is
	// required: a long fact without a summary is unreadable in a listing, and
	// the store's table of contents is the summary. 0 disables the
	// requirement. SummaryMaxLen caps the summary itself; 0 means no cap.
	SummaryMinLen int `json:"summary_min_len"`
	SummaryMaxLen int `json:"summary_max_len"`

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

	// FilePerm is the mode of the config file that was read (zero when no
	// file was used). status warns when a plaintext password lives in a
	// file group/world-readable.
	FilePerm os.FileMode `json:"-"`
}

// KeyringService is the service name under which OpenSearch credentials live
// in the OS keyring; the account is the host[:port] of the effective URL.
const KeyringService = "memento"

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		OpenSearchURL:      "https://127.0.0.1:9200",
		OpenSearchUser:     "admin",
		OpenSearchPassword: "",
		// TLS verification is on by default: credentials and facts must not
		// transit a MITM-able channel. Self-signed dev clusters (the common
		// local case) set insecure_skip_verify explicitly.
		InsecureSkipVerify:  false,
		IndexName:           "memento",
		HistoryLimit:        20,
		HistoryIndexPeriod:  PeriodMonth,
		SecretGuard:         GuardReject,
		DocsIndexName:       "memento-docs",
		FetchTimeoutSeconds: 30,
		FetchMaxBytes:       2_000_000,
		DocsTTLDays:         90,
		// A browser-shaped UA by default: several documentation sites 403
		// known bot strings. The fetch still identifies itself honestly in
		// robots handling; this is a UA, not a fingerprint.
		FetchUserAgent:      DefaultFetchUserAgent,
		FetchRender:         RenderOff,
		FetchObscuraPath:    "obscura",
		FetchObscuraStealth: true,
		LocalRepoScanDepth:  2,
		// A ~500-token fact is where a listing stops being skimmable; a
		// ~100-token summary is the table-of-contents budget (about 10x
		// compression).
		SummaryMinLen: 2000,
		SummaryMaxLen: 400,
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
// MEMENTO_HISTORY_LIMIT, MEMENTO_HISTORY_PERIOD, MEMENTO_SUMMARY_MIN_LEN,
// MEMENTO_SUMMARY_MAX_LEN, MEMENTO_SECRET_GUARD, MEMENTO_ALLOW_PLAINTEXT,
// MEMENTO_AUTHOR, MEMENTO_MODEL, MEMENTO_HOST, MEMENTO_SESSION,
// MEMENTO_DOCS_INDEX, MEMENTO_FETCH_TIMEOUT, MEMENTO_FETCH_MAX_BYTES,
// MEMENTO_DOCS_TTL_DAYS, MEMENTO_FETCH_HOSTS (comma-separated),
// MEMENTO_FETCH_USER_AGENT, MEMENTO_FETCH_PROXY_URL,
// MEMENTO_FETCH_PROXY_FALLBACK, MEMENTO_FETCH_RESPECT_ROBOTS,
// MEMENTO_FETCH_ALLOW_PRIVATE, MEMENTO_LOCAL_REPO_ROOTS (comma-separated),
// MEMENTO_REPO_SCAN_DEPTH.
func LoadConfigFrom(explicitPath string) (*Config, error) {
	return loadConfig(explicitPath, FindConfigPath)
}

// LoadConfigNoCWD loads the config ignoring the working-directory entry
// (see FindConfigPathNoCWD). This is what `memento mcp` uses when no
// explicit --conf was given.
func LoadConfigNoCWD() (*Config, error) {
	return loadConfig("", FindConfigPathNoCWD)
}

func loadConfig(explicitPath string, finder func() string) (*Config, error) {
	cfg := DefaultConfig()
	path := explicitPath
	if path == "" {
		path = finder()
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
	if v := os.Getenv("MEMENTO_HISTORY_PERIOD"); v != "" {
		p, err := normalizePeriod(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_HISTORY_PERIOD: %w", err)
		}
		cfg.HistoryIndexPeriod = p
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
	if v := os.Getenv("MEMENTO_AUTHOR"); v != "" {
		a, err := NormalizeLabel(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_AUTHOR: %w", err)
		}
		cfg.Author = a
	}
	if v := os.Getenv("MEMENTO_MODEL"); v != "" {
		m, err := NormalizeLabel(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_MODEL: %w", err)
		}
		cfg.Model = m
	}
	if v := os.Getenv("MEMENTO_HOST"); v != "" {
		h, err := NormalizeLabel(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_HOST: %w", err)
		}
		cfg.Host = h
	}
	if v := os.Getenv("MEMENTO_SESSION"); v != "" {
		s, err := NormalizeLabel(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_SESSION: %w", err)
		}
		cfg.Session = s
	}
	if v := os.Getenv("MEMENTO_SUMMARY_MIN_LEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_SUMMARY_MIN_LEN: %w", err)
		}
		// Same bound as the file path: a negative min would silently disable
		// the summary requirement (the check is "> 0"), a negative cap the cap.
		if n < 0 {
			return nil, fmt.Errorf("MEMENTO_SUMMARY_MIN_LEN: must be >= 0, got %d", n)
		}
		cfg.SummaryMinLen = n
	}
	if v := os.Getenv("MEMENTO_SUMMARY_MAX_LEN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_SUMMARY_MAX_LEN: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("MEMENTO_SUMMARY_MAX_LEN: must be >= 0, got %d", n)
		}
		cfg.SummaryMaxLen = n
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
	if v := os.Getenv("MEMENTO_FETCH_RENDER"); v != "" {
		r, err := normalizeRender(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_RENDER: %w", err)
		}
		cfg.FetchRender = r
	}
	if v := os.Getenv("MEMENTO_FETCH_OBSCURA_PATH"); v != "" {
		cfg.FetchObscuraPath = v
	}
	if v := os.Getenv("MEMENTO_FETCH_OBSCURA_STEALTH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_OBSCURA_STEALTH: %w", err)
		}
		cfg.FetchObscuraStealth = b
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
	if v := os.Getenv("MEMENTO_FETCH_ALLOW_PRIVATE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MEMENTO_FETCH_ALLOW_PRIVATE: %w", err)
		}
		cfg.FetchAllowPrivate = b
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
	u, uerr := url.Parse(cfg.OpenSearchURL)
	if uerr != nil {
		// A url that fails to parse must not flow into the config: the client
		// errors would echo the raw text (url.Parse's error quotes the input),
		// and a credential-carrying url usually fails to parse on purpose
		// (percent-escapes, spaces in the password). No echo in either case.
		if strings.Contains(cfg.OpenSearchURL, "@") {
			return nil, fmt.Errorf("opensearch_url is not a valid url: it carries user info; put the user in opensearch_user and the password in the keyring/env (its text is not echoed)")
		}
		return nil, fmt.Errorf("opensearch_url is not a valid url: %w", uerr)
	}
	if u.User != nil {
		// Credentials embedded in the URL reach every error message, the
		// status output and the store server as basic-auth material. The
		// fetch path refuses them for the same reason (NormalizeURL).
		return nil, fmt.Errorf("opensearch_url carries user info; put the user in opensearch_user and the password in the keyring/env (url: %s)", RedactURL(cfg.OpenSearchURL))
	}
	resolvePassword(cfg)
	return cfg, nil
}

// RedactURL strips the password from a URL for display (status, error
// messages, logs). A URL without credentials is returned unchanged.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
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
// "absent" (keep the default false: TLS verification stays ON) is
// distinguishable from an explicit true (opt in to unverified TLS).
type fileConfig struct {
	OpenSearchURL       string   `json:"opensearch_url"`
	OpenSearchUser      string   `json:"opensearch_user"`
	OpenSearchPassword  string   `json:"opensearch_password"`
	InsecureSkipVerify  *bool    `json:"insecure_skip_verify"`
	IndexName           string   `json:"index_name"`
	HistoryLimit        *int     `json:"history_limit"`
	HistoryIndexPeriod  *string  `json:"history_index_period"`
	SecretGuard         *string  `json:"secret_guard"`
	DocsIndexName       string   `json:"docs_index_name"`
	FetchTimeoutSeconds *int     `json:"fetch_timeout_seconds"`
	FetchMaxBytes       *int     `json:"fetch_max_bytes"`
	DocsTTLDays         *int     `json:"docs_ttl_days"`
	FetchAllowHosts     []string `json:"fetch_allow_hosts"`
	FetchUserAgent      string   `json:"fetch_user_agent"`
	FetchRender         string   `json:"fetch_render"`
	FetchObscuraPath    string   `json:"fetch_obscura_path"`
	FetchObscuraStealth *bool    `json:"fetch_obscura_stealth"`
	FetchRespectRobots  *bool    `json:"fetch_respect_robots"`
	FetchProxyURL       string   `json:"fetch_proxy_url"`
	FetchProxyFallback  *bool    `json:"fetch_proxy_fallback"`
	FetchAllowPrivate   *bool    `json:"fetch_allow_private"`
	LocalRepoRoots      []string `json:"local_repo_roots"`
	LocalRepoScanDepth  *int     `json:"local_repo_scan_depth"`
	AllowPlaintext      *bool    `json:"allow_plaintext"`
	Author              *string  `json:"author"`
	Model               *string  `json:"model"`
	Host                *string  `json:"host"`
	Session             *string  `json:"session"`
	SummaryMinLen       *int     `json:"summary_min_len"`
	SummaryMaxLen       *int     `json:"summary_max_len"`
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
	if fi, err := os.Stat(path); err == nil {
		cfg.FilePerm = fi.Mode().Perm()
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
	if c.HistoryIndexPeriod != nil {
		p, err := normalizePeriod(*c.HistoryIndexPeriod)
		if err != nil {
			return fmt.Errorf("config %s: history_index_period: %w", path, err)
		}
		cfg.HistoryIndexPeriod = p
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
	if c.FetchRender != "" {
		r, err := normalizeRender(c.FetchRender)
		if err != nil {
			return fmt.Errorf("fetch_render: %w", err)
		}
		cfg.FetchRender = r
	}
	if c.FetchObscuraPath != "" {
		cfg.FetchObscuraPath = c.FetchObscuraPath
	}
	if c.FetchObscuraStealth != nil {
		cfg.FetchObscuraStealth = *c.FetchObscuraStealth
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
	if c.FetchAllowPrivate != nil {
		cfg.FetchAllowPrivate = *c.FetchAllowPrivate
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
	for _, l := range []struct {
		name string
		in   *string
		out  *string
	}{
		{"author", c.Author, &cfg.Author},
		{"model", c.Model, &cfg.Model},
		{"host", c.Host, &cfg.Host},
		{"session", c.Session, &cfg.Session},
	} {
		if l.in == nil {
			continue
		}
		v, err := NormalizeLabel(*l.in)
		if err != nil {
			return fmt.Errorf("config %s: %s: %w", path, l.name, err)
		}
		*l.out = v
	}
	if c.SummaryMinLen != nil {
		if *c.SummaryMinLen < 0 {
			return fmt.Errorf("config %s: summary_min_len: must be >= 0", path)
		}
		cfg.SummaryMinLen = *c.SummaryMinLen
	}
	if c.SummaryMaxLen != nil {
		if *c.SummaryMaxLen < 0 {
			return fmt.Errorf("config %s: summary_max_len: must be >= 0", path)
		}
		cfg.SummaryMaxLen = *c.SummaryMaxLen
	}
	return nil
}

// normalizePeriod maps a configured journal period to its canonical form,
// rejecting typos loudly: a silently ignored period would split the journal
// across two naming schemes and hide entries from reads.
func normalizePeriod(v string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(v))
	switch p {
	case PeriodDay, PeriodWeek, PeriodMonth, PeriodQuarter, PeriodYear:
		return p, nil
	}
	return "", fmt.Errorf("must be one of day|week|month|quarter|year, got %q", v)
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

// normalizeRender maps the render backend to its canonical form, rejecting
// typos loudly (the normalizeGuard rule): silently ignoring "obscure" would
// run plain fetches while the operator believes pages are JS-rendered.
func normalizeRender(v string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(v))
	switch r {
	case RenderOff, RenderObscura:
		return r, nil
	}
	return "", fmt.Errorf("must be one of off|obscura, got %q", v)
}

// LabelMaxRunes caps every authorship label. They land in stored documents
// and in listings: a runaway label bloats every later read, so the config
// edge is where the bound lives (the client's sanitizeLabel enforces the
// same bound on labels that come from outside: server, OS user, hostname).
const LabelMaxRunes = 200

// NormalizeLabel canonicalizes an authorship label (author, model, host,
// session): trim, drop control bytes (they would forge terminal output in
// listings), and cap the length. A label that is blank after normalization
// is an error, not an empty string: a blank label silently falls back to
// the next chain link (the OS user), and "the config said nothing" reads
// as "the config said this" in a shared store's audit metadata.
func NormalizeLabel(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("label is blank after trimming: refusing a blank label that would silently fall back to the OS user")
	}
	if utf8.RuneCountInString(s) > LabelMaxRunes {
		return "", fmt.Errorf("label longer than %d characters", LabelMaxRunes)
	}
	return s, nil
}

// FindConfigPath returns the path of the config file to use, or "" when none
// exists. Lookup order:
//  1. MEMENTO_CONF env var (explicit path)
//  2. memento.conf in the current working directory
//  3. memento.conf next to the executable
//  4. ~/.memento.conf
func FindConfigPath() string { return findConfigPath(true) }

// FindConfigPathNoCWD is FindConfigPath without the working-directory entry.
// It exists for `memento mcp`: the MCP server runs with the cwd of whatever
// project the editor opened, and a cloned repository's memento.conf must
// never hijack the store endpoint (that file decides where the user's
// credentials and facts are sent).
func FindConfigPathNoCWD() string { return findConfigPath(false) }

func findConfigPath(includeCWD bool) string {
	if p := os.Getenv("MEMENTO_CONF"); p != "" {
		return p
	}
	if includeCWD {
		if _, err := os.Stat("memento.conf"); err == nil {
			return "memento.conf"
		}
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
