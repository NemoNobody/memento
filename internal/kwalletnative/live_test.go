//go:build linux

package kwalletnative

import (
	"context"
	"errors"
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

	c, err := dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx := context.Background()
	handle, wallet, err := c.open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Logf("wallet %q, handle %d", wallet, handle)

	has, err := c.hasFolder(ctx, handle, service)
	if err != nil {
		t.Fatalf("hasFolder: %v", err)
	}
	created := false
	// Registered before the folder is created, so a failure during creation
	// (or later) cannot leave it behind; only a folder this test created is
	// removed (a pre-existing one may hold the user's entries).
	t.Cleanup(func() {
		if err := Delete(service, user); err != nil && !errors.Is(err, ErrNotFound) {
			t.Logf("Delete %q: %v", service, err)
		}
		if !created {
			return
		}
		var ok bool
		if err := c.call(ctx, "removeFolder", handle, service, appID).Store(&ok); err != nil {
			t.Logf("removeFolder %q: %v", service, err)
		}
	})
	if !has {
		if err := c.createFolder(ctx, handle, service); err != nil {
			// The daemon may have created the folder before failing; keep the
			// cleanup honest about what is on disk.
			if again, herr := c.hasFolder(ctx, handle, service); herr == nil && again {
				created = true
			}
			t.Fatalf("createFolder: %v", err)
		}
		created = true
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
	if _, err := Get(service, user); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}
