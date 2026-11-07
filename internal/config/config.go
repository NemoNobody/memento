package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		OpenSearchURL:      "https://127.0.0.1:9200",
		OpenSearchUser:     "admin",
		OpenSearchPassword: "",
		InsecureSkipVerify: true,
		IndexName:          "memento",
		HistoryLimit:       20,
		SecretGuard:        GuardReject,
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
// MEMENTO_HISTORY_LIMIT, MEMENTO_SECRET_GUARD.
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
	return cfg, nil
}

// fileConfig mirrors Config for JSON decoding. Fields are optional: a missing
// key must leave the default in place. InsecureSkipVerify is a *bool so that
// "absent" (keep default true) is distinguishable from an explicit false.
type fileConfig struct {
	OpenSearchURL      string  `json:"opensearch_url"`
	OpenSearchUser     string  `json:"opensearch_user"`
	OpenSearchPassword string  `json:"opensearch_password"`
	InsecureSkipVerify *bool   `json:"insecure_skip_verify"`
	IndexName          string  `json:"index_name"`
	HistoryLimit       *int    `json:"history_limit"`
	SecretGuard        *string `json:"secret_guard"`
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
