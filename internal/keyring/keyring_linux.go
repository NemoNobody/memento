//go:build linux

package keyring

import (
	"errors"
	"fmt"

	"github.com/NemoNobody/memento/internal/keyringnative"
	"github.com/NemoNobody/memento/internal/kwalletnative"
)

// Linux provider order:
//
//  1. Secret Service (keyringnative) whenever BackendError says its provider
//     is usable: with a KDE wallet serving the name (kwalletd on KF 6.13,
//     ksecretd from KF 6.14) or with gnome-keyring on GNOME, going through the
//     freedesktop API keeps one entry visible to every Secret Service client
//     instead of splitting storage per wallet backend.
//  2. KWallet-native D-Bus (kwalletnative) when nobody owns
//     org.freedesktop.secrets: a KDE box whose kwalletd did not take the name
//     still has a wallet, and no other backend can reach it. This is not
//     restricted to KDE: a native KWallet is a durable store wherever it runs.
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
	Backend = backendName
	ErrNotFound = keyringnative.ErrNotFound
	ErrBackend = keyringnative.ErrBackend
}

// The backends are variables so the dispatch order and its error mapping can be
// tested without a bus; production never reassigns them.
var (
	ssVerdict = keyringnative.BackendError
	ssGet     = keyringnative.Get
	ssSet     = keyringnative.Set
	ssDelete  = keyringnative.Delete
	walletOK  = kwalletnative.Available
	walletGet = kwalletnative.Get
	walletSet = kwalletnative.Set
	walletDel = kwalletnative.Delete
)

// backendName reports which provider the next operation would use.
func backendName() string {
	if err := ssVerdict(); err == nil {
		return "secret-service"
	}
	if walletOK() {
		return "kwallet-native"
	}
	return ""
}

// refusedFallback wraps a KWallet-native failure with the Secret Service
// refusal that sent us there: the user needs both halves to fix the setup.
func refusedFallback(err, verdict error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (Secret Service refused: %v)", err, verdict)
}

// dispatchGet runs one read through the first usable backend.
func dispatchGet(service, user string) (string, error) {
	if verdict := ssVerdict(); verdict != nil {
		if !walletOK() {
			return "", verdict
		}
		value, kerr := walletGet(service, user)
		if errors.Is(kerr, kwalletnative.ErrNotFound) {
			return "", ErrNotFound
		}
		return value, refusedFallback(kerr, verdict)
	}
	value, err := ssGet(service, user)
	// The probe said usable a moment ago. If it refuses now, the provider went
	// away between the two calls: report the backend problem, not a raw D-Bus
	// error that reads like a broken store.
	if err != nil && !errors.Is(err, ErrNotFound) {
		if again := ssVerdict(); again != nil {
			return "", again
		}
	}
	return value, err
}

// dispatchSet runs one write through the first usable backend.
func dispatchSet(service, user, value string) error {
	if verdict := ssVerdict(); verdict != nil {
		if !walletOK() {
			return verdict
		}
		return refusedFallback(walletSet(service, user, value), verdict)
	}
	err := ssSet(service, user, value)
	if err != nil {
		if again := ssVerdict(); again != nil {
			return again
		}
	}
	return err
}

// dispatchDelete runs one removal through the first usable backend.
func dispatchDelete(service, user string) error {
	if verdict := ssVerdict(); verdict != nil {
		if !walletOK() {
			return verdict
		}
		kerr := walletDel(service, user)
		if errors.Is(kerr, kwalletnative.ErrNotFound) {
			return ErrNotFound
		}
		return refusedFallback(kerr, verdict)
	}
	err := ssDelete(service, user)
	if err != nil && !errors.Is(err, ErrNotFound) {
		if again := ssVerdict(); again != nil {
			return again
		}
	}
	return err
}
