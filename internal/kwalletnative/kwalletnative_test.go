//go:build linux

package kwalletnative

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

// TestWalletNameFromEnv: an explicit wallet name wins and never touches the
// daemon (the client here has no connection at all).
func TestWalletNameFromEnv(t *testing.T) {
	t.Setenv("MEMENTO_KWALLET_NAME", "wallet-x")
	c := &client{}
	if got := c.walletName(context.Background()); got != "wallet-x" {
		t.Errorf("walletName = %q, want wallet-x", got)
	}
}

// TestWalletNameFromEnvTrimmed: kwalletd would treat " wallet-x" as a different,
// brand-new wallet, so the override is trimmed.
func TestWalletNameFromEnvTrimmed(t *testing.T) {
	t.Setenv("MEMENTO_KWALLET_NAME", "  wallet-x\t")
	c := &client{}
	if got := c.walletName(context.Background()); got != "wallet-x" {
		t.Errorf("walletName = %q, want wallet-x", got)
	}
}

// TestErrNotFoundIdentity: the dispatcher maps this sentinel, so it must stay a
// plain comparable error.
func TestErrNotFoundIdentity(t *testing.T) {
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatal("ErrNotFound does not match itself")
	}
}

// TestSetRejectsEmptySecret: an empty value reads back as "not found", so the
// write must not look successful.
func TestSetRejectsEmptySecret(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")
	if err := Set("service", "user", ""); err == nil {
		t.Fatal("Set accepted an empty secret")
	}
}

func TestBusUsable(t *testing.T) {
	t.Run("explicit address", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
		if err := busUsable(); err != nil {
			t.Fatalf("busUsable: %v", err)
		}
	})
	t.Run("autolaunch is refused", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "autolaunch:")
		if err := busUsable(); err == nil {
			t.Fatal("autolaunch: accepted; godbus would try to spawn a bus")
		}
	})
	t.Run("discovered socket is accepted", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		t.Setenv("XDG_RUNTIME_DIR", dir)
		l, err := net.Listen("unix", filepath.Join(dir, "bus"))
		if err != nil {
			t.Skipf("unix sockets unavailable: %v", err)
		}
		defer func() { _ = l.Close() }()
		if err := busUsable(); err != nil {
			t.Fatalf("busUsable with a live socket: %v", err)
		}
	})
	t.Run("no address and no socket", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		if err := busUsable(); err == nil {
			t.Fatal("missing bus accepted")
		}
	})
}

// fakeObj records D-Bus calls and fails them, so a helper stops right after the
// call it made. It is what catches the argument-order class of bugs that made
// an earlier iteration look like a corrupted wallet: the wallet name sent where
// kwalletd expects the appId.
type fakeObj struct {
	calls []recordedCall
}

type recordedCall struct {
	method string
	args   []any
}

var errFakeStop = errors.New("fake: stop after recording")

func (f *fakeObj) rec(method string, args []any) *dbus.Call {
	f.calls = append(f.calls, recordedCall{method: method, args: args})
	return &dbus.Call{Err: errFakeStop}
}

func (f *fakeObj) Call(m string, _ dbus.Flags, a ...any) *dbus.Call { return f.rec(m, a) }
func (f *fakeObj) CallWithContext(_ context.Context, m string, _ dbus.Flags, a ...any) *dbus.Call {
	return f.rec(m, a)
}
func (f *fakeObj) Go(m string, _ dbus.Flags, _ chan *dbus.Call, a ...any) *dbus.Call {
	return f.rec(m, a)
}
func (f *fakeObj) GoWithContext(_ context.Context, m string, _ dbus.Flags, _ chan *dbus.Call, a ...any) *dbus.Call {
	return f.rec(m, a)
}
func (f *fakeObj) AddMatchSignal(string, string, ...dbus.MatchOption) *dbus.Call {
	return &dbus.Call{Err: errFakeStop}
}
func (f *fakeObj) RemoveMatchSignal(string, string, ...dbus.MatchOption) *dbus.Call {
	return &dbus.Call{Err: errFakeStop}
}
func (f *fakeObj) GetProperty(string) (dbus.Variant, error) { return dbus.Variant{}, errFakeStop }
func (f *fakeObj) StoreProperty(string, any) error          { return errFakeStop }
func (f *fakeObj) SetProperty(string, any) error            { return errFakeStop }
func (f *fakeObj) Destination() string                      { return busName }
func (f *fakeObj) Path() dbus.ObjectPath                    { return objPath }

// TestWalletCallsPassAppIDLast pins the shape of every wallet call: the handle
// first, the appId last, and the wallet name nowhere. It is the regression test
// for the bug that broke writes against a real kwalletd.
func TestWalletCallsPassAppIDLast(t *testing.T) {
	f := &fakeObj{}
	c := &client{obj: f}
	ctx := context.Background()
	h := int32(7)

	_, _ = c.hasFolder(ctx, h, "svc")
	_ = c.createFolder(ctx, h, "svc")
	_, _ = c.hasEntry(ctx, h, "svc", "user")
	_, _ = c.sessionOpen(ctx, h)
	var s string
	_ = c.call(ctx, "readPassword", h, "svc", "user", appID).Store(&s)
	var n int32
	_ = c.call(ctx, "writePassword", h, "svc", "user", "value", appID).Store(&n)
	_ = c.call(ctx, "removeEntry", h, "svc", "user", appID).Store(&n)

	wantMethods := []string{"hasFolder", "createFolder", "hasEntry", "isOpen", "readPassword", "writePassword", "removeEntry"}
	if len(f.calls) != len(wantMethods) {
		t.Fatalf("recorded %d calls, want %d: %+v", len(f.calls), len(wantMethods), f.calls)
	}
	for i, want := range wantMethods {
		got := f.calls[i]
		if got.method != iface+"."+want {
			t.Errorf("call %d: method %q, want %q", i, got.method, iface+"."+want)
		}
		if len(got.args) == 0 || got.args[0] != int32(7) {
			t.Errorf("call %d (%s): first arg %v, want the handle", i, want, got.args)
		}
		for _, a := range got.args {
			if str, ok := a.(string); ok && str == defaultWallet {
				t.Errorf("call %d (%s): the wallet name is sent as an argument (%v); kwalletd expects the appId", i, want, got.args)
			}
		}
		if want == "isOpen" {
			if len(got.args) != 1 {
				t.Errorf("isOpen args = %v, want only the handle", got.args)
			}
			continue
		}
		if last := got.args[len(got.args)-1]; last != appID {
			t.Errorf("call %d (%s): last arg = %v, want appID %q", i, want, last, appID)
		}
	}
}
