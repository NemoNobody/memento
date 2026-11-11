//go:build linux

package keyringnative

import (
	"errors"
	"fmt"
	"os"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

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
