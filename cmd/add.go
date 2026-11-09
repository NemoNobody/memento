package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"

	"github.com/spf13/cobra"
)

var addFlags struct {
	project     string
	source      string
	notes       string
	summary     string
	allowSecret bool
}

func AddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <key> <value> <category>",
		Short: "Add a fact",
		Args:  cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			key, value, category := args[0], args[1], args[2]
			// Validate the shape before guard and store: an empty key produces
			// a cryptic store error, and an unknown category never ages
			// (silent rot). The MCP server enforces the same checks.
			if strings.TrimSpace(key) == "" {
				fatalErr(errors.New("key must not be empty"))
			}
			if strings.TrimSpace(value) == "" {
				fatalErr(errors.New("value must not be empty"))
			}
			if !model.ValidCategory(category) {
				fatalErr(fmt.Errorf("unknown category %q (want location|config|endpoint|behavior|context_map)", category))
			}
			// Canonicalize for storage: the TTL table and the search category
			// filter are case-sensitive, so a mixed-case write would silently
			// never age and never match --category. ValidCategory accepted it
			// case-insensitively; store the lowercase canonical form.
			category = strings.ToLower(category)
			// Guard before any store contact: a rejected write must not even
			// create the index (the MCP server enforces the same order).
			cfg, err := config.LoadConfig()
			if err != nil {
				fatalErr(err)
			}
			if err := guardCheck(cfg.SecretGuard, addFlags.allowSecret,
				guardField{"key", key},
				guardField{"value", value},
				guardField{"notes", addFlags.notes},
				guardField{"summary", addFlags.summary},
				guardField{"source", addFlags.source}); err != nil {
				fatalErr(err)
			}
			c, err := newClientFromConfig(cfg)
			if err != nil {
				fatalErr(err)
			}
			ctx := context.Background()
			f := model.NewFact(key, value, category, addFlags.project, addFlags.source, addFlags.notes)
			f.Summary = addFlags.summary
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
	cmd.Flags().StringVar(&addFlags.summary, "summary", "",
		"short table-of-contents rendering of the value (under summary_max_len chars); required when the value is longer than summary_min_len; ignored when the value is unchanged (same-value re-add touches notes/source only)")
	cmd.Flags().BoolVar(&addFlags.allowSecret, "allow-secret", false,
		"confirm the flagged text is not a secret (secret_guard=ask only, after user approval)")
	return cmd
}
