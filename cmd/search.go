package cmd

import (
	"context"
	"os"
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
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			facts, err := c.Search(ctx, query, searchFlags.project, searchFlags.category, searchFlags.status, searchFlags.limit)
			if err != nil {
				fatalErr(err)
			}
			if len(facts) == 0 {
				if err := writeLine(os.Stdout, "no matches"); err != nil {
					fatalErr(err)
				}
				return
			}
			if err := printFacts(os.Stdout, facts, time.Now().UTC()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVarP(&searchFlags.project, "project", "p", "", "project name")
	cmd.Flags().StringVarP(&searchFlags.category, "category", "c", "", "category")
	cmd.Flags().StringVarP(&searchFlags.status, "status", "s", "verified", "status")
	cmd.Flags().IntVar(&searchFlags.limit, "limit", 10, "max hits to return (0 = no cap)")
	return cmd
}
