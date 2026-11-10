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
// extractor drops (a <script> body, an attribute). The HTML is never served
// to agents and the generic shapes fire on ordinary markup, so a
// high-confidence secret in HTML is redacted inside the stored copy (the
// write proceeds, the plaintext store stays clean) — while a secret in the
// extracted text the agent reads refuses the write.
func TestGuardFetchRedactsStoredHTML(t *testing.T) {
	doc := &model.Doc{
		Title:    "Page",
		URL:      "https://example.com/x",
		Sections: []model.Section{{Order: 0, Heading: "H", Text: "clean text"}},
		HTML:     `<script>const k = "` + guardSentinel + `";</script><p>prose</p>`,
	}
	if _, err := guardFetch(doc, config.GuardReject, false); err != nil {
		t.Fatalf("html secret must be redacted, not reject the write: %v", err)
	}
	if strings.Contains(doc.HTML, guardSentinel) {
		t.Errorf("the secret survived the html redaction: %s", doc.HTML)
	}
	if !strings.Contains(doc.HTML, "[memento-redacted:aws-access-key]") {
		t.Errorf("redaction marker missing: %s", doc.HTML)
	}
	if !strings.Contains(doc.HTML, "prose") {
		t.Errorf("redaction destroyed the surrounding content: %s", doc.HTML)
	}

	// Generic shapes (data: images, asset hashes) must survive untouched in
	// the HTML: redacting them was the false positive that made normal pages
	// unusable (and they are medium: never redacted, only refused in the
	// agent-visible text).
	doc.HTML = `<img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABGdCCgQAUQIAAAAAAAAAAAAAAAAABgUAAEg="><p>prose</p>`
	if _, err := guardFetch(doc, config.GuardReject, false); err != nil {
		t.Fatalf("data-uri image must not be redacted: %v", err)
	}
	if !strings.Contains(doc.HTML, "iVBORw0KGgo") {
		t.Errorf("generic base64 blob was redacted: %s", doc.HTML)
	}

	// A high-confidence secret in the extracted text (what the agent reads)
	// is redacted IN PLACE and the page is stored: the red line is kept by
	// the redaction, the documentation is saved, the answer reports it.
	doc.HTML = "<html><body><p>plain prose</p></body></html>"
	doc.Sections = []model.Section{{Order: 0, Heading: "H", Text: "the key is " + guardSentinel}}
	redactions, err := guardFetch(doc, config.GuardReject, false)
	if err != nil {
		t.Fatalf("high-confidence shape in text must be redacted, not reject: %v", err)
	}
	if strings.Contains(doc.Sections[0].Text, guardSentinel) {
		t.Errorf("the secret survived the section redaction: %s", doc.Sections[0].Text)
	}
	if len(redactions) == 0 {
		t.Error("the redaction must be reported (field/pattern/chars)")
	}

	// A medium shape in the agent-visible text refuses at reject, and the
	// confirmation flag does NOT bypass there.
	doc.Sections = []model.Section{{Order: 0, Heading: "H", Text: "commit 0123456789abcdef0123456789abcdef01234567"}}
	if _, err := guardFetch(doc, config.GuardReject, true); err == nil {
		t.Fatal("secret_guard=reject has no confirmation bypass for medium findings")
	}
	// At ask the finding is confirmable (the operator saw the masked shape).
	if _, err := guardFetch(doc, config.GuardAsk, false); err == nil {
		t.Fatal("unconfirmed medium finding at ask must refuse")
	}
	if _, err := guardFetch(doc, config.GuardAsk, true); err != nil {
		t.Fatalf("confirmed medium finding at ask must proceed: %v", err)
	}
}
