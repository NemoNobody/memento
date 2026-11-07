// Package secretguard is a best-effort detector for credential-like text in
// fact content. The facts store is plaintext, so a secret written once is
// exposed in every later read (and in every agent context that reads it).
//
// The detector is regex-based: a secret without a recognizable pattern will
// pass. It is a guard rail against accidents, never a guarantee. Findings
// carry a masked preview only — the matched text must never be echoed back
// into the caller's context (that is how a secret leaks a second time).
package secretguard

import (
	"regexp"
	"strings"
)

// Finding is one credential-like span in one input field.
type Finding struct {
	Field   string // the caller's field name: "value", "notes", "source"...
	Pattern string // human-readable name of the matched pattern
	Length  int    // length of the matched text
	Masked  string // preview: at most the first 4 characters, the rest is '*'

	start int
	end   int
}

// pattern is a named detector.
type pattern struct {
	name string
	re   *regexp.Regexp
}

// patterns is the best-effort set: known vendor prefixes first (high
// confidence), then generic shapes (long hex/base64, credential assignments)
// that trade false positives for coverage.
var patterns = []pattern{
	{"aws-access-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`)},
	{"github-fine-grained-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs][A-Za-z0-9-]{12,}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"openai-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`)},
	{"stripe-key", regexp.MustCompile(`\b[sp]k_(?:live|test)_[A-Za-z0-9]{20,}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}`)},
	{"private-key-pem", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer-header", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]{16,}`)},
	{"credential-assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key)\b\s*[:=]\s*[^\s"']{6,}`)},
	{"long-hex", regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)},
	{"long-base64", regexp.MustCompile(`[A-Za-z0-9+/]{40,}=`)},
	{"long-opaque-token", regexp.MustCompile(`[A-Za-z0-9+/]{48,}`)},
}

// Scan reports credential-like spans in text, attributed to field. Detections
// that fully cover an earlier match are folded into it, so one secret produces
// one finding, not a pile across overlapping patterns.
func Scan(field, text string) []Finding {
	var out []Finding
	for _, p := range patterns {
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			start, end := loc[0], loc[1]
			if covered(out, start, end) {
				continue
			}
			out = append(out, Finding{
				Field:   field,
				Pattern: p.name,
				Length:  end - start,
				Masked:  mask(text[start:end]),
				start:   start,
				end:     end,
			})
		}
	}
	return out
}

// covered reports whether [start,end) lies entirely inside an existing finding.
func covered(findings []Finding, start, end int) bool {
	for _, f := range findings {
		if f.start <= start && end <= f.end {
			return true
		}
	}
	return false
}

// mask keeps at most the first 4 characters of the matched text. The preview
// identifies the shape of the hit without re-exposing the secret.
func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}
