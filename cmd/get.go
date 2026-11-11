package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/memento/internal/client"
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
			key := strings.TrimSpace(args[0])
			if key == "" {
				fatalErr(errors.New("key is required"))
			}
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			f, err := c.Get(ctx, key)
			if err != nil {
				fatalIfNotFound(err)
			}
			// --history on a journal-era fact must not print nothing: the
			// in-doc array is empty there, the chain lives in the journal
			// indices. Fetch it (best-effort: a journal read failure is
			// reported under the fact, not a fatal error after the fact).
			var entries []model.JournalEntry
			var jTotal int
			var jErr error
			if getFlags.history && len(f.History) == 0 {
				var jMeta client.JournalMeta
				entries, jMeta, jErr = c.JournalEntries(ctx, key, 20)
				jTotal = jMeta.Total
			}
			if err := printFact(os.Stdout, f, time.Now().UTC(), getFlags.history, entries, jTotal, jErr); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().BoolVar(&getFlags.history, "history", false,
		"include the full history chain (off by default: history bloats the context)")
	return cmd
}

// printFact renders one fact for humans on w. The journal entries (and
// their total) come from the caller so this function stays pure and
// testable: journal-era facts have no in-doc history array, and --history
// renders the fetched journal instead (or says explicitly that there is
// nothing, rather than printing nothing).
func printFact(w io.Writer, f *model.Fact, now time.Time, history bool, entries []model.JournalEntry, jTotal int, jErr error) error {
	var b strings.Builder
	b.WriteString("key: ")
	b.WriteString(strconv.Quote(f.Key))
	b.WriteString("\n")
	b.WriteString("value: ")
	b.WriteString(displaySafe(f.Value, true))
	b.WriteString("\n")
	// The summary is the table of contents of a long value: show it next to
	// the value (stored facts predate the field, so it is often empty).
	if f.Summary != "" {
		b.WriteString("summary: ")
		b.WriteString(displaySafe(f.Summary, false))
		b.WriteString("\n")
	}
	b.WriteString("category: ")
	b.WriteString(strconv.Quote(f.Category))
	b.WriteString("\n")
	b.WriteString("project: ")
	b.WriteString(strconv.Quote(f.Project))
	b.WriteString("\n")
	b.WriteString("status: ")
	b.WriteString(strconv.Quote(f.Status))
	b.WriteString("\n")
	b.WriteString("source: ")
	b.WriteString(strconv.Quote(f.Source))
	b.WriteString("\n")
	// Authorship is self-reported (audit metadata, not authentication): show
	// who wrote the fact and which chain link named them.
	if f.Author != "" {
		b.WriteString("author: ")
		b.WriteString(strconv.Quote(f.Author))
		b.WriteString(" (")
		b.WriteString(f.AuthorSource)
		b.WriteString(")\n")
	}
	if f.UpdatedBy != "" && f.UpdatedBy != f.Author {
		b.WriteString("updated_by: ")
		b.WriteString(strconv.Quote(f.UpdatedBy))
		b.WriteString("\n")
	}
	if f.Notes != "" {
		b.WriteString("notes: ")
		b.WriteString(displaySafe(f.Notes, false))
		b.WriteString("\n")
	}
	b.WriteString("observed_at: ")
	b.WriteString(displayTime(f.ObservedAt))
	b.WriteString("\n")
	b.WriteString("age_days: ")
	b.WriteString(displayAge(f.AgeDays(now)))
	b.WriteString("\n")
	b.WriteString("stale: ")
	b.WriteString(displayStale(f, now))
	b.WriteString("\n")
	b.WriteString("updated_at: ")
	b.WriteString(displayTime(f.UpdatedAt))
	b.WriteString("\n")
	if history {
		if len(f.History) > 0 {
			b.WriteString("history:\n")
			for _, h := range f.History {
				b.WriteString("  - ")
				b.WriteString(strconv.Quote(h.Value))
				b.WriteString(" @ ")
				b.WriteString(displayTime(h.ObservedAt))
				b.WriteString(" [")
				b.WriteString(h.Source)
				b.WriteString("]\n")
			}
		} else if jErr != nil {
			b.WriteString("history: journal read failed: ")
			b.WriteString(jErr.Error())
			b.WriteString("\n")
		} else if jTotal > 0 {
			b.WriteString("history (journal, newest first): ")
			b.WriteString(strconv.Itoa(jTotal))
			b.WriteString(" entries")
			if jTotal > len(entries) {
				b.WriteString(" — showing the newest ")
				b.WriteString(strconv.Itoa(len(entries)))
				b.WriteString(" (see `memento journal ")
				b.WriteString(strconv.Quote(f.Key))
				b.WriteString("`)")
			}
			b.WriteString("\n")
			for _, e := range entries {
				b.WriteString("  - ")
				b.WriteString(displayTime(e.CreatedAt))
				b.WriteString(" [")
				b.WriteString(e.Op)
				b.WriteString("] value=")
				b.WriteString(strconv.Quote(e.Value))
				b.WriteString("\n")
			}
		} else {
			b.WriteString("history: none (no in-doc chain, no journal entries)\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// displayTime renders a timestamp for humans, or "unknown" for the zero
// time (facts created before observed_at existed; an unset updated_at
// renders the same way instead of year 1). The trailing Z states the
// obvious — every stored timestamp is UTC — so listings from different
// machines cannot be read as local time.
func displayTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

// displayAge renders the age in days, marking an unknown observation.
func displayAge(days int) string {
	if days < 0 {
		return "unknown"
	}
	return strconv.Itoa(days)
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
	return " [stale: " + strconv.Itoa(age) + "d]"
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
		return "true (observed_at unknown, ttl for " + strconv.Quote(f.Category) + " is " + strconv.Itoa(ttl) + "d)"
	}
	if f.Stale(now) {
		var b strings.Builder
		b.WriteString("true (age ")
		b.WriteString(strconv.Itoa(f.AgeDays(now)))
		b.WriteString("d, ttl for ")
		b.WriteString(strconv.Quote(f.Category))
		b.WriteString(" is ")
		b.WriteString(strconv.Itoa(ttl))
		b.WriteString("d) — re-verify before trusting")
		return b.String()
	}
	return "false"
}
