package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/model"
	"github.com/spf13/cobra"
)

var searchFlags struct {
	project  string
	category string
	status   string
	keyword  string
	limit    int
}

func SearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Keyword search facts",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			query := args[0]
			// Shape before store contact: an empty query, an invalid status or
			// a negative limit are the caller's problem. A negative limit would
			// read as "no cap" (0 = up to 1000) and silently widen the page; an
			// empty query answers a silent zero (navigation is what list/keys
			// are for — the search_docs edge refuses the same input).
			if strings.TrimSpace(query) == "" {
				fatalErr(errors.New("query is required"))
			}
			if searchFlags.limit < 0 {
				fatalErr(fmt.Errorf("search: --limit must be >= 0, got %d (0 = up to 1000)", searchFlags.limit))
			}
			if searchFlags.limit > client.NavSizeCap {
				// Clamp instead of forwarding: beyond the navigation cap the
				// cluster answers a raw 400 about result windows; the cap is the
				// honest ceiling of one page (the truncation note below says so),
				// and the stderr note keeps the caller from believing a capped
				// page was complete.
				searchFlags.limit = client.NavSizeCap
				if err := writeLine(os.Stderr, "note: --limit clamped to "+strconv.Itoa(client.NavSizeCap)+" (the navigation cap of one page)"); err != nil {
					fatalErr(err)
				}
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
			keyword := ""
			if searchFlags.keyword != "" {
				kw, kerr := model.NormalizeKeywords([]string{searchFlags.keyword})
				if kerr != nil {
					fatalErr(kerr)
				}
				if len(kw) == 0 {
					fatalErr(errors.New("--keyword must not be blank"))
				}
				keyword = kw[0]
			}
			facts, meta, err := c.Search(ctx, query, strings.TrimSpace(searchFlags.project), strings.ToLower(strings.TrimSpace(searchFlags.category)), status, keyword, searchFlags.limit)
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
				advice := "narrow the query (the page is already at the cap)"
				// limit==0 means "the whole page": it already sits at the cap,
				// so "raise --limit" would be advice that cannot be followed.
				if searchFlags.limit != 0 && searchFlags.limit < client.NavSizeCap {
					advice = "raise --limit (0 = up to " + strconv.Itoa(client.NavSizeCap) + ") or narrow the query"
				}
				if err := writeLine(os.Stdout, "truncated: showing the first "+strconv.Itoa(len(facts))+
					" of "+strconv.Itoa(meta.Total)+
					" matches; "+advice); err != nil {
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
	cmd.Flags().StringVar(&searchFlags.keyword, "keyword", "", "tag filter: exact keyword match (stored tags are lowercase single tokens)")
	cmd.Flags().IntVar(&searchFlags.limit, "limit", 10, "max hits to return (0 = up to 1000)")
	return cmd
}
