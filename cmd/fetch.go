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
			// the caller's problem, not an availability problem.
			rawURL, err := client.NormalizeURL(args[0])
			if err != nil {
				fatalErr(err)
			}
			cfg, err := config.LoadConfig()
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
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			doc, cached, err := c.FetchDoc(ctx, rawURL, fetchFlags.project, fetchFlags.refresh,
				func(d *model.Doc) error {
					// The guard runs after extraction, before the store write:
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
			hash := args[0]
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
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			docs, err := c.SearchDocs(ctx, args[0], docsFlags.project, docsFlags.limit)
			if err != nil {
				fatalErr(err)
			}
			if len(docs) == 0 {
				if err := writeLine(os.Stdout, "no matching documents"); err != nil {
					fatalErr(err)
				}
				return
			}
			if err := printDocHits(os.Stdout, docs, time.Now().UTC()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().IntVar(&docsFlags.limit, "limit", 5, "max documents to return")
	cmd.Flags().StringVarP(&docsFlags.project, "project", "p", "",
		"limit to documents tagged with this project")
	return cmd
}

// guardFetch scans the extracted text with the secret guard before the store
// write. Page text is untrusted: a scraped page can quote credentials, and
// every later read of the doc would expose them (the store is plaintext).
func guardFetch(doc *model.Doc, level string, allowed bool) error {
	fields := []guardField{
		{Name: "title", Text: doc.Title},
		{Name: "url", Text: doc.URL},
	}
	for i, s := range doc.Sections {
		fields = append(fields, guardField{Name: "section " + strconv.Itoa(i), Text: s.Text})
	}
	return guardCheck(level, allowed, fields...)
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
