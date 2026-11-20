// Package keyring is the platform dispatcher for secret storage: on Linux it
// uses a native D-Bus Secret Service client (internal/keyringnative), on
// darwin/windows the zalando/go-keyring providers (system Keychain /
// Credential Manager). Callers use the package-level Get/Set/Delete and match
// ErrNotFound with errors.Is; every call is bounded by the provider's own
// timeouts and a prompt on Linux is a human at the desktop, not a hang.
package keyring

// Get returns the secret stored for (service, user). It returns a value
// matching ErrNotFound via errors.Is when no entry exists, and other errors
// when the provider is unreachable (no session bus in a container, locked
// wallet, disabled daemon).
var Get func(service, user string) (string, error)

// Set stores (service, user) -> value, replacing an existing entry.
var Set func(service, user, value string) error

// Delete removes the entry for (service, user).
var Delete func(service, user string) error

// ErrNotFound is the "no such entry" sentinel of the active provider.
var ErrNotFound error

// ErrBackend is the "no usable keyring backend" sentinel: the provider gate
// of internal/keyringnative refused this desktop (no session bus, no Secret
// Service provider, or gnome-keyring on KDE). Callers match it with errors.Is
// to tell an unusable provider from a missing entry. Nil on darwin/windows,
// which have a single always-present provider.
var ErrBackend error
