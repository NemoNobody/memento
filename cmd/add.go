package cmd

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/NemoNobody/memento/internal/model"

	"github.com/spf13/cobra"
)

var addFlags struct {
	project     string
	source      string
	notes       string
	allowSecret bool
}

func AddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <key> <value> <category>",
		Short: "Add a fact",
		Args:  cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			key, value, category := args[0], args[1], args[2]
			c, cfg, err := newClientWithConfig()
			if err != nil {
				fatalErr(err)
			}
			if err := guardCheck(cfg.SecretGuard, addFlags.allowSecret,
				guardField{"value", value},
				guardField{"notes", addFlags.notes},
				guardField{"source", addFlags.source}); err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			f := model.NewFact(key, value, category, addFlags.project, addFlags.source, addFlags.notes)
			created, err := c.Add(ctx, f)
			if err != nil {
				fatalErr(err)
			}
			verb := "updated"
			if created {
				verb = "added"
			}
			var b strings.Builder
			b.WriteString(verb)
			b.WriteString(" fact: key=")
			b.WriteString(strconv.Quote(key))
			b.WriteString(" category=")
			b.WriteString(strconv.Quote(category))
			b.WriteString(" project=")
			b.WriteString(strconv.Quote(addFlags.project))
			b.WriteString("\n")
			if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
				fatalErr(err)
			}
		},
	}
	cmd.Flags().StringVarP(&addFlags.project, "project", "p", "global", "project name")
	cmd.Flags().StringVarP(&addFlags.source, "source", "s", "user:confirmed",
		"where the fact came from (tool:read_file, experiment, doc:README)")
	cmd.Flags().StringVarP(&addFlags.notes, "notes", "n", "",
		"qualifiers: what was checked, how far, caveats")
	cmd.Flags().BoolVar(&addFlags.allowSecret, "allow-secret", false,
		"confirm the flagged text is not a secret (secret_guard=ask only, after user approval)")
	return cmd
}
