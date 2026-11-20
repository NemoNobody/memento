//go:build linux

package kwalletnative

import (
	"context"
	"os"
	"testing"
)

// TestLiveKWallet is opt-in: it writes one entry into the user's default wallet
// and removes it (and its folder) again. The folder round-trip is the part that
// catches a wrong appId in the D-Bus calls: kwalletd then answers folderList
// with an empty list and every write with -1 instead of an error.
func TestLiveKWallet(t *testing.T) {
	if os.Getenv("MEMENTO_LIVE_KWALLET") != "1" {
		t.Skip("set MEMENTO_LIVE_KWALLET=1 to exercise the real wallet")
	}
	const service, user, secret = "memento-live-test", "probe", "probe-secret"
	t.Cleanup(func() { _ = Delete(service, user) })

	c, err := dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	handle, wallet, err := c.open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Logf("wallet %q, handle %d", wallet, handle)

	if has, err := c.hasFolder(ctx, handle, service); err != nil {
		t.Fatalf("hasFolder: %v", err)
	} else if !has {
		if err := c.createFolder(ctx, handle, service); err != nil {
			t.Fatalf("createFolder: %v", err)
		}
	}

	if err := Set(service, user, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := Get(service, user)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != secret {
		t.Fatalf("Get = %q, want %q", got, secret)
	}
	if err := Delete(service, user); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Get(service, user); err != ErrNotFound {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}

	// Cleanup only: leave no empty test folder in the user's wallet.
	var ok bool
	if err := c.obj.CallWithContext(ctx, iface+".removeFolder", 0, handle, service, appID).Store(&ok); err != nil {
		t.Logf("removeFolder %q: %v", service, err)
	}
}
