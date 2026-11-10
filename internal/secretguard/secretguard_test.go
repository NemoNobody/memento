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
		{"hex40", "commit " + sentinelHex40, "long-hex"},
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
		{"hex32", "commit " + strings.Repeat("a", 32), "long-hex", ConfMedium},
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
