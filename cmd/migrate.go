package cmd

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/model"
	"github.com/spf13/cobra"
)

var migrateFlags struct {
	project string
	yes     bool
}

// needsMigration reports whether one fact still carries work for the
// journal migration. A fact that is journal-linked (current_ref set) AND no
// longer carries the legacy history array is done. A linked fact that still
// carries the array is NOT done: an early migration pass or a post-upgrade
// edit linked the ref but left the array behind, and only this migration
// removes it.
func needsMigration(f model.Fact) bool {
	return f.CurrentRef == "" || len(f.History) > 0
}

// planMigrate computes the plan counters over a scanned page: facts to
// touch, journal entries the touch will append (the legacy entries, plus
// one for the current value only while the fact is still unlinked), and
// facts already done. Keeping the counters next to needsMigration pins
// plan/execute agreement: the plan a user approves must match what --yes
// writes.
func planMigrate(facts []model.Fact) (toMigrate, entries, skipped int) {
	for _, f := range facts {
		if !needsMigration(f) {
			skipped++
			continue
		}
		toMigrate++
		entries += len(f.History)
		if f.CurrentRef == "" {
			entries++
		}
	}
	return toMigrate, entries, skipped
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
			// Project labels are stored trimmed; the term filter is
			// whitespace-exact (the CLI trimming contract).
			migrateFlags.project = strings.TrimSpace(migrateFlags.project)
			facts, meta, err := c.ScanAllFacts(ctx, migrateFlags.project)
			if err != nil {
				fatalErr(err)
			}
			toMigrate, entries, skipped := planMigrate(facts)
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
				b.WriteString("already migrated (skipped): ")
				b.WriteString(strconv.Itoa(skipped))
				b.WriteString("\n")
			}
			if meta.Total > len(facts)+meta.Skipped {
				if len(facts) == 0 && meta.Skipped > 0 {
					// A corruption-only page: the scan restarts at the key head,
					// so reruns advance nothing — "rerun until the plan is empty"
					// would be an instruction that cannot be followed.
					b.WriteString("note: the whole page is undecodable (corruption): reruns cannot progress past it — inspect the facts index manually\n")
				} else {
					// Only a real page cut says "rerun": with the corrupt-history
					// arm, Total permanently exceeds what the page decoded even when
					// the page is complete (the remainder is undecodable, counted in
					// decode-skipped).
					b.WriteString("note: scan cap ")
					b.WriteString(strconv.Itoa(len(facts) + meta.Skipped))
					b.WriteString(" < total ")
					b.WriteString(strconv.Itoa(meta.Total))
					b.WriteString(" — rerun until the plan is empty\n")
				}
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
				if !needsMigration(f) {
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
