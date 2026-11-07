package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func ListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <project>",
		Short: "List all facts for a project",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			project := args[0]
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			facts, err := c.List(ctx, project)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if len(facts) == 0 {
				fmt.Printf("no facts for project %q\n", project)
				return
			}
			now := time.Now().UTC()
			for _, f := range facts {
				fmt.Printf("  %s: %s (status=%q)%s\n", f.Key, f.Value, f.Status, staleMark(&f, now))
			}
		},
	}
	return cmd
}
