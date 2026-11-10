package client

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/NemoNobody/memento/internal/config"
)

// The obscura render fallback: when a plain fetch is refused the way
// anti-bot walls refuse (IP/TLS-fingerprint bans, 403/422/429, network
// walls), an external headless browser with a Chrome-shaped TLS/HTTP2
// fingerprint often gets through. It is strictly opt-in (fetch_render=
// obscura): an extra ~75MB binary with glibc requirements is the operator's
// call, never a dependency of this module.
//
// v1 limits (documented): the dump mode does not report the post-redirect
// URL, so a rendered doc stores final_url = the normalized request URL;
// obscura's exit codes are undocumented, so success is exit 0 AND a
// non-empty output file. The rendered HTML flows through the exact same
// extract + guard + store pipeline as a plain fetch.
//
// The render path is serialized (single-flight): concurrent fetches must
// not share the obscura worker/port. Render is rare (fallbacks only), so
// serialization is cheap insurance. The semaphore is ctx-aware: a caller
// that gives up releases its slot, and queued renders do not block past
// their own deadline.

// obscuraSem serializes render subprocesses across Clients (Clients are
// built per call, so the semaphore is package-level). Capacity 1.
var obscuraSem = make(chan struct{}, 1)

// cappedWriter keeps at most the first n bytes written: the renderer's
// stderr is diagnostics, not a stream to buffer.
type cappedWriter struct {
	buf [4096]byte
	n   int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.n < len(w.buf) {
		n := copy(w.buf[w.n:], p)
		w.n += n
	}
	return len(p), nil
}

func (w *cappedWriter) String() string { return string(w.buf[:w.n]) }

// renderObscura renders url through the obscura binary and returns the
// rendered HTML (capped at fetch_max_bytes). argv carries only the URL and
// a proxy without credentials; a proxy URL with userinfo is not forwarded
// (secrets do not belong in process argv, visible in /proc).
func renderObscura(ctx context.Context, cfg *config.Config, normalizedURL string) ([]byte, error) {
	bin := cfg.FetchObscuraPath
	if bin == "" {
		bin = "obscura"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("obscura: binary %q not found (install with 'make obscura' or point fetch_obscura_path at it): %w", bin, err)
	}

	// The proxy is the operator's chosen egress: when fetch_proxy_url is set,
	// the renderer MUST go through it. A value that cannot be forwarded
	// (credentials, bogus scheme) is a loud config error — rendering DIRECT
	// from the machine's real IP behind the operator's back is the failure
	// mode this path must never take. (validateProxyURL already refuses
	// userinfo and scheme-less values at config load; this check re-arms the
	// rule for callers that build Config in code.)
	proxyArg := ""
	if cfg.FetchProxyURL != "" {
		if verr := validateProxyURLForRender(cfg.FetchProxyURL); verr != nil {
			return nil, fmt.Errorf("obscura: %w", verr)
		}
		proxyArg = cfg.FetchProxyURL
	}

	select {
	case obscuraSem <- struct{}{}:
		defer func() { <-obscuraSem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("obscura: waiting for the render slot: %w", ctx.Err())
	}

	dir, err := os.MkdirTemp("", "memento-obscura-")
	if err != nil {
		return nil, fmt.Errorf("obscura: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }() // all paths, including kill/timeout
	out := filepath.Join(dir, "page.html")

	nav := cfg.FetchTimeoutSeconds
	if nav <= 0 {
		nav = 30
	}
	maxBytes := cfg.FetchMaxBytes
	if maxBytes <= 0 {
		maxBytes = 2_000_000
	}

	args := []string{}
	if cfg.FetchObscuraStealth {
		args = append(args, "--stealth")
	}
	args = append(args, "fetch", normalizedURL, "--dump", "html", "--quiet",
		"--timeout", strconv.Itoa(nav), "-o", out)
	if proxyArg != "" {
		args = append(args, "--proxy", proxyArg)
	}

	// The nav timeout is obscura's own; the Go context adds headroom for
	// startup and teardown (V8 deadlines run beyond nav).
	cctx, cancel := context.WithTimeout(ctx, time.Duration(nav+20)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	var stderr cappedWriter
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		diag := stderr.String()
		if diag == "" {
			diag = "(no stderr)"
		}
		return nil, fmt.Errorf("obscura: %v: %s", err, diag)
	}
	// Cap before the read: a hostile rendered dump must not force a
	// multi-GB allocation.
	if fi, serr := os.Stat(out); serr == nil && fi.Size() > int64(maxBytes) {
		return nil, fmt.Errorf("obscura: rendered page exceeds fetch_max_bytes (%d): too large to cache", maxBytes)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("obscura: exit 0 but no output file: %w", err)
	}
	if len(data) == 0 {
		// Exit codes are undocumented: an empty dump is a failed render,
		// not an empty page.
		return nil, fmt.Errorf("obscura: exit 0 with an empty dump (render failed)")
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("obscura: rendered page exceeds fetch_max_bytes (%d): too large to cache", maxBytes)
	}
	return data, nil
}

// validateProxyURLForRender mirrors config.validateProxyURL for Config
// values built in code (tests, embedding): the render path re-arms the
// egress rule itself instead of trusting the load pipeline.
func validateProxyURLForRender(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("fetch_proxy_url has invalid syntax (its text is not echoed); the renderer refuses to run direct behind it")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("fetch_proxy_url scheme %q is not forwardable to the renderer (want http/https/socks5/socks5h); refusing to render direct", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("fetch_proxy_url has no host; refusing to render direct")
	}
	if u.User != nil {
		return fmt.Errorf("fetch_proxy_url carries user info; a credential proxy is never forwarded to argv; refusing to render direct")
	}
	return nil
}
