package cmd

import (
	"context"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/version"

	"github.com/spf13/cobra"
)

// NewRootCmd builds and returns the root command with all subcommands
// registered. It is safe to call multiple times: each call returns a fresh,
// independent command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "memento",
		Short:   "Verified-fact memory",
		Version: version.Version,
	}
	root.AddCommand(
		SearchCmd(),
		GetCmd(),
		KeysCmd(),
		AddCmd(),
		ObsoleteCmd(),
		PruneCmd(),
		ListCmd(),
		StatusCmd(),
		FetchCmd(),
		DocCmd(),
		DocsCmd(),
		CredsCmd(),
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
	c, err := newClientFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return c, cfg, nil
}

// newClientFromConfig builds a client for an already-loaded config: commands
// that guard the write path load the config once and hand it here, so the
// config (and the keyring, behind a locked wallet) is touched exactly once
// per command.
func newClientFromConfig(cfg *config.Config) (*client.Client, error) {
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
