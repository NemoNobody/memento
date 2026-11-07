package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
)

func TestParseGitHubURL(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		owner string
		repo  string
		ref   string
		path  string
	}{
		{"https://github.com/NemoNobody/memento/blob/master/README.md", true, "NemoNobody", "memento", "master", "README.md"},
		{"https://github.com/NemoNobody/memento/blob/master/docs/a.md", true, "NemoNobody", "memento", "master", "docs/a.md"},
		{"https://github.com/NemoNobody/memento/raw/master/x.txt", true, "NemoNobody", "memento", "master", "x.txt"},
		{"https://raw.githubusercontent.com/NemoNobody/memento/master/x.txt", true, "NemoNobody", "memento", "master", "x.txt"},
		{"https://github.com/o/r/tree/master", false, "", "", "", ""},
		{"https://github.com/o/r", false, "", "", "", ""},
		{"https://gitlab.com/o/r/blob/master/x", false, "", "", "", ""},
		{"https://raw.githubusercontent.com/o/r/ref", false, "", "", "", ""},
	}
	for _, tc := range cases {
		got, ok := parseGitHubURL(tc.in)
		if ok != tc.ok {
			t.Errorf("parseGitHubURL(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && (got.owner != tc.owner || got.repo != tc.repo || got.ref != tc.ref || got.path != tc.path) {
			t.Errorf("parseGitHubURL(%q) = %+v", tc.in, got)
		}
	}
}

func TestNormalizeRemote(t *testing.T) {
	cases := []struct {
		in          string
		ok          bool
		owner, repo string
	}{
		{"git@github.com:NemoNobody/memento.git", true, "NemoNobody", "memento"},
		{"git@github.com:NemoNobody/memento", true, "NemoNobody", "memento"},
		{"ssh://git@github.com/o/r.git", true, "o", "r"},
		{"https://github.com/o/r.git", true, "o", "r"},
		{"https://github.com/o/r", true, "o", "r"},
		{"https://GitLab.com/o/r", false, "", ""},
		{"git@gitlab.com:o/r.git", false, "", ""},
		{"", false, "", ""},
		{"https://github.com/o", false, "", ""},
	}
	for _, tc := range cases {
		o, r, ok := normalizeRemote(tc.in)
		if ok != tc.ok {
			t.Errorf("normalizeRemote(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && (o != tc.owner || r != tc.repo) {
			t.Errorf("normalizeRemote(%q) = %q/%q, want %q/%q", tc.in, o, r, tc.owner, tc.repo)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newLocalRepo builds a throwaway clone-like repo: git init with a fixed
// branch, origin remote, committed file(s) as requested.
func newLocalRepo(t *testing.T, dir, branch, remote string, files ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", branch, dir)
	runGit(t, dir, "-C", dir, "remote", "add", "origin", remote)
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t",
		"-C", dir, "add", "-A")
	runGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t",
		"-C", dir, "commit", "-q", "-m", "init")
	return strings.TrimSpace(runGit(t, dir, "-C", dir, "rev-parse", "HEAD"))
}

func TestGitHubLocal(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "checkout", "memento")
	sha := newLocalRepo(t, repoDir, "master", "git@github.com:NemoNobody/memento.git",
		"README.md", "docs/a.md")

	cfg := &config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 3}

	// branch ref → local path
	p, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/memento/blob/master/README.md")
	if !ok || p != filepath.Join(repoDir, "README.md") {
		t.Errorf("branch ref: got %q ok=%v", p, ok)
	}
	// nested path
	p, ok = GitHubLocal(cfg, "https://raw.githubusercontent.com/NemoNobody/memento/master/docs/a.md")
	if !ok || p != filepath.Join(repoDir, "docs", "a.md") {
		t.Errorf("nested path: got %q ok=%v", p, ok)
	}
	// commit sha ref
	p, ok = GitHubLocal(cfg, "https://github.com/NemoNobody/memento/blob/"+sha+"/README.md")
	if !ok || p == "" {
		t.Errorf("sha ref: got %q ok=%v", p, ok)
	}
	// sha prefix (>=7)
	if len(sha) >= 8 {
		p, ok = GitHubLocal(cfg, "https://github.com/NemoNobody/memento/blob/"+sha[:8]+"/README.md")
		if !ok || p == "" {
			t.Errorf("sha prefix: got %q ok=%v", p, ok)
		}
	}
	// stale ref must NOT be served from the working tree
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/memento/blob/main/README.md"); ok {
		t.Error("stale ref must not hit the local clone")
	}
	// missing file
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/memento/blob/master/absent.md"); ok {
		t.Error("missing file must not hit")
	}
	// path escaping the repo root (decoded ../)
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/memento/blob/master/..%2Fsecret"); ok {
		t.Error("path traversal must be rejected")
	}
	// other repo
	if _, ok := GitHubLocal(cfg, "https://github.com/other/memento/blob/master/README.md"); ok {
		t.Error("other owner must not hit")
	}
	// no roots configured
	if _, ok := GitHubLocal(&config.Config{}, "https://github.com/NemoNobody/memento/blob/master/README.md"); ok {
		t.Error("empty roots must not hit")
	}
}

func TestFindLocalRepoDepth(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "memento")
	newLocalRepo(t, deep, "master", "https://github.com/NemoNobody/memento.git", "README.md")

	if _, ok := FindLocalRepo(&config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 2}, "NemoNobody", "memento"); ok {
		t.Error("depth 2 must not find a repo at depth 3")
	}
	dir, ok := FindLocalRepo(&config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 3}, "NemoNobody", "memento")
	if !ok || dir != deep {
		t.Errorf("depth 3: got %q ok=%v, want %q", dir, ok, deep)
	}
}
