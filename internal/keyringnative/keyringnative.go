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
// by the same "service"/"username" attributes zalando uses. Every prompt
// is bounded by a timeout and a dismissed prompt is a hard error.
//
// Provider note: on KDE the Secret Service name is served by kwalletd6
// (collections = wallets, aliases from kwalletrc "Default Wallet");
// gnome-keyring-daemon may race for the bus name and serve its own
// empty-password login keyring — see docs/setup.md. On GNOME the
// provider is gnome-keyring.
package keyringnative

import (
	"errors"
	"fmt"
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
)

// ErrNotFound is returned when the collection or the item does not exist.
// Callers match it with errors.Is, like zalando's keyring.ErrNotFound.
var ErrNotFound = errors.New("keyring: entry not found")

// collectionName is the Label of the fallback collection, set from the
// config key "collection" via SetCollectionName (default: the KDE wallet
// name). Not safe for concurrent mutation; the CLI is single-shot.
var collectionName = defaultCollectionName

// SetCollectionName points the fallback collection at an explicit label
// (config key "collection"). Empty keeps the default. On KDE this must
// match kwalletrc "Default Wallet"; the login wallet itself is preferred
// whenever its alias is alive, regardless of this value.
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
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus (is a keyring service running, e.g. KDE Wallet/GNOME Keyring?): %w", err)
	}
	c := &client{conn: conn, service: conn.Object(busName, objPath)}
	var ignored dbus.Variant
	if err := c.service.Call(ifaceService+".OpenSession", 0, "plain", dbus.MakeVariant("plain")).Store(&ignored, &c.session); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open secret-service session: %w", err)
	}
	return c, nil
}

func (c *client) close() {
	_ = c.conn.Object(busName, c.session).Call(ifaceSession+".Close", 0).Store()
	_ = c.conn.Close()
}

// unlock unlocks a collection or item, driving the desktop password dialog
// when the target is password-protected and closed. The login wallet is
// unlocked at login (PAM), so on a normal desktop this is silent.
func (c *client) unlock(path dbus.ObjectPath) error {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := c.service.Call(ifaceService+".Unlock", 0, []dbus.ObjectPath{path}).Store(&unlocked, &prompt); err != nil {
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
	call := c.conn.Object(busName, collection).Call(ifaceCollection+".SearchItems", 0, attrs)
	var items []dbus.ObjectPath
	var searches dbus.ObjectPath
	if err := call.Store(&searches, &items); err != nil {
		if err2 := call.Store(&items); err2 != nil {
			return nil, fmt.Errorf("search items: %w", err)
		}
	}
	if len(items) == 0 && searches != "" && searches != "/" {
		v, err := c.conn.Object(busName, searches).GetProperty(ifaceSearches + ".items")
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
	if err := c.conn.Object(busName, prompt).Call(ifacePrompt+".Prompt", 0, "").Store(); err != nil {
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
	v, err := c.conn.Object(busName, p).GetProperty(ifaceCollection + ".Label")
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
	v, err := c.service.GetProperty(ifaceService + ".Collections")
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
	if err := c.service.Call(ifaceService+".CreateCollection", 0, props, "").Store(&coll, &prompt); err != nil {
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
	if err := c.unlock(items[0]); err != nil {
		return "", err
	}
	var sec secret
	if err := c.conn.Object(busName, items[0]).Call(ifaceItem+".GetSecret", 0, c.session).Store(&sec); err != nil {
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
	props := map[string]dbus.Variant{
		ifaceItem + ".Label":      dbus.MakeVariant(appCollectionLabel + " API key for " + user),
		ifaceItem + ".Attributes": dbus.MakeVariant(attributes(service, user)),
	}
	var item, prompt dbus.ObjectPath
	if err := c.conn.Object(busName, coll).
		Call(ifaceCollection+".CreateItem", 0, props, newSecret(c.session, value), true).
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
	items, err := c.searchItems(coll, attributes(service, user))
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return ErrNotFound
	}
	var prompt dbus.ObjectPath
	if err := c.conn.Object(busName, items[0]).Call(ifaceItem+".Delete", 0).Store(&prompt); err != nil {
		return fmt.Errorf("delete entry: %w", err)
	}
	if prompt != "/" {
		dismissed, _, err := c.handlePrompt(prompt)
		if err != nil {
			return err
		}
		if dismissed {
			return errors.New("the keyring dialog was cancelled (delete)")
		}
	}
	return nil
}
