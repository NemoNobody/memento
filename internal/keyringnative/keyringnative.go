//go:build linux

// Package keyringnative is a minimal D-Bus client for the freedesktop.org
// Secret Service API on Linux, replacing zalando/go-keyring's Linux
// provider, which is unusable on KDE Plasma 6:
//
//   - it only targets the "/collection/login" collection — a dead object
//     under ksecretd (the path is listed in Collections, the object is
//     gone) — and its fallback path races the Completed signal
//     (zalando issues #104/#105) and counts a cancelled prompt as success
//     (#161/#162).
//
// Secrets are stored in the user's default (login) collection —
// "/org/freedesktop/secrets/aliases/default": the wallet the desktop
// unlocks at login (PAM). No creation ceremony, no extra password,
// encrypted at rest. When the alias is unavailable, the collection named
// by the config key "collection" (default "kdewallet", the KDE wallet
// name; the login wallet on GNOME keyring is "Default keyring") is located
// by Label; only if that is missing too, writes create it — on KDE that
// asks for a dedicated wallet password, because PAM auto-opens exactly one
// wallet. Secret Service has no per-collection ACLs, so such collections
// buy namespace, not isolation; items in every collection are identified
// by the same "service"/"username" attributes zalando uses. Every method
// call carries a deadline (rpcTimeout), every dialog is bounded by
// promptTimeout, and a dismissed prompt is a hard error: a wedged or
// restarting provider can never hang the process.
//
// Provider note: on KDE the Secret Service name is served by kwalletd6
// (collections = wallets, aliases from kwalletrc "Default Wallet");
// gnome-keyring-daemon may race for the bus name and serve its own
// empty-password login keyring — see docs/setup.md. On GNOME the
// provider is gnome-keyring.
package keyringnative

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	dbus "github.com/godbus/dbus/v5"
)

const (
	busName         = "org.freedesktop.secrets"
	objPath         = "/org/freedesktop/secrets"
	ifaceService    = "org.freedesktop.Secret.Service"
	ifaceCollection = "org.freedesktop.Secret.Collection"
	ifaceItem       = "org.freedesktop.Secret.Item"
	ifaceSession    = "org.freedesktop.Secret.Session"
	ifacePrompt     = "org.freedesktop.Secret.Prompt"
	ifaceSearches   = "org.freedesktop.Secret.Searches"

	// defaultCollection is the login-unlocked wallet alias (kwalletrc
	// "Default Wallet" on KDE, the login keyring on GNOME).
	defaultCollection = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")

	// defaultCollectionName is the fallback collection's Label when the
	// default alias is unavailable (config key "collection"). On KDE the
	// Secret Service collection label equals the wallet name; GNOME's login
	// keyring is labelled "Default keyring".
	defaultCollectionName = "kdewallet"

	// appCollectionLabel prefixes item labels so entries are identifiable
	// in wallet managers.
	appCollectionLabel = "memento"

	// promptTimeout bounds the interactive part: unlocking a closed wallet
	// needs a human at the desktop dialog; 120s is generous.
	promptTimeout = 120 * time.Second

	// rpcTimeout bounds every non-interactive D-Bus call. Flagless Call
	// carries no deadline at all: a wedged or restarting provider (a
	// kwalletd mid-restart) must not hang a CLI command or an MCP tool
	// call forever.
	rpcTimeout = 15 * time.Second
)

// rpcCall runs a D-Bus method call under the rpc deadline.
func (c *client) rpcCall(ctx context.Context, obj dbus.BusObject, method string, args ...interface{}) *dbus.Call {
	return obj.CallWithContext(ctx, method, 0, args...)
}

// getProperty is GetProperty with a deadline (GetProperty itself has no
// context variant; Properties.Get is the identical call underneath).
func (c *client) getProperty(ctx context.Context, obj dbus.BusObject, iface, prop string) (dbus.Variant, error) {
	var v dbus.Variant
	if err := c.rpcCall(ctx, obj, "org.freedesktop.DBus.Properties.Get", iface, prop).Store(&v); err != nil {
		return dbus.Variant{}, err
	}
	return v, nil
}

// ErrNotFound is returned when the collection or the item does not exist.
// Callers match it with errors.Is, like zalando's keyring.ErrNotFound.
var ErrNotFound = errors.New("keyring: entry not found")

// collectionName is the Label of the fallback collection (default: the KDE
// wallet name). Read-only at runtime (init-time writes only) so concurrent
// MCP tool calls can read it safely.
var collectionName = defaultCollectionName

// SetCollectionName points the fallback collection at an explicit label.
// Empty keeps the default. Not currently wired to the config; call at
// init time only. On KDE this must match kwalletrc "Default Wallet"; the
// login wallet itself is preferred whenever its alias is alive.
func SetCollectionName(name string) {
	if name != "" {
		collectionName = name
	}
}

// secret mirrors the D-Bus secret struct (signature (oayays)), same shape
// zalando uses for wire compatibility.
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func newSecret(session dbus.ObjectPath, value string) secret {
	return secret{
		Session:     session,
		Parameters:  []byte{},
		Value:       []byte(value),
		ContentType: "text/plain; charset=utf8",
	}
}

type client struct {
	conn    *dbus.Conn
	service dbus.BusObject
	session dbus.ObjectPath
}

func newClient() (*client, error) {
	// A connection this client owns and may close. SessionBus returns
	// godbus's cached shared connection, and closing it from close() broke
	// concurrent callers (two MCP tools racing one conn). Private variants
	// Dial without Auth/Hello in godbus v5.2.4, and the bus silently drops
	// calls from unauthenticated connections (hang). Connect does the full
	// Dial+Auth+Hello. No autolaunch: spawning dbus-launch where no session
	// bus is meant to run (containers, ssh) hangs — fail fast into the env
	// fallback instead.
	address := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if address == "" || address == "autolaunch:" {
		return nil, fmt.Errorf("no session D-Bus address (DBUS_SESSION_BUS_ADDRESS unset): " +
			"in containers/ssh export OPENSEARCH_PASSWORD instead of using the keyring")
	}
	conn, err := dbus.Connect(address)
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus (is a keyring service running, e.g. KDE Wallet/GNOME Keyring?): %w", err)
	}
	c := &client{conn: conn, service: conn.Object(busName, objPath)}
	var ignored dbus.Variant
	sessionCtx, cancelSession := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelSession()
	if err := c.rpcCall(sessionCtx, c.service, ifaceService+".OpenSession", "plain", dbus.MakeVariant("plain")).Store(&ignored, &c.session); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open secret-service session: %w", err)
	}
	return c, nil
}

func (c *client) close() {
	closeCtx, cancelClose := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelClose()
	_ = c.rpcCall(closeCtx, c.conn.Object(busName, c.session), ifaceSession+".Close").Store()
	_ = c.conn.Close()
}

// unlock unlocks a collection or item, driving the desktop password dialog
// when the target is password-protected and closed. The login wallet is
// unlocked at login (PAM), so on a normal desktop this is silent.
func (c *client) unlock(path dbus.ObjectPath) error {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	unlockCtx, cancelUnlock := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelUnlock()
	if err := c.rpcCall(unlockCtx, c.service, ifaceService+".Unlock", []dbus.ObjectPath{path}).Store(&unlocked, &prompt); err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	if len(unlocked) > 0 || prompt == "/" {
		return nil
	}
	dismissed, _, err := c.handlePrompt(prompt)
	if err != nil {
		return err
	}
	if dismissed {
		return errors.New("the keyring dialog was cancelled (unlock)")
	}
	return nil
}

// searchItems returns item paths in the collection matching attrs. Providers
// differ in reply shape (spec: searches object + items; kwalletd5: bare
// items array), so the items property is read from the searches object as a
// fallback.
func (c *client) searchItems(collection dbus.ObjectPath, attrs map[string]string) ([]dbus.ObjectPath, error) {
	searchCtx, cancelSearch := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelSearch()
	call := c.rpcCall(searchCtx, c.conn.Object(busName, collection), ifaceCollection+".SearchItems", attrs)
	var items []dbus.ObjectPath
	var searches dbus.ObjectPath
	if err := call.Store(&searches, &items); err != nil {
		if err2 := call.Store(&items); err2 != nil {
			return nil, fmt.Errorf("search items: %w", err)
		}
	}
	if len(items) == 0 && searches != "" && searches != "/" {
		// The fallback must carry the rpc deadline too: a flagless GetProperty
		// has none, and a wedged/restarting provider would hang the caller
		// forever (the package contract: every method call carries a deadline).
		propCtx, cancelProp := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancelProp()
		v, err := c.getProperty(propCtx, c.conn.Object(busName, searches), ifaceSearches, "items")
		if err == nil {
			items, _ = v.Value().([]dbus.ObjectPath)
		}
	}
	return items, nil
}

// handlePrompt triggers a provider prompt object and waits for its Completed
// signal. The match is installed BEFORE Prompt() is called: Prompt() can
// return and Completed can fire while a late match is being set up
// (zalando issues #104/#105).
func (c *client) handlePrompt(prompt dbus.ObjectPath) (bool, dbus.Variant, error) {
	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(prompt),
		dbus.WithMatchInterface(ifacePrompt),
		// Match rules take the bare signal name ("Completed"); the interface
		// is constrained by the option above. sig.Name below is "iface.member".
		dbus.WithMatchMember("Completed"),
	}
	if err := c.conn.AddMatchSignal(match...); err != nil {
		return false, dbus.Variant{}, fmt.Errorf("prompt: add signal match: %w", err)
	}
	defer func() { _ = c.conn.RemoveMatchSignal(match...) }()

	sigs := make(chan *dbus.Signal, 1)
	c.conn.Signal(sigs)
	defer c.conn.RemoveSignal(sigs)

	// Prompt(STRING interaction_requestor): ksecretd (Plasma 6) declares
	// (s); the freedesktop spec's (s,b) is rejected by it, so send (s).
	// The Prompt call blocks while the dialog is open (providers keep it
	// pending), so it is bounded by the generous dialog timeout, not the
	// rpc deadline.
	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), promptTimeout)
	defer cancelPrompt()
	if err := c.rpcCall(promptCtx, c.conn.Object(busName, prompt), ifacePrompt+".Prompt", "").Store(); err != nil {
		return false, dbus.Variant{}, fmt.Errorf("prompt: %w", err)
	}

	select {
	case sig := <-sigs:
		if len(sig.Body) < 2 {
			return false, dbus.Variant{}, fmt.Errorf("prompt Completed: unexpected signal body: %v", sig.Body)
		}
		dismissed, ok := sig.Body[0].(bool)
		if !ok {
			return false, dbus.Variant{}, fmt.Errorf("prompt Completed: dismissed flag is not a bool: %v", sig.Body[0])
		}
		result, ok := sig.Body[1].(dbus.Variant)
		if !ok {
			return false, dbus.Variant{}, fmt.Errorf("prompt Completed: result is not a variant: %v", sig.Body[1])
		}
		return dismissed, result, nil
	case <-time.After(promptTimeout):
		return false, dbus.Variant{}, fmt.Errorf(
			"timed out (%s) waiting for the keyring dialog; type the wallet password in the desktop dialog and retry", promptTimeout)
	}
}

func attributes(service, user string) map[string]string {
	return map[string]string{"service": service, "username": user}
}

// collectionLabel reads a collection's Label. It doubles as the liveness
// check: providers list dead collection paths (observed under ksecretd:
// "/collection/login" is in Collections, the object is gone), and property
// reads on such paths fail.
func (c *client) collectionLabel(p dbus.ObjectPath) (string, error) {
	labelCtx, cancelLabel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelLabel()
	v, err := c.getProperty(labelCtx, c.conn.Object(busName, p), ifaceCollection, "Label")
	if err != nil {
		return "", err
	}
	label, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("collection %s: Label is not a string", p)
	}
	return label, nil
}

// findCollection returns the path of the collection labelled with label,
// if one exists. Dead listed collections are skipped (their Label read
// fails).
func (c *client) findCollection(label string) (dbus.ObjectPath, error) {
	collCtx, cancelColl := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelColl()
	v, err := c.getProperty(collCtx, c.service, ifaceService, "Collections")
	if err != nil {
		return "", fmt.Errorf("list collections: %w", err)
	}
	collections, ok := v.Value().([]dbus.ObjectPath)
	if !ok {
		return "", errors.New("the Collections property has an unexpected type")
	}
	for _, p := range collections {
		if l, err := c.collectionLabel(p); err == nil && l == label {
			return p, nil
		}
	}
	return "", ErrNotFound
}

// collectionFromPromptResult accepts both object-path shapes providers put
// in the Completed signal (freedesktop spec: "o"; ksecretd: "ao").
func collectionFromPromptResult(v dbus.Variant) (dbus.ObjectPath, error) {
	switch val := v.Value().(type) {
	case dbus.ObjectPath:
		return val, nil
	case []dbus.ObjectPath:
		if len(val) > 0 {
			return val[0], nil
		}
		return "", errors.New("prompt result contains no collection")
	default:
		return "", fmt.Errorf("prompt result has an unexpected type: %T", v.Value())
	}
}

// createCollection creates the fallback collection with the given Label.
// On KDE this shows the wallet-creation dialog (the user sets a dedicated
// wallet password); it is reached only when the default (login) collection
// and the configured collection name are both unavailable.
func (c *client) createCollection(label string) (dbus.ObjectPath, error) {
	props := map[string]dbus.Variant{
		ifaceCollection + ".Label": dbus.MakeVariant(label),
	}
	var coll, prompt dbus.ObjectPath
	createCtx, cancelCreate := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelCreate()
	if err := c.rpcCall(createCtx, c.service, ifaceService+".CreateCollection", props, "").Store(&coll, &prompt); err != nil {
		return "", fmt.Errorf("create collection: %w", err)
	}
	// ksecretd returns collection="/" plus a prompt; the real path arrives
	// in the Completed signal result.
	if coll == "" || coll == "/" {
		if prompt == "/" {
			return "", errors.New("create collection: the provider returned no collection and no prompt")
		}
		dismissed, result, err := c.handlePrompt(prompt)
		if err != nil {
			return "", err
		}
		if dismissed {
			return "", errors.New("the keyring dialog was cancelled (create collection)")
		}
		if coll, err = collectionFromPromptResult(result); err != nil {
			return "", err
		}
	}
	if err := c.unlock(coll); err != nil {
		return "", err
	}
	return coll, nil
}

// pickCollection resolves where secrets live: the default (login) wallet
// first, then the collection named by the config key "collection"
// (default "kdewallet": on KDE a wallet's collection Label is the wallet
// name, so an explicit name finds the live login wallet even without a
// registered alias). Reads never create anything — with no usable
// collection they report ErrNotFound. write=true (Set) may create the
// fallback collection as a last resort.
func (c *client) pickCollection(write bool) (dbus.ObjectPath, error) {
	if _, err := c.collectionLabel(defaultCollection); err == nil {
		return defaultCollection, nil
	}
	if p, err := c.findCollection(collectionName); err == nil {
		return p, nil
	}
	if !write {
		return "", ErrNotFound
	}
	return c.createCollection(collectionName)
}

// deleteItems removes every item in the collection matching attrs, handling
// provider prompts. Providers may keep more than one item per attribute set
// (CreateItem never deduplicates), so the upsert contract requires clearing
// all matches: leaving a stale duplicate makes Get read whichever item the
// provider lists first — typically the oldest, i.e. the pre-rotation password.
func (c *client) deleteItems(coll dbus.ObjectPath, attrs map[string]string) (int, error) {
	return c.deleteItemsExcept(coll, attrs, "")
}

// deleteItemsExcept is deleteItems with one item spared (the freshly
// created one during an upsert sweep).
func (c *client) deleteItemsExcept(coll dbus.ObjectPath, attrs map[string]string, keep dbus.ObjectPath) (int, error) {
	items, err := c.searchItems(coll, attrs)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, p := range items {
		if p == keep {
			continue
		}
		var prompt dbus.ObjectPath
		deleteCtx, cancelDelete := context.WithTimeout(context.Background(), rpcTimeout)
		err := c.rpcCall(deleteCtx, c.conn.Object(busName, p), ifaceItem+".Delete").Store(&prompt)
		cancelDelete()
		if err != nil {
			return removed, fmt.Errorf("delete entry: %w", err)
		}
		if prompt != "/" {
			dismissed, _, err := c.handlePrompt(prompt)
			if err != nil {
				return removed, err
			}
			if dismissed {
				return removed, errors.New("the keyring dialog was cancelled (delete)")
			}
		}
		removed++
	}
	return removed, nil
}

// Get returns the secret stored for (service, user). Read-only: it never
// creates collections.
func Get(service, user string) (string, error) {
	c, err := newClient()
	if err != nil {
		return "", err
	}
	defer c.close()

	coll, err := c.pickCollection(false)
	if err != nil {
		return "", err
	}
	items, err := c.searchItems(coll, attributes(service, user))
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", ErrNotFound
	}
	if len(items) > 1 {
		// A duplicate set (foreign writer, or a crash between an older
		// create and its sweep) must not make reads provider-dependent:
		// the paths are sorted so every call sees the same item, and the
		// next Set's sweep removes the rest.
		sort.Slice(items, func(i, j int) bool { return items[i] < items[j] })
	}
	if err := c.unlock(items[0]); err != nil {
		return "", err
	}
	var sec secret
	secretCtx, cancelSecret := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelSecret()
	if err := c.rpcCall(secretCtx, c.conn.Object(busName, items[0]), ifaceItem+".GetSecret", c.session).Store(&sec); err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return string(sec.Value), nil
}

// Set stores the secret for (service, user). With the default (login)
// wallet unlocked at session login — the normal case — this runs without
// any dialog.
func Set(service, user, value string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	defer c.close()

	coll, err := c.pickCollection(true)
	if err != nil {
		return err
	}
	if err := c.unlock(coll); err != nil {
		return err
	}
	// Upsert, create-then-delete (the zalando order flipped): deleting all
	// matches first would destroy the old secret for good when creation
	// then fails (cancelled prompt, RPC drop, timeout), while creating
	// first keeps at least one working item at every moment. A crash
	// between create and the sweep leaves a duplicate; Get then picks
	// deterministically and the next Set's sweep cleans up (CreateItem
	// never deduplicates).
	props := map[string]dbus.Variant{
		ifaceItem + ".Label":      dbus.MakeVariant(appCollectionLabel + " API key for " + user),
		ifaceItem + ".Attributes": dbus.MakeVariant(attributes(service, user)),
	}
	var item, prompt dbus.ObjectPath
	itemCtx, cancelItem := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancelItem()
	if err := c.rpcCall(itemCtx, c.conn.Object(busName, coll), ifaceCollection+".CreateItem", props, newSecret(c.session, value), true).
		Store(&item, &prompt); err != nil {
		return fmt.Errorf("store secret: %w", err)
	}
	if prompt != "/" {
		dismissed, _, err := c.handlePrompt(prompt)
		if err != nil {
			return err
		}
		if dismissed {
			return errors.New("the keyring dialog was cancelled (store)")
		}
	}
	if item == "" || item == "/" {
		// The provider stored the item but did not identify its path (the
		// ksecretd-shaped prompt indirection). Sweeping with a bogus keep
		// path would delete the secret that was just stored; refusing the
		// sweep can strand an old duplicate that Get may keep preferring.
		// Both are worse than an honest instruction, so say what to do.
		items, serr := c.searchItems(coll, attributes(service, user))
		if serr != nil {
			return fmt.Errorf("stored, but the provider did not identify the new entry and the entries could not be counted (%v): run `memento creds rm` and set again (a stranded duplicate may outrank the new secret)", serr)
		}
		if len(items) > 1 {
			return fmt.Errorf("stored, but the provider did not identify the new entry and %d entries exist for this pair: run `memento creds rm` and set again (a later Get may otherwise read the old secret)", len(items))
		}
		return nil
	}
	// Sweep the superseded entries. The secret is already stored: a failed
	// sweep is reported honestly but must not claim the write itself failed.
	// The sweep is only safe when the provider-reported path is one of the
	// search matches: deleteItemsExcept spares exactly that path, so a path
	// outside the matches would spare nothing and delete the stored secret
	// — the exact shape this guard exists to prevent.
	items, serr := c.searchItems(coll, attributes(service, user))
	if serr != nil {
		return fmt.Errorf("stored, but the superseded entries could not be enumerated (Get stays deterministic; a later Set retries the sweep): %w", serr)
	}
	keepFound := false
	for _, p := range items {
		if p == item {
			keepFound = true
			break
		}
	}
	if !keepFound {
		return fmt.Errorf("stored, but the provider reported an entry outside the search results: run `memento creds rm` and set again (sweeping with an unknown keep-path could delete the stored secret)")
	}
	if len(items) > 1 {
		if _, err := c.deleteItemsExcept(coll, attributes(service, user), item); err != nil {
			return fmt.Errorf("stored, but removing the superseded entries failed (Get stays deterministic; a later Set retries the sweep): %w", err)
		}
	}
	return nil
}

// Delete removes the entry for (service, user).
func Delete(service, user string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	defer c.close()

	coll, err := c.pickCollection(false)
	if err != nil {
		return err
	}
	if err := c.unlock(coll); err != nil {
		return err
	}
	n, err := c.deleteItems(coll, attributes(service, user))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
