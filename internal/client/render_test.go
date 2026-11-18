package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/config"
)

// The render tests drive a /bin/sh stub that plays the obscura CLI: argv is
// recorded (the flag contract is part of the design), and the dump file is
// written per STUB_MODE. Windows has no /bin/sh — the suite skips there
// (the render path is a linux x86_64 feature by design).

func writeObscuraStub(t *testing.T, logPath string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("render stub needs /bin/sh")
	}
	stub := filepath.Join(t.TempDir(), "obscura-stub.sh")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
out=""; prev=""
for a in "$@"; do
	if [ "$prev" = "-o" ]; then out="$a"; fi
	prev="$a"
done
case "$STUB_MODE" in
	fail) echo "boom: this build rejects --stealth" >&2; exit 3 ;;
	empty) : > "$out" ;;
	*) printf '%%s' '<html><head><title>Rendered</title></head><body><h1>Rendered</h1><p>js page</p></body></html>' > "$out" ;;
esac
exit 0
`, logPath)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("stub: %v", err)
	}
	return stub
}

func renderCfg(stub string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.FetchRender = config.RenderObscura
	cfg.FetchObscuraPath = stub
	// The stub renders run offline: the SSRF gate resolves the target before
	// spawning, and the stub suite must not depend on DNS. The gate itself is
	// unit-tested on literal addresses (TestRenderSSRFCheck); the stub path
	// runs with the documented self-hosted opt-in on.
	cfg.FetchAllowPrivate = true
	return cfg
}

func readLog(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestRenderEligible(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"network", &FetchError{Class: ClassNetwork, Err: fmt.Errorf("dial")}, true},
		{"403", &FetchError{Class: ClassStatus, Status: 403, Err: fmt.Errorf("http")}, true},
		{"422", &FetchError{Class: ClassStatus, Status: 422, Err: fmt.Errorf("http")}, true},
		{"429", &FetchError{Class: ClassStatus, Status: 429, Err: fmt.Errorf("http")}, true},
		{"404", &FetchError{Class: ClassStatus, Status: 404, Err: fmt.Errorf("http")}, false},
		{"401", &FetchError{Class: ClassStatus, Status: 401, Err: fmt.Errorf("http")}, false},
		{"redirect", &FetchError{Class: ClassRedirect, Err: fmt.Errorf("redir")}, false},
		{"policy", &FetchError{Class: ClassPolicy, Err: fmt.Errorf("allowlist")}, false},
		{"oversize", &FetchError{Class: ClassOversize, Err: fmt.Errorf("big")}, false},
		{"wrapped", fmt.Errorf("fetch x: %w", &FetchError{Class: ClassNetwork, Err: fmt.Errorf("dns")}), true},
		{"plain", fmt.Errorf("some error"), false},
	}
	for _, tc := range cases {
		if got := renderEligible(tc.err); got != tc.want {
			t.Errorf("%s: renderEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRenderObscuraStub(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)
	t.Setenv("STUB_MODE", "ok")

	html, err := renderObscura(context.Background(), renderCfg(stub), "https://example.com/docs")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "<h1>Rendered</h1>") {
		t.Errorf("dump = %q", html)
	}
	argv := readLog(t, logPath)
	if !strings.Contains(argv, "--stealth") || !strings.Contains(argv, "fetch https://example.com/docs --dump html --quiet") {
		t.Errorf("argv contract: %s", argv)
	}
}

func TestRenderObscuraStealthOff(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)
	t.Setenv("STUB_MODE", "ok")

	cfg := renderCfg(stub)
	cfg.FetchObscuraStealth = false
	if _, err := renderObscura(context.Background(), cfg, "https://example.com/x"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(readLog(t, logPath), "--stealth") {
		t.Error("--stealth passed with fetch_obscura_stealth=false")
	}
}

func TestRenderObscuraProxyPolicy(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)
	t.Setenv("STUB_MODE", "ok")

	// A forwardable proxy is passed to the renderer (the egress the operator
	// chose is kept, not bypassed).
	cfg := renderCfg(stub)
	cfg.FetchProxyURL = "http://proxy.internal:3128"
	if _, err := renderObscura(context.Background(), cfg, "https://example.com/x"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(readLog(t, logPath), "--proxy http://proxy.internal:3128") {
		t.Errorf("proxy not forwarded: %s", readLog(t, logPath))
	}

	// A credential proxy is NEVER forwarded (argv is visible in /proc) — and
	// the renderer must NOT run direct behind it: the render fails loudly.
	logPath2 := filepath.Join(dir, "argv2.log")
	stub2 := writeObscuraStub(t, logPath2)
	cfg2 := renderCfg(stub2)
	cfg2.FetchProxyURL = "http://user:sekret@proxy.internal:3128"
	_, err := renderObscura(context.Background(), cfg2, "https://example.com/x")
	if err == nil || !strings.Contains(err.Error(), "refusing to render direct") {
		t.Errorf("credential proxy must fail the render loudly: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "sekret") {
		t.Errorf("the credential proxy URL leaked into the error: %v", err)
	}
	if readLog(t, logPath2) != "" {
		t.Error("the renderer ran behind an unforwardable proxy")
	}

	// Scheme-less proxy values fail the same way (they are network-class
	// errors at fetch time, which is exactly how they used to leak into
	// direct render egress).
	cfg3 := renderCfg(stub2)
	cfg3.FetchProxyURL = "proxy.internal:3128"
	if _, err := renderObscura(context.Background(), cfg3, "https://example.com/x"); err == nil {
		t.Error("scheme-less proxy must fail the render, not bypass it")
	}
}

func TestRenderObscuraFailures(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)

	// Non-zero exit: the stderr tail rides into the error (the operator
	// must see why the renderer refused — e.g. a plain build rejecting
	// --stealth).
	t.Setenv("STUB_MODE", "fail")
	if _, err := renderObscura(context.Background(), renderCfg(stub), "https://example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "rejects --stealth") {
		t.Errorf("exit failure must carry stderr: %v", err)
	}

	// Exit 0 with an empty dump is a failed render, not an empty page.
	t.Setenv("STUB_MODE", "empty")
	if _, err := renderObscura(context.Background(), renderCfg(stub), "https://example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "empty dump") {
		t.Errorf("empty dump must be a failure: %v", err)
	}

	// Missing binary: the message names the install path forward.
	if _, err := renderObscura(context.Background(), renderCfg("/nonexistent/obscura"), "https://example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "make obscura") {
		t.Errorf("missing binary message: %v", err)
	}
}

// TestFetchPageRenderChain: the plain fetch hits the 403 wall, the renderer
// (stub) gets through, and the rendered HTML flows through the normal
// extract pipeline with Via=obscura and final_url=normalized (documented
// v1 limit: the dump mode does not report the post-redirect URL).
func TestFetchPageRenderChain(t *testing.T) {
	ft := newFetchTarget(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)
	t.Setenv("STUB_MODE", "ok")

	cfg := renderCfg(stub)
	cfg.FetchAllowPrivate = true

	doc, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/blocked", nil)
	if err != nil {
		t.Fatalf("render chain: %v", err)
	}
	if doc.Via != ViaObscura {
		t.Errorf("via = %q, want obscura", doc.Via)
	}
	if doc.Title != "Rendered" || len(doc.Sections) == 0 {
		t.Errorf("rendered doc not extracted: %+v", doc)
	}

	// 404 is NOT render-eligible: the renderer is not invoked at all.
	logPath2 := filepath.Join(dir, "argv2.log")
	stub2 := writeObscuraStub(t, logPath2)
	cfg2 := renderCfg(stub2)
	cfg2.FetchAllowPrivate = true
	if _, err := FetchPage(context.Background(), cfg2, ft.srv.URL+"/missing", nil); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404 must stay final: %v", err)
	}
	if readLog(t, logPath2) != "" {
		t.Error("renderer invoked for a non-eligible failure")
	}

	// Render failure surfaces BOTH reasons.
	t.Setenv("STUB_MODE", "fail")
	cfg3 := renderCfg(stub)
	cfg3.FetchAllowPrivate = true
	_, err = FetchPage(context.Background(), cfg3, ft.srv.URL+"/blocked", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "rejects --stealth") {
		t.Errorf("both reasons must surface: %v", err)
	}
}

// TestFetchPageRenderOff: with fetch_render=off (the default) a 403 is the
// plain error, and the renderer is never invoked.
func TestFetchPageRenderOff(t *testing.T) {
	ft := newFetchTarget(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)
	t.Setenv("STUB_MODE", "ok")

	cfg := renderCfg(stub)
	cfg.FetchRender = config.RenderOff
	cfg.FetchAllowPrivate = true

	if _, err := FetchPage(context.Background(), cfg, ft.srv.URL+"/blocked", nil); err == nil ||
		!strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("render off: 403 must be plain: %v", err)
	}
	if readLog(t, logPath) != "" {
		t.Error("renderer invoked with fetch_render=off")
	}
}

// TestRenderSSRFCheck pins the render path's SSRF gate: the plain fetch
// refuses non-public dial addresses, and a host refused that way is
// render-ELIGIBLE — obscura must not become the bypass (DNS rebinding, a
// redirect target, a literal internal address).
func TestRenderSSRFCheck(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.FetchRender = config.RenderObscura
	blocked := []string{
		"http://127.0.0.1/x",
		"http://localhost:8080/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/x",
		"http://192.168.1.50:8080/",
		"http://[::1]/x",
		"http://app.internal/x",
	}
	for _, u := range blocked {
		if err := renderSSRFCheck(context.Background(), cfg, u); err == nil {
			t.Errorf("renderSSRFCheck(%q) = nil, want refusal", u)
		}
	}
	// The documented self-hosted opt-in opens the gate (fetch_allow_private).
	priv := config.DefaultConfig()
	priv.FetchAllowPrivate = true
	for _, u := range []string{"http://127.0.0.1/x", "http://localhost:8080/"} {
		if err := renderSSRFCheck(context.Background(), priv, u); err != nil {
			t.Errorf("fetch_allow_private must open the gate for %q: %v", u, err)
		}
	}
}

// TestRenderObscuraSSRFGateIntegration: the gate runs before the spawn — a
// blocked target never reaches the renderer at all.
func TestRenderObscuraSSRFGateIntegration(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := writeObscuraStub(t, logPath)
	t.Setenv("STUB_MODE", "ok")
	cfg := renderCfg(stub)
	cfg.FetchAllowPrivate = false // gate on: the target is checked, not stubbed
	if _, err := renderObscura(context.Background(), cfg, "http://127.0.0.1/admin"); err == nil {
		t.Fatal("the renderer ran on a loopback target")
	} else if !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("want the SSRF refusal, got: %v", err)
	}
	if readLog(t, logPath) != "" {
		t.Error("obscura was spawned for a blocked target")
	}
}

// TestRenderEgressProxy pins the egress contract: the explicit proxy wins,
// the environment proxy is the operator's intent and is forwarded, a proxy
// that cannot be forwarded fails the render, and no proxy means direct
// (the caller gates the route).
func TestRenderEgressProxy(t *testing.T) {
	cfg := config.DefaultConfig()
	fixed := func(want string) func(*http.Request) (*url.URL, error) {
		return func(*http.Request) (*url.URL, error) {
			if want == "" {
				return nil, nil
			}
			u, err := url.Parse(want)
			return u, err
		}
	}

	// Explicit wins over env.
	cfg.FetchProxyURL = "http://explicit:3128"
	p, err := renderEgressProxyWith(cfg, "https://example.com/x", fixed("http://env:3128"))
	if err != nil || p != "http://explicit:3128" {
		t.Errorf("explicit proxy: %q %v", p, err)
	}
	cfg.FetchProxyURL = ""

	// The env proxy is forwarded (the plain transport honored it; the render
	// must not run direct behind it).
	p, err = renderEgressProxyWith(cfg, "https://example.com/x", fixed("http://env:3128"))
	if err != nil || p != "http://env:3128" {
		t.Errorf("env proxy: %q %v", p, err)
	}

	// NO_PROXY coverage: the env decision says "direct for this target".
	p, err = renderEgressProxyWith(cfg, "https://example.com/x", fixed(""))
	if err != nil || p != "" {
		t.Errorf("no proxy: %q %v", p, err)
	}

	// A credential proxy is never forwarded to argv — and the render fails
	// rather than going direct behind it.
	if _, err := renderEgressProxyWith(cfg, "https://example.com/x", fixed("http://user:sekret@env:3128")); err == nil {
		t.Error("a credential env proxy must fail the render")
	} else if strings.Contains(err.Error(), "sekret") {
		t.Errorf("the proxy URL leaked into the error: %v", err)
	}
}

// TestRenderSlotIsCtxAware pins the semaphore: a caller that already gave up
// must not queue behind a busy renderer (the render window is minutes; the
// queue is cancelable).
func TestRenderSlotIsCtxAware(t *testing.T) {
	stub := writeObscuraStub(t, filepath.Join(t.TempDir(), "argv.log"))
	cfg := renderCfg(stub)
	obscuraSem <- struct{}{} // the slot is busy
	defer func() { <-obscuraSem }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := renderObscura(ctx, cfg, "https://example.com/x")
	if err == nil || !strings.Contains(err.Error(), "render slot") {
		t.Fatalf("want the slot-wait refusal, got: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("the queue ignored the cancelled context (%v)", time.Since(start))
	}
}

// TestRenderEgressEnvProxyHardening pins the struct-level env-proxy
// validation: Go 1.27's proxy machinery normalizes a malformed env proxy so
// the credentials hide in the path of String(); a string-level re-validation
// waves that through and the secret lands in obscura's argv (/proc-visible).
func TestRenderEgressEnvProxyHardening(t *testing.T) {
	cfg := config.DefaultConfig()

	// The classic credential form: the parsed struct carries User.
	cred := func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", User: url.UserPassword("user", "sekret"), Host: "env:3128"}, nil
	}
	if _, err := renderEgressProxyWith(cfg, "https://example.com/x", cred); err == nil {
		t.Error("env proxy with user info must fail the render")
	} else if strings.Contains(err.Error(), "sekret") {
		t.Errorf("credential leaked into the message: %v", err)
	}

	// The normalized-mangle shape: User==nil, the credential hides in the
	// path. A string re-parse passes every field check; the '@' defense must
	// refuse it.
	mangled := func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: "http", Path: "//user:sekret@p:3128;"}, nil
	}
	if _, err := renderEgressProxyWith(cfg, "https://example.com/x", mangled); err == nil {
		t.Error("normalized mangle with '@' in the path must fail the render")
	} else if strings.Contains(err.Error(), "sekret") {
		t.Errorf("credential leaked into the message: %v", err)
	}

	// A well-formed socks5 env proxy is forwarded verbatim (obscura must
	// support the scheme; documented in docs/setup.md).
	socks := func(*http.Request) (*url.URL, error) {
		return url.Parse("socks5://env:1080")
	}
	p, err := renderEgressProxyWith(cfg, "https://example.com/x", socks)
	if err != nil || p != "socks5://env:1080" {
		t.Errorf("socks5 env proxy: %q %v", p, err)
	}

	// The env-parser error text embeds the offending spec (credentials
	// possible): the message is fixed and does not echo it.
	loud := func(*http.Request) (*url.URL, error) {
		return nil, errors.New(`proxy file line "http://user:sekret@p:3128" is invalid`)
	}
	if _, err := renderEgressProxyWith(cfg, "https://example.com/x", loud); err == nil {
		t.Error("env proxy parse error must fail the render")
	} else if strings.Contains(err.Error(), "sekret") {
		t.Errorf("raw env value echoed into the message: %v", err)
	}
}

// TestRenderSSRFCheckEmptyHost pins the empty-host refusal; the SSRF gate
// runs before obscura spawns, so a URL with no host must never reach it.
func TestRenderSSRFCheckEmptyHost(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.FetchRender = config.RenderObscura
	if err := renderSSRFCheck(context.Background(), cfg, "http://:8080/x"); err == nil {
		t.Error("empty host must be refused")
	}
}

// TestBlockedInList pins the "one blocked answer is enough" rule with
// literal addresses (no DNS): a hostname answering both a public and a
// non-public A record — the DNS-rebinding shape — must refuse.
func TestBlockedInList(t *testing.T) {
	cases := []struct {
		name  string
		ips   []net.IPAddr
		want  bool
		badIP string
	}{
		{"all public", []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, false, ""},
		{"public plus loopback", []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("127.0.0.1")}}, true, "127.0.0.1"},
		{"metadata endpoint", []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, true, "169.254.169.254"},
		{"loopback v6", []net.IPAddr{{IP: net.ParseIP("::1")}}, true, "::1"},
		{"empty", nil, false, ""},
	}
	for _, tc := range cases {
		bad, blocked := blockedInList(tc.ips)
		if blocked != tc.want {
			t.Errorf("%s: blocked = %v, want %v", tc.name, blocked, tc.want)
		}
		if tc.want && (bad == nil || bad.String() != tc.badIP) {
			t.Errorf("%s: reported address = %v, want %s", tc.name, bad, tc.badIP)
		}
	}
}

// TestRenderProxyAtInvariant pins the strict argv invariant: no '@' reaches
// the renderer argv from EITHER branch — not from a config proxy with an
// '@' in the path, nor from either branch with a '%40'-encoded credential
// (it decodes to '@' inside the Path). The check is symmetric: the same
// decoded-form rule guards the config string and the env struct.
func TestRenderProxyAtInvariant(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.FetchProxyURL = "http://proxy.corp:3128/tok=sk_live_51Hb@x"
	noEnv := func(*http.Request) (*url.URL, error) { return nil, nil }
	if _, err := renderEgressProxyWith(cfg, "https://example.com/x", noEnv); err == nil {
		t.Error("config proxy with '@' in the path must be refused (argv is visible in /proc)")
	}
	// The config branch must catch the encoded form too (the round-4 fix
	// covered env only — the same artifact, the same invariant).
	cfg2 := config.DefaultConfig()
	cfg2.FetchProxyURL = "http://p:3128/k%40user:sekret"
	if _, err := renderEgressProxyWith(cfg2, "https://example.com/x", noEnv); err == nil {
		t.Error("'%40'-encoded config proxy credential must be refused")
	}
	pct := func(*http.Request) (*url.URL, error) {
		return url.Parse("http://p:3128/k%40user:sekret")
	}
	if _, err := renderEgressProxyWith(config.DefaultConfig(), "https://example.com/x", pct); err == nil {
		t.Error("'%40'-encoded env proxy credential must be refused")
	}
	// The invariant must not become over-blocking: a plain path segment is
	// a legitimate proxy base path.
	cfg3 := config.DefaultConfig()
	cfg3.FetchProxyURL = "http://proxy.corp:3128/corp-base"
	got, err := renderEgressProxyWith(cfg3, "https://example.com/x", noEnv)
	if err != nil || got != cfg3.FetchProxyURL {
		t.Errorf("clean config proxy: got %q, err %v; want it forwarded", got, err)
	}
}
