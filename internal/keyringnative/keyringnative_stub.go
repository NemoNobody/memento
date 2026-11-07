//go:build !linux

// Package keyringnative is Linux-only (D-Bus Secret Service). This stub
// keeps `go build ./...` green on darwin/windows, where the CLI uses
// zalando/go-keyring instead (see internal/keyring/keyring_other.go).
package keyringnative

import "errors"

var ErrNotFound = errors.New("keyring: entry not found")

func Get(service, user string) (string, error) {
	panic("unreachable: keyringnative is Linux-only")
}

func Set(service, user, value string) error {
	panic("unreachable: keyringnative is Linux-only")
}

func Delete(service, user string) error {
	panic("unreachable: keyringnative is Linux-only")
}
