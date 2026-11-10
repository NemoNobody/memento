package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// the renderer MUST go through it. The environment proxy (HTTP_PROXY,
	// honored by the plain transport) is the same operator intent, so the
	// renderer consults it too — falling back to DIRECT from the machine's
	// real IP behind the operator's back is the failure mode this path must
	// never take. A proxy that cannot be forwarded (credentials, bogus
	// scheme) is a loud error, never a silent direct render. (Config load
	// already validates fetch_proxy_url; this check re-arms the rule for
	// env proxies and Config values built in code.)
	proxyArg, err := renderEgressProxy(cfg, normalizedURL)
	if err != nil {
		return nil, fmt.Errorf("obscura: %w", err)
	}
	if proxyArg == "" {
		// Direct render: obscura resolves, dials and follows redirects on its
		// own, so the render is the SSRF gate for this route — the plain
		// fetch refused the same target at dial time (which is what made it
		// render-eligible), and obscura must not become the bypass.
		if err := renderSSRFCheck(ctx, cfg, normalizedURL); err != nil {
			return nil, fmt.Errorf("obscura: %w", err)
		}
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

// renderEgressProxy returns the proxy the renderer must use for rawURL ("")
// means "render direct, and guard the route yourself"). The explicit
// fetch_proxy_url wins; otherwise the environment proxy the plain transport
// honors (HTTP_PROXY/HTTPS_PROXY/NO_PROXY) is the operator's egress for this
// target and is forwarded to the renderer. A proxy that config.ValidateProxyURL
// refuses (credentials, scheme-less, bogus scheme) fails the render loudly:
// the renderer never runs direct behind a proxy it cannot forward, and
// argv never carries credentials (visible in /proc).
func renderEgressProxy(cfg *config.Config, rawURL string) (string, error) {
	return renderEgressProxyWith(cfg, rawURL, http.ProxyFromEnvironment)
}

// renderEgressProxyWith takes the env-proxy decision as a parameter: the
// production wiring is http.ProxyFromEnvironment (whose config is cached for
// the process lifetime), and tests inject the same decision shape. The
// contract under test is the render's, not the stdlib's.
func renderEgressProxyWith(cfg *config.Config, rawURL string, envProxy func(*http.Request) (*url.URL, error)) (string, error) {
	proxy := cfg.FetchProxyURL
	if proxy == "" {
		u, perr := url.Parse(rawURL)
		if perr != nil {
			return "", nil // NormalizeURL validated the URL; no env decision
		}
		evU, eerr := envProxy(&http.Request{Method: http.MethodGet, URL: u})
		if eerr != nil {
			// Go's proxyparse error text embeds the offending proxy spec —
			// which can carry credentials. The rule for every message that
			// reaches agent context: the raw env value is not echoed.
			return "", fmt.Errorf("the environment proxy is unreadable; refusing to render direct (check HTTP_PROXY/HTTPS_PROXY)")
		}
		if evU == nil {
			return "", nil
		}
		// Validate the STRUCT http.ProxyFromEnvironment produced, not the
		// string round-trip: Go normalizes a malformed env proxy so the
		// credentials hide in the path of a re-parse, and a string-level
		// check would wave that through into obscura's argv (/proc-visible).
		if verr := config.ValidateProxy(evU); verr != nil {
			return "", fmt.Errorf("the environment proxy %v; refusing to render direct", verr)
		}
		if strings.Contains(evU.Path, "@") {
			// Defense in depth: a normalized form can hide user info in the
			// path (and '%40' decodes to '@' in the Path of the parsed form).
			return "", fmt.Errorf("the environment proxy carries user info; refusing to render direct (argv is visible in /proc)")
		}
		proxy = evU.String()
	}
	if verr := config.ValidateProxyURL(proxy); verr != nil {
		return "", fmt.Errorf("%v; refusing to render direct", verr)
	}
	if strings.Contains(proxy, "@") {
		// The final string is what lands in the renderer argv. User info is
		// refused upstream (both branches); a literal '@' left in the path is
		// a credential-shaped artifact — the argv invariant is kept strictly:
		// no '@' reaches the subprocess, for env or config proxies.
		return "", fmt.Errorf("the proxy URL carries an '@' in its path; refusing to render direct (argv is visible in /proc)")
	}
	return proxy, nil
}

// renderSSRFCheck is the render path's SSRF gate for direct renders: the
// plain transport refuses non-public dial addresses (guardedDialContext),
// and a host refused that way fails as a network error — which makes it
// render-eligible. obscura resolves and dials on its own, so without this
// pre-check the render would be the SSRF bypass (DNS rebinding, a 302 to
// loopback, RFC1918 or 169.254.169.254). Direct renders resolve the target
// through the same blockedIP rule and refuse a non-public answer. With a
// proxy the proxy is the egress and enforces the policy (the documented
// model), so memento does not resolve the name locally.
//
// Known limitation (documented): obscura dials after this check, so a
// DNS-rebinding window between the resolution here and obscura's own dial
// remains; for a policy-enforced egress run the renderer behind a proxy.
func renderSSRFCheck(ctx context.Context, cfg *config.Config, rawURL string) error {
	if cfg.FetchAllowPrivate {
		return nil // the documented opt-in to self-hosted targets
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil // NormalizeURL validated the URL
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("refusing to render: empty host")
	}
	if blockedHost(host) {
		return fmt.Errorf("refusing to render %s: not a public address (the plain fetch refuses it at dial time)", host)
	}
	ips, rerr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if rerr != nil {
		return fmt.Errorf("refusing to render: cannot verify %s is public: %v", host, rerr)
	}
	if bad, blocked := blockedInList(ips); blocked {
		return fmt.Errorf("refusing to render %s: it resolves to the non-public address %s", host, bad)
	}
	return nil
}

// blockedInList applies the blockedIP rule to every resolved address: one
// blocked answer in the set is enough to refuse (a hostname with a public
// and a loopback A record is the DNS-rebinding shape). Extracted so the
// rule is unit-testable with literal addresses, offline.
func blockedInList(ips []net.IPAddr) (net.IP, bool) {
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return ip.IP, true
		}
	}
	return nil, false
}
