package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var semanticFlags struct {
	topK int
}

func SemanticCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "semantic <query>",
		Short: "Semantic search facts (not available yet)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("semantic search is not available yet: it needs an embedding model")
		},
	}
	cmd.Flags().IntVarP(&semanticFlags.topK, "top-k", "k", 5, "number of results")
	return cmd
}
