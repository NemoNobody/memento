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
