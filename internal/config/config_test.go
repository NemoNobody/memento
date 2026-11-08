package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/keyring"
)

// TestMain makes the keyring dispatcher safe for every test in this package:
// the default fake never touches the real wallet. A real provider call from a
// test binary can block on a desktop access prompt (a new app id asking the
// wallet for an existing secret) — the suite must never depend on the
// machine's keyring state. Keyring-specific tests install their own fakes
// via fakeKeyring.
func TestMain(m *testing.M) {
	get, set, del := keyring.Get, keyring.Set, keyring.Delete
	keyring.Get = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	keyring.Set = func(string, string, string) error { return nil }
	keyring.Delete = func(string, string) error { return keyring.ErrNotFound }
	code := m.Run()
	keyring.Get, keyring.Set, keyring.Delete = get, set, del
	os.Exit(code)
}

// removeFile deletes path for test cleanup. A real removal failure fails the
// test (it can leave a stray memento.conf next to the test binary and skew
// later tests); a file that is already gone is fine.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// chdir switches the working directory and returns a cleanup function that
// restores it. Call the returned func at the end of each test (or via t.Cleanup).
func chdir(t *testing.T, dir string) func() {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getcwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	return func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	}
}

// fakeKeyring replaces the OS keyring calls: config tests must resolve
// credentials only from env/file, never from the developer's real wallet.
func fakeKeyring(t *testing.T, pw string, err error) {
	t.Helper()
	oldGet, oldSet, oldDel := keyring.Get, keyring.Set, keyring.Delete
	keyring.Get = func(service, user string) (string, error) { return pw, err }
	keyring.Set = func(service, user, value string) error { return nil }
	keyring.Delete = func(service, user string) error { return nil }
	t.Cleanup(func() { keyring.Get, keyring.Set, keyring.Delete = oldGet, oldSet, oldDel })
}

// clearEnv resets the config-related env vars to empty so tests are
// deterministic regardless of the surrounding environment, and isolates the
// keyring (the real service is never consulted from tests).
func clearEnv(t *testing.T) {
	fakeKeyring(t, "", keyring.ErrNotFound)
	for _, k := range []string{
		"MEMENTO_CONF",
		"OPENSEARCH_URL",
		"OPENSEARCH_USER",
		"OPENSEARCH_PASSWORD",
		"MEMENTO_INDEX",
		"MEMENTO_HISTORY_LIMIT",
		"MEMENTO_SECRET_GUARD",
		"MEMENTO_ALLOW_PLAINTEXT",
		"MEMENTO_DOCS_INDEX",
		"MEMENTO_DOCS_TTL_DAYS",
		"MEMENTO_FETCH_TIMEOUT",
		"MEMENTO_FETCH_MAX_BYTES",
		"MEMENTO_FETCH_ALLOW_PRIVATE",
		"MEMENTO_FETCH_HOSTS",
		"OPENSEARCH_INSECURE_SKIP_VERIFY",
		"MEMENTO_FETCH_USER_AGENT",
		"MEMENTO_FETCH_PROXY_URL",
		"MEMENTO_FETCH_PROXY_FALLBACK",
		"MEMENTO_FETCH_RESPECT_ROBOTS",
		"MEMENTO_LOCAL_REPO_ROOTS",
		"MEMENTO_REPO_SCAN_DEPTH",
	} {
		t.Setenv(k, "")
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://admin:secret@127.0.0.1:9200", "https://admin:xxxxx@127.0.0.1:9200"},
		{"https://127.0.0.1:9200", "https://127.0.0.1:9200"},
		{"https://admin@host", "https://admin@host"},
		{"::not a url::", "::not a url::"},
	}
	for _, c := range cases {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Credentials in opensearch_url leak into every error message and status
// print; the config refuses them at load time (the fetch path does the same
// for user URLs).
func TestLoadConfigRejectsURLUserInfo(t *testing.T) {
	t.Setenv("OPENSEARCH_URL", "https://admin:hunter2@127.0.0.1:9200")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("opensearch_url with user info must be rejected")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection message echoes the password: %v", err)
	}
	if !strings.Contains(err.Error(), "admin:xxxxx") {
		t.Errorf("the rejection message must show a redacted url: %v", err)
	}

	// A credential url that fails url.Parse (broken percent-escape) must be
	// refused too, without echoing the raw text: url.Parse's own error quotes
	// the input, and RedactURL returns raw on parse failure.
	t.Setenv("OPENSEARCH_URL", "https://admin:sec%zzret@os.test:9200")
	_, err = LoadConfig()
	if err == nil {
		t.Fatal("unparseable opensearch_url with credentials must be rejected")
	}
	if strings.Contains(err.Error(), "sec%zzret") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the rejection message echoes the credentials: %v", err)
	}

	// A plain (credential-free) unparseable url is a config error as well,
	// with the parse error quoted (nothing to hide).
	t.Setenv("OPENSEARCH_URL", "http://[::1")
	_, err = LoadConfig()
	if err == nil {
		t.Fatal("unparseable opensearch_url must be rejected")
	}
}

// FindConfigPathNoCWD must skip a ./memento.conf: `memento mcp` runs with
// the cwd of an arbitrary (possibly cloned, untrusted) project.
func TestFindConfigPathNoCWD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MEMENTO_CONF", "")
	t.Chdir(dir)

	if got := FindConfigPath(); got != "memento.conf" {
		t.Errorf("FindConfigPath() = %q, want the cwd entry", got)
	}
	if got := FindConfigPathNoCWD(); got != "" {
		t.Errorf("FindConfigPathNoCWD() = %q, want no cwd entry", got)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.OpenSearchURL != "https://127.0.0.1:9200" {
		t.Errorf("OpenSearchURL = %q, want default", cfg.OpenSearchURL)
	}
	if cfg.OpenSearchUser != "admin" {
		t.Errorf("OpenSearchUser = %q, want admin", cfg.OpenSearchUser)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be false by default (TLS is verified)")
	}
	if cfg.IndexName != "memento" {
		t.Errorf("IndexName = %q, want memento", cfg.IndexName)
	}
	if cfg.HistoryLimit != 20 {
		t.Errorf("HistoryLimit = %d, want default 20", cfg.HistoryLimit)
	}
	if cfg.SecretGuard != GuardReject {
		t.Errorf("SecretGuard = %q, want the safe default %q", cfg.SecretGuard, GuardReject)
	}
}

func TestLoadConfig_NoFileNoEnv(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchURL != "https://127.0.0.1:9200" {
		t.Errorf("OpenSearchURL = %q, want default (no file)", cfg.OpenSearchURL)
	}
	if cfg.IndexName != "memento" {
		t.Errorf("IndexName = %q, want memento (no file)", cfg.IndexName)
	}
	if cfg.HistoryLimit != 20 {
		t.Errorf("HistoryLimit = %d, want default 20 (no file)", cfg.HistoryLimit)
	}
}

func TestLoadConfig_File(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(fc, []byte(`{
  "opensearch_url": "https://custom:9200",
  "opensearch_user": "user1",
  "opensearch_password": "pass1",
  "index_name": "myindex"
}`), 0644); err != nil {
		t.Fatalf("write memento.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchURL != "https://custom:9200" {
		t.Errorf("OpenSearchURL = %q, want file value", cfg.OpenSearchURL)
	}
	if cfg.OpenSearchUser != "user1" {
		t.Errorf("OpenSearchUser = %q, want user1", cfg.OpenSearchUser)
	}
	if cfg.IndexName != "myindex" {
		t.Errorf("IndexName = %q, want myindex", cfg.IndexName)
	}
	// Regression: a file without insecure_skip_verify must keep the default
	// (verified TLS: verification is only ever off by explicit opt-in).
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want default false when key omitted")
	}
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	fc := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(fc, []byte(`{
  "opensearch_url": "https://file:9200",
  "index_name": "fileindex"
}`), 0644); err != nil {
		t.Fatalf("write memento.conf: %v", err)
	}
	t.Setenv("OPENSEARCH_URL", "https://env:9200")
	t.Setenv("MEMENTO_INDEX", "envindex")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OpenSearchURL != "https://env:9200" {
		t.Errorf("OpenSearchURL = %q, want env value (override)", cfg.OpenSearchURL)
	}
	if cfg.IndexName != "envindex" {
		t.Errorf("IndexName = %q, want env value (override)", cfg.IndexName)
	}
}

// TestLoadConfig_HistoryLimit pins the per-key history retention knob: an
// explicit file value overrides the default, an explicit 0 means "keep
// everything" and must survive decoding (the *int in fileConfig exists for
// exactly this distinction).
func TestLoadConfig_HistoryLimit(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		want int
	}{
		{"file override", `{"history_limit": 5}`, "", 5},
		{"file zero keeps everything", `{"history_limit": 0}`, "", 0},
		{"file negative disables retention", `{"history_limit": -1}`, "", -1},
		{"env overrides file", `{"history_limit": 5}`, "7", 7},
		{"env zero", `{}`, "0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restore := chdir(t, dir)
			defer restore()
			clearEnv(t)
			fc := filepath.Join(dir, "memento.conf")
			if err := os.WriteFile(fc, []byte(tc.file), 0644); err != nil {
				t.Fatalf("write memento.conf: %v", err)
			}
			if tc.env != "" {
				t.Setenv("MEMENTO_HISTORY_LIMIT", tc.env)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.HistoryLimit != tc.want {
				t.Errorf("HistoryLimit = %d, want %d", cfg.HistoryLimit, tc.want)
			}
		})
	}
}

func TestLoadConfig_HistoryLimitBadEnvIsAnError(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	t.Setenv("MEMENTO_HISTORY_LIMIT", "not-a-number")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for MEMENTO_HISTORY_LIMIT=not-a-number, want error")
	}
}

// TestLoadConfig_SecretGuard pins the guard rail: the default is the safe
// level, file and env set it, typos fail loudly (a silently ignored guard
// value is a guard that is not there).
func TestLoadConfig_SecretGuard(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		want string
	}{
		{"file ask", `{"secret_guard": "ask"}`, "", GuardAsk},
		{"file allow", `{"secret_guard": "allow"}`, "", GuardAllow},
		{"file canonicalizes", `{"secret_guard": "  ASK "}`, "", GuardAsk},
		{"env overrides file", `{"secret_guard": "allow"}`, "reject", GuardReject},
		{"env canonicalizes", `{}`, "Allow", GuardAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restore := chdir(t, dir)
			defer restore()
			clearEnv(t)
			fc := filepath.Join(dir, "memento.conf")
			if err := os.WriteFile(fc, []byte(tc.file), 0644); err != nil {
				t.Fatalf("write memento.conf: %v", err)
			}
			if tc.env != "" {
				t.Setenv("MEMENTO_SECRET_GUARD", tc.env)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.SecretGuard != tc.want {
				t.Errorf("SecretGuard = %q, want %q", cfg.SecretGuard, tc.want)
			}
		})
	}
}

func TestLoadConfig_SecretGuardBadValuesAreErrors(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	fc := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(fc, []byte(`{"secret_guard": "permissive"}`), 0644); err != nil {
		t.Fatalf("write memento.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for secret_guard=permissive, want error")
	}

	if err := os.WriteFile(fc, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write memento.conf: %v", err)
	}
	t.Setenv("MEMENTO_SECRET_GUARD", "yes-please")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for MEMENTO_SECRET_GUARD=yes-please, want error")
	}
}

func TestLoadConfig_InsecureSkipVerifyExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(fc, []byte(`{"insecure_skip_verify": false}`), 0644); err != nil {
		t.Fatalf("write memento.conf: %v", err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want explicit false from file")
	}
}

func TestLoadConfig_MalformedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	fc := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(fc, []byte(`{"opensearch_url": `), 0644); err != nil {
		t.Fatalf("write memento.conf: %v", err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for malformed JSON, want error")
	}
}

func TestLoadConfig_MissingExplicitPathIsAnError(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	t.Setenv("MEMENTO_CONF", filepath.Join(dir, "does-not-exist.conf"))
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() = nil error for missing MEMENTO_CONF path, want error")
	}
}

func TestFindConfigPath(t *testing.T) {
	// case 1: MEMENTO_CONF wins.
	t.Setenv("MEMENTO_CONF", "/explicit/path/conf.json")
	if p := FindConfigPath(); p != "/explicit/path/conf.json" {
		t.Errorf("MEMENTO_CONF path = %q, want explicit", p)
	}

	// case 2: cwd memento.conf wins.
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	cwdFile := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(cwdFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write cwd memento.conf: %v", err)
	}
	if p := FindConfigPath(); p != "memento.conf" {
		t.Errorf("cwd path = %q, want memento.conf", p)
	}

	// case 3: executable-dir memento.conf wins (no cwd file).
	removeFile(t, cwdFile)
	execPath, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	execConf := filepath.Join(filepath.Dir(execPath), "memento.conf")
	if err := os.WriteFile(execConf, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write exec memento.conf: %v", err)
	}
	t.Cleanup(func() { removeFile(t, execConf) })
	if p := FindConfigPath(); p != execConf {
		t.Errorf("exec path = %q, want %s", p, execConf)
	}

	// case 4: home .memento.conf wins (no cwd or exec file).
	removeFile(t, execConf)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	homeFile := filepath.Join(homeDir, ".memento.conf")
	if err := os.WriteFile(homeFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write home .memento.conf: %v", err)
	}
	t.Cleanup(func() { removeFile(t, homeFile) })
	if p := FindConfigPath(); p != homeFile {
		t.Errorf("home path = %q, want %s", p, homeFile)
	}

	// case 5: nothing found.
	removeFile(t, homeFile)
	t.Setenv("HOME", t.TempDir())
	if p := FindConfigPath(); p != "" {
		t.Errorf("no-file path = %q, want empty", p)
	}
}
