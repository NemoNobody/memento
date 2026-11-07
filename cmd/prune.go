package cmd

import (
	"context"
	"fmt"
	"os"

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
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
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
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			total := len(f.History)
			if total <= keep {
				fmt.Printf("key %q has %d history entries, keep=%d: nothing to prune\n", key, total, keep)
				return
			}
			if !pruneFlags.yes {
				fmt.Printf("plan: key %q has %d history entries — keep the newest %d, drop %d.\n",
					key, total, keep, total-keep)
				fmt.Println("this removes the dropped observations for good; re-run with --yes to confirm")
				return
			}
			removed, err := c.Prune(ctx, key, keep)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("pruned key %q: dropped %d history entries, kept the newest %d\n", key, removed, keep)
		},
	}
	cmd.Flags().IntVar(&pruneFlags.keep, "keep", -1,
		"number of newest history entries to keep (default: history_limit from config)")
	cmd.Flags().BoolVar(&pruneFlags.yes, "yes", false,
		"confirm the deletion (without it only the plan is printed)")
	return cmd
}
