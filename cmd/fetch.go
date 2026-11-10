package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
	"github.com/NemoNobody/memento/internal/secretguard"

	"github.com/spf13/cobra"
)

var fetchFlags struct {
	refresh     bool
	project     string
	allowSecret bool
}

// FetchCmd returns the `memento fetch` command: download a page, extract its
// text into sections, store it in the docs index, print the outline. The page
// text stays in the store; only the outline enters the terminal (context
// economy: agents read sections on demand, not whole pages).
func FetchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fetch <url>",
		Short: "Fetch a page into the docs index and print its outline",
		Long: `Download a page, extract readable text into sections, store it in the
docs index, and print the outline (title, sections, hash). A second fetch of
the same URL returns the stored snapshot (cache) until its TTL expires;
--refresh forces a new download.

Only the outline is printed: section text is read on demand with
'memento doc <hash> <n>'. Requests send no cookies and no credentials.
A host outside fetch_allow_hosts is rejected before the request is made.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			// Validate before any store contact: a bad url or a denied host is
			// the caller's problem, not an availability problem. The config is
			// local (no network), and normalization needs fetch_allow_private.
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			rawURL, err := client.NormalizeURL(args[0], cfg.FetchAllowPrivate)
			if err != nil {
				fatalErr(err)
			}
			if err := client.HostAllowed(cfg, rawURL); err != nil {
				fatalErr(err)
			}
			if p, ok := client.GitHubLocal(cfg, rawURL); ok {
				// The content is in a local clone: print the path and stop.
				// No fetch, no store write: the repo mapping is machine-specific
				// and must never leak into the shared index.
				if err := writeLine(os.Stdout, "local: "+p); err != nil {
					fatalErr(err)
				}
				return
			}
			// The project tag is agent/user-supplied text that lands in a shared
			// index, and the cache-hit path re-tags a stored doc WITHOUT the
			// content guard: scan the tag here, once, for both paths. The tag
			// is trimmed before the guard and the store (MCP fetchDoc parity):
			// the docs term filter is exact on spaces, so a padded tag would
			// make the MCP search_docs edge miss the CLI's own documents.
			fetchFlags.project = strings.TrimSpace(fetchFlags.project)
			if err := guardCheck(cfg.SecretGuard, fetchFlags.allowSecret,
				guardField{Name: "project", Text: fetchFlags.project}); err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, err := newClientFromConfig(cfg)
			if err != nil {
				fatalErr(err)
			}
			doc, cached, err := c.FetchDoc(ctx, rawURL, fetchFlags.project, fetchFlags.refresh,
				func(d *model.Doc) ([]model.Redaction, error) {
					// The guard runs after extraction, before the doc is finalized:
					// page text is untrusted input and can carry credential-like
					// strings; the store is plaintext.
					return guardFetch(d, cfg.SecretGuard, fetchFlags.allowSecret)
				})
			if err != nil {
				fatalErr(err)
			}
			if err := printDocOutline(os.Stdout, doc, cached, time.Now().UTC()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().BoolVar(&fetchFlags.refresh, "refresh", false,
		"force a fresh download even when a stored snapshot exists")
	cmd.Flags().StringVarP(&fetchFlags.project, "project", "p", "",
		"tag the document with this project")
	cmd.Flags().BoolVar(&fetchFlags.allowSecret, "allow-secret", false,
		"confirm a secret-guard finding is a false positive (secret_guard=ask only)")
	return cmd
}

var docFlags struct {
	section int
	full    bool
}

// DocCmd returns the `memento doc` command: read one section (or the whole
// doc with --full) of a stored document by hash.
func DocCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doc <hash> [section]",
		Short: "Read a stored document section by hash",
		Args:  cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			// Hashes are looked up exactly; trim the argument like every
			// other key-shaped edge so " <hash> " finds the document. An empty
			// hash is refused here, not by the store (the MCP get_doc contract):
			// a store-side miss reads like a corrupt index, not a caller error.
			hash := strings.TrimSpace(args[0])
			if hash == "" {
				fatalErr(errors.New("hash is required"))
			}
			section := docFlags.section
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil {
					fatalErr(err)
				}
				section = n
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			doc, err := c.GetDocByHash(ctx, hash)
			if err != nil {
				fatalErr(err)
			}
			switch {
			case docFlags.full:
				if err := printDocFull(os.Stdout, doc, time.Now().UTC()); err != nil {
					fatalErr(err)
				}
			case section >= 0:
				for _, s := range doc.Sections {
					if s.Order == section {
						if err := writeSection(os.Stdout, doc, s, time.Now().UTC()); err != nil {
							fatalErr(err)
						}
						return
					}
				}
				fatalErr(errors.New("doc " + hash + " has no section " + strconv.Itoa(section) +
					" (see the outline with 'memento doc " + hash + "')"))
			default:
				if err := printDocOutline(os.Stdout, doc, false, time.Now().UTC()); err != nil {
					fatalErr(err)
				}
			}
		},
	}
	cmd.Flags().IntVar(&docFlags.section, "section", -1, "section number to print (from the outline)")
	cmd.Flags().BoolVar(&docFlags.full, "full", false, "print every section (bloats the context; prefer one section)")
	return cmd
}

var docsFlags struct {
	project string
	limit   int
}

// DocsCmd returns the `memento docs` command: search stored documents.
func DocsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs <query>",
		Short: "Search stored documents by text",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			// Shape before store contact (the MCP search_docs contract): an
			// empty query is a caller error, and a padded one would answer a
			// silent zero against the doc terms.
			query := strings.TrimSpace(args[0])
			if query == "" {
				fatalErr(errors.New("query is required"))
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			docs, meta, err := c.SearchDocs(ctx, query, strings.TrimSpace(docsFlags.project), docsFlags.limit)
			if err != nil {
				fatalErr(err)
			}
			if meta.Total > len(docs)+meta.Skipped {
				// The page is cut: say so, even when the limit is below the
				// cap (a silent 5-of-50 reads as "these are all the matches").
				// Skipped docs are reported by noteSkipped and are not
				// recoverable via --limit, so they do not count here.
				if err := writeLine(os.Stdout, "truncated: showing the first "+strconv.Itoa(len(docs))+
					" of "+strconv.Itoa(meta.Total)+" matching documents; raise --limit (max "+
					strconv.Itoa(client.DocsSearchCap)+", the context budget) or narrow the query"); err != nil {
					fatalErr(err)
				}
			}
			if len(docs) == 0 {
				if err := writeLine(os.Stdout, "no matching documents"); err != nil {
					fatalErr(err)
				}
				if err := noteSkipped(os.Stdout, meta); err != nil {
					fatalErr(err)
				}
				return
			}
			if err := printDocHits(os.Stdout, docs, time.Now().UTC()); err != nil {
				fatalErr(err)
			}
			if err := noteSkipped(os.Stdout, meta); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().IntVar(&docsFlags.limit, "limit", 5, "max documents to return")
	cmd.Flags().StringVarP(&docsFlags.project, "project", "p", "",
		"limit to documents tagged with this project")
	return cmd
}

// guardFetch enforces the secret guard on a fetched page before the store
// write. The policy is fixed by level x confidence (security posture, not
// per-request):
//
//   - high-confidence shapes in the agent-visible fields (title, section
//     text/headings, image alt/title) are REDACTED in place and the page is
//     stored: the secret never enters the plaintext store (the red line is
//     kept by the redaction), and the documentation page is saved. The
//     stored raw HTML is redacted the same way (agents never read it).
//   - medium-confidence findings in the agent-visible fields REFUSE the
//     write: these shapes fire on ordinary content (git SHAs, asset hashes,
//     config names), so silent redaction would mangle real documentation.
//     At secret_guard=ask a finding can be confirmed as a false positive
//     (--allow-secret / confirm_secret); at reject there is no bypass.
//   - allow: no scan at all (the operator opted out).
//
// Deliberate tradeoff (documented in README): high-confidence secrets end up
// stored redacted — the redaction must therefore out-cover a reject. Medium
// findings inside the stored HTML are left alone (agents never read it).
func guardFetch(doc *model.Doc, level string, allowed bool) ([]model.Redaction, error) {
	if level == config.GuardAllow {
		return nil, nil
	}
	var redactions []model.Redaction
	// The stored raw HTML: high-confidence shapes only, twice — the second
	// pass closes the latent overlap-tail gap (a span that starts inside an
	// earlier replacement is dropped by Redact's fold and must not survive
	// raw), and markers are a fixed point (the assignment rule refuses a
	// right-hand side that starts with '['). HTML redactions join the audit
	// trail (field "html": never served to agents, but the store surgery is
	// reported).
	var htmlFindings []secretguard.Finding
	doc.HTML, htmlFindings = secretguard.Redact("html", doc.HTML)
	if len(htmlFindings) > 0 {
		// The second pass only has work when the first one rewrote spans.
		var more []secretguard.Finding
		doc.HTML, more = secretguard.Redact("html", doc.HTML)
		htmlFindings = append(htmlFindings, more...)
	}
	for _, f := range htmlFindings {
		redactions = append(redactions, model.Redaction{Field: "html", Pattern: f.Pattern, Chars: f.Length})
	}

	redact := func(field, text string) string {
		// Same double pass as the stored HTML: the second pass catches a span
		// the first pass' fold dropped (a greedy span that swallowed part of
		// an adjacent credential). Redact reports only applied findings, so
		// the audit lines match what the text really lost.
		red, findings := secretguard.Redact(field, text)
		if len(findings) > 0 {
			var more []secretguard.Finding
			red, more = secretguard.Redact(field, red)
			findings = append(findings, more...)
		}
		for _, f := range findings {
			redactions = append(redactions, model.Redaction{Field: field, Pattern: f.Pattern, Chars: f.Length})
		}
		return red
	}
	doc.Title = redact("title", doc.Title)
	fields := []guardField{
		// The URL is identity: it cannot be redacted, and medium shapes are
		// ordinary in URLs (git SHAs in GitHub paths). Only the deterministic
		// credential shapes may refuse it.
		{Name: "url", Text: doc.URL, HighOnly: true},
		// The post-redirect URL carries the same identity role, and a redirect
		// target that bears credentials (admin:S3cr3t@host) is not a document
		// — refuse the doc, do not redact the audit trail.
		{Name: "final_url", Text: doc.FinalURL, HighOnly: true},
		{Name: "project", Text: doc.Project},
		{Name: "title", Text: doc.Title},
	}
	for i := range doc.Sections {
		doc.Sections[i].Heading = redact("section "+strconv.Itoa(i)+" heading", doc.Sections[i].Heading)
		doc.Sections[i].Text = redact("section "+strconv.Itoa(i), doc.Sections[i].Text)
		fields = append(fields,
			guardField{Name: "section " + strconv.Itoa(i) + " heading", Text: doc.Sections[i].Heading},
			guardField{Name: "section " + strconv.Itoa(i), Text: doc.Sections[i].Text})
	}
	for i := range doc.Images {
		// An href is documentation: the high-confidence credential shapes are
		// redacted in place (a basic-auth URL in src is a leak, not a link);
		// medium shapes (asset hashes) stay — refusing on them was the
		// false-positive class that made pages unusable.
		doc.Images[i].URL = redact("image "+strconv.Itoa(i)+" url", doc.Images[i].URL)
		doc.Images[i].Alt = redact("image "+strconv.Itoa(i)+" alt", doc.Images[i].Alt)
		doc.Images[i].Title = redact("image "+strconv.Itoa(i)+" title", doc.Images[i].Title)
		fields = append(fields,
			guardField{Name: "image " + strconv.Itoa(i) + " alt", Text: doc.Images[i].Alt},
			guardField{Name: "image " + strconv.Itoa(i) + " title", Text: doc.Images[i].Title})
	}

	// The post-redaction scan sees only medium shapes (the high spans are
	// already markers): refuse per the ask/reject policy.
	if err := guardCheck(level, allowed, fields...); err != nil {
		return nil, err
	}
	return redactions, nil
}

// printDocOutline renders the title + section table: the context-cheap view an
// agent reads to decide which section to pull.
func printDocOutline(w io.Writer, doc *model.Doc, cached bool, now time.Time) error {
	var b strings.Builder
	b.WriteString("url: ")
	b.WriteString(doc.URL)
	b.WriteString("\n")
	b.WriteString("hash: ")
	b.WriteString(doc.URLHash)
	b.WriteString("\n")
	b.WriteString("title: ")
	b.WriteString(doc.Title)
	b.WriteString("\n")
	b.WriteString("fetched_at: ")
	b.WriteString(displayTime(doc.FetchedAt))
	b.WriteString("\n")
	b.WriteString("expires_at: ")
	b.WriteString(displayTime(doc.ExpiresAt))
	b.WriteString("\n")
	b.WriteString("stale: ")
	b.WriteString(strconv.FormatBool(doc.Stale(now)))
	b.WriteString("\n")
	if doc.Via != "" && doc.Via != client.ViaDirect {
		// The route is audit: proxy/obscura fetched this, not the direct path.
		b.WriteString("via: ")
		b.WriteString(doc.Via)
		b.WriteString("\n")
	}
	if doc.Project != "" {
		b.WriteString("project: ")
		b.WriteString(doc.Project)
		b.WriteString("\n")
	}
	if cached {
		b.WriteString("cached: true\n")
	}
	if doc.Truncated {
		b.WriteString("truncated: true (extraction hit a limit; the stored text is a prefix)\n")
	}
	if len(doc.Redactions) > 0 {
		b.WriteString("redacted: ")
		b.WriteString(strconv.Itoa(len(doc.Redactions)))
		b.WriteString(" high-confidence credential span(s) were redacted out of the stored snapshot (the page is stored; pattern/field/size listed, no preview)\n")
		for _, r := range doc.Redactions {
			b.WriteString("  - ")
			b.WriteString(r.Pattern)
			b.WriteString(" in ")
			b.WriteString(r.Field)
			b.WriteString(" (")
			b.WriteString(strconv.Itoa(r.Chars))
			b.WriteString(" chars)\n")
		}
	}
	b.WriteString("sections:\n")
	for _, s := range doc.Sections {
		b.WriteString("  [")
		b.WriteString(strconv.Itoa(s.Order))
		b.WriteString("] ")
		b.WriteString(s.Heading)
		b.WriteString(" (")
		b.WriteString(strconv.Itoa(len([]rune(s.Text))))
		b.WriteString(" chars)\n")
	}
	b.WriteString("read a section: memento doc ")
	b.WriteString(doc.URLHash)
	b.WriteString(" <n>\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// printDocFull prints every section; it is the escape hatch, not the default.
func printDocFull(w io.Writer, doc *model.Doc, now time.Time) error {
	if err := printDocOutline(w, doc, false, now); err != nil {
		return err
	}
	for _, s := range doc.Sections {
		if err := writeSection(w, doc, s, now); err != nil {
			return err
		}
	}
	return nil
}

func writeSection(w io.Writer, doc *model.Doc, s model.Section, now time.Time) error {
	var b strings.Builder
	b.WriteString("--- section [")
	b.WriteString(strconv.Itoa(s.Order))
	b.WriteString("] ")
	b.WriteString(s.Heading)
	b.WriteString(" (fetched ")
	b.WriteString(displayTime(doc.FetchedAt))
	if doc.Stale(now) {
		b.WriteString(", STALE: re-fetch before quoting")
	}
	b.WriteString(") ---\n")
	b.WriteString(s.Text)
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// printDocHits renders search results: the hash first so the reader can pull
// the section straight away.
func printDocHits(w io.Writer, docs []model.Doc, now time.Time) error {
	var b strings.Builder
	for _, d := range docs {
		b.WriteString(d.URLHash)
		b.WriteString("  ")
		b.WriteString(d.Title)
		if d.Stale(now) {
			b.WriteString(" [stale]")
		}
		b.WriteString("\n  ")
		b.WriteString(d.URL)
		b.WriteString("\n")
		if len(d.Sections) > 0 {
			b.WriteString("  ")
			b.WriteString(snippet(d.Sections[0].Text, 200))
			b.WriteString("\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// snippet collapses whitespace and caps the text at n runes.
func snippet(text string, n int) string {
	r := []rune(text)
	if len(r) > n {
		r = append(r[:n], '…')
	}
	return strings.Join(strings.Fields(string(r)), " ")
}
