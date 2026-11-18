//go:build linux

package keyring

import (
	"github.com/NemoNobody/memento/internal/keyringnative"
)

// Linux: native D-Bus Secret Service client. zalando's Linux provider is
// unusable on KDE Plasma 6 (it targets only the dead "login" collection and
// never creates collections); see internal/keyringnative for the full story.
func init() {
	Get = keyringnative.Get
	Set = keyringnative.Set
	Delete = keyringnative.Delete
	ErrNotFound = keyringnative.ErrNotFound
}
