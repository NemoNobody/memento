package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var searchFlags struct {
	project  string
	category string
	status   string
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
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			facts, err := c.Search(ctx, query, searchFlags.project, searchFlags.category, searchFlags.status)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if len(facts) == 0 {
				fmt.Println("no matches")
				return
			}
			now := time.Now().UTC()
			for _, f := range facts {
				fmt.Printf("  %s: %s (status=%q)%s\n", f.Key, f.Value, f.Status, staleMark(&f, now))
			}
		},
	}
	cmd.Flags().StringVarP(&searchFlags.project, "project", "p", "", "project name")
	cmd.Flags().StringVarP(&searchFlags.category, "category", "c", "", "category")
	cmd.Flags().StringVarP(&searchFlags.status, "status", "s", "verified", "status")
	return cmd
}
