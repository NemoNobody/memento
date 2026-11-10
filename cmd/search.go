package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var searchFlags struct {
	project  string
	category string
	status   string
	limit    int
}

func SearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Keyword search facts",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			query := args[0]
			// Shape before store contact: an invalid status, a negative limit
			// or an empty query are the caller's problem. A negative limit
			// would read as "no cap" (0 = up to 1000) and silently widen the
			// page; an empty query answers a silent zero (navigation is what
			// list/keys are for — the search_docs edge refuses the same input).
			if strings.TrimSpace(query) == "" {
				fatalErr(errors.New("query is required"))
			}
			// Shape before store contact: an invalid status or a negative
			// limit are the caller's problem. A negative limit would read as
			// "no cap" (0 = up to 1000) and silently widen the page.
			if searchFlags.limit < 0 {
				fatalErr(fmt.Errorf("search: --limit must be >= 0, got %d (0 = up to 1000)", searchFlags.limit))
			}
			status, serr := resolveStatus(searchFlags.status)
			if serr != nil {
				fatalErr(serr)
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			// Write edges store the lowercase canonical category; the term
			// filter is case-sensitive and whitespace-exact, so canonicalize
			// the filter the same way (search -c " Config" must match "config").
			facts, meta, err := c.Search(ctx, query, strings.TrimSpace(searchFlags.project), strings.ToLower(strings.TrimSpace(searchFlags.category)), status, searchFlags.limit)
			if err != nil {
				fatalErr(err)
			}
			if len(facts) == 0 {
				if err := writeLine(os.Stdout, "no matches"); err != nil {
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
			if meta.Total > len(facts)+meta.Skipped {
				// The page is cut by the limit. Skipped hits are reported by
				// noteSkipped and are NOT recoverable by raising --limit, so
				// they must not count as "more matches to show".
				if err := writeLine(os.Stdout, "truncated: showing the first "+strconv.Itoa(len(facts))+
					" of "+strconv.Itoa(meta.Total)+
					" matches; raise --limit (0 = up to 1000) or narrow the query"); err != nil {
					fatalErr(err)
				}
			}
			if err := noteSkipped(os.Stdout, meta); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVarP(&searchFlags.project, "project", "p", "", "project name")
	cmd.Flags().StringVarP(&searchFlags.category, "category", "c", "", "category")
	cmd.Flags().StringVarP(&searchFlags.status, "status", "s", "verified", "status filter: verified, obsolete, any")
	cmd.Flags().IntVar(&searchFlags.limit, "limit", 10, "max hits to return (0 = up to 1000)")
	return cmd
}
