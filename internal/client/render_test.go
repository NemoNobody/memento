package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
