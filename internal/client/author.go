package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/memento/internal/model"
)

// author_source values: which link of the chain produced the label.
const (
	AuthorSourceSecurity = "security" // the store's authenticated user
	AuthorSourceConfig   = "config"   // config author / MEMENTO_AUTHOR
	AuthorSourceOS       = "os"       // the OS user running memento
	AuthorSourceNone     = "none"     // nothing resolved
)

// AuthorInfo is the writer-label set carried by facts and journal entries
// (self-reported by design; see resolveAuthor).
type AuthorInfo struct {
	Author       string
	AuthorSource string
	Model        string
	Host         string
	Session      string
}

// resolveAuthor resolves the writer label once per process and caches it
// in the client: every caller (journal append, correction script, create
// stamp) reads the same labels, and the security probe runs at most once.
// Chain: store-authenticated user (security plugin) → config author → OS
// user.
//
// Authorship is SELF-REPORTED by design: the chain is best-effort audit
// metadata for shared stores, not an authentication factor (the threat
// model is a local/trusted contour, not an opponent with store access).
// With the security plugin off — the common single-node case — authinfo is
// unavailable and the chain falls to config/OS silently.
func (c *Client) resolveAuthor(ctx context.Context) AuthorInfo {
	c.authorOnce.Do(func() {
		info := AuthorInfo{
			Model:   c.cfg.Model,
			Session: c.cfg.Session,
			Host:    c.cfg.Host,
		}
		if info.Host == "" {
			if h, err := os.Hostname(); err == nil {
				info.Host = h
			}
		}
		if u := c.securityUser(ctx); u != "" {
			info.Author, info.AuthorSource = u, AuthorSourceSecurity
		} else if c.cfg.Author != "" {
			info.Author, info.AuthorSource = c.cfg.Author, AuthorSourceConfig
		} else if u, err := user.Current(); err == nil && u.Username != "" {
			info.Author, info.AuthorSource = u.Username, AuthorSourceOS
		} else {
			info.AuthorSource = AuthorSourceNone
		}
		c.author = info
	})
	return c.author
}

// AuthorLabels returns the effective authorship labels for writes made
// through this client (self-reported audit metadata, not authentication).
func (c *Client) AuthorLabels(ctx context.Context) AuthorInfo {
	return c.resolveAuthor(ctx)
}

// securityUser asks the store's security plugin who the authenticated user
// is. Every non-success means "no label": security off (404/400), missing
// privilege (403), unreachable (error) — authorship degrades down the
// chain, writes never fail on this probe. The probe carries a short
// deadline so a stalled plugin cannot slow every first write.
func (c *Client) securityUser(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The transport rewrites scheme/host to the pool node; the path is what
	// travels.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/_security/authinfo", nil)
	if err != nil {
		return ""
	}
	resp, err := c.api.Client.Stream(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		UserName string `json:"user_name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) != nil {
		return ""
	}
	return sanitizeLabel(body.UserName)
}

// labelMaxRunes caps every authorship label. They land in stored documents
// and every later read, so a runaway label must not happen. The config
// edge enforces the same bound on config-supplied labels.
const labelMaxRunes = 200

// sanitizeLabel bounds a label that came from outside (server-provided
// usernames): control bytes stripped (they would forge terminal output in
// listings), over-long labels dropped whole (a truncated name reads as a
// real name; a rejected one reads as absent).
func sanitizeLabel(v string) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > labelMaxRunes {
		return ""
	}
	return s
}

// applyTo stamps the entry with the writer labels: every journal entry names
// its own writer (the journal is a log; "who wrote this version" is a
// per-entry fact).
func (a AuthorInfo) applyTo(e *model.JournalEntry) {
	e.Author, e.AuthorSource = a.Author, a.AuthorSource
	e.Model, e.Host, e.Session = a.Model, a.Host, a.Session
}

// stampFact sets the birth labels on a new fact (write-once: who/what/where
// created it). Corrections never rewrite these; they move updated_by only.
func (a AuthorInfo) stampFact(f *model.Fact) {
	f.Author, f.AuthorSource = a.Author, a.AuthorSource
	f.Model, f.Host, f.Session = a.Model, a.Host, a.Session
}
