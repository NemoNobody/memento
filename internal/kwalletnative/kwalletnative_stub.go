//go:build !linux

// Package kwalletnative is the KWallet transport for Linux; other platforms use
// their own OS keyring and this one is inert.
package kwalletnative

import "errors"

// ErrNotFound mirrors keyring.ErrNotFound.
var ErrNotFound = errors.New("keyring: entry not found")

// Available reports whether KWallet is reachable: never outside Linux.
func Available() bool { return false }

func unsupported() error {
	return errors.New("kwallet: not supported on this platform")
}

// Get reports the platform as unsupported; the dispatcher only calls it on Linux.
func Get(string, string) (string, error) { return "", unsupported() }

// Set reports the platform as unsupported; the dispatcher only calls it on Linux.
func Set(string, string, string) error { return unsupported() }

// Delete reports the platform as unsupported; the dispatcher only calls it on Linux.
func Delete(string, string) error { return unsupported() }
