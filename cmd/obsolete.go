package cmd

import (
	"context"
	"os"
	"strconv"

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
				fatalErr(err)
			}
			if err := c.Obsolete(ctx, key, reason); err != nil {
				fatalErr(err)
			}
			if err := writeLine(os.Stdout, "obsolete: key="+strconv.Quote(key)); err != nil {
				fatalErr(err)
			}
		},
	}
	return cmd
}
