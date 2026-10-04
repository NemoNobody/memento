package cmd

import (
	"context"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"

	"github.com/spf13/cobra"
)

// NewRootCmd builds and returns the root command with all subcommands
// registered. It is safe to call multiple times: each call returns a fresh,
// independent command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "memento",
		Short: "Verified-fact memory",
	}
	root.AddCommand(
		SearchCmd(),
		GetCmd(),
		AddCmd(),
		ObsoleteCmd(),
		ListCmd(),
		SemanticCmd(),
		StatusCmd(),
		McpCmd(),
	)
	return root
}

// newClient creates a client and ensures the index exists.
func newClient() (*client.Client, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		return nil, err
	}
	return c, nil
}
