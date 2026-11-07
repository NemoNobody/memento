package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NemoNobody/memento/internal/keyring"
)

// writePlainConf writes a config file with a plaintext password.
func writePlainConf(t *testing.T, dir string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordPrecedence(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	fileBody := `{"opensearch_url": "https://os.internal:9200", "opensearch_password": "filepw"}`

	cases := []struct {
		name       string
		keyringPw  string
		keyringErr error
		envPw      string
		plaintext  bool
		wantPw     string
		wantSource string
	}{
		{"env wins over keyring and file", "keyringpw", nil, "envpw", true, "envpw", "env"},
		{"keyring wins over file", "keyringpw", nil, "", false, "keyringpw", "keyring"},
		{"file only when allow_plaintext", "", keyring.ErrNotFound, "", true, "filepw", "file"},
		{"file refused by default", "", keyring.ErrNotFound, "", false, "", "none"},
		{"keyring outage falls back to file", "", errors.New("no session bus"), "", true, "filepw", "file"},
		{"keyring outage, no plaintext: none", "", errors.New("no session bus"), "", false, "", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(fileBody), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { removeFile(t, filepath.Join(dir, "memento.conf")) })
			fakeKeyring(t, tc.keyringPw, tc.keyringErr)
			if tc.envPw != "" {
				t.Setenv("OPENSEARCH_PASSWORD", tc.envPw)
			}
			if tc.plaintext {
				t.Setenv("MEMENTO_ALLOW_PLAINTEXT", "true")
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.OpenSearchPassword != tc.wantPw {
				t.Errorf("password = %q, want %q", cfg.OpenSearchPassword, tc.wantPw)
			}
			if cfg.PasswordSource != tc.wantSource {
				t.Errorf("source = %q, want %q", cfg.PasswordSource, tc.wantSource)
			}
		})
	}
}

func TestKeyringServiceAndAccount(t *testing.T) {
	if KeyringService != "memento" {
		t.Errorf("service = %q, want memento", KeyringService)
	}
	cases := map[string]string{
		"https://127.0.0.1:9200":   "127.0.0.1:9200",
		"https://os.internal":      "os.internal",
		"http://search.lan:8080/x": "search.lan:8080",
		"not a url":                "not a url",
	}
	for in, want := range cases {
		if got := keyringAccount(in); got != want {
			t.Errorf("keyringAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowPlaintextDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	writePlainConf(t, dir, `{"opensearch_url": "https://127.0.0.1:9200"}`)
	t.Cleanup(func() { removeFile(t, filepath.Join(dir, "memento.conf")) })

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowPlaintext {
		t.Error("allow_plaintext must default to false")
	}
	if cfg.PasswordSource != "none" {
		t.Errorf("source = %q, want none", cfg.PasswordSource)
	}
}

func TestAllowPlaintextFileField(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)
	writePlainConf(t, dir, `{"allow_plaintext": true, "opensearch_password": "filepw"}`)
	t.Cleanup(func() { removeFile(t, filepath.Join(dir, "memento.conf")) })

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AllowPlaintext || cfg.PasswordSource != "file" || cfg.OpenSearchPassword != "filepw" {
		t.Errorf("allow_plaintext file field not applied: %+v", cfg)
	}
}
