//go:build linux

// Package kwalletnative talks to KWallet directly over D-Bus
// (org.kde.kwalletd6). It is the fallback for a desktop where nothing serves
// the cross-desktop Secret Service API on the bus: KF 6.13 kwalletd publishes
// that name only when it was started by PAM and nobody else holds it, so on a
// box where gnome-keyring owns the name a Secret Service client has nowhere to
// go. The wallet itself is always reachable through org.kde.KWallet:
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
	"os/user"
	"path/filepath"
	"strings"
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
	// openTimeout covers the unlock dialog: the human may need a moment. It is
	// applied on its own, not inside the per-operation budget, otherwise the
	// dialog would get the shorter rpc deadline.
	openTimeout = 20 * time.Second
)

// ErrNotFound mirrors keyring.ErrNotFound: the dispatcher maps it to the
// package-wide sentinel.
var ErrNotFound = errors.New("keyring: entry not found")

type client struct {
	conn *dbus.Conn
	obj  dbus.BusObject
}

// busUsable reports whether a session bus can be reached without dbus-launch.
// With no usable address godbus discovers /run/user/<uid>/bus (the socket) or
// /run/user/<uid>/dbus-session (an address file); that discovery is mirrored
// here, because an explicit "autolaunch:" would otherwise make godbus spawn a
// bus instead of reporting the missing one, which hangs in containers and over
// ssh.
func busUsable() error {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr != "" && addr != "autolaunch:" {
		return nil
	}
	if busDiscovered() {
		return nil
	}
	if addr == "autolaunch:" {
		return errors.New("kwallet: the session bus address is autolaunch and nothing was discovered (containers/ssh): use OPENSEARCH_PASSWORD")
	}
	return errors.New("kwallet: no session bus (containers/ssh): use OPENSEARCH_PASSWORD")
}

// busDiscovered mirrors godbus' own fallback: the runtime directory is
// /run/user/<uid> (not $XDG_RUNTIME_DIR), and either file counts. A variable so
// tests can drive the branches without a live bus.
var busDiscovered = func() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	dir := filepath.Join("/run/user", u.Uid)
	for _, name := range []string{"bus", "dbus-session"} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil {
			return true
		}
	}
	return false
}

// dial binds the wallet object. dbus.SessionBus returns a process-wide shared
// connection, so it is never closed: closing it would break every later call in
// the same process (an MCP server reads the password repeatedly). Kept as a
// variable so tests can substitute a connection.
var dial = func() (*client, error) {
	if err := busUsable(); err != nil {
		return nil, err
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
	if err := busUsable(); err != nil {
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

// call runs one org.kde.KWallet method under the rpc deadline. The context it
// receives carries no deadline of its own: one operation makes several calls
// and open() needs a longer budget than the rest, so the deadline belongs to
// the individual call, not to the operation.
func (c *client) call(ctx context.Context, method string, args ...any) *dbus.Call {
	cctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	return c.obj.CallWithContext(cctx, iface+"."+method, 0, args...)
}

// walletName resolves the wallet to use: an explicit MEMENTO_KWALLET_NAME,
// then kwalletd's localWallet() (kwalletrc "Default Wallet"), then the
// conventional name. The override is trimmed: kwalletd would treat
// " kdewallet" as a different, brand-new (empty) wallet.
func (c *client) walletName(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv("MEMENTO_KWALLET_NAME")); v != "" {
		return v
	}
	var name string
	if err := c.call(ctx, "localWallet").Store(&name); err == nil && name != "" {
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
	err := c.call(ctx, "hasFolder", handle, folder, appID).Store(&has)
	return has, err
}

func (c *client) createFolder(ctx context.Context, handle int32, folder string) error {
	var ok bool
	if err := c.call(ctx, "createFolder", handle, folder, appID).Store(&ok); err != nil {
		return fmt.Errorf("kwallet: create folder %q: %w", folder, err)
	}
	if !ok {
		// createFolder reports false when the folder already exists, and
		// another process may have created it between our check and this call.
		has, err := c.hasFolder(ctx, handle, folder)
		if err != nil {
			return fmt.Errorf("kwallet: create folder %q: %w", folder, err)
		}
		if !has {
			return fmt.Errorf("kwallet: create folder %q refused", folder)
		}
	}
	return nil
}

func (c *client) hasEntry(ctx context.Context, handle int32, folder, key string) (bool, error) {
	var has bool
	err := c.call(ctx, "hasEntry", handle, folder, key, appID).Store(&has)
	return has, err
}

// sessionOpen reports whether the handle still belongs to a live session.
// hasEntry answers false for a dead handle or a mismatched appId too — the very
// shape a regression in the argument order produces — so "no entry" must be
// confirmed before it is reported as a missing secret.
func (c *client) sessionOpen(ctx context.Context, handle int32) (bool, error) {
	var open bool
	err := c.call(ctx, "isOpen", handle).Store(&open)
	return open, err
}

// errNoEntry distinguishes "the wallet has no such entry" from "the session is
// gone"; the caller maps the former to ErrNotFound. A failure of the check
// itself is reported as a failure: an unreadable state must never be passed off
// as "there is no secret".
func (c *client) errNoEntry(ctx context.Context, handle int32) error {
	open, err := c.sessionOpen(ctx, handle)
	if err != nil {
		return fmt.Errorf("kwallet: isOpen: %w", err)
	}
	if !open {
		return errors.New("kwallet: the wallet session is no longer open (closed or kwalletd restarted): retry, or unlock the wallet")
	}
	return ErrNotFound
}

// Get returns the secret stored under (service, user).
func Get(service, user string) (string, error) {
	c, err := dial()
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	handle, _, err := c.open(ctx)
	if err != nil {
		return "", err
	}
	has, err := c.hasEntry(ctx, handle, service, user)
	if err != nil {
		return "", fmt.Errorf("kwallet: hasEntry %s/%s: %w", service, user, err)
	}
	if !has {
		return "", c.errNoEntry(ctx, handle)
	}
	var value string
	if err := c.call(ctx, "readPassword", handle, service, user, appID).Store(&value); err != nil {
		return "", fmt.Errorf("kwallet: readPassword %s/%s: %w", service, user, err)
	}
	if value == "" {
		return "", c.errNoEntry(ctx, handle)
	}
	return value, nil
}

// Set stores the secret under (service, user), creating the folder if needed.
func Set(service, user, value string) error {
	if value == "" {
		// An empty value reads back as "no entry", so accepting it would make
		// the write look successful and the read fail: refuse it up front.
		return errors.New("kwallet: refusing an empty secret")
	}
	c, err := dial()
	if err != nil {
		return err
	}
	ctx := context.Background()
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
	if err := c.call(ctx, "writePassword", handle, service, user, value, appID).Store(&code); err != nil {
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
	ctx := context.Background()
	handle, _, err := c.open(ctx)
	if err != nil {
		return err
	}
	has, err := c.hasEntry(ctx, handle, service, user)
	if err != nil {
		return fmt.Errorf("kwallet: hasEntry %s/%s: %w", service, user, err)
	}
	if !has {
		return c.errNoEntry(ctx, handle)
	}
	var code int32
	if err := c.call(ctx, "removeEntry", handle, service, user, appID).Store(&code); err != nil {
		return fmt.Errorf("kwallet: removeEntry %s/%s: %w", service, user, err)
	}
	if code < 0 {
		return fmt.Errorf("kwallet: removeEntry %s/%s refused (code %d)", service, user, code)
	}
	return nil
}
