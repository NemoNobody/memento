package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A set-but-empty MEMENTO_FETCH_HOSTS must not silently blank the file's
// allowlist: an empty allowlist means "any public host", so a value like
// "," widens the SSRF scope with nobody noticing (finding 143).
func TestFetchHostsEnvMustNameAtLeastOneHost(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(`{"fetch_allow_hosts": ["keep.me"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMENTO_FETCH_HOSTS", " , ")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "names no hosts") {
		t.Errorf("set-but-empty MEMENTO_FETCH_HOSTS must be refused, got %v", err)
	}
}

// docs_ttl_days < 0 is rejected for file and env alike: consumers gate on
// "> 0", so a negative value inverts the intent (docs never expire —
// finding 144). Zero stays legal ("never expire", stated on purpose).
func TestDocsTTLNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(`{"docs_ttl_days": -1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "docs_ttl_days") {
		t.Errorf("negative file docs_ttl_days must be refused, got %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMENTO_DOCS_TTL_DAYS", "-7")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "docs_ttl_days") {
		t.Errorf("negative env docs_ttl_days must be refused, got %v", err)
	}
}

// An opensearch_url that parses but carries no host ("https://") would
// only fail at request time; load refuses it where the typo was made
// (finding 149).
func TestOpenSearchURLEmptyHostRejected(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	clearEnv(t)

	t.Setenv("OPENSEARCH_URL", "https://")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "no host") {
		t.Errorf("hostless opensearch_url must be refused at load, got %v", err)
	}
}

// NormalizeLabel strips invisible Unicode — C1 controls, zero-width
// spaces, bidi overrides — not just C0/DEL: those are exactly the bytes
// that forge terminal output in shared listings, the raison d'etre of the
// function (finding 146).
func TestNormalizeLabelInvisibleChars(t *testing.T) {
	got, err := NormalizeLabel(" ali\u0085ce\u200b\u202Ebob ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "alicebob" {
		t.Errorf("NormalizeLabel = %q, want %q", got, "alicebob")
	}
	if _, err := NormalizeLabel("\u200b\u202E"); err == nil {
		t.Error("invisible-only label must be refused after stripping")
	}
	// Ordinary Unicode (accented, CJK) survives: the filter targets
	// invisibility, not non-ASCII.
	ok, err := NormalizeLabel("Renée 佐藤")
	if err != nil || ok != "Renée 佐藤" {
		t.Errorf("legitimate Unicode mangled: %q, %v", ok, err)
	}
}
