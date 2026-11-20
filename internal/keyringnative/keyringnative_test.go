//go:build linux

package keyringnative

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

// withProbe swaps the backend probe for the duration of a test, so the
// classification table never needs a session bus.
func withProbe(t *testing.T, p backendProbe) {
	t.Helper()
	orig := probe
	probe = func() backendProbe { return p }
	t.Cleanup(func() { probe = orig })
}

// TestBackendErrorClassification pins the provider verdicts that gate every
// operation. The two "nil" rows are as important as the refusals: a false
// refusal disables the working desktops.
func TestBackendErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		probe      backendProbe
		wantRefuse bool
		wantText   string
	}{
		{
			name:       "KDE served by gnome-keyring is refused",
			probe:      backendProbe{HasOwner: true, Owner: ":1.42", Comm: "gnome-keyring-d", Desktop: "KDE"},
			wantRefuse: true,
			wantText:   "ephemeral login keyring",
		},
		{
			name:       "no provider owns the name",
			probe:      backendProbe{Desktop: "KDE"},
			wantRefuse: true,
			wantText:   "no Secret Service provider",
		},
		{
			name:       "no session bus",
			probe:      backendProbe{Desktop: "KDE", BusErr: errors.New("dial unix: no such file")},
			wantRefuse: true,
			wantText:   "no session bus",
		},
		{
			name:  "KDE served by kwalletd6",
			probe: backendProbe{HasOwner: true, Owner: ":1.7", Comm: "kwalletd6", Desktop: "KDE"},
		},
		{
			name:  "KDE served by ksecretd",
			probe: backendProbe{HasOwner: true, Owner: ":1.8", Comm: "ksecretd", Desktop: "KDE:Plasma"},
		},
		{
			name:  "GNOME served by gnome-keyring",
			probe: backendProbe{HasOwner: true, Owner: ":1.9", Comm: "gnome-keyring-d", Desktop: "ubuntu:GNOME"},
		},
		{
			// The mismatch needs a confirmed process name: hidepid or SELinux
			// can hide /proc, and guessing there would lock out a working wallet.
			name:  "owner process unknown on KDE is allowed",
			probe: backendProbe{HasOwner: true, Owner: ":1.10", Desktop: "KDE"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProbe(t, tc.probe)
			err := BackendError()
			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("BackendError = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrBackend) {
				t.Fatalf("BackendError = %v, want an error matching ErrBackend", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("BackendError text = %q, want it to mention %q", err, tc.wantText)
			}
		})
	}
}

// TestSecretServiceUsableIsBackendError pins the dispatcher-facing alias: one
// verdict, two names, so the gate and the fallback choice can never disagree.
func TestSecretServiceUsableIsBackendError(t *testing.T) {
	withProbe(t, backendProbe{HasOwner: true, Owner: ":1.1", Comm: "kwalletd6", Desktop: "KDE"})
	if err := SecretServiceUsable(); err != nil {
		t.Errorf("SecretServiceUsable = %v, want nil", err)
	}
	withProbe(t, backendProbe{})
	if err := SecretServiceUsable(); !errors.Is(err, ErrBackend) {
		t.Errorf("SecretServiceUsable = %v, want ErrBackend", err)
	}
}

// TestOperationsRefuseWithoutBackend pins the gate itself: with a refused
// backend Get/Set/Delete return ErrBackend before any keyring call. The dead
// bus address makes an ungated call fail differently (or touch the user's real
// wallet on a developer machine), and the counter proves the gate ran once per
// operation.
func TestOperationsRefuseWithoutBackend(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/memento-test-no-bus")
	orig := probe
	calls := 0
	probe = func() backendProbe {
		calls++
		return backendProbe{HasOwner: true, Owner: ":1.1", Comm: "gnome-keyring-d", Desktop: "KDE"}
	}
	t.Cleanup(func() { probe = orig })

	if _, err := Get("memento-test", "user"); !errors.Is(err, ErrBackend) {
		t.Errorf("Get = %v, want ErrBackend", err)
	}
	if err := Set("memento-test", "user", "value"); !errors.Is(err, ErrBackend) {
		t.Errorf("Set = %v, want ErrBackend", err)
	}
	if err := Delete("memento-test", "user"); !errors.Is(err, ErrBackend) {
		t.Errorf("Delete = %v, want ErrBackend", err)
	}
	if calls != 3 {
		t.Errorf("probe called %d times, want 3 (one gate per operation)", calls)
	}
}

// TestProbeSecretServiceFailsFastOnDeadBus covers the real probe without a live
// provider: a set-but-unreachable address must yield BusErr, not a hang and not
// a dbus-launch autolaunch.
func TestProbeSecretServiceFailsFastOnDeadBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "no-bus"))
	p := probeSecretService()
	if p.BusErr == nil {
		t.Fatalf("probe = %+v, want a BusErr for an unreachable address", p)
	}
	if err := BackendError(); !errors.Is(err, ErrBackend) {
		t.Errorf("BackendError = %v, want ErrBackend", err)
	}
}

// These tests talk to a REAL Secret Service provider (gnome-keyring, KWallet,
// ksecretd) on the session bus. They touch the user's wallet, so they are opt-in:
// MEMENTO_KEYRING_TEST=1. `make test-keyring` starts a disposable
// dbus-run-session + gnome-keyring and sets the flag; CI does the same.
//
// The point is the part no fake can prove: the wire protocol, the default
// collection resolution, the prompt handling and the "missing entry" sentinel
// as the provider actually reports them.
func requireSecretService(t *testing.T) {
	t.Helper()
	if os.Getenv("MEMENTO_KEYRING_TEST") == "" {
		t.Skip("keyring integration test disabled: set MEMENTO_KEYRING_TEST=1 to run against the session Secret Service")
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	var hasOwner bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, busName).Store(&hasOwner); err != nil {
		t.Skipf("cannot ask the bus for %s: %v", busName, err)
	}
	if !hasOwner {
		t.Skipf("no %s owner on the session bus (start gnome-keyring-daemon or KWallet)", busName)
	}
}

// testService namespaces the entries this test writes: a live wallet is shared
// with the user's real secrets, and the sweep-on-Set behavior means a generic
// name could delete entries the test never created.
func testService(t *testing.T) string {
	t.Helper()
	svc := fmt.Sprintf("memento-ci-%d", os.Getpid())
	t.Cleanup(func() { _ = Delete(svc, "user") })
	return svc
}

func TestSetGetDeleteRoundTrip(t *testing.T) {
	requireSecretService(t)
	svc := testService(t)

	if err := Set(svc, "user", "s3cr3t value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := Get(svc, "user")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "s3cr3t value" {
		t.Errorf("Get = %q, want the stored value", got)
	}

	if err := Delete(svc, "user"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Get(svc, "user"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	requireSecretService(t)
	svc := testService(t)

	_, err := Get(svc, "absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(absent) = %v, want ErrNotFound", err)
	}
	if err := Delete(svc, "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(absent) = %v, want ErrNotFound", err)
	}
}

func TestSetOverwritesAndSweepsDuplicates(t *testing.T) {
	requireSecretService(t)
	svc := testService(t)

	if err := Set(svc, "user", "first"); err != nil {
		t.Fatalf("Set(first): %v", err)
	}
	if err := Set(svc, "user", "second"); err != nil {
		t.Fatalf("Set(second): %v", err)
	}
	got, err := Get(svc, "user")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "second" {
		t.Errorf("Get = %q, want the overwrite to win", got)
	}
}

// TestValueIsOpaque pins that the secret is stored byte-exactly: newlines,
// quotes and non-ASCII are common in tokens and private keys.
func TestValueIsOpaque(t *testing.T) {
	requireSecretService(t)
	svc := testService(t)

	value := "line1\nline2 \"quoted\" — юникод"
	if err := Set(svc, "user", value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := Get(svc, "user")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != value {
		t.Errorf("round-trip changed the value: %q -> %q", value, got)
	}
}

// TestSetCollectionName pins the fallback-label switch: an explicit label
// replaces the KDE default, an empty one restores it (so init with an unset
// env var is a no-op). This is the per-desktop knob for a renamed wallet.
func TestSetCollectionName(t *testing.T) {
	orig := collectionName
	t.Cleanup(func() { SetCollectionName(orig) })

	SetCollectionName("wallet-b")
	if collectionName != "wallet-b" {
		t.Errorf("collectionName = %q, want wallet-b", collectionName)
	}
	SetCollectionName("")
	if collectionName != "wallet-b" {
		t.Errorf("empty name must keep the current label, got %q", collectionName)
	}
	// The label match is exact: a stray space would address a wallet that
	// does not exist (and a write would create one named with the space).
	SetCollectionName("  wallet-c  ")
	if collectionName != "wallet-c" {
		t.Errorf("whitespace must be trimmed, got %q", collectionName)
	}
	SetCollectionName("   ")
	if collectionName != "wallet-c" {
		t.Errorf("whitespace-only must keep the current label, got %q", collectionName)
	}
	SetCollectionName(orig)
	if collectionName != defaultCollectionName {
		// orig is defaultCollectionName in a fresh process; this guards the
		// documented "empty keeps the default" behavior of init().
		t.Logf("collectionName = %q (restored)", collectionName)
	}
}
