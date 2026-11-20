package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/spf13/cobra"
)

func ListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <project>",
		Short: "List all facts for a project",
		Long: `List the facts of one project, sorted by key (stable navigation
order), every status — obsolete rows included: the CLI list is the human's
session-start view, and a retired fact's context belongs there (the MCP
velruna_list defaults to verified-only).

The MCP tool also defaults to updated_at desc — "what moved since the last
session" — with sort: "key" for this CLI's order; the asymmetry is
deliberate per audience, do not expect one to match the other by default.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			// Project labels are stored trimmed (write edges); the term
			// filter is whitespace-exact, so trim the query too.
			project := strings.TrimSpace(args[0])
			if project == "" {
				fatalErr(errors.New("project is required"))
			}
			// The prefix normalizes before any store contact (the keys
			// contract): a bad prefix is answered as a caller error, not as a
			// store failure after the index ensure.
			normPrefix, pErr := client.NormalizePrefix(listFlags.prefix)
			if pErr != nil {
				fatalErr(pErr)
			}
			listFlags.prefix = normPrefix
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			facts, meta, err := c.List(ctx, project, listFlags.prefix, "key")
			if err != nil {
				fatalErr(err)
			}
			if len(facts) == 0 {
				if err := writeLine(os.Stdout, "no facts for project "+strconv.Quote(project)); err != nil {
					fatalErr(err)
				}
				if err := noteSkipped(os.Stdout, meta); err != nil {
					fatalErr(err)
				}
				return
			}
			if err := printFacts(os.Stdout, facts, time.Now().UTC()); err != nil {
				fatalErr(err)
			}
			if err := noteSkipped(os.Stdout, meta); err != nil {
				fatalErr(err)
			}
			if meta.Total > client.NavSizeCap {
				if err := writeLine(os.Stdout, "truncated: showing the first "+strconv.Itoa(client.NavSizeCap)+" facts; narrow with --prefix"); err != nil {
					fatalErr(err)
				}
			}
		},
	}
	cmd.Flags().StringVar(&listFlags.prefix, "prefix", "",
		"keep only keys starting with this prefix (trailing * allowed), e.g. \"<project>:state:\"")
	// The status/sort asymmetry with velruna_list (verified-only,
	// updated-desc default) is a deliberate per-audience choice, stated in
	// the Long above — not drift.
	return cmd
}

var listFlags struct {
	prefix string
}

// printFacts renders one-line fact listings to w; shared by list and search.
// The line shows the summary when one is stored (the table-of-contents
// contract: listings stay skimmable without pulling long values), and the
// value for facts stored before summaries existed.
func printFacts(w io.Writer, facts []model.Fact, now time.Time) error {
	var b strings.Builder
	for _, f := range facts {
		b.WriteString("  ")
		// Control/zero-width runes never reach the terminal: a stored key or
		// value can carry them (add accepts arbitrary text) and a single ESC
		// sequence would forge everything after it in the listing.
		b.WriteString(displaySafe(f.Key, false))
		b.WriteString(": ")
		if f.Summary != "" {
			b.WriteString(displaySafe(f.Summary, false))
			b.WriteString(" [summary]")
		} else {
			b.WriteString(displaySafe(f.Value, false))
		}
		b.WriteString(" (status=")
		b.WriteString(strconv.Quote(f.Status))
		b.WriteString(")")
		b.WriteString(staleMark(&f, now))
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
