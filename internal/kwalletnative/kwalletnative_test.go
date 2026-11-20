//go:build linux

package kwalletnative

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// TestErrNotFoundIdentity is covered by the dispatcher tests: this package's
// sentinel is mapped there (internal/keyring), not here.

// TestSetRejectsEmptySecret: an empty value reads back as "not found", so the
// write must be refused before any wallet call.
func TestSetRejectsEmptySecret(t *testing.T) {
	orig := dial
	dial = func() (*client, error) {
		t.Fatal("Set must reject an empty secret before touching the wallet")
		return nil, errors.New("unreachable")
	}
	t.Cleanup(func() { dial = orig })

	if err := Set("service", "user", ""); err == nil {
		t.Fatal("Set accepted an empty secret")
	}
}

func TestBusUsable(t *testing.T) {
	withDiscovery := func(t *testing.T, discovered bool) {
		t.Helper()
		orig := busDiscovered
		busDiscovered = func() bool { return discovered }
		t.Cleanup(func() { busDiscovered = orig })
	}

	t.Run("explicit address", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
		withDiscovery(t, false)
		if err := busUsable(); err != nil {
			t.Fatalf("busUsable: %v", err)
		}
	})
	t.Run("discovered bus without the variable", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		withDiscovery(t, true)
		if err := busUsable(); err != nil {
			t.Fatalf("busUsable with a discovered bus: %v", err)
		}
	})
	t.Run("autolaunch is refused when nothing is discovered", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "autolaunch:")
		withDiscovery(t, false)
		if err := busUsable(); err == nil {
			t.Fatal("autolaunch: accepted; godbus would try to spawn a bus")
		}
	})
	t.Run("no address and no discovery", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		withDiscovery(t, false)
		if err := busUsable(); err == nil {
			t.Fatal("missing bus accepted")
		}
	})
}

// fakeObj records D-Bus calls and answers them from reply, so a test can drive
// Get/Set/Delete end to end and compare the exact argument list of every call.
// It is what catches the argument-order class of bugs that made an earlier
// iteration look like a corrupted wallet.
type fakeObj struct {
	calls []recordedCall
	reply func(method string, args []any) ([]any, error)
}

type recordedCall struct {
	method string
	args   []any
}

var errFakeStop = errors.New("fake: no reply configured")

func (f *fakeObj) rec(method string, args []any) *dbus.Call {
	f.calls = append(f.calls, recordedCall{method: method, args: args})
	if f.reply == nil {
		return &dbus.Call{Err: errFakeStop}
	}
	body, err := f.reply(method, args)
	return &dbus.Call{Body: body, Err: err}
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

// withFake makes dial return a client backed by f for the duration of a test.
// MEMENTO_KWALLET_NAME is cleared so a developer's shell cannot change which
// calls the test expects.
func withFake(t *testing.T, f *fakeObj) {
	t.Helper()
	t.Setenv("MEMENTO_KWALLET_NAME", "")
	orig := dial
	dial = func() (*client, error) { return &client{obj: f}, nil }
	t.Cleanup(func() { dial = orig })
}

// replies maps a method to its canned answer; an unlisted method fails the test.
func replies(t *testing.T, table map[string]any) func(string, []any) ([]any, error) {
	t.Helper()
	return func(method string, _ []any) ([]any, error) {
		body, ok := table[method]
		if !ok {
			t.Errorf("unexpected D-Bus method %q", method)
			return nil, errFakeStop
		}
		return body.([]any), nil
	}
}

// assertCalls compares the whole call list. Only method names and argument
// counts are printed: a future test may carry a real secret in an argument, and
// a failing assertion must not copy it into the log.
func assertCalls(t *testing.T, got []recordedCall, want []recordedCall) {
	t.Helper()
	describe := func(calls []recordedCall) string {
		out := make([]string, len(calls))
		for i, c := range calls {
			out[i] = fmt.Sprintf("%s(%d args)", c.method, len(c.args))
		}
		return strings.Join(out, ", ")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n got  %s\n want %s", describe(got), describe(want))
	}
}

// TestGetCallShape pins every argument of a miss: the handle first, the appId
// last, and the middle arguments in their documented order.
func TestGetCallShape(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet": []any{"kdewallet"},
		iface + ".open":        []any{int32(7)},
		iface + ".hasEntry":    []any{false},
		iface + ".isOpen":      []any{true},
	})}
	withFake(t, f)

	if _, err := Get("svc", "user"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
	assertCalls(t, f.calls, []recordedCall{
		{iface + ".localWallet", nil},
		{iface + ".open", []any{"kdewallet", int64(0), appID}},
		{iface + ".hasEntry", []any{int32(7), "svc", "user", appID}},
		{iface + ".isOpen", []any{int32(7)}},
	})
}

// TestGetReturnsValue covers the hit path (no isOpen call).
func TestGetReturnsValue(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet":  []any{"kdewallet"},
		iface + ".open":         []any{int32(7)},
		iface + ".hasEntry":     []any{true},
		iface + ".readPassword": []any{"v1"},
	})}
	withFake(t, f)

	got, err := Get("svc", "user")
	if err != nil || got != "v1" {
		t.Fatalf("Get = (%q, %v), want (v1, nil)", got, err)
	}
	assertCalls(t, f.calls, []recordedCall{
		{iface + ".localWallet", nil},
		{iface + ".open", []any{"kdewallet", int64(0), appID}},
		{iface + ".hasEntry", []any{int32(7), "svc", "user", appID}},
		{iface + ".readPassword", []any{int32(7), "svc", "user", appID}},
	})
}

// TestSetCallShape pins the write path, including the folder creation.
func TestSetCallShape(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet":   []any{"kdewallet"},
		iface + ".open":          []any{int32(7)},
		iface + ".hasFolder":     []any{false},
		iface + ".createFolder":  []any{true},
		iface + ".writePassword": []any{int32(0)},
	})}
	withFake(t, f)

	if err := Set("svc", "user", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertCalls(t, f.calls, []recordedCall{
		{iface + ".localWallet", nil},
		{iface + ".open", []any{"kdewallet", int64(0), appID}},
		{iface + ".hasFolder", []any{int32(7), "svc", appID}},
		{iface + ".createFolder", []any{int32(7), "svc", appID}},
		{iface + ".writePassword", []any{int32(7), "svc", "user", "v1", appID}},
	})
}

// TestSetRefusedCodeIsAnError: a negative return code must never look like a
// successful write.
func TestSetRefusedCodeIsAnError(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet":   []any{"kdewallet"},
		iface + ".open":          []any{int32(7)},
		iface + ".hasFolder":     []any{true},
		iface + ".writePassword": []any{int32(-1)},
	})}
	withFake(t, f)
	if err := Set("svc", "user", "v1"); err == nil {
		t.Fatal("Set reported success for return code -1")
	}
}

// TestDeleteCallShape pins the removal path.
func TestDeleteCallShape(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet": []any{"kdewallet"},
		iface + ".open":        []any{int32(7)},
		iface + ".hasEntry":    []any{true},
		iface + ".removeEntry": []any{int32(0)},
	})}
	withFake(t, f)

	if err := Delete("svc", "user"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertCalls(t, f.calls, []recordedCall{
		{iface + ".localWallet", nil},
		{iface + ".open", []any{"kdewallet", int64(0), appID}},
		{iface + ".hasEntry", []any{int32(7), "svc", "user", appID}},
		{iface + ".removeEntry", []any{int32(7), "svc", "user", appID}},
	})
}

// TestClosedSessionIsNotANotFound: a dead handle must not be reported as a
// missing secret; the caller has to retry or unlock the wallet.
func TestClosedSessionIsNotANotFound(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet": []any{"kdewallet"},
		iface + ".open":        []any{int32(7)},
		iface + ".hasEntry":    []any{false},
		iface + ".isOpen":      []any{false},
	})}
	withFake(t, f)

	_, err := Get("svc", "user")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want a session error, not ErrNotFound", err)
	}
}

// TestGetFallsBackToDefaultWalletName: when kwalletd does not name a wallet,
// the conventional name is used, and the override wins over both.
func TestGetFallsBackToDefaultWalletName(t *testing.T) {
	f := &fakeObj{reply: func(method string, _ []any) ([]any, error) {
		switch method {
		case iface + ".localWallet":
			return nil, errors.New("no such method")
		case iface + ".open":
			return []any{int32(7)}, nil
		case iface + ".hasEntry":
			return []any{false}, nil
		case iface + ".isOpen":
			return []any{true}, nil
		}
		return nil, errFakeStop
	}}
	withFake(t, f)
	_, _ = Get("svc", "user")
	if len(f.calls) < 2 || f.calls[1].method != iface+".open" {
		t.Fatalf("unexpected call list: %v", f.calls)
	}
	if got := f.calls[1].args[0]; got != defaultWallet {
		t.Errorf("open wallet = %v, want the default %q", got, defaultWallet)
	}
}

// TestOpenRefusedHandleIsAnError: a negative handle means the wallet stayed
// locked; it must surface as an error, not as an empty store.
func TestOpenRefusedHandleIsAnError(t *testing.T) {
	f := &fakeObj{reply: replies(t, map[string]any{
		iface + ".localWallet": []any{"kdewallet"},
		iface + ".open":        []any{int32(-1)},
	})}
	withFake(t, f)
	if _, err := Get("svc", "user"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want a refused-wallet error", err)
	}
}

// TestCreateFolderRefusedIsAnError: a folder that is neither created nor present
// must not be followed by a write.
func TestCreateFolderRefusedIsAnError(t *testing.T) {
	f := &fakeObj{reply: func(method string, _ []any) ([]any, error) {
		switch method {
		case iface + ".localWallet":
			return []any{"kdewallet"}, nil
		case iface + ".open":
			return []any{int32(7)}, nil
		case iface + ".hasFolder":
			return []any{false}, nil
		case iface + ".createFolder":
			return []any{false}, nil
		}
		return nil, errFakeStop
	}}
	withFake(t, f)
	if err := Set("svc", "user", "v1"); err == nil {
		t.Fatal("Set succeeded although the folder was refused")
	}
	for _, c := range f.calls {
		if c.method == iface+".writePassword" {
			t.Error("writePassword was called although the folder was refused")
		}
	}
}

// TestReadPasswordFailureIsAnError: a hit whose read fails must not look like a
// missing entry.
func TestReadPasswordFailureIsAnError(t *testing.T) {
	f := &fakeObj{reply: func(method string, _ []any) ([]any, error) {
		switch method {
		case iface + ".localWallet":
			return []any{"kdewallet"}, nil
		case iface + ".open":
			return []any{int32(7)}, nil
		case iface + ".hasEntry":
			return []any{true}, nil
		case iface + ".readPassword":
			return nil, errors.New("dbus: timed out")
		}
		return nil, errFakeStop
	}}
	withFake(t, f)
	if _, err := Get("svc", "user"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want the read failure", err)
	}
}

// TestIsOpenFailureIsNotANotFound: a failed check is a failure, never "the
// secret is absent" — that would let a broken bus look like an empty store.
func TestIsOpenFailureIsNotANotFound(t *testing.T) {
	f := &fakeObj{reply: func(method string, _ []any) ([]any, error) {
		switch method {
		case iface + ".localWallet":
			return []any{"kdewallet"}, nil
		case iface + ".open":
			return []any{int32(7)}, nil
		case iface + ".hasEntry":
			return []any{false}, nil
		case iface + ".isOpen":
			return nil, errors.New("dbus: connection closed")
		}
		return nil, errFakeStop
	}}
	withFake(t, f)

	_, err := Get("svc", "user")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want the isOpen failure to surface", err)
	}
}
