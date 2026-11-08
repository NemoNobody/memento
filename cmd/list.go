package cmd

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/model"

	"github.com/spf13/cobra"
)

func ListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <project>",
		Short: "List all facts for a project",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			project := args[0]
			ctx := context.Background()
			c, err := newClient()
			if err != nil {
				fatalErr(err)
			}
			facts, meta, err := c.List(ctx, project, listFlags.prefix)
			if err != nil {
				fatalErr(err)
			}
			if len(facts) == 0 {
				if err := writeLine(os.Stdout, "no facts for project "+strconv.Quote(project)); err != nil {
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
				if err := writeLine(os.Stdout, "truncated: showing the first "+strconv.Itoa(client.NavSizeCap)+" facts; narrow with --prefix"); err != nil {
					fatalErr(err)
				}
			}
		},
	}
	cmd.Flags().StringVar(&listFlags.prefix, "prefix", "",
		"keep only keys starting with this prefix (trailing * allowed), e.g. \"<project>:state:\"")
	return cmd
}

var listFlags struct {
	prefix string
}

// printFacts renders one-line fact listings to w; shared by list and search.
func printFacts(w io.Writer, facts []model.Fact, now time.Time) error {
	var b strings.Builder
	for _, f := range facts {
		b.WriteString("  ")
		b.WriteString(f.Key)
		b.WriteString(": ")
		b.WriteString(f.Value)
		b.WriteString(" (status=")
		b.WriteString(strconv.Quote(f.Status))
		b.WriteString(")")
		b.WriteString(staleMark(&f, now))
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
