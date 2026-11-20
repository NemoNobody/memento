//go:build linux

package kwalletnative

import (
	"context"
	"errors"
	"testing"
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

// TestErrNotFoundIdentity: the dispatcher maps this sentinel, so it must stay a
// plain comparable error.
func TestErrNotFoundIdentity(t *testing.T) {
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatal("ErrNotFound does not match itself")
	}
}
