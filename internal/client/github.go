package client

// GitHub content URLs that point at a repository already cloned on this
// machine are served from the local clone: the content is identical, the
// read is free and offline, and nothing leaves the machine. Only the
// checked-out head (branch name or HEAD commit) is served: answering a
// different ref from the working tree would silently return wrong content,
// so any other ref falls back to a normal network fetch.
//
// The repo mapping is machine-specific by nature and is therefore derived
// from this machine's filesystem at call time; it is never stored in the
// shared index.

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/NemoNobody/memento/internal/config"
)

// ghRef is a parsed GitHub content URL: one of
// github.com/owner/repo/{blob,raw}/ref/path... or
// raw.githubusercontent.com/owner/repo/ref/path...
type ghRef struct {
	owner, repo, ref, path string
}

// parseGitHubURL recognizes the supported GitHub forms. The ref is a single
// path segment: refs containing slashes (feature branches) are not
// recognized and fall back to a network fetch (v1 limitation).
func parseGitHubURL(normalized string) (ghRef, bool) {
	u, err := url.Parse(normalized)
	if err != nil {
		return ghRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch strings.ToLower(u.Host) {
	case "raw.githubusercontent.com":
		if len(parts) < 4 {
			return ghRef{}, false
		}
		return ghRef{owner: parts[0], repo: parts[1], ref: parts[2],
			path: strings.Join(parts[3:], "/")}, true
	case "github.com":
		if len(parts) < 5 {
			return ghRef{}, false
		}
		kind := strings.ToLower(parts[2])
		if kind != "blob" && kind != "raw" {
			return ghRef{}, false
		}
		return ghRef{owner: parts[0], repo: parts[1], ref: parts[3],
			path: strings.Join(parts[4:], "/")}, true
	}
	return ghRef{}, false
}

// normalizeRemote extracts owner/repo from a git remote URL: scp-style ssh
// (git@github.com:o/r.git), ssh://, and https forms. Only github.com
// remotes are recognized.
func normalizeRemote(raw string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", "", false
		}
		return splitRepoPath(u.Path)
	}
	// scp-style: [user@]host:path
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		raw = raw[i+1:]
	}
	i := strings.IndexByte(raw, ':')
	if i < 0 || !strings.EqualFold(raw[:i], "github.com") {
		return "", "", false
	}
	return splitRepoPath(raw[i+1:])
}

func splitRepoPath(p string) (string, string, bool) {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// FindLocalRepo locates a clone of owner/repo among local_repo_roots,
// scanning down to local_repo_scan_depth: a directory with a .git whose
// origin remote resolves to the same repository.
func FindLocalRepo(cfg *config.Config, owner, repo string) (string, bool) {
	for _, root := range cfg.LocalRepoRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if dir, ok := scanRepo(root, cfg.LocalRepoScanDepth, owner, repo); ok {
			return dir, true
		}
	}
	return "", false
}

func scanRepo(root string, depth int, owner, repo string) (string, bool) {
	if depth <= 0 {
		return "", false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil && st.IsDir() {
			if o, r, ok := remoteOf(dir); ok && o == owner && r == repo {
				return dir, true
			}
		}
		// Nested trees (mono-repo checkouts, workspace dirs) are scanned
		// one level deeper.
		if sub, ok := scanRepo(dir, depth-1, owner, repo); ok {
			return sub, true
		}
	}
	return "", false
}

func remoteOf(dir string) (string, string, bool) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", false
	}
	return normalizeRemote(string(out))
}

// headRefs returns the current branch name and HEAD commit sha of a clone
// (empty when unavailable: a fresh init has a branch but no commit).
func headRefs(dir string) (branch, sha string) {
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(out))
	}
	return branch, sha
}

// GitHubLocal resolves a GitHub content URL to a file inside a local clone,
// requiring the URL ref to name the checked-out head (branch name, commit
// sha, or sha prefix >= 7). Paths escaping the repo root are rejected.
func GitHubLocal(cfg *config.Config, normalizedURL string) (string, bool) {
	ref, ok := parseGitHubURL(normalizedURL)
	if !ok {
		return "", false
	}
	dir, ok := FindLocalRepo(cfg, ref.owner, ref.repo)
	if !ok {
		return "", false
	}
	branch, sha := headRefs(dir)
	if ref.ref != branch && (len(ref.ref) < 7 || !strings.HasPrefix(sha, ref.ref)) {
		return "", false
	}
	path := filepath.Join(dir, filepath.FromSlash(ref.path))
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// The lexical check is not enough: a symlink committed into the repo
	// (README.md -> ~/.ssh/id_rsa) passes it and resolves outside the clone.
	// Evaluate symlinks and re-check containment against the resolved root.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	root := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		root = r
	}
	rel, err = filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if st, err := os.Stat(resolved); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return resolved, true
}
