package client

import (
	"strings"
	"testing"
)

// The painless scripts compile on the cluster at request time, so a lexer
// mistake ships silently: the cluster's painless string literals process no
// escapes beyond \\ and \" (verified against OpenSearch 3.8.0: a "\n" in the
// source is a compile error, both single- and double-quoted), and every
// scripted write then fails with "failed to execute script". The suite runs
// offline, so the cluster never vets these sources — the invariant lives
// here instead: a script may contain no single quote and no backslash at
// all. Newlines and other special characters belong in params (see nsep),
// which is data and bypasses the lexer entirely.
func TestPainlessScriptsAreEscapeFree(t *testing.T) {
	scripts := map[string]string{
		"addCorrectionScript": addCorrectionScript,
		"obsoleteScript":      obsoleteScript,
		"patchScript":         patchScript,
		"pruneHistoryScript":  pruneHistoryScript,
	}
	for name, src := range scripts {
		if strings.Contains(src, "'") {
			t.Errorf("%s: single-quoted literal in painless source (escapes are invalid there): use double quotes", name)
		}
		if strings.Contains(src, "\\") {
			t.Errorf("%s: backslash in painless source: the lexer processes no standard escapes, pass special characters as params", name)
		}
	}
}
