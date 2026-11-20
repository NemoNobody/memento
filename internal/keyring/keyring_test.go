//go:build darwin || windows

package keyring

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// Cross-platform keyring providers (macOS Keychain, Windows Credential
// Manager) have no in-process fake here: the value is the real store, and it
// is the only way to know the provider works at all on that platform.
// VELRUNA_KEYRING_TEST=1 opts in (CI's macOS job sets it); a developer's local
// keychain is not touched by a plain `go test ./...`.
func TestProviderRoundTrip(t *testing.T) {
	if os.Getenv("VELRUNA_KEYRING_TEST") == "" {
		t.Skip("keyring integration test disabled: set VELRUNA_KEYRING_TEST=1 to run against the system keyring")
	}
	service := fmt.Sprintf("velruna-ci-%d", os.Getpid())
	user := "user"
	t.Cleanup(func() { _ = Delete(service, user) })

	if err := Set(service, user, "s3cr3t value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := Get(service, user)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "s3cr3t value" {
		t.Errorf("Get = %q, want the stored value", got)
	}

	if err := Delete(service, user); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Get(service, user); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}
