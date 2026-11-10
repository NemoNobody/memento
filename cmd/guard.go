package cmd

import (
	"errors"
	"strconv"
	"strings"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/secretguard"
)

// guardField is one user-supplied text that the secret guard inspects on a
// write path.
type guardField struct {
	Name string
	Text string
	// HighOnly narrows the scan to high-confidence shapes: fields that carry
	// identity (a document URL) cannot be redacted and legitimately contain
	// medium-shape content (git SHAs in paths), so only the deterministic
	// credential shapes may refuse them.
	HighOnly bool
}

// guardCheck enforces the secret_guard policy over the fields of one write.
// The store is plaintext: a secret written once is exposed in every later
// read, so the write is the last place it can be stopped.
//
// reject: credential-like text is never stored, no confirmation exists.
// ask: the write proceeds only with an explicit confirmation (the operator
// saw the masked finding and approved it).
// allow: the operator opted out; nothing is scanned.
//
// The message lists masked findings only — echoing the matched text back
// would leak it a second time through the error path.
func guardCheck(level string, allowed bool, fields ...guardField) error {
	if level == config.GuardAllow {
		return nil
	}
	var findings []secretguard.Finding
	for _, f := range fields {
		for _, fd := range secretguard.Scan(f.Name, f.Text) {
			if f.HighOnly && fd.Confidence != secretguard.ConfHigh {
				continue
			}
			findings = append(findings, fd)
		}
	}
	if len(findings) == 0 {
		return nil
	}
	if level == config.GuardAsk && allowed {
		return nil
	}

	var b strings.Builder
	b.WriteString("secret-guard: probable secret(s) detected; the fact was NOT stored.\n")
	for _, fd := range findings {
		b.WriteString("  - field ")
		b.WriteString(fd.Field)
		b.WriteString(": pattern ")
		b.WriteString(fd.Pattern)
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(fd.Length))
		b.WriteString(" chars, preview ")
		b.WriteString(fd.Masked)
		b.WriteString("\n")
	}
	b.WriteString("The store is plaintext: every later read exposes this text. ")
	if level == config.GuardAsk {
		b.WriteString("If the finding is a false positive (e.g. a git SHA under long-hex), ")
		b.WriteString("show it to the user and re-run with --allow-secret (CLI) or ")
		b.WriteString("confirm_secret=true (MCP) only after their explicit approval.")
	} else {
		b.WriteString("Remove the secret from the fact and store it without it; ")
		b.WriteString("this mode (secret_guard=reject) has no confirmation flag.")
	}
	return errors.New(b.String())
}
