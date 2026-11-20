//go:build !linux

package keyring

import (
	"runtime"

	"github.com/zalando/go-keyring"
)

// darwin/windows: zalando's providers (macOS Keychain, Windows Credential
// Manager) are solid there; only its Linux provider is broken (KDE Plasma 6),
// which is why Linux uses internal/keyringnative instead.
func init() {
	Get = keyring.Get
	Set = keyring.Set
	Delete = keyring.Delete
	Backend = func() string {
		if runtime.GOOS == "windows" {
			return "windows-credential-manager"
		}
		return "macos-keychain"
	}
	ErrNotFound = keyring.ErrNotFound
}
