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
	"sort"
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

// pattern is a named detector. generic marks the credential shapes that
// also fire on ordinary content (asset hashes, inline data: images, minified
// bundles); high-confidence patterns match credential shapes rare enough in
// legitimate text to act on.
type pattern struct {
	name    string
	re      *regexp.Regexp
	generic bool
}

// patterns is the best-effort set: known vendor prefixes first (high
// confidence), then generic shapes (long hex/base64, credential assignments)
// that trade false positives for coverage.
var patterns = []pattern{
	{"aws-access-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), false},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`), false},
	{"github-fine-grained-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), false},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20}\b`), false},
	{"slack-token", regexp.MustCompile(`\bxox[baprs][A-Za-z0-9-]{12,}\b`), false},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), false},
	{"openai-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`), false},
	{"stripe-key", regexp.MustCompile(`\b[sp]k_(?:live|test)_[A-Za-z0-9]{20,}\b`), false},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}`), false},
	{"private-key-pem", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), false},
	{"bearer-header", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]{16,}`), false},
	{"credential-assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key)\b\s*[:=]\s*[^\s"']{6,}`), false},
	{"long-hex", regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), true},
	{"long-base64", regexp.MustCompile(`[A-Za-z0-9+/]{40,}=`), true},
	{"long-opaque-token", regexp.MustCompile(`[A-Za-z0-9+/]{48,}`), true},
}

// Scan reports credential-like spans in text, attributed to field. Detections
// that fully cover an earlier match are folded into it, so one secret produces
// one finding, not a pile across overlapping patterns.
func Scan(field, text string) []Finding {
	return scan(field, text, false)
}

// Redact replaces high-confidence credential-like spans with a marker and
// returns the rewritten text with the findings. Generic shapes are skipped:
// in markup they match ordinary content (asset hashes, inline data: images),
// and a fetched page must not be destroyed by a false positive. This is the
// mode for the stored raw HTML, which agents never read: redacting on write
// keeps the plaintext store clean without making normal pages uncacheable.
func Redact(field, text string) (string, []Finding) {
	findings := scan(field, text, true)
	if len(findings) == 0 {
		return text, nil
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].start < findings[j].start })
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, f := range findings {
		if f.start < last {
			continue // a folded overlap: the earlier span already replaced it
		}
		b.WriteString(text[last:f.start])
		b.WriteString("[memento-redacted:")
		b.WriteString(f.Pattern)
		b.WriteString("]")
		last = f.end
	}
	b.WriteString(text[last:])
	return b.String(), findings
}

// scan is Scan with the pattern-set switch: highOnly drops the generic
// shapes. Findings are masked previews only (see the package doc).
func scan(field, text string, highOnly bool) []Finding {
	var out []Finding
	for _, p := range patterns {
		if highOnly && p.generic {
			continue
		}
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
	r := []rune(s)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	// rune-based: a byte cut can split a multi-byte character and put
	// invalid UTF-8 into the guard message (the preview must stay printable).
	return string(r[:4]) + strings.Repeat("*", len(r)-4)
}
