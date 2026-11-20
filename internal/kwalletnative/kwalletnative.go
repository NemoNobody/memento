//go:build linux

// Package kwalletnative talks to KWallet directly over D-Bus
// (org.kde.kwalletd6). The cross-desktop Secret Service API is the first
// choice, but KDE before KF 6.14 (Debian 13/trixie) ships no provider for it:
// kwalletd6 owns only its own bus name, so a Secret Service client has nowhere
// to go — and gnome-keyring, when it grabs the name, writes to an ephemeral
// login keyring. The wallet itself is reachable through org.kde.KWallet:
// folder = service, key = account, application = "memento".
//
// Every call carries a deadline. open() on a locked wallet makes kwalletd show
// a desktop password dialog, so it gets the longer budget and a timeout is
// reported as "unlock the wallet", never retried: a headless MCP call must not
// wait forever for a dialog nobody sees.
package kwalletnative

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	dbus "github.com/godbus/dbus/v5"
)

const (
	busName = "org.kde.kwalletd6"
	objPath = "/modules/kwalletd6"
	iface   = "org.kde.KWallet"
	appID   = "memento"

	// defaultWallet is used when kwalletd does not name one (normally
	// localWallet() returns kwalletrc "Default Wallet").
	defaultWallet = "kdewallet"

	rpcTimeout = 15 * time.Second
	// openTimeout covers the unlock dialog: the human may need a moment.
	openTimeout = 20 * time.Second
)

// ErrNotFound mirrors keyring.ErrNotFound: the dispatcher maps it to the
// package-wide sentinel.
var ErrNotFound = errors.New("keyring: entry not found")

type client struct {
	conn *dbus.Conn
	obj  dbus.BusObject
}

// dial binds the wallet object. dbus.SessionBus returns a process-wide shared
// connection, so it is never closed: closing it would break every later call in
// the same process (an MCP server reads the password repeatedly). Kept as a
// variable so tests can substitute a connection.
var dial = func() (*client, error) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return nil, errors.New("kwallet: no session bus (containers/ssh): use OPENSEARCH_PASSWORD")
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("kwallet: session bus: %w", err)
	}
	return &client{conn: conn, obj: conn.Object(busName, objPath)}, nil
}

// Available reports whether kwalletd6 owns its bus name. Read-only: it never
// opens a wallet and never prompts.
func Available() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	var has bool
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, busName).Store(&has); err != nil {
		return false
	}
	return has
}

// walletName resolves the wallet to use: an explicit MEMENTO_KWALLET_NAME,
// then kwalletd's localWallet() (kwalletrc "Default Wallet"), then the
// conventional name.
func (c *client) walletName(ctx context.Context) string {
	if v := os.Getenv("MEMENTO_KWALLET_NAME"); v != "" {
		return v
	}
	var name string
	if err := c.obj.CallWithContext(ctx, iface+".localWallet", 0).Store(&name); err == nil && name != "" {
		return name
	}
	return defaultWallet
}

// open returns a handle for the wallet. A negative handle and a deadline are
// both "the wallet is not available to this process": the caller must unlock
// it in KWallet or fall back to the environment password.
func (c *client) open(ctx context.Context) (int32, string, error) {
	wallet := c.walletName(ctx)
	octx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	var handle int32
	call := c.obj.CallWithContext(octx, iface+".open", 0, wallet, int64(0), appID)
	if call.Err != nil {
		return 0, wallet, fmt.Errorf("kwallet: open %q: %w (unlock the wallet in KDE Wallet, or set OPENSEARCH_PASSWORD)", wallet, call.Err)
	}
	if err := call.Store(&handle); err != nil {
		return 0, wallet, fmt.Errorf("kwallet: open %q: %w", wallet, err)
	}
	if handle < 0 {
		return 0, wallet, fmt.Errorf("kwallet: wallet %q is closed (handle %d): unlock it in KDE Wallet, or set OPENSEARCH_PASSWORD", wallet, handle)
	}
	return handle, wallet, nil
}

// The wallet methods below take the caller's appId, not the wallet name: the
// daemon resolves the wallet from the handle and rejects the call when the
// appId does not match the one open() registered (kwalletd.h in KF 6.13).
// Passing the wallet name there makes every call fail with -1.
func (c *client) hasFolder(ctx context.Context, handle int32, folder string) (bool, error) {
	var has bool
	err := c.obj.CallWithContext(ctx, iface+".hasFolder", 0, handle, folder, appID).Store(&has)
	return has, err
}

func (c *client) createFolder(ctx context.Context, handle int32, folder string) error {
	var ok bool
	if err := c.obj.CallWithContext(ctx, iface+".createFolder", 0, handle, folder, appID).Store(&ok); err != nil {
		return fmt.Errorf("kwallet: create folder %q: %w", folder, err)
	}
	if !ok {
		// createFolder reports false when the folder already exists, and
		// another process may have created it between our check and this call.
		if has, err := c.hasFolder(ctx, handle, folder); err == nil && has {
			return nil
		}
		return fmt.Errorf("kwallet: create folder %q refused", folder)
	}
	return nil
}

func (c *client) hasEntry(ctx context.Context, handle int32, folder, key string) (bool, error) {
	var has bool
	err := c.obj.CallWithContext(ctx, iface+".hasEntry", 0, handle, folder, key, appID).Store(&has)
	return has, err
}

// Get returns the secret stored under (service, user).
func Get(service, user string) (string, error) {
	c, err := dial()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	handle, _, err := c.open(ctx)
	if err != nil {
		return "", err
	}
	has, err := c.hasEntry(ctx, handle, service, user)
	if err != nil {
		return "", fmt.Errorf("kwallet: hasEntry %s/%s: %w", service, user, err)
	}
	if !has {
		return "", ErrNotFound
	}
	var value string
	if err := c.obj.CallWithContext(ctx, iface+".readPassword", 0, handle, service, user, appID).Store(&value); err != nil {
		return "", fmt.Errorf("kwallet: readPassword %s/%s: %w", service, user, err)
	}
	if value == "" {
		return "", ErrNotFound
	}
	return value, nil
}

// Set stores the secret under (service, user), creating the folder if needed.
func Set(service, user, value string) error {
	c, err := dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	handle, _, err := c.open(ctx)
	if err != nil {
		return err
	}
	has, err := c.hasFolder(ctx, handle, service)
	if err != nil {
		return fmt.Errorf("kwallet: hasFolder %s: %w", service, err)
	}
	if !has {
		if err := c.createFolder(ctx, handle, service); err != nil {
			return err
		}
	}
	var code int32
	if err := c.obj.CallWithContext(ctx, iface+".writePassword", 0, handle, service, user, value, appID).Store(&code); err != nil {
		return fmt.Errorf("kwallet: writePassword %s/%s: %w", service, user, err)
	}
	if code < 0 {
		return fmt.Errorf("kwallet: writePassword %s/%s refused (code %d)", service, user, code)
	}
	return nil
}

// Delete removes the secret under (service, user).
func Delete(service, user string) error {
	c, err := dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	handle, _, err := c.open(ctx)
	if err != nil {
		return err
	}
	has, err := c.hasEntry(ctx, handle, service, user)
	if err != nil {
		return fmt.Errorf("kwallet: hasEntry %s/%s: %w", service, user, err)
	}
	if !has {
		return ErrNotFound
	}
	var code int32
	if err := c.obj.CallWithContext(ctx, iface+".removeEntry", 0, handle, service, user, appID).Store(&code); err != nil {
		return fmt.Errorf("kwallet: removeEntry %s/%s: %w", service, user, err)
	}
	if code < 0 {
		return fmt.Errorf("kwallet: removeEntry %s/%s refused (code %d)", service, user, code)
	}
	return nil
}
