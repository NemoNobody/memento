package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"

	"github.com/spf13/cobra"
)

// statusEnvVars are the environment variables that override the config file.
var statusEnvVars = []string{
	"OPENSEARCH_URL",
	"OPENSEARCH_USER",
	"OPENSEARCH_PASSWORD",
	"OPENSEARCH_INSECURE_SKIP_VERIFY",
	"MEMENTO_INDEX",
}

// statusReport is the shared diagnostics payload behind `memento status` and the
// memento_status MCP tool. It never carries an error: it describes the situation
// so both a human and an agent can act on it.
type statusReport struct {
	OK           bool                 `json:"ok" jsonschema:"true when OpenSearch is reachable and the store is usable"`
	URL          string               `json:"url" jsonschema:"the OpenSearch URL that was tried"`
	Index        string               `json:"index" jsonschema:"the index name used for facts"`
	ConfigFile   string               `json:"config_file" jsonschema:"config file actually used, empty when none was found"`
	EnvOverrides []string             `json:"env_overrides,omitempty" jsonschema:"environment variables that override the config file"`
	ClusterName  string               `json:"cluster_name,omitempty" jsonschema:"OpenSearch cluster name, when reachable"`
	Version      string               `json:"version,omitempty" jsonschema:"OpenSearch version, when reachable"`
	Storage      *client.StorageState `json:"storage,omitempty" jsonschema:"cluster health, disk usage and read-only state; absent when the store is unreachable"`
	StorageNote  string               `json:"storage_note,omitempty" jsonschema:"what the health status means for this index, in words"`
	Error        string               `json:"error,omitempty" jsonschema:"why the store is not usable"`
	Hint         string               `json:"hint,omitempty" jsonschema:"what the user should do about it"`
}

// resolvedConfigPath returns the config file that would be used, given an
// optional explicit path.
func resolvedConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return config.FindConfigPath()
}

// setEnvOverrides records which environment variables are in effect, so
// diagnostics do not blame the config file for a value that came from the
// environment.
func setEnvOverrides(rep *statusReport) {
	for _, name := range statusEnvVars {
		if os.Getenv(name) != "" {
			rep.EnvOverrides = append(rep.EnvOverrides, name)
		}
	}
}

// envNote returns a human-readable suffix naming active env overrides.
func envNote(rep statusReport) string {
	if len(rep.EnvOverrides) == 0 {
		return ""
	}
	return fmt.Sprintf(" Note: %v are set in the environment and override the file.", rep.EnvOverrides)
}

// collectStatus gathers connectivity and configuration diagnostics. It never
// returns an error: an unavailable store is reported, not raised.
func collectStatus(ctx context.Context, confPath string) statusReport {
	rep := statusReport{ConfigFile: resolvedConfigPath(confPath)}
	setEnvOverrides(&rep)

	cfg, err := config.LoadConfigFrom(confPath)
	if err != nil {
		rep.Error = err.Error()
		rep.Hint = "Fix the config file (or point --conf / MEMENTO_CONF at a valid one). See docs/setup.md."
		return rep
	}
	rep.URL = cfg.OpenSearchURL
	rep.Index = cfg.IndexName

	c, err := client.NewClient(cfg)
	if err != nil {
		rep.Error = err.Error()
		rep.Hint = "The OpenSearch client could not be created; check opensearch_url. See docs/setup.md."
		return rep
	}

	info, err := c.Info(ctx)
	if err != nil {
		rep.Error = err.Error()
		rep.Hint = fmt.Sprintf(
			"OpenSearch is not reachable at %s. Check that the server is running and that "+
				"opensearch_url / opensearch_user / opensearch_password are correct (config file: %s).%s "+
				"If OpenSearch is not set up yet, see docs/setup.md — a single node is enough to start, "+
				"and nothing in memento assumes one or the other. "+
				"Report this to the user; do not retry in a loop.",
			cfg.OpenSearchURL, configPathOrNone(rep.ConfigFile), envNote(rep))
		return rep
	}

	rep.OK = true
	rep.ClusterName = info.ClusterName
	rep.Version = info.Version.Number
	rep.Storage = c.StorageState(ctx)
	rep.StorageNote = storageNote(rep.Storage)
	return rep
}

// storageNote explains the index health in words. It states what is true without
// assuming a cluster shape: whether "yellow" matters depends on the deployment,
// and that judgement belongs to the reader.
func storageNote(st *client.StorageState) string {
	if st == nil || st.IndexHealth == "" {
		return ""
	}
	switch st.IndexHealth {
	case "red":
		return "some primary shards are unassigned: parts of the index are unavailable"
	case "yellow":
		return "all primary shards are active; some replica shards are unassigned, so redundancy is reduced"
	case "green":
		return "all shards are active"
	}
	return ""
}

// configPathOrNone makes an empty path readable in messages.
func configPathOrNone(path string) string {
	if path == "" {
		return "none found"
	}
	return path
}

var statusFlags struct {
	conf string
}

// StatusCmd returns the `memento status` command: the same diagnostics as the
// memento_status MCP tool, for use from a shell.
func StatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check that OpenSearch is reachable and show the effective configuration",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			rep := collectStatus(context.Background(), statusFlags.conf)
			printStatus(rep)
			if !rep.OK {
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&statusFlags.conf, "conf", "",
		"config file to use (overrides MEMENTO_CONF and the default lookup)")
	return cmd
}

// printStatus renders a report for humans on stdout.
func printStatus(rep statusReport) {
	fmt.Printf("config file: %s\n", configPathOrNone(rep.ConfigFile))
	if len(rep.EnvOverrides) > 0 {
		fmt.Printf("env overrides: %v\n", rep.EnvOverrides)
	}
	if rep.URL == "" {
		fmt.Printf("status: NOT AVAILABLE\n  %s\n  hint: %s\n", rep.Error, rep.Hint)
		return
	}
	fmt.Printf("url: %s\n", rep.URL)
	fmt.Printf("index: %s\n", rep.Index)
	if rep.OK {
		fmt.Printf("cluster: %s (OpenSearch %s)\n", rep.ClusterName, rep.Version)
		printStorage(rep)
		fmt.Println("status: OK")
		return
	}
	fmt.Printf("status: NOT AVAILABLE\n  error: %s\n  hint: %s\n", rep.Error, rep.Hint)
}

// printStorage renders the storage state. Facts only — no verdicts about whether
// a given cluster shape is expected.
func printStorage(rep statusReport) {
	st := rep.Storage
	if st == nil {
		return
	}
	fmt.Printf("storage: cluster=%s index=%s unassigned_shards=%d data_nodes=%d\n",
		st.ClusterHealth, st.IndexHealth, st.Unassigned, st.DataNodes)
	if rep.StorageNote != "" {
		fmt.Printf("  %s\n", rep.StorageNote)
	}
	if st.WorstNode != "" {
		fmt.Printf("disk: busiest data node %s at %.0f%% used\n", st.WorstNode, st.WorstNodeUsedPercent)
	}
	if st.ReadOnlyIndex {
		fmt.Println("warning: the index has a read-only block — writes are rejected until it is cleared")
	}
	if st.Unavailable != "" {
		fmt.Printf("storage details unavailable: %s\n", st.Unavailable)
	}
}
