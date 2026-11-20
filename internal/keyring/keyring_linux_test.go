//go:build linux

package keyring

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/keyringnative"
	"github.com/NemoNobody/memento/internal/kwalletnative"
)

// stubs replaces both backends for the duration of a test. A backend that is
// not expected to be called is left nil and its call fails the test: the point
// of these cases is which backend is chosen, not only what is returned.
type stubs struct {
	verdict error
	wallet  bool

	ssGet func(string, string) (string, error)
	ssSet func(string, string, string) error
	ssDel func(string, string) error

	wGet func(string, string) (string, error)
	wSet func(string, string, string) error
	wDel func(string, string) error
}

func use(t *testing.T, s stubs) {
	t.Helper()
	orig := []any{ssVerdict, ssGet, ssSet, ssDelete, walletOK, walletGet, walletSet, walletDel}
	t.Cleanup(func() {
		ssVerdict, ssGet, ssSet, ssDelete, walletOK, walletGet, walletSet, walletDel =
			orig[0].(func() error), orig[1].(func(string, string) (string, error)),
			orig[2].(func(string, string, string) error), orig[3].(func(string, string) error),
			orig[4].(func() bool), orig[5].(func(string, string) (string, error)),
			orig[6].(func(string, string, string) error), orig[7].(func(string, string) error)
	})
	unexpected := func(name string) { t.Errorf("%s must not be called in this case", name) }
	ssVerdict = func() error { return s.verdict }
	walletOK = func() bool { return s.wallet }
	ssGet = func(a, b string) (string, error) {
		unexpected("secret-service Get")
		return "", errors.New("unexpected")
	}
	ssSet = func(a, b, c string) error { unexpected("secret-service Set"); return errors.New("unexpected") }
	ssDelete = func(a, b string) error { unexpected("secret-service Delete"); return errors.New("unexpected") }
	walletGet = func(a, b string) (string, error) { unexpected("kwallet Get"); return "", errors.New("unexpected") }
	walletSet = func(a, b, c string) error { unexpected("kwallet Set"); return errors.New("unexpected") }
	walletDel = func(a, b string) error { unexpected("kwallet Delete"); return errors.New("unexpected") }
	if s.ssGet != nil {
		ssGet = s.ssGet
	}
	if s.ssSet != nil {
		ssSet = s.ssSet
	}
	if s.ssDel != nil {
		ssDelete = s.ssDel
	}
	if s.wGet != nil {
		walletGet = s.wGet
	}
	if s.wSet != nil {
		walletSet = s.wSet
	}
	if s.wDel != nil {
		walletDel = s.wDel
	}
}

var errRefusal = fmt.Errorf("%w: gnome-keyring on KDE writes an ephemeral login keyring", keyringnative.ErrBackend)

func TestDispatchUsesSecretServiceWhenUsable(t *testing.T) {
	use(t, stubs{
		wallet: false,
		ssGet:  func(s, u string) (string, error) { return "from-ss", nil },
	})
	if got, err := Get("svc", "user"); err != nil || got != "from-ss" {
		t.Fatalf("Get = (%q, %v), want (from-ss, nil)", got, err)
	}
}

func TestDispatchFallsBackToKWallet(t *testing.T) {
	use(t, stubs{
		verdict: errRefusal,
		wallet:  true,
		wGet:    func(s, u string) (string, error) { return "from-wallet", nil },
	})
	if got, err := Get("svc", "user"); err != nil || got != "from-wallet" {
		t.Fatalf("Get = (%q, %v), want (from-wallet, nil)", got, err)
	}
}

func TestDispatchWalletNotFoundMapsToErrNotFound(t *testing.T) {
	use(t, stubs{
		verdict: errRefusal,
		wallet:  true,
		wGet:    func(s, u string) (string, error) { return "", kwalletnative.ErrNotFound },
	})
	if _, err := Get("svc", "user"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
}

func TestDispatchWalletFailureKeepsRefusalReason(t *testing.T) {
	use(t, stubs{
		verdict: errRefusal,
		wallet:  true,
		wGet:    func(s, u string) (string, error) { return "", errors.New("wallet closed") },
	})
	_, err := Get("svc", "user")
	if err == nil {
		t.Fatal("Get succeeded, want a wrapped failure")
	}
	if !strings.Contains(err.Error(), "Secret Service refused") {
		t.Errorf("error %q does not say why the backend was refused", err)
	}
}

func TestDispatchRefusesWithoutAnyBackend(t *testing.T) {
	use(t, stubs{verdict: errRefusal, wallet: false})
	if _, err := Get("svc", "user"); !errors.Is(err, ErrBackend) {
		t.Fatalf("Get = %v, want ErrBackend", err)
	}
	if err := Set("svc", "user", "v"); !errors.Is(err, ErrBackend) {
		t.Fatalf("Set = %v, want ErrBackend", err)
	}
	if err := Delete("svc", "user"); !errors.Is(err, ErrBackend) {
		t.Fatalf("Delete = %v, want ErrBackend", err)
	}
}

// TestDispatchProviderVanishesBetweenProbes covers the race the two probes
// (dispatcher + gate) create: the operation fails, and the explanation is the
// backend, not a raw D-Bus error.
func TestDispatchProviderVanishesBetweenProbes(t *testing.T) {
	calls := 0
	use(t, stubs{
		verdict: nil, // usable for the dispatcher
		ssGet: func(s, u string) (string, error) {
			calls++
			return "", errors.New("dbus: connection closed")
		},
	})

	// The second verdict comes from the gate after the failed call; override it
	// to simulate the provider disappearing in between.
	origVerdict := ssVerdict
	t.Cleanup(func() { ssVerdict = origVerdict })
	n := 0
	ssVerdict = func() error {
		n++
		if n == 1 {
			return nil
		}
		return errRefusal
	}
	if _, err := Get("svc", "user"); !errors.Is(err, ErrBackend) {
		t.Fatalf("Get = %v, want ErrBackend for a provider that vanished", err)
	}
	if calls != 1 {
		t.Errorf("secret-service Get called %d times, want 1", calls)
	}
}

func TestBackendName(t *testing.T) {
	use(t, stubs{verdict: nil, wallet: true})
	if got := Backend(); got != "secret-service" {
		t.Errorf("Backend = %q, want secret-service", got)
	}
	use(t, stubs{verdict: errRefusal, wallet: true})
	if got := Backend(); got != "kwallet-native" {
		t.Errorf("Backend = %q, want kwallet-native", got)
	}
	use(t, stubs{verdict: errRefusal, wallet: false})
	if got := Backend(); got != "" {
		t.Errorf("Backend = %q, want empty", got)
	}
}
