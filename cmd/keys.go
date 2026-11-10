package cmd

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
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
			// Normalize and validate before any store contact: a bad query is
			// the caller's problem, not an availability problem. The match and
			// project are trimmed at this edge too (the MCP keys tool does the
			// same): store keys and project labels are trimmed, so a padded
			// filter answers a silent zero, and a whitespace-only --match must
			// mean "no filter" in the empty-query check, not "query given".
			keysFlags.match = strings.TrimSpace(keysFlags.match)
			keysFlags.project = strings.TrimSpace(keysFlags.project)
			// The prefix normalizes before the empty-query check: a " " or "*"
			// prefix normalizes to "" and must count as "no query", not slip
			// through to the store's shape error (which reads like a failure
			// of the store, not of the caller).
			normPrefix, pErr := client.NormalizePrefix(prefix)
			if pErr != nil {
				fatalErr(pErr)
			}
			if normPrefix == "" && keysFlags.match == "" {
				fatalErr(errors.New("keys: provide a key prefix and/or --match"))
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			status, serr := resolveStatus(keysFlags.status)
			if serr != nil {
				fatalErr(serr)
			}
			facts, meta, err := c.Keys(ctx, normPrefix, keysFlags.match, keysFlags.project, status)
			if err != nil {
				fatalErr(err)
			}
			if len(facts) == 0 {
				if err := writeLine(os.Stdout, "no matching keys"); err != nil {
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
				if err := writeLine(os.Stdout, "truncated: showing the first "+strconv.Itoa(client.NavSizeCap)+" keys; narrow with prefix/--match/--project/--status"); err != nil {
					fatalErr(err)
				}
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
