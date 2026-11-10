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
	// Confidence drives the policy: high shapes are credential-specific and
	// deterministic (vendor prefixes, key blocks, strong assignments) — safe
	// to act on (redact). medium covers the generic shapes (long hex/base64,
	// weak assignments) that fire on ordinary content too — never redacted
	// (a page must not be destroyed by a false positive), only refused
	// (confirmable at secret_guard=ask).
	Confidence string

	start int
	end   int
}

// Confidence levels.
const (
	ConfHigh   = "high"
	ConfMedium = "medium"
)

// pattern is a named detector. confidence is ConfHigh for credential shapes
// rare enough in legitimate text to act on, ConfMedium for the generic
// shapes that also fire on ordinary content (asset hashes, inline data:
// images, minified bundles). dynamic marks the one rule whose level is
// computed per match (credential-assignment: the shape is common, the
// entropy of the value decides).
type pattern struct {
	name       string
	re         *regexp.Regexp
	confidence string
	dynamic    bool
}

// patterns is the best-effort set: known vendor prefixes first (they close
// the false-negative class of all-hex-32-style keys — the prefix names the
// vendor), then generic shapes that trade false positives for coverage.
var patterns = []pattern{
	{"aws-access-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), ConfHigh, false},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`), ConfHigh, false},
	{"github-fine-grained-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), ConfHigh, false},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20}\b`), ConfHigh, false},
	{"slack-token", regexp.MustCompile(`\bxox[baprs][A-Za-z0-9-]{12,}\b`), ConfHigh, false},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), ConfHigh, false},
	{"openai-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`), ConfHigh, false},
	{"stripe-key", regexp.MustCompile(`\b[sp]k_(?:live|test)_[A-Za-z0-9]{20,}\b`), ConfHigh, false},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}`), ConfHigh, false},
	{"private-key-pem", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), ConfHigh, false},
	{"bearer-header", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]{16,}`), ConfHigh, false},
	{"basic-auth-url", regexp.MustCompile(`(?i)\bhttps?://[^\s/@:]+:[^\s@]+@[^\s/]+`), ConfHigh, false},
	{"credential-assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key)\b\s*[:=]\s*[^\s"']{6,}`), ConfMedium, true},
	{"long-hex", regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), ConfMedium, false},
	{"long-base64", regexp.MustCompile(`[A-Za-z0-9+/]{40,}=`), ConfMedium, false},
	{"long-opaque-token", regexp.MustCompile(`[A-Za-z0-9+/]{48,}`), ConfMedium, false},
}

// classifyAssignment implements the deterministic rule for credential
// assignments (the Shannon-score false positives it replaces): the matched
// span is split at the assignment operator, and the right-hand side is
// high-confidence only when it looks like a generated secret: 20+ characters
// spanning at least 3 character classes (upper/lower/digit/symbol). A short
// or low-entropy right-hand side (an identifier, a config name) is medium:
// assignments to variables are ordinary code text.
func classifyAssignment(match string) string {
	idx := strings.IndexAny(match, "=:")
	if idx < 0 {
		return ConfMedium
	}
	rhs := strings.TrimSpace(match[idx+1:])
	r := []rune(rhs)
	if len(r) < 20 {
		return ConfMedium
	}
	var upper, lower, digit, symbol bool
	for _, c := range r {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	n := 0
	for _, b := range []bool{upper, lower, digit, symbol} {
		if b {
			n++
		}
	}
	if n >= 3 {
		return ConfHigh
	}
	return ConfMedium
}

// Scan reports credential-like spans in text, attributed to field. Detections
// that fully cover an earlier match are folded into it, so one secret produces
// one finding, not a pile across overlapping patterns.
func Scan(field, text string) []Finding {
	return scan(field, text, false)
}

// Redact replaces high-confidence credential-like spans with a marker and
// returns the rewritten text with the findings. Medium-confidence shapes are
// skipped: in markup they match ordinary content (asset hashes, inline
// data: images), and a fetched page must not be destroyed by a false
// positive. This is the mode for agent-visible doc fields and for the stored
// raw HTML, which agents never read: redacting on write keeps the plaintext
// store clean without making normal pages uncacheable.
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

// scan is Scan with the pattern-set switch: highOnly drops the
// medium-confidence shapes (Redact's set). Findings are masked previews only
// (see the package doc).
func scan(field, text string, highOnly bool) []Finding {
	var out []Finding
	for _, p := range patterns {
		conf := p.confidence
		if highOnly && conf != ConfHigh {
			continue
		}
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			start, end := loc[0], loc[1]
			if covered(out, start, end) {
				continue
			}
			level := conf
			if p.dynamic {
				level = classifyAssignment(text[start:end])
			}
			out = append(out, Finding{
				Field:      field,
				Pattern:    p.name,
				Length:     end - start,
				Masked:     mask(text[start:end]),
				Confidence: level,
				start:      start,
				end:        end,
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
