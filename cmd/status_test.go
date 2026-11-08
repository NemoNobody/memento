package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"
)

func TestStorageNote(t *testing.T) {
	tests := []struct {
		name string
		st   *client.StorageState
		want string
	}{
		{"nil", nil, ""},
		{
			"absent",
			&client.StorageState{ClusterHealth: "yellow", IndexAbsent: true},
			"index does not exist yet: it is created on the first write",
		},
		{"red", &client.StorageState{IndexHealth: "red"}, "some primary shards are unassigned: parts of the index are unavailable"},
		{"yellow", &client.StorageState{IndexHealth: "yellow"}, "all primary shards are active; some replica shards are unassigned, so redundancy is reduced"},
		{"green", &client.StorageState{IndexHealth: "green"}, "all shards are active"},
		{"no health collected", &client.StorageState{ClusterHealth: "green"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := storageNote(tt.st); got != tt.want {
				t.Errorf("storageNote() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrintStorage_IndexAbsent(t *testing.T) {
	rep := statusReport{
		Storage:     &client.StorageState{ClusterHealth: "yellow", IndexAbsent: true, DataNodes: 1},
		StorageNote: storageNote(&client.StorageState{IndexAbsent: true}),
	}
	var b strings.Builder
	if err := printStorage(&b, rep); err != nil {
		t.Fatalf("printStorage: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "index=absent") {
		t.Errorf("output must mark the index absent, got:\n%s", out)
	}
	if strings.Contains(out, "index= ") {
		t.Errorf("empty index health leaked into the output:\n%s", out)
	}
	if !strings.Contains(out, "index does not exist yet") {
		t.Errorf("missing explanatory note:\n%s", out)
	}
}

// permWarning is the config-file hygiene flag: a plaintext password in a
// file group/others can read is an invitation.
func TestPermWarning(t *testing.T) {
	got := permWarning(&config.Config{FileHasPlaintext: true, FilePerm: 0o644}, "memento.conf")
	if !strings.Contains(got, "chmod 600 memento.conf") || !strings.Contains(got, "644") {
		t.Errorf("permWarning = %q, want the chmod hint with the mode", got)
	}
	if got := permWarning(&config.Config{FileHasPlaintext: true, FilePerm: 0o600}, "x"); got != "" {
		t.Errorf("mode 600 must not warn, got %q", got)
	}
	if got := permWarning(&config.Config{FileHasPlaintext: false, FilePerm: 0o644}, "x"); got != "" {
		t.Errorf("no plaintext password: no warning, got %q", got)
	}
}

// TestCollectStatusConfigChain is the credential-hijack boundary: the MCP
// server resolves config through the NoCWD chain, so a cloned repo's
// memento.conf in the editor's cwd cannot move the report (and its
// credentialed request) to an attacker endpoint. The CLI keeps the cwd entry.
func TestCollectStatusConfigChain(t *testing.T) {
	dir := t.TempDir()
	hijack := `{"opensearch_url":"https://hijack.invalid:9999","opensearch_user":"u","opensearch_password":"p","allow_plaintext":true}`
	if err := os.WriteFile(filepath.Join(dir, "memento.conf"), []byte(hijack), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("MEMENTO_CONF", "")
	t.Setenv("OPENSEARCH_URL", "")
	t.Setenv("OPENSEARCH_PASSWORD", "")
	t.Setenv("HOME", dir) // ~/.memento.conf absent; the file present is ./memento.conf

	rep := collectStatus(context.Background(), "", true)
	if rep.ConfigFile == "memento.conf" || rep.ConfigFile == filepath.Join(dir, "memento.conf") {
		t.Errorf("MCP status read the cwd config: %q", rep.ConfigFile)
	}
	if strings.Contains(rep.URL, "9999") {
		t.Errorf("MCP status reports the hijack endpoint: %q", rep.URL)
	}

	// The CLI chain is allowed to pick the cwd file (explicit local use).
	rep = collectStatus(context.Background(), "", false)
	if rep.ConfigFile != "memento.conf" {
		t.Errorf("CLI status: config_file = %q, want the cwd memento.conf", rep.ConfigFile)
	}
	if !strings.Contains(rep.URL, "9999") {
		t.Errorf("CLI status URL = %q, want the cwd file's endpoint", rep.URL)
	}
}
