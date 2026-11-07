package cmd

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var pruneFlags struct {
	keep int
	yes  bool
}

func PruneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune <key>",
		Short: "Drop old history entries, keeping the newest N (destructive)",
		Long: `Drop history entries beyond the newest N. The current value is never touched.

Pruning is destructive: the dropped observations are gone (history has no
undo). Without --yes nothing is written: the command prints the plan and exits,
so the operator sees exactly what will be lost. This is the barrier an agent
must respect — show the plan to the user, then re-run with --yes only after
their approval.`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			key := args[0]
			ctx := context.Background()
			c, cfg, err := newClientWithConfig()
			if err != nil {
				fatalErr(err)
			}
			keep := pruneFlags.keep
			if keep <= 0 {
				keep = cfg.HistoryLimit
				if keep <= 0 {
					keep = 1
				}
			}
			f, err := c.Get(ctx, key)
			if err != nil {
				fatalErr(err)
			}
			total := len(f.History)
			if total <= keep {
				line := "key " + strconv.Quote(key) + " has " + strconv.Itoa(total) +
					" history entries, keep=" + strconv.Itoa(keep) + ": nothing to prune"
				if err := writeLine(os.Stdout, line); err != nil {
					fatalErr(err)
				}
				return
			}
			if !pruneFlags.yes {
				var b strings.Builder
				b.WriteString("plan: key ")
				b.WriteString(strconv.Quote(key))
				b.WriteString(" has ")
				b.WriteString(strconv.Itoa(total))
				b.WriteString(" history entries — keep the newest ")
				b.WriteString(strconv.Itoa(keep))
				b.WriteString(", drop ")
				b.WriteString(strconv.Itoa(total - keep))
				b.WriteString(".")
				b.WriteString("\nthis removes the dropped observations for good; re-run with --yes to confirm")
				if err := writeLine(os.Stdout, b.String()); err != nil {
					fatalErr(err)
				}
				return
			}
			removed, err := c.Prune(ctx, key, keep)
			if err != nil {
				fatalErr(err)
			}
			line := "pruned key " + strconv.Quote(key) + ": dropped " + strconv.Itoa(removed) +
				" history entries, kept the newest " + strconv.Itoa(keep)
			if err := writeLine(os.Stdout, line); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().IntVar(&pruneFlags.keep, "keep", -1,
		"number of newest history entries to keep (default: history_limit from config)")
	cmd.Flags().BoolVar(&pruneFlags.yes, "yes", false,
		"confirm the deletion (without it only the plan is printed)")
	return cmd
}
