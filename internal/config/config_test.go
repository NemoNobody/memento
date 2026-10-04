package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

// clearEnv resets the config-related env vars to empty so tests are
// deterministic regardless of the surrounding environment.
func clearEnv(t *testing.T) {
	for _, k := range []string{
		"MEMENTO_CONF",
		"OPENSEARCH_URL",
		"OPENSEARCH_USER",
		"OPENSEARCH_PASSWORD",
		"MEMENTO_INDEX",
	} {
		t.Setenv(k, "")
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
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true by default")
	}
	if cfg.IndexName != "memento" {
		t.Errorf("IndexName = %q, want memento", cfg.IndexName)
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
	// Regression: a file without insecure_skip_verify must keep the default.
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false, want default true when key omitted")
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
	os.Remove(cwdFile)
	execPath, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	execConf := filepath.Join(filepath.Dir(execPath), "memento.conf")
	if err := os.WriteFile(execConf, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write exec memento.conf: %v", err)
	}
	defer os.Remove(execConf)
	if p := FindConfigPath(); p != execConf {
		t.Errorf("exec path = %q, want %s", p, execConf)
	}

	// case 4: home .memento.conf wins (no cwd or exec file).
	os.Remove(execConf)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	homeFile := filepath.Join(homeDir, ".memento.conf")
	if err := os.WriteFile(homeFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write home .memento.conf: %v", err)
	}
	defer os.Remove(homeFile)
	if p := FindConfigPath(); p != homeFile {
		t.Errorf("home path = %q, want %s", p, homeFile)
	}

	// case 5: nothing found.
	os.Remove(homeFile)
	t.Setenv("HOME", t.TempDir())
	if p := FindConfigPath(); p != "" {
		t.Errorf("no-file path = %q, want empty", p)
	}
}
