package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/NemoNobody/velruna/internal/client"
	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/dashboards"

	"github.com/spf13/cobra"
)

var dashFlags struct {
	conf  string
	index string
	file  string
}

// DashboardsCmd returns the `velruna dashboards` command group: maintenance of
// the OpenSearch Dashboards side of the store.
func DashboardsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboards",
		Short: "Install the ready-made OpenSearch Dashboards bundle",
	}
	cmd.AddCommand(dashboardsPushCmd())
	return cmd
}

func dashboardsPushCmd() *cobra.Command {
	push := &cobra.Command{
		Use:   "push",
		Short: "Write the overview bundle into the global Dashboards tenant",
		Long: `Write the bundled index patterns, visualizations and the "Velruna — store
overview" dashboard into the global tenant.

The objects are written straight into the saved-objects index behind the
.kibana alias, with their stable velruna-store-* ids, so the command is
re-runnable: a second run overwrites the same documents instead of creating
duplicates. The saved-objects REST API is not used because it cannot target the
global tenant with basic-auth credentials (the tenant follows the browser
cookie, so an import silently lands in the personal tenant).

Credentials and the address come from the same config, keyring and environment
chain as every other command. The Dashboards index only exists after
OpenSearch Dashboards has started at least once against the cluster.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cfg, err := loadDashboardsConfig()
			if err != nil {
				fatalErr(err)
			}
			// No EnsureIndex: pushing dashboards must not create store indices
			// as a side effect (it is a read-mostly maintenance action).
			c, err := client.NewClient(cfg)
			if err != nil {
				fatalErr(err)
			}
			bundle := dashboards.Bundle()
			if dashFlags.file != "" {
				bundle, err = os.ReadFile(dashFlags.file)
				if err != nil {
					fatalErr(fmt.Errorf("read bundle: %w", err))
				}
			}
			res, err := c.PushDashboards(context.Background(), bundle, dashFlags.index)
			if err != nil {
				fatalErr(err)
			}
			if werr := printDashboardPush(res, cfg.OpenSearchURL); werr != nil {
				fatalErr(werr)
			}
			if res.Failed > 0 {
				os.Exit(1)
			}
		},
	}
	push.Flags().StringVar(&dashFlags.conf, "conf", "",
		"config file to use (overrides VELRUNA_CONF and the default lookup)")
	push.Flags().StringVar(&dashFlags.index, "index", "",
		"saved-objects write index (default: resolve the .kibana alias)")
	push.Flags().StringVar(&dashFlags.file, "file", "",
		"bundle to push (default: the one embedded in this binary)")
	return push
}

func loadDashboardsConfig() (*config.Config, error) {
	if dashFlags.conf != "" {
		return config.LoadConfigFrom(dashFlags.conf)
	}
	return config.LoadConfig()
}

// printDashboardPush reports what landed and how to see it. The tenant and
// reload notes are not decoration: the UI keeps the index-pattern list per
// session, so a fresh push looks absent until the tab is reloaded, and any
// tenant other than Global shows the personal copy instead.
func printDashboardPush(res *client.DashboardPush, url string) error {
	if err := writeLine(os.Stdout, fmt.Sprintf("indexed %d saved object(s) into %s (global tenant) at %s",
		res.Indexed, res.Index, url)); err != nil {
		return err
	}
	for _, e := range res.Errors {
		if _, werr := fmt.Fprintf(os.Stderr, "failed: %s\n", e); werr != nil {
			return werr
		}
	}
	if res.Failed > 0 {
		if _, werr := fmt.Fprintf(os.Stderr, "%d object(s) failed\n", res.Failed); werr != nil {
			return werr
		}
		return nil
	}
	return writeLine(os.Stdout, "open Dashboards, pick the Global tenant in the tenant switcher and "+
		"hard-reload the tab (the UI caches the index-pattern list per session)")
}
