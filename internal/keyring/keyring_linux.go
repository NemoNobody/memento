//go:build linux

package keyring

import (
	"errors"

	"github.com/NemoNobody/memento/internal/keyringnative"
	"github.com/NemoNobody/memento/internal/kwalletnative"
)

// Linux provider order:
//
//  1. Secret Service (keyringnative) whenever BackendError says its provider
//     is usable: on KDE Plasma 6 that is ksecretd (or kwalletd6), and going
//     through the freedesktop API keeps one entry visible to every Secret
//     Service client instead of splitting storage per wallet backend.
//  2. KWallet-native D-Bus (kwalletnative) when nobody owns
//     org.freedesktop.secrets: a KDE box without ksecretd still has a wallet,
//     and no other backend can reach it.
//  3. Refusal otherwise — in particular on KDE served by gnome-keyring, which
//     writes an ephemeral login keyring whose credentials die at logout. That
//     combination is refused rather than "worked around", because a write
//     there succeeds now and silently loses the secret at the next login.
//
// zalando's Linux provider is unused (it targets only the dead "login"
// collection); see internal/keyringnative for the full story.
func init() {
	Get = dispatchGet
	Set = dispatchSet
	Delete = dispatchDelete
	ErrNotFound = keyringnative.ErrNotFound
	ErrBackend = keyringnative.ErrBackend
}

// dispatchGet runs one read through the first usable backend.
func dispatchGet(service, user string) (string, error) {
	if err := keyringnative.BackendError(); err != nil {
		if !kwalletnative.Available() {
			return "", err
		}
		value, kerr := kwalletnative.Get(service, user)
		if errors.Is(kerr, kwalletnative.ErrNotFound) {
			return "", ErrNotFound
		}
		return value, kerr
	}
	return keyringnative.Get(service, user)
}

// dispatchSet runs one write through the first usable backend.
func dispatchSet(service, user, value string) error {
	if err := keyringnative.BackendError(); err != nil {
		if !kwalletnative.Available() {
			return err
		}
		kerr := kwalletnative.Set(service, user, value)
		if errors.Is(kerr, kwalletnative.ErrNotFound) {
			// Set has no "not found" outcome of its own; map it anyway so the
			// sentinel contract stays identical across backends.
			return ErrNotFound
		}
		return kerr
	}
	return keyringnative.Set(service, user, value)
}

// dispatchDelete runs one removal through the first usable backend.
func dispatchDelete(service, user string) error {
	if err := keyringnative.BackendError(); err != nil {
		if !kwalletnative.Available() {
			return err
		}
		kerr := kwalletnative.Delete(service, user)
		if errors.Is(kerr, kwalletnative.ErrNotFound) {
			return ErrNotFound
		}
		return kerr
	}
	return keyringnative.Delete(service, user)
}
