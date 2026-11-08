package cmd

import (
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/model"
)

const guardSentinel = "AKIAIOSFODNN7EXAMPLE" // the AWS documentation example key

func fields() []guardField {
	return []guardField{
		{"value", "the admin key is " + guardSentinel},
		{"notes", "checked by hand"},
	}
}

func TestGuardCheckReject(t *testing.T) {
	if err := guardCheck(config.GuardReject, false, fields()...); err == nil {
		t.Fatal("reject mode stored credential-like text")
	}
	// reject has no confirmation: the flag must not open a bypass.
	if err := guardCheck(config.GuardReject, true, fields()...); err == nil {
		t.Fatal("reject mode bypassed with the confirmation flag")
	}
	if err := guardCheck(config.GuardReject, false, guardField{"value", "plain fact text"}); err != nil {
		t.Errorf("reject blocked clean text: %v", err)
	}
}

func TestGuardCheckAsk(t *testing.T) {
	err := guardCheck(config.GuardAsk, false, fields()...)
	if err == nil {
		t.Fatal("ask mode stored credential-like text without confirmation")
	}
	if !strings.Contains(err.Error(), "--allow-secret") {
		t.Errorf("ask message must name the confirmation flag: %v", err)
	}
	if err := guardCheck(config.GuardAsk, true, fields()...); err != nil {
		t.Errorf("ask with confirmation must proceed, got: %v", err)
	}
}

func TestGuardCheckAllow(t *testing.T) {
	if err := guardCheck(config.GuardAllow, false, fields()...); err != nil {
		t.Errorf("allow mode must not block, got: %v", err)
	}
}

// TestGuardCheckMessageIsMasked is the leak guarantee: the guard error is the
// only channel that reports a hit, and it must carry masked previews only.
func TestGuardCheckMessageIsMasked(t *testing.T) {
	err := guardCheck(config.GuardReject, false, fields()...)
	if err == nil {
		t.Fatal("want a guard error")
	}
	if strings.Contains(err.Error(), guardSentinel) {
		t.Errorf("guard message echoes the secret: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"field value", "aws-access-key", "****"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// The stored doc carries the raw HTML; a secret can hide in markup the
// extractor drops (a <script> body, an attribute). The guard must scan it:
// what reaches the plaintext store must pass the guard.
func TestGuardFetchScansStoredHTML(t *testing.T) {
	doc := &model.Doc{
		Title:    "Page",
		URL:      "https://example.com/x",
		Sections: []model.Section{{Order: 0, Heading: "H", Text: "clean text"}},
		HTML:     `<script>const k = "` + guardSentinel + `";</script><p>prose</p>`,
	}
	if err := guardFetch(doc, config.GuardReject, false); err == nil {
		t.Fatal("secret in stored html must be caught before the store write")
	}
	doc.HTML = "<html><body><p>plain prose</p></body></html>"
	if err := guardFetch(doc, config.GuardReject, false); err != nil {
		t.Fatalf("clean doc rejected: %v", err)
	}
}
