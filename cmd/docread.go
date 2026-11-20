package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/NemoNobody/memento/internal/client"
	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

// Per-call budgets. They bound one answer, not the store: the document keeps
// every stored block, and the agent reads what fits.
const (
	outlineRows   = 60    // outline lines before the answer summarizes the rest
	docReadBudget = 32000 // runes of block text in one read
	maxReadBlocks = 10    // blocks a single read may request
)

// resolveExtractLimits applies per-call overrides on top of the configured
// defaults (fetch_block_chars, fetch_doc_chars). A zero maxChars means "store
// the whole document"; negative values are refused here because the MCP and CLI
// surfaces are the user boundary.
func resolveExtractLimits(cfg *config.Config, blockChars, maxChars *int) (client.ExtractLimits, error) {
	limits := client.ExtractLimits{BlockChars: cfg.FetchBlockChars, MaxChars: cfg.FetchDocChars}
	if blockChars != nil {
		limits.BlockChars = *blockChars
	}
	if maxChars != nil {
		limits.MaxChars = *maxChars
	}
	if limits.BlockChars < 1000 || limits.BlockChars > 64000 {
		return limits, fmt.Errorf("block_chars must be between 1000 and 64000, got %d", limits.BlockChars)
	}
	if limits.MaxChars < 0 {
		return limits, fmt.Errorf("max_chars must be 0 or greater, got %d", limits.MaxChars)
	}
	return limits, nil
}

// readDocBlocks returns the text of up to want consecutive blocks starting at
// start (orders below start are skipped), each with a header. included counts the
// blocks in the answer; next is the order to continue from, or -1 when the
// document ends there. The answer never exceeds budget runes, except for the
// first block: an empty answer would hide the document.
func readDocBlocks(doc *model.Doc, start, want, budget int) (text string, included, next int) {
	if want < 1 {
		want = 1
	}
	var b strings.Builder
	used := 0
	next = -1
	for _, sec := range doc.Sections {
		if sec.Order < start {
			continue
		}
		if included >= want {
			next = sec.Order
			break
		}
		n := len([]rune(sec.Text))
		if included > 0 && used+n > budget {
			next = sec.Order
			break
		}
		b.WriteString("--- [")
		b.WriteString(strconv.Itoa(sec.Order))
		b.WriteString("] ")
		b.WriteString(sec.Heading)
		b.WriteString(" ---\n")
		b.WriteString(sec.Text)
		b.WriteString("\n")
		used += n
		included++
	}
	return b.String(), included, next
}

// outlineSummary describes the blocks the outline did not list: their range and
// their text length. Empty when the outline listed everything.
func outlineSummary(doc *model.Doc, shown int) string {
	if shown >= len(doc.Sections) {
		return ""
	}
	rest := doc.Sections[shown:]
	chars := 0
	for _, s := range rest {
		chars += len([]rune(s.Text))
	}
	return fmt.Sprintf("more %d blocks, orders %d..%d, %d chars (read any with memento_get_doc(hash, section=order))",
		len(rest), rest[0].Order, rest[len(rest)-1].Order, chars)
}
