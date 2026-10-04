package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func ObsoleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "obsolete <key> [reason]",
		Short: "Mark a fact obsolete because its subject is gone",
		Args:  cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			key := args[0]
			reason := ""
			if len(args) == 2 {
				reason = args[1]
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if err := c.Obsolete(ctx, key, reason); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("obsolete: key=%q\n", key)
		},
	}
	return cmd
}
