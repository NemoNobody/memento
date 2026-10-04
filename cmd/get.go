package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func GetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Get fact by key",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			key := args[0]
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			f, err := c.Get(ctx, key)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("key: %q\n", f.Key)
			fmt.Printf("value: %s\n", f.Value)
			fmt.Printf("category: %q\n", f.Category)
			fmt.Printf("project: %q\n", f.Project)
			fmt.Printf("status: %q\n", f.Status)
			fmt.Printf("source: %q\n", f.Source)
			if f.Notes != "" {
				fmt.Printf("notes: %s\n", f.Notes)
			}
			fmt.Printf("observed_at: %s\n", displayTime(f.ObservedAt))
			fmt.Printf("updated_at: %s\n", f.UpdatedAt.Format("2006-01-02 15:04:05"))
			if len(f.History) > 0 {
				fmt.Println("history:")
				for _, h := range f.History {
					fmt.Printf("  - %q @ %s [%s]\n", h.Value, displayTime(h.ObservedAt), h.Source)
				}
			}
		},
	}
	return cmd
}

// displayTime renders a timestamp for humans, or "unknown" for the zero time
// (facts created before observed_at existed).
func displayTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format("2006-01-02 15:04:05")
}
