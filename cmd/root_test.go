package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// cmdCase describes one subcommand under test.
type cmdCase struct {
	name    string
	build   func() *cobra.Command
	use     string
	minArgs int
	maxArgs int
	flagDef map[string]string // flag name -> shorthand ("" if none)
}

func allCommands() []cmdCase {
	return []cmdCase{
		{"add", AddCmd, "add <key> <value> <category>", 3, 3,
			map[string]string{"project": "p", "source": "s", "notes": "n"}},
		{"get", GetCmd, "get <key>", 1, 1, nil},
		{"list", ListCmd, "list <project>", 1, 1, nil},
		{"obsolete", ObsoleteCmd, "obsolete <key> [reason]", 1, 2, nil},
		{"search", SearchCmd, "search <query>", 1, 1,
			map[string]string{"project": "p", "category": "c", "status": "s"}},
		{"semantic", SemanticCmd, "semantic <query>", 1, 1,
			map[string]string{"top-k": "k"}},
	}
}

func TestCommandUseStrings(t *testing.T) {
	for _, tc := range allCommands() {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.build().Use; got != tc.use {
				t.Errorf("Use = %q, want %q", got, tc.use)
			}
		})
	}
}

func TestArgsValidation(t *testing.T) {
	for _, tc := range allCommands() {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.build()
			if cmd.Args == nil {
				t.Fatal("Args validator is nil; command accepts anything")
			}

			ok := make([]string, tc.maxArgs)
			for i := range ok {
				ok[i] = "x"
			}
			// Max count must pass.
			if err := cmd.Args(cmd, ok); err != nil {
				t.Errorf("Args(%d) = %v, want nil", tc.maxArgs, err)
			}
			// Min count must also pass — it differs from max for range validators.
			if tc.minArgs != tc.maxArgs {
				if err := cmd.Args(cmd, ok[:tc.minArgs]); err != nil {
					t.Errorf("Args(%d) = %v, want nil", tc.minArgs, err)
				}
			}
			// One too few must fail (when at least one arg is required).
			if tc.minArgs > 1 {
				if err := cmd.Args(cmd, ok[:tc.minArgs-1]); err == nil {
					t.Errorf("Args(%d) = nil, want error", tc.minArgs-1)
				}
			}
			// Zero args must fail.
			if err := cmd.Args(cmd, nil); err == nil {
				t.Error("Args(0) = nil, want error")
			}
			// One too many must fail.
			tooMany := append(append([]string{}, ok...), "extra")
			if err := cmd.Args(cmd, tooMany); err == nil {
				t.Errorf("Args(%d) = nil, want error", tc.maxArgs+1)
			}
		})
	}
}

func TestCommandFlagShorthands(t *testing.T) {
	for _, tc := range allCommands() {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.build()
			for name, shorthand := range tc.flagDef {
				f := cmd.Flags().Lookup(name)
				if f == nil {
					t.Errorf("flag %q not defined", name)
					continue
				}
				if f.Shorthand != shorthand {
					t.Errorf("flag %q shorthand = %q, want %q", name, f.Shorthand, shorthand)
				}
			}
		})
	}
}

func TestAddCmdFlagDefaultsAndParse(t *testing.T) {
	t.Cleanup(func() {
		addFlags.project, addFlags.source, addFlags.notes = "global", "user:confirmed", ""
	})

	cmd := AddCmd()
	defaults := map[string]string{
		"project": "global",
		"source":  "user:confirmed",
		"notes":   "",
	}
	for name, want := range defaults {
		if got := cmd.Flags().Lookup(name).DefValue; got != want {
			t.Errorf("%s default = %q, want %q", name, got, want)
		}
	}

	if err := cmd.Flags().Parse([]string{"-p", "myproj", "-s", "tool:read_file", "-n", "dev only"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if addFlags.project != "myproj" {
		t.Errorf("project = %q, want myproj", addFlags.project)
	}
	if addFlags.source != "tool:read_file" {
		t.Errorf("source = %q, want tool:read_file", addFlags.source)
	}
	if addFlags.notes != "dev only" {
		t.Errorf("notes = %q, want %q", addFlags.notes, "dev only")
	}
}

func TestSearchCmdFlagDefaultsAndParse(t *testing.T) {
	t.Cleanup(func() {
		searchFlags.project, searchFlags.category, searchFlags.status = "", "", "verified"
	})

	cmd := SearchCmd()
	defaults := map[string]string{"project": "", "category": "", "status": "verified"}
	for name, want := range defaults {
		if got := cmd.Flags().Lookup(name).DefValue; got != want {
			t.Errorf("%s default = %q, want %q", name, got, want)
		}
	}

	if err := cmd.Flags().Parse([]string{"-p", "proj", "-c", "config", "-s", "obsolete"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if searchFlags.project != "proj" {
		t.Errorf("project = %q, want proj", searchFlags.project)
	}
	if searchFlags.category != "config" {
		t.Errorf("category = %q, want config", searchFlags.category)
	}
	if searchFlags.status != "obsolete" {
		t.Errorf("status = %q, want obsolete", searchFlags.status)
	}
}

func TestSemanticCmdFlagDefaultsAndParse(t *testing.T) {
	t.Cleanup(func() { semanticFlags.topK = 5 })

	cmd := SemanticCmd()
	if got := cmd.Flags().Lookup("top-k").DefValue; got != "5" {
		t.Errorf("top-k default = %q, want 5", got)
	}

	if err := cmd.Flags().Parse([]string{"-k", "10"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if semanticFlags.topK != 10 {
		t.Errorf("topK = %d, want 10", semanticFlags.topK)
	}
}

func TestRootCmdRegistersSubcommands(t *testing.T) {
	root := NewRootCmd()
	if root.Use != "memento" {
		t.Errorf("root Use = %q, want memento", root.Use)
	}
	want := map[string]bool{
		"add": false, "get": false, "list": false,
		"obsolete": false, "search": false, "semantic": false,
	}
	for _, c := range root.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q not registered", name)
		}
	}
}

func TestNewRootCmdIsIdempotent(t *testing.T) {
	first := len(NewRootCmd().Commands())
	second := len(NewRootCmd().Commands())
	if first != second {
		t.Errorf("command count changed across calls: %d then %d", first, second)
	}
}
