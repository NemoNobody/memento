package cmd

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var migrateFlags struct {
	project string
	yes     bool
}

func MigrateHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate-history",
		Short: "Move in-doc history arrays into the journal (one-time, big-bang)",
		Long: `The journal replaced the in-doc history array: every legacy array
becomes journal entries (op=write, oldest first, observed_at preserved),
the current value gets its own entry, current_ref links the fact to it,
and the array is removed from _source.

This is the release's one-time migration: run it once after upgrading.
Without --yes it only prints the plan (facts to touch, entries to append),
so the operator sees the scope before anything is written. The store is
personal by design — a big-bang migration is the honest shape; there are
no mixed readers to stage around.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			ctx := context.Background()
			c, _, err := newClientWithConfig()
			if err != nil {
				fatalErr(err)
			}
			facts, meta, err := c.ScanAllFacts(ctx, migrateFlags.project)
			if err != nil {
				fatalErr(err)
			}
			var toMigrate int
			var entries int
			var skipped int
			for _, f := range facts {
				if f.CurrentRef != "" {
					skipped++ // already journal-linked
					continue
				}
				toMigrate++
				entries += len(f.History) + 1
			}
			var b strings.Builder
			b.WriteString("facts scanned: ")
			b.WriteString(strconv.Itoa(len(facts)))
			b.WriteString(" (matches: ")
			b.WriteString(strconv.Itoa(meta.Total))
			b.WriteString(")")
			if meta.Skipped > 0 {
				b.WriteString(", decode-skipped: ")
				b.WriteString(strconv.Itoa(meta.Skipped))
			}
			b.WriteString("\n")
			b.WriteString("to migrate: ")
			b.WriteString(strconv.Itoa(toMigrate))
			b.WriteString(" facts → ")
			b.WriteString(strconv.Itoa(entries))
			b.WriteString(" journal entries\n")
			if skipped > 0 {
				b.WriteString("already linked (skipped): ")
				b.WriteString(strconv.Itoa(skipped))
				b.WriteString("\n")
			}
			if meta.Total > len(facts) {
				b.WriteString("note: scan cap ")
				b.WriteString(strconv.Itoa(len(facts)))
				b.WriteString(" < total ")
				b.WriteString(strconv.Itoa(meta.Total))
				b.WriteString(" — rerun until the plan is empty\n")
			}
			if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
				fatalErr(err)
			}
			if !migrateFlags.yes {
				if err := writeLine(os.Stdout, "plan only: re-run with --yes to migrate"); err != nil {
					fatalErr(err)
				}
				return
			}
			now := time.Now().UTC()
			var done, failed int
			for _, f := range facts {
				if f.CurrentRef != "" {
					continue
				}
				if _, err := c.MigrateFact(ctx, &f, now); err != nil {
					failed++
					if err := writeLine(os.Stderr, "migrate "+strconv.Quote(f.Key)+": "+err.Error()); err != nil {
						fatalErr(err)
					}
					continue
				}
				done++
			}
			out := "migrated: " + strconv.Itoa(done) + " facts"
			if failed > 0 {
				out += ", failed: " + strconv.Itoa(failed) + " (rerun — already-linked facts are skipped)"
			}
			if err := writeLine(os.Stdout, out); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVarP(&migrateFlags.project, "project", "p", "",
		"migrate only this project (default: all)")
	cmd.Flags().BoolVar(&migrateFlags.yes, "yes", false,
		"perform the migration (without it only the plan is printed)")
	return cmd
}
