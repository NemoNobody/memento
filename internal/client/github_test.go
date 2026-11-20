package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/config"
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
		{"https://github.com/NemoNobody/velruna/blob/master/README.md", true, "NemoNobody", "velruna", "master", "README.md"},
		{"https://github.com/NemoNobody/velruna/blob/master/docs/a.md", true, "NemoNobody", "velruna", "master", "docs/a.md"},
		{"https://github.com/NemoNobody/velruna/raw/master/x.txt", true, "NemoNobody", "velruna", "master", "x.txt"},
		{"https://raw.githubusercontent.com/NemoNobody/velruna/master/x.txt", true, "NemoNobody", "velruna", "master", "x.txt"},
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
		{"git@github.com:NemoNobody/velruna.git", true, "NemoNobody", "velruna"},
		{"git@github.com:NemoNobody/velruna", true, "NemoNobody", "velruna"},
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

// runGit runs a git command for a throwaway repository. Signing is disabled
// explicitly: a machine with commit.gpgsign/tag.gpgsign on would otherwise stop
// at a pinentry prompt and hang the test run instead of failing.
func runGit(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(full, " "), err, out)
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
	runGit(t, "init", "-q", "-b", branch, dir)
	runGit(t, "-C", dir, "remote", "add", "origin", remote)
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "-c", "user.email=t@t", "-c", "user.name=t",
		"-C", dir, "add", "-A")
	runGit(t, "-c", "user.email=t@t", "-c", "user.name=t",
		"-C", dir, "commit", "-q", "-m", "init")
	return strings.TrimSpace(runGit(t, "-C", dir, "rev-parse", "HEAD"))
}

func TestGitHubLocal(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "checkout", "velruna")
	sha := newLocalRepo(t, repoDir, "master", "git@github.com:NemoNobody/velruna.git",
		"README.md", "docs/a.md")

	cfg := &config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 3}

	// branch ref → local path
	p, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/master/README.md")
	if !ok || p != filepath.Join(repoDir, "README.md") {
		t.Errorf("branch ref: got %q ok=%v", p, ok)
	}
	// nested path
	p, ok = GitHubLocal(cfg, "https://raw.githubusercontent.com/NemoNobody/velruna/master/docs/a.md")
	if !ok || p != filepath.Join(repoDir, "docs", "a.md") {
		t.Errorf("nested path: got %q ok=%v", p, ok)
	}
	// commit sha ref
	p, ok = GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/"+sha+"/README.md")
	if !ok || p == "" {
		t.Errorf("sha ref: got %q ok=%v", p, ok)
	}
	// sha prefix (>=7)
	if len(sha) >= 8 {
		p, ok = GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/"+sha[:8]+"/README.md")
		if !ok || p == "" {
			t.Errorf("sha prefix: got %q ok=%v", p, ok)
		}
	}
	// stale ref must NOT be served from the working tree
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/main/README.md"); ok {
		t.Error("stale ref must not hit the local clone")
	}
	// missing file
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/master/absent.md"); ok {
		t.Error("missing file must not hit")
	}
	// path escaping the repo root (decoded ../)
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/master/..%2Fsecret"); ok {
		t.Error("path traversal must be rejected")
	}
	// committed/working-tree symlink escaping the clone: the lexical check
	// passes it, the content is outside the repo (key-theft by cloning a
	// hostile repo whose README.md -> ~/.ssh/id_rsa)
	secret := filepath.Join(root, "id_secret")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repoDir, "ESCAPE.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := GitHubLocal(cfg, "https://github.com/NemoNobody/velruna/blob/master/ESCAPE.md"); ok {
		t.Error("symlink escaping the clone root must be rejected")
	}
	// other repo
	if _, ok := GitHubLocal(cfg, "https://github.com/other/velruna/blob/master/README.md"); ok {
		t.Error("other owner must not hit")
	}
	// no roots configured
	if _, ok := GitHubLocal(&config.Config{}, "https://github.com/NemoNobody/velruna/blob/master/README.md"); ok {
		t.Error("empty roots must not hit")
	}
}

func TestFindLocalRepoDepth(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "velruna")
	newLocalRepo(t, deep, "master", "https://github.com/NemoNobody/velruna.git", "README.md")

	if _, ok := FindLocalRepo(&config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 2}, "NemoNobody", "velruna"); ok {
		t.Error("depth 2 must not find a repo at depth 3")
	}
	dir, ok := FindLocalRepo(&config.Config{LocalRepoRoots: []string{root}, LocalRepoScanDepth: 3}, "NemoNobody", "velruna")
	if !ok || dir != deep {
		t.Errorf("depth 3: got %q ok=%v, want %q", dir, ok, deep)
	}
}
