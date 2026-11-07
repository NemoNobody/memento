package cmd

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/NemoNobody/memento/internal/client"

	"github.com/spf13/cobra"
)

var keysFlags struct {
	match   string
	project string
	status  string
}

// KeysCmd returns the `memento keys` command: key-level navigation across the
// store. It answers "what is under this key subtree" (prefix) and "which keys
// mention this token" (match) without pulling fact content into the query.
func KeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys [prefix]",
		Short: "Find facts by key: prefix and/or substring, across projects",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			prefix := ""
			if len(args) == 1 {
				prefix = args[0]
			}
			// Validate before any store contact: a bad query is the caller's
			// problem, not an availability problem.
			if prefix == "" && keysFlags.match == "" {
				fatalErr(errors.New("keys: provide a key prefix and/or --match"))
			}
			if _, err := client.NormalizePrefix(prefix); err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			facts, err := c.Keys(ctx, prefix, keysFlags.match, keysFlags.project, resolveStatus(keysFlags.status))
			if err != nil {
				fatalErr(err)
			}
			if len(facts) == 0 {
				if err := writeLine(os.Stdout, "no matching keys"); err != nil {
					fatalErr(err)
				}
				return
			}
			if err := printFacts(os.Stdout, facts, time.Now().UTC()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVar(&keysFlags.match, "match", "",
		"key substring; * and ? work as wildcards")
	cmd.Flags().StringVar(&keysFlags.project, "project", "",
		"limit to this project (default: all projects)")
	cmd.Flags().StringVar(&keysFlags.status, "status", "",
		"limit results to this status: verified (default), obsolete, or any")
	return cmd
}
