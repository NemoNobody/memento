package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/keyring"
)

// fakeKeyringCmd replaces the OS keyring for cmd tests: stored entries are
// recorded in memory, the real wallet is never touched.
func fakeKeyringCmd(t *testing.T) (stored map[string]string) {
	t.Helper()
	stored = map[string]string{}
	oldGet, oldSet, oldDel := keyring.Get, keyring.Set, keyring.Delete
	t.Cleanup(func() {
		keyring.Get, keyring.Set, keyring.Delete = oldGet, oldSet, oldDel
	})
	keyring.Get = func(service, user string) (string, error) {
		pw, ok := stored[service+"/"+user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return pw, nil
	}
	keyring.Set = func(service, user, value string) error {
		stored[service+"/"+user] = value
		return nil
	}
	keyring.Delete = func(service, user string) error {
		if _, ok := stored[service+"/"+user]; !ok {
			return keyring.ErrNotFound
		}
		delete(stored, service+"/"+user)
		return nil
	}
	return stored
}

// credsTestEnv isolates config resolution into a temp dir.
func credsTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	conf := filepath.Join(dir, "memento.conf")
	if err := os.WriteFile(conf, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMENTO_CONF", conf)
	t.Setenv("OPENSEARCH_PASSWORD", "")
}

// withStdin replaces os.Stdin with a piped file for the duration of fn.
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; _ = f.Close() })
	fn()
}

func TestCredsSetPipedAndRm(t *testing.T) {
	credsTestEnv(t)
	stored := fakeKeyringCmd(t)

	cmd := CredsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	withStdin(t, "sekret pw\n", func() {
		cmd.SetArgs([]string{"set", "--url", "https://os.test:9200"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("creds set: %v", err)
		}
	})
	if got := out.String(); !strings.Contains(got, "account=os.test:9200") {
		t.Errorf("set output: %q", got)
	}
	if stored["memento/os.test:9200"] != "sekret pw" {
		t.Errorf("keyring after set: %v", stored)
	}

	out.Reset()
	cmd = CredsCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--url", "https://os.test:9200"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm: %v", err)
	}
	if _, ok := stored["memento/os.test:9200"]; ok {
		t.Error("entry survived rm")
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("rm output: %q", out.String())
	}

	// rm on a missing entry is an answer, not a crash.
	out.Reset()
	cmd = CredsCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rm", "--url", "https://os.test:9200"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("creds rm (absent): %v", err)
	}
	if !strings.Contains(out.String(), "no entry") {
		t.Errorf("rm absent output: %q", out.String())
	}
}

// The credential-leak guards: a --url carrying userinfo must be refused, and
// no code path may echo the password back (prompts, "stored:" lines, status).
func TestCredsRefuseUserinfo(t *testing.T) {
	credsTestEnv(t)
	fakeKeyringCmd(t)

	for _, raw := range []string{
		"https://admin:hunter2@os.test:9200",
		"https://admin:hunter2@os%test:9200", // parse failure with "@": no echo
	} {
		if err := refuseUserinfo(raw); err == nil {
			t.Errorf("refuseUserinfo(%q): want error, got nil", raw)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("refuseUserinfo echoes the password: %v", err)
		}
	}
	for _, ok := range []string{"https://os.test:9200", "http://localhost:9200"} {
		if err := refuseUserinfo(ok); err != nil {
			t.Errorf("refuseUserinfo(%q): %v, want nil", ok, err)
		}
	}

	// accountFor must never return a string carrying the credentials.
	if got := accountFor("https://admin:hunter2@os.test:9200"); got != "os.test:9200" {
		t.Errorf("accountFor userinfo url = %q, want the bare host", got)
	}
	if got := accountFor("https://admin:hunter2@os%test:9200"); strings.Contains(got, "hunter2") {
		t.Errorf("accountFor echoes the password on parse failure: %q", got)
	}

	// credsTarget (used by set and rm) refuses the userinfo flag.
	old := credsFlags.url
	t.Cleanup(func() { credsFlags.url = old })
	credsFlags.url = "https://admin:hunter2@os.test:9200"
	if _, err := credsTarget(); err == nil {
		t.Error("credsTarget: want refusal for userinfo --url, got nil")
	} else if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("credsTarget echoes the password: %v", err)
	}
	credsFlags.url = "https://os.test:9200"
	if got, err := credsTarget(); err != nil || got != "https://os.test:9200" {
		t.Errorf("credsTarget clean url = %q, %v", got, err)
	}
}

func TestCredsSetEmptyPasswordAborts(t *testing.T) {
	credsTestEnv(t)
	fakeKeyringCmd(t)
	// fatalErr exits the process on empty input, so exercise readSecret
	// directly: the guard under test is the empty-result contract.
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	pw, err := readSecret("prompt: ", f, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("readSecret: %v", err)
	}
	if pw != "" {
		t.Errorf("pw = %q, want empty (aborted by the command)", pw)
	}
}

func TestRenderCredsStatus(t *testing.T) {
	cfg := config.DefaultConfig()

	// keyring present, resolving.
	cfg.PasswordSource = "keyring"
	text := renderCredsStatus("os.test:9200", cfg, nil)
	for _, want := range []string{"account: memento/os.test:9200", "source: keyring", "entry present"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sekret") {
		t.Error("the status renderer has no access to values by design")
	}

	// keyring empty, file plaintext refused.
	cfg = config.DefaultConfig()
	cfg.PasswordSource = "none"
	cfg.FileHasPlaintext = true
	cfg.AllowPlaintext = false
	text = renderCredsStatus("os.test:9200", cfg, errors.New("no session bus"))
	for _, want := range []string{"REFUSED", "no session bus", "hint:"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}

	// --url overrides the endpoint: the config's password source belongs to a
	// different host, so it must not be shown, and the config-file section
	// (plaintext status, migration hint) is about the configured host too.
	text = renderCredsStatus("os.test:9200", nil, nil)
	for _, want := range []string{"source: n/a (--url", "entry present"} {
		if !strings.Contains(text, want) {
			t.Errorf("nil-config text lacks %q:\n%s", want, text)
		}
	}
	for _, deny := range []string{"config file plaintext", "hint:"} {
		if strings.Contains(text, deny) {
			t.Errorf("nil-config text must not mention %q:\n%s", deny, text)
		}
	}
}
