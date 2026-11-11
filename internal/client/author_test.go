package client

import (
	"context"
	"net/http"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

// The author chain is the audit spine of a shared store: the store's
// authenticated user wins, then the config label, then the OS user. The
// labels are self-reported by design — these tests pin the chain order,
// not an authentication guarantee.

func TestAuthorChainSecurityUserWins(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		if r.Method == http.MethodGet && r.Path == "/_security/authinfo" {
			return http.StatusOK, `{"user_name":"alice"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})
	info := c.resolveAuthor(context.Background())
	if info.Author != "alice" || info.AuthorSource != AuthorSourceSecurity {
		t.Errorf("author = %q/%q, want alice/security", info.Author, info.AuthorSource)
	}
}

func TestAuthorChainFallsToConfig(t *testing.T) {
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) { cfg.Author = "cfg-user" },
		func(r recordedRequest) (int, string) {
			// Security off (404) is the common local case: the probe must
			// degrade silently, never fail the write.
			return http.StatusNotFound, `{"status":404}`
		})
	info := c.resolveAuthor(context.Background())
	if info.Author != "cfg-user" || info.AuthorSource != AuthorSourceConfig {
		t.Errorf("author = %q/%q, want cfg-user/config", info.Author, info.AuthorSource)
	}
}

func TestAuthorChainFallsToOSUser(t *testing.T) {
	c, _ := newFakeClient(t, func(r recordedRequest) (int, string) {
		return http.StatusForbidden, `{"status":403}`
	})
	info := c.resolveAuthor(context.Background())
	u, err := user.Current()
	if err != nil || u.Username == "" {
		t.Skip("no OS user in this environment")
	}
	if info.Author != u.Username || info.AuthorSource != AuthorSourceOS {
		t.Errorf("author = %q/%q, want %q/os", info.Author, info.AuthorSource, u.Username)
	}
}

func TestAuthorChainIgnoresOversizedServerLabel(t *testing.T) {
	// A store that hands back an absurd user_name must not bloat every fact:
	// the label is dropped whole and the chain falls to the config.
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) { cfg.Author = "cfg-user" },
		func(r recordedRequest) (int, string) {
			if r.Path == "/_security/authinfo" {
				return http.StatusOK, `{"user_name":"` + strings.Repeat("x", 300) + `"}`
			}
			return http.StatusInternalServerError, `{"status":500}`
		})
	info := c.resolveAuthor(context.Background())
	if info.Author != "cfg-user" || info.AuthorSource != AuthorSourceConfig {
		t.Errorf("author = %q/%q, want cfg-user/config", info.Author, info.AuthorSource)
	}
}

func TestSanitizeLabel(t *testing.T) {
	if got := sanitizeLabel(" ali\nce "); got != "alice" {
		t.Errorf("control bytes not stripped: %q", got)
	}
	if got := sanitizeLabel(strings.Repeat("x", 201)); got != "" {
		t.Error("over-long label must be dropped whole")
	}
}

// The write path stamps the labels: birth labels on create (write-once),
// updated_by on every toucher, and every journal entry names its writer.

func TestAddCreateCarriesAuthorLabels(t *testing.T) {
	var putBody string
	c, fake := newFakeClientCfg(t, func(cfg *config.Config) {
		cfg.Author = "alice"
		cfg.Model = "strata"
		cfg.Host = "tower"
		cfg.Session = "s-42"
	}, func(r recordedRequest) (int, string) {
		if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
			return code, body
		}
		switch {
		case r.Method == http.MethodGet && r.Path == "/_security/authinfo":
			return http.StatusNotFound, `{"status":404}`
		case r.Method == http.MethodGet && r.Path == "/memento/_doc/new":
			return http.StatusNotFound, `{"status":404,"error":"document_missing_exception"}`
		case r.Method == http.MethodPut && r.Path == "/memento/_doc/new":
			putBody = r.Body
			return http.StatusCreated, `{"_index":"memento","_id":"new","result":"created"}`
		case r.Method == http.MethodPost && r.Path == "/memento/_update/new":
			return http.StatusOK, `{"_index":"memento","_id":"new","result":"updated"}`
		}
		return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
	})

	if _, err := c.Add(context.Background(), model.NewFact("new", "v", "config", "p", "test:author", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, want := range []string{
		`"author":"alice"`,
		`"author_source":"config"`,
		`"model":"strata"`,
		`"host":"tower"`,
		`"session":"s-42"`,
		`"updated_by":"alice"`,
	} {
		if !strings.Contains(putBody, want) {
			t.Errorf("create body lacks %s: %s", want, putBody)
		}
	}
	if len(fake.requestsFor(http.MethodPost, "/memento-history-2026.11/_doc")) == 0 {
		// the journal entry of the birth value carries the same labels
		// (month index asserted loosely: naming is pinned by journal tests)
		if !fake.sawAnyPrefix("/memento-history-") {
			t.Error("birth value was not journaled")
		}
	}
}

func TestAddCorrectionCarriesUpdatedBy(t *testing.T) {
	var updateBody, journalBody string
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) { cfg.Author = "bob" },
		func(r recordedRequest) (int, string) {
			if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Path, "/memento-history-") && strings.Contains(r.Path, "/_doc") {
				journalBody = r.Body
				return http.StatusOK, `{"_id":"j1","result":"created"}`
			}
			if code, body := journalRoutesOK(r); code != http.StatusInternalServerError {
				return code, body
			}
			switch {
			case r.Method == http.MethodGet && r.Path == "/_security/authinfo":
				return http.StatusNotFound, `{"status":404}`
			case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
				return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
					"key":"k","value":"old","author":"alice","author_source":"config",
					"category":"config","status":"verified"}}`
			case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
				updateBody = r.Body
				return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
		})

	if _, err := c.Add(context.Background(), model.NewFact("k", "new", "config", "p", "test:author", "")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.Contains(updateBody, `"updated_by":"bob"`) {
		t.Errorf("correction must stamp updated_by: %s", updateBody)
	}
	// The birth labels are write-once: the script params carry no top-level
	// author fields. (confirmed_by legitimately names its confirmers; the
	// assertion is about the fact-level birth labels, so it parses the params.)
	params := patchParams(t, updateBody)
	for _, forbidden := range []string{"author", "author_source"} {
		if _, has := params[forbidden]; has {
			t.Errorf("correction params carry the birth label %q (write-once): %+v", forbidden, params)
		}
	}
	// The journal entry names its own writer.
	if !strings.Contains(journalBody, `"author":"bob"`) {
		t.Errorf("journal entry must carry the author: %s", journalBody)
	}
}

func TestAddSameValueCarriesUpdatedBy(t *testing.T) {
	var updateBody string
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) { cfg.Author = "carol" },
		func(r recordedRequest) (int, string) {
			switch {
			case r.Method == http.MethodGet && r.Path == "/_security/authinfo":
				return http.StatusNotFound, `{"status":404}`
			case r.Method == http.MethodGet && r.Path == "/memento/_doc/k":
				return http.StatusOK, `{"_index":"memento","_id":"k","found":true,"_source":{
					"key":"k","value":"same","author":"alice","status":"verified"}}`
			case r.Method == http.MethodPost && r.Path == "/memento/_update/k":
				updateBody = r.Body
				return http.StatusOK, `{"_index":"memento","_id":"k","result":"updated"}`
			}
			return http.StatusInternalServerError, `{"status":500,"error":"unexpected ` + r.Path + `"}`
		})

	if _, err := c.Add(context.Background(), model.NewFact("k", "same", "config", "p", "test:author", "note")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.Contains(updateBody, `"updated_by":"carol"`) {
		t.Errorf("notes-only update must stamp updated_by: %s", updateBody)
	}
}

// sanitizeLabel is the filter for labels the config edge does not pass
// (server usernames, OS usernames, hostnames): non-printable runes — C0/C1
// controls, bidi overrides, zero-width format runes — would forge terminal
// output in get/journal/status listings.
func TestSanitizeLabelStripsNonPrintable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"alice", "alice"},
		{"al ice", "al ice"}, // spaces stay: they are printable
		{"a\u202eb", "ab"},   // bidi override
		{"a\u200bb", "ab"},   // zero-width space
		{"a\u0085b", "ab"},   // C1 control
		{"a\tb", "ab"},       // C0 control
		{"  alice  ", "alice"},
		{strings.Repeat("x", 201), ""}, // over-long: dropped whole
	} {
		if got := sanitizeLabel(tc.in); got != tc.want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A degraded chain (security probe failed: outage, plugin cold start) must
// be re-probed after a backoff: a long-lived MCP process may not freeze a
// transient outage as the writer identity forever. The probe never runs
// more than once per backoff window.
func TestAuthorChainSecurityProbeRetriedAfterBackoff(t *testing.T) {
	probeCalls := 0
	c, _ := newFakeClientCfg(t, func(cfg *config.Config) { cfg.Author = "cfg-user" },
		func(r recordedRequest) (int, string) {
			if r.Method == http.MethodGet && r.Path == "/_security/authinfo" {
				probeCalls++
				if probeCalls == 1 {
					// 403, not 503: the transport retries 5xx (the outage
					// would self-heal inside one probe call).
					return http.StatusForbidden, `{"status":403,"error":{"type":"security_exception","reason":"no privilege"}}`
				}
				return http.StatusOK, `{"user_name":"alice"}`
			}
			return http.StatusNotFound, `{"status":404}`
		})
	info := c.resolveAuthor(context.Background())
	if info.AuthorSource != AuthorSourceConfig {
		t.Fatalf("first resolve = %q, want config (a probe outage degrades the chain)", info.AuthorSource)
	}
	c.resolveAuthor(context.Background())
	if probeCalls != 1 {
		t.Fatalf("probes = %d, want 1: the degraded chain is cached inside the backoff window", probeCalls)
	}
	c.authorRetryAt = time.Now().Add(-time.Second) // simulate the window expiring
	info = c.resolveAuthor(context.Background())
	if info.Author != "alice" || info.AuthorSource != AuthorSourceSecurity {
		t.Errorf("after the backoff: author = %q/%q, want alice/security", info.Author, info.AuthorSource)
	}
	// A security-resolved identity is stable: no further probes scheduled.
	info = c.resolveAuthor(context.Background())
	if info.AuthorSource != AuthorSourceSecurity || probeCalls != 2 {
		t.Errorf("security identity must be cached: source=%q probes=%d", info.AuthorSource, probeCalls)
	}
}
