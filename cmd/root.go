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
		PruneCmd(),
		ListCmd(),
		StatusCmd(),
		McpCmd(),
	)
	return root
}

// newClient creates a client and ensures the index exists.
func newClient() (*client.Client, error) {
	c, _, err := newClientWithConfig()
	return c, err
}

// newClientWithConfig is newClient plus the effective config, for commands
// that need configuration values beyond the connection (e.g. prune's default
// --keep from history_limit).
func newClientWithConfig() (*client.Client, *config.Config, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, nil, err
	}
	c, err := client.NewClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		return nil, nil, err
	}
	return c, cfg, nil
}
