package cmd

import (
	"context"
	"os"
	"strconv"

	"github.com/NemoNobody/memento/internal/config"

	"github.com/spf13/cobra"
)

var obsoleteFlags struct {
	allowSecret bool
}

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
			// Guard before any store contact (same order as MCP): a rejected
			// write must not create the index.
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			if err := guardCheck(cfg.SecretGuard, obsoleteFlags.allowSecret,
				guardField{"reason", reason}); err != nil {
				fatalErr(err)
			}
			c, _, err := newClientWithConfig()
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
	cmd.Flags().BoolVar(&obsoleteFlags.allowSecret, "allow-secret", false,
		"confirm the flagged text is not a secret (secret_guard=ask only, after user approval)")
	return cmd
}
