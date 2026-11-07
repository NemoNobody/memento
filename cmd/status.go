package cmd

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"

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
	"MEMENTO_HISTORY_LIMIT",
	"MEMENTO_SECRET_GUARD",
}

// statusReport is the shared diagnostics payload behind `memento status` and the
// memento_status MCP tool. It never carries an error: it describes the situation
// so both a human and an agent can act on it.
type statusReport struct {
	OK           bool                 `json:"ok" jsonschema:"true when OpenSearch is reachable and the store is usable"`
	URL          string               `json:"url" jsonschema:"the OpenSearch URL that was tried"`
	Index        string               `json:"index" jsonschema:"the index name used for facts"`
	HistoryLimit int                  `json:"history_limit" jsonschema:"per-key history retention applied on corrections; 0 keeps everything"`
	SecretGuard  string               `json:"secret_guard,omitempty" jsonschema:"secret guard level in effect on writes: reject | ask | allow"`
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
	return " Note: " + listNote(rep.EnvOverrides) + " are set in the environment and override the file."
}

// listNote renders a list of names the way fmt's %v does, without the
// reflection cost.
func listNote(names []string) string {
	return "[" + strings.Join(names, " ") + "]"
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
	rep.HistoryLimit = cfg.HistoryLimit
	rep.SecretGuard = cfg.SecretGuard

	c, err := client.NewClient(cfg)
	if err != nil {
		rep.Error = err.Error()
		rep.Hint = "The OpenSearch client could not be created; check opensearch_url. See docs/setup.md."
		return rep
	}

	info, err := c.Info(ctx)
	if err != nil {
		rep.Error = err.Error()
		var b strings.Builder
		b.WriteString("OpenSearch is not reachable at ")
		b.WriteString(cfg.OpenSearchURL)
		b.WriteString(". Check that the server is running and that opensearch_url / " +
			"opensearch_user / opensearch_password are correct (config file: ")
		b.WriteString(configPathOrNone(rep.ConfigFile))
		b.WriteString(").")
		b.WriteString(envNote(rep))
		b.WriteString(" If OpenSearch is not set up yet, see docs/setup.md — a single node is " +
			"enough to start, and nothing in memento assumes one or the other. " +
			"Report this to the user; do not retry in a loop.")
		rep.Hint = b.String()
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
			if err := printStatus(os.Stdout, rep); err != nil {
				fatalErr(err)
			}
			if !rep.OK {
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&statusFlags.conf, "conf", "",
		"config file to use (overrides MEMENTO_CONF and the default lookup)")
	return cmd
}

// printStatus renders a report for humans on w.
func printStatus(w io.Writer, rep statusReport) error {
	var b strings.Builder
	b.WriteString("config file: ")
	b.WriteString(configPathOrNone(rep.ConfigFile))
	b.WriteString("\n")
	if len(rep.EnvOverrides) > 0 {
		b.WriteString("env overrides: ")
		b.WriteString(listNote(rep.EnvOverrides))
		b.WriteString("\n")
	}
	if rep.URL == "" {
		b.WriteString("status: NOT AVAILABLE\n  ")
		b.WriteString(rep.Error)
		b.WriteString("\n  hint: ")
		b.WriteString(rep.Hint)
		b.WriteString("\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	b.WriteString("url: ")
	b.WriteString(rep.URL)
	b.WriteString("\nindex: ")
	b.WriteString(rep.Index)
	b.WriteString("\nhistory_limit: ")
	b.WriteString(strconv.Itoa(rep.HistoryLimit))
	b.WriteString("\nsecret_guard: ")
	b.WriteString(rep.SecretGuard)
	b.WriteString("\n")
	if rep.OK {
		b.WriteString("cluster: ")
		b.WriteString(rep.ClusterName)
		b.WriteString(" (OpenSearch ")
		b.WriteString(rep.Version)
		b.WriteString(")\n")
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
		if err := printStorage(w, rep); err != nil {
			return err
		}
		return writeLine(w, "status: OK")
	}
	b.WriteString("status: NOT AVAILABLE\n  error: ")
	b.WriteString(rep.Error)
	b.WriteString("\n  hint: ")
	b.WriteString(rep.Hint)
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// printStorage renders the storage state. Facts only — no verdicts about whether
// a given cluster shape is expected.
func printStorage(w io.Writer, rep statusReport) error {
	st := rep.Storage
	if st == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("storage: cluster=")
	b.WriteString(st.ClusterHealth)
	b.WriteString(" index=")
	b.WriteString(st.IndexHealth)
	b.WriteString(" unassigned_shards=")
	b.WriteString(strconv.Itoa(st.Unassigned))
	b.WriteString(" data_nodes=")
	b.WriteString(strconv.Itoa(st.DataNodes))
	b.WriteString("\n")
	if rep.StorageNote != "" {
		b.WriteString("  ")
		b.WriteString(rep.StorageNote)
		b.WriteString("\n")
	}
	if st.WorstNode != "" {
		b.WriteString("disk: busiest data node ")
		b.WriteString(st.WorstNode)
		b.WriteString(" at ")
		b.WriteString(strconv.FormatFloat(st.WorstNodeUsedPercent, 'f', 0, 64))
		b.WriteString("% used\n")
	}
	if st.ReadOnlyIndex {
		b.WriteString("warning: the index has a read-only block — writes are rejected until it is cleared\n")
	}
	if st.Unavailable != "" {
		b.WriteString("storage details unavailable: ")
		b.WriteString(st.Unavailable)
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
