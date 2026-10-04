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
		Short: "Semantic search facts (needs embeddings)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("semantic: %q (top_k=%d)\n", args[0], semanticFlags.topK)
			fmt.Println("  semantic search needs an embedding model; not yet wired up")
		},
	}
	cmd.Flags().IntVarP(&semanticFlags.topK, "top-k", "k", 5, "number of results")
	return cmd
}
