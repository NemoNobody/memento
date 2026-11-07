package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/NemoNobody/memento/internal/model"

	"github.com/spf13/cobra"
)

var getFlags struct {
	history bool
}

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
			now := time.Now().UTC()
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
			fmt.Printf("age_days: %s\n", displayAge(f.AgeDays(now)))
			fmt.Printf("stale: %s\n", displayStale(f, now))
			fmt.Printf("updated_at: %s\n", f.UpdatedAt.Format("2006-01-02 15:04:05"))
			if !getFlags.history {
				return
			}
			if len(f.History) > 0 {
				fmt.Println("history:")
				for _, h := range f.History {
					fmt.Printf("  - %q @ %s [%s]\n", h.Value, displayTime(h.ObservedAt), h.Source)
				}
			}
		},
	}
	cmd.Flags().BoolVar(&getFlags.history, "history", false,
		"include the full history chain (off by default: history bloats the context)")
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

// displayAge renders the age in days, marking an unknown observation.
func displayAge(days int) string {
	if days < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", days)
}

// staleMark is the compact staleness marker for one-line listings; empty when
// the fact is fresh, so clean output stays clean.
func staleMark(f *model.Fact, now time.Time) string {
	if !f.Stale(now) {
		return ""
	}
	age := f.AgeDays(now)
	if age < 0 {
		return " [stale: observed_at unknown]"
	}
	return fmt.Sprintf(" [stale: %dd]", age)
}

// displayStale explains the staleness verdict: past-TTL facts carry the numbers
// so the reader knows exactly what to re-verify, and an unknown observation
// time is treated as stale (the age cannot be trusted).
func displayStale(f *model.Fact, now time.Time) string {
	ttl := model.TTLDays(f.Category)
	if ttl == 0 {
		return "false (category has no TTL)"
	}
	if f.ObservedAt.IsZero() {
		return fmt.Sprintf("true (observed_at unknown, ttl for %q is %dd)", f.Category, ttl)
	}
	if f.Stale(now) {
		return fmt.Sprintf("true (age %dd, ttl for %q is %dd) — re-verify before trusting",
			f.AgeDays(now), f.Category, ttl)
	}
	return "false"
}
