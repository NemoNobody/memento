package secretguard

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	sentinelAWS   = "AKIAIOSFODNN7EXAMPLE" // the AWS documentation example key
	sentinelHex40 = "0123456789abcdef0123456789abcdef01234567"
)

func TestScanCleanText(t *testing.T) {
	clean := []string{
		"",
		"the handler lives in internal/client/opensearch.go under func Add",
		"opensearch_url is https://127.0.0.1:9200; OPENSEARCH_PASSWORD is set in the env",
		"history_limit = 20; keep the newest 20 entries",
		"short git sha 4dda7fa is the current master",
		"tokenizer settings: max = 8192",
	}
	for _, s := range clean {
		if got := Scan("value", s); len(got) != 0 {
			t.Errorf("Scan(%q) = %d findings, want 0: %+v", s, len(got), got)
		}
	}
}

func TestScanKnownShapes(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		patName string
	}{
		{"aws", "key is " + sentinelAWS, "aws-access-key"},
		{"github", "token ghp_" + strings.Repeat("aB3xYw9", 5) + "a", "github-token"},
		{"gitlab", "glpat-" + strings.Repeat("Qz8", 6) + "ab", "gitlab-token"},
		{"openai", "sk-" + strings.Repeat("k9QdLm2Zx7", 3) + "ab", "openai-key"},
		{"jwt", "auth eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0", "jwt"},
		{"pem", "rotate it: -----BEGIN OPENSSH PRIVATE KEY-----\nabc", "private-key-pem"},
		{"bearer", "send Authorization: Bearer eyJ0eXAi.aaaaaaaaaaaaaaaaaaaaaa", "bearer-header"},
		{"assignment", "login with password = Sup3rS3cret!", "credential-assignment"},
		{"hex40", "value " + sentinelHex40, "long-hex"},
		{"base64", "value " + strings.Repeat("aB3xYw9", 6) + "=", "long-base64"},
		{"opaque", strings.Repeat("k", 48), "long-opaque-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := Scan("value", tc.text)
			if len(findings) == 0 {
				t.Fatalf("no findings for %q", tc.text)
			}
			found := false
			for _, f := range findings {
				if f.Pattern == tc.patName {
					found = true
				}
			}
			if !found {
				t.Errorf("want pattern %q, got %+v", tc.patName, findings)
			}
		})
	}
}

// TestScanFoldsOverlaps pins the dedup: one secret matched by several generic
// patterns yields one finding, not a stack.
// TestMaskRuneSafe pins that mask never cuts a multi-byte character in half:
// the preview reaches terminal output and agent context, so invalid UTF-8
// there is a rendering corruption bug.
func TestMaskRuneSafe(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ключевое", "********"},                             // <=8 runes: fully masked, rune count
		{"ключ" + strings.Repeat("x", 10), "ключ**********"}, // >8 runes: first 4 kept whole
		{"Ёё", "**"},
	}
	for _, tc := range cases {
		got := mask(tc.in)
		if got != tc.want {
			t.Errorf("mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("mask(%q) produced invalid UTF-8: %q", tc.in, got)
		}
	}
}

func TestScanFoldsOverlaps(t *testing.T) {
	secret := strings.Repeat("aB3xYw9", 7) + "=" // matches long-base64 and long-opaque-token
	findings := Scan("notes", secret)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Pattern != "long-base64" {
		t.Errorf("pattern = %q, want the more specific long-base64", findings[0].Pattern)
	}
}

// TestScanNeverEchoesSecret is the redaction guarantee behind the whole
// feature (see the 2026-11-07 password incident): a finding must not carry
// the matched text back to the caller's context.
func TestScanNeverEchoesSecret(t *testing.T) {
	findings := Scan("value", "admin_password="+sentinelAWS+" tail")
	if len(findings) == 0 {
		t.Fatal("no findings for a credential assignment")
	}
	for _, f := range findings {
		if strings.Contains(f.Masked, sentinelAWS) {
			t.Errorf("Masked echoes the secret: %q", f.Masked)
		}
		if f.Length <= 0 {
			t.Errorf("Length = %d, want positive", f.Length)
		}
		if i := strings.IndexByte(f.Masked, '*'); i > 4 {
			t.Errorf("Masked preview is longer than 4 chars: %q", f.Masked)
		}
	}
}

// TestConfidenceRules pins the deterministic triage (the Shannon-score
// replacement): vendor prefixes, key blocks, basic-auth URLs and strong
// assignments are high (redactable); the generic shapes and weak assignments
// are medium (refuse only, never mangle content).
func TestConfidenceRules(t *testing.T) {
	cases := []struct {
		name string
		text string
		pat  string
		conf string
	}{
		{"prefix-aws", "key is " + sentinelAWS, "aws-access-key", ConfHigh},
		{"basic-auth", "connect https://admin:Su1perS3cret!@wiki.internal/x", "basic-auth-url", ConfHigh},
		{"assignment-strong", "api_key: A1b2C3d4E5f6G7h8I9j0Kl", "credential-assignment", ConfHigh},
		{"assignment-symbol-class", "password = Xk9#mQ2$vL7@pR4!tW8&nZ5", "credential-assignment", ConfHigh},
		{"assignment-identifier", "password = someVeryLongVarNameForAuth", "credential-assignment", ConfMedium},
		{"assignment-short", "login with password = Sup3rS3cret!", "credential-assignment", ConfMedium},
		{"hex32", "value " + strings.Repeat("a", 32), "long-hex", ConfMedium},
		{"opaque", strings.Repeat("k", 48), "long-opaque-token", ConfMedium},
		{"pem", "rotate it: -----BEGIN OPENSSH PRIVATE KEY-----\nabc", "private-key-pem", ConfHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hit *Finding
			fs := Scan("value", tc.text)
			for i := range fs {
				if fs[i].Pattern == tc.pat {
					hit = &fs[i]
				}
			}
			if hit == nil {
				t.Fatalf("pattern %q not found in %+v", tc.pat, fs)
			}
			if hit.Confidence != tc.conf {
				t.Errorf("confidence = %q, want %q", hit.Confidence, tc.conf)
			}
		})
	}
}

// TestScanHashContextExemption: the technical-hash habitat (usage report
// P1: HF revisions, sha256 digests, SRI attributes) must not block
// technical facts — a token named by a neighboring word is a content
// fingerprint. The exemption is local: an unnamed shape (no hint inside the
// window) and a credential assignment (never exempt) stay detected.
func TestScanHashContextExemption(t *testing.T) {
	clean := []string{
		"model revision " + sentinelHex40 + " (sha256: " + strings.Repeat("aB3xYw9", 6) + ")",
		"commit " + sentinelHex40,
		"checksum 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"integrity=\"sha384-" + strings.Repeat("k9Qd", 24) + "=\"",
	}
	for _, s := range clean {
		if got := Scan("value", s); len(got) != 0 {
			t.Errorf("named hash must be exempt: %q -> %+v", s, got)
		}
	}
	if got := Scan("value", "value "+sentinelHex40); len(got) == 0 {
		t.Error("unnamed long-hex must stay detected")
	}
	if got := Scan("value", "commit note: api_key = "+strings.Repeat("k9QdLm2Zx7", 3)+"ab"); len(got) == 0 {
		t.Error("assignment beside a hint word must stay detected")
	}
}

// TestRedactSkipsMedium: redaction is reserved for the high shapes. A git
// SHA (long-hex) inside fetched text must survive intact — mangling real
// documentation on a medium hit is worse than refusing it.
func TestRedactSkipsMedium(t *testing.T) {
	text := "commit " + sentinelHex40 + " is the base"
	out, findings := Redact("section 0", text)
	if out != text {
		t.Errorf("medium shape was redacted: %q", out)
	}
	if len(findings) != 0 {
		t.Errorf("Redact must report only high findings, got %+v", findings)
	}
	out, findings = Redact("section 0", "the key is "+sentinelAWS)
	if strings.Contains(out, sentinelAWS) || len(findings) != 1 {
		t.Errorf("high shape must be redacted: %q %+v", out, findings)
	}
}

// TestRedactReportsOnlyApplied: the findings list is an audit trail (the
// store records field/pattern/chars per redaction), so it must describe the
// spans the text actually lost — never a match the fold skipped (the
// round-5 audit-lie class: a reported redaction that never landed).
// The invariant is pinned directly: every reported finding's marker is in
// the output, and the reported spans are disjoint and in order.
func TestRedactReportsOnlyApplied(t *testing.T) {
	text := "token eyJabcdefghijk http://user:pass@host and key " + sentinelAWS
	out, applied := Redact("section 0", text)
	if len(applied) == 0 {
		t.Fatalf("expected high findings, got none")
	}
	prevEnd := 0
	for _, f := range applied {
		marker := "[memento-redacted:" + f.Pattern + "]"
		if !strings.Contains(out, marker) {
			t.Errorf("reported %q but no marker for it in %q", f.Pattern, out)
		}
		if f.start < prevEnd || f.end <= f.start {
			t.Errorf("reported spans must be disjoint and ordered: %+v", applied)
		}
		prevEnd = f.end
	}
	// Scanned findings may exceed applied ones (fold skips): the contract is
	// applied ⊆ scanned, never the reverse.
	if scanned := len(Scan("section 0", text)); scanned < len(applied) {
		t.Errorf("scan=%d < applied=%d", scanned, len(applied))
	}
}

// TestHex32UnderPrefix: an AWS key is also a 20-char alnum blob; the vendor
// prefix classifies it high — the generic-shape false-negative class
// (all-hex-32 tokens that ARE keys) closes through the prefix list.
func TestHex32UnderPrefix(t *testing.T) {
	fs := Scan("value", sentinelAWS)
	high := false
	for _, f := range fs {
		if f.Confidence == ConfHigh {
			high = true
		}
	}
	if !high {
		t.Errorf("AWS key must be high-confidence via the prefix: %+v", fs)
	}
}

// TestConfidenceBoundaries: the 20-rune rule is the classification edge —
// a 20-rune 3-class right-hand side is high, 19 runes is medium (the rule
// must be exact, not fuzzy).
func TestConfidenceBoundaries(t *testing.T) {
	r20 := strings.Repeat("a", 17) + "A1!" // 20 runes: lower, upper, digit, symbol
	r19 := strings.Repeat("a", 16) + "A1!" // 19 runes, same class spread
	findings := Scan("value", "api_key: "+r20)
	if len(findings) != 1 || findings[0].Confidence != ConfHigh {
		t.Errorf("20-rune RHS: %+v, want one high", findings)
	}
	findings = Scan("value", "api_key: "+r19)
	if len(findings) != 1 || findings[0].Confidence != ConfMedium {
		t.Errorf("19-rune RHS: %+v, want one medium", findings)
	}
}

// TestQuotedAssignment: JSON/YAML config lines are the main habitat of
// credential assignments; an opening quote on the right-hand side must not
// open a false-negative hole.
func TestQuotedAssignment(t *testing.T) {
	cases := []string{
		`"api_key": "A1b2C3d4E5f6G7h8I9j0Kl"`,
		"password = 'Xk9mQ2vL7pR4tW8nZ5bJ3cY1dA6e' ",
	}
	for _, text := range cases {
		fs := Scan("value", text)
		high := false
		for _, f := range fs {
			if f.Pattern == "credential-assignment" && f.Confidence == ConfHigh {
				high = true
			}
		}
		if !high {
			t.Errorf("quoted assignment must yield a high finding: %q -> %+v", text, fs)
		}
	}
}

// TestRedactionMarkerIsStable: the marker must not itself fire any pattern
// (the redact->rescan loop must terminate on the second pass).
func TestRedactionMarkerIsStable(t *testing.T) {
	text := "the key is " + sentinelAWS + " and a secret = " + strings.Repeat("aB3x", 6) + "X!"
	out, first := Redact("section 0", text)
	if len(first) == 0 {
		t.Fatal("expected findings")
	}
	out2, second := Redact("section 0", out)
	if len(second) != 0 {
		t.Errorf("second redact pass found new shapes (marker re-fires): %+v", second)
	}
	if out2 != out {
		t.Error("the redacted text must be a fixed point of Redact")
	}
	for _, f := range Scan("section 0", out) {
		if f.Confidence == ConfHigh {
			t.Errorf("high finding survives redaction: %+v", f)
		}
	}
}

// TestScanHashContextVocabulary (finding 106): the hint vocabulary must not
// blanket-exempt whole habitats. "git" sat next to every hex-ish token in
// any checkout context; "sum" fired inside ordinary prose. Both are out.
func TestScanHashContextVocabulary(t *testing.T) {
	for _, s := range []string{
		"git " + sentinelHex40,
		"the sum " + sentinelHex40,
	} {
		if got := Scan("value", s); len(got) == 0 {
			t.Errorf("removed hint word must not exempt: %q -> %+v", s, got)
		}
	}
}

// TestInHashContextNoFabricatedBoundaries (finding 107): the window slice
// must not manufacture word boundaries at its cut edges — a longer word
// ("rehash") cut to a hint ("hash") is not a hash context, and neither is
// "git" inside "gitignore". A clean hint word still exempts.
func TestInHashContextNoFabricatedBoundaries(t *testing.T) {
	// "rehash" cut by the window start: the slice would begin with "hash".
	text := "rehash" + strings.Repeat(" ", hashHintWindow-4) + sentinelHex40
	start := strings.Index(text, sentinelHex40)
	if inHashContext(text, start, start+len(sentinelHex40)) {
		t.Error("window-cut 'rehash' must not fabricate a 'hash' hint")
	}
	// Clean hint word, same distance: exempts.
	text2 := "checksum" + strings.Repeat(" ", hashHintWindow-8) + sentinelHex40
	start2 := strings.Index(text2, sentinelHex40)
	if !inHashContext(text2, start2, start2+len(sentinelHex40)) {
		t.Error("clean 'checksum' within the window must exempt")
	}
	// "git" inside "gitignore" is no hint (the word survives the window).
	text3 := ".gitignore " + sentinelHex40
	start3 := strings.Index(text3, sentinelHex40)
	if inHashContext(text3, start3, start3+len(sentinelHex40)) {
		t.Error("'git' inside 'gitignore' must not count as a hint")
	}
}

// TestRedactAllDeterministicOnSharedStart pins the fold contract for spans that
// begin at the same byte: the wider match wins, so the marker covers the whole
// token and the applied audit row describes the span that really went away.
func TestRedactAllDeterministicOnSharedStart(t *testing.T) {
	// 47 hex chars + '=': long-base64 matches the whole 48 (it requires a
	// trailing '='), long-hex matches the leading 47 (every 'A' is a hex
	// digit). Same start, different end; the wide span must win on every run.
	token := strings.Repeat("A", 47) + "="
	out, findings := RedactAll("value", "value "+token)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one folded span", findings)
	}
	if strings.Contains(out, strings.Repeat("A", 8)) {
		t.Errorf("the token survived the redaction: %q", out)
	}
	if findings[0].Length != len(token) {
		t.Errorf("applied %d chars, want the wide span %d", findings[0].Length, len(token))
	}
}
