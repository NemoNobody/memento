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
		{Name: "value", Text: "the admin key is " + guardSentinel},
		{Name: "notes", Text: "checked by hand"},
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
	if err := guardCheck(config.GuardReject, false, guardField{Name: "value", Text: "plain fact text"}); err != nil {
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
	// confirmation flag does NOT bypass there. The habitat is neutral ("value
	// "): a named hash beside a hint word is exempt by design (long-hex
	// ctxExempt), so the test must use an unnamed opaque blob.
	doc.Sections = []model.Section{{Order: 0, Heading: "H", Text: "value " + strings.Repeat("k", 48)}}
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

// The URL is identity: it cannot be redacted, and medium shapes are ordinary
// in it (git SHAs in GitHub paths). Only the deterministic high shapes may
// refuse a doc, and the HTML redactions join the audit trail.
func TestGuardFetchURLIsHighOnly(t *testing.T) {
	// A git SHA (long-hex, medium) in the URL must not reject at reject — the
	// URL is an identity field and GitHub commit links are legitimate.
	doc := &model.Doc{
		Title:    "Page",
		URL:      "https://github.com/o/r/commit/0123456789abcdef0123456789abcdef01234567",
		Sections: []model.Section{{Order: 0, Heading: "H", Text: "clean"}},
		HTML:     "<html></html>",
	}
	if _, err := guardFetch(doc, config.GuardReject, false); err != nil {
		t.Fatalf("medium shape in the URL must not reject: %v", err)
	}
	// The same shape in the agent-visible text does refuse (control). Neutral
	// habitat: named-hash contexts are exempt for the generic shapes.
	doc.Sections = []model.Section{{Order: 0, Heading: "H", Text: "value " + strings.Repeat("k", 48)}}
	if _, err := guardFetch(doc, config.GuardReject, false); err == nil {
		t.Fatal("medium shape in visible text must refuse at reject")
	}
	// A high-confidence credential shape in the URL: no redaction is possible
	// on an identity field, so the doc is refused.
	doc.Sections = []model.Section{{Order: 0, Heading: "H", Text: "clean"}}
	doc.URL = "https://example.com/o/r?token=" + guardSentinel
	if _, err := guardFetch(doc, config.GuardReject, true); err == nil {
		t.Fatal("high shape in the URL must refuse (reject has no bypass)")
	}
	// HTML redactions are reported in the audit trail as field "html".
	doc.URL = "https://example.com/x"
	doc.HTML = `<script>const k = "` + guardSentinel + `";</script>`
	redactions, err := guardFetch(doc, config.GuardReject, false)
	if err != nil {
		t.Fatalf("html secret is redacted, not refused: %v", err)
	}
	found := false
	for _, r := range redactions {
		if r.Field == "html" {
			found = true
		}
	}
	if !found {
		t.Errorf("the html redaction must be reported: %+v", redactions)
	}
}

// The post-redirect URL carries the same identity role as the URL: a git SHA
// in the chain is ordinary (pass), a credential-bearing redirect target is
// not a document (refuse). Image hrefs are documentation: the high-confidence
// credential shapes are redacted in place, the audit trail reports them.
func TestGuardFetchFinalURLAndImages(t *testing.T) {
	doc := &model.Doc{
		Title:    "Page",
		URL:      "https://example.com/x",
		FinalURL: "https://github.com/o/r/commit/0123456789abcdef0123456789abcdef01234567",
		Sections: []model.Section{{Order: 0, Heading: "H", Text: "clean"}},
		HTML:     "<html></html>",
	}
	if _, err := guardFetch(doc, config.GuardReject, false); err != nil {
		t.Fatalf("a SHA in the redirect chain must not reject: %v", err)
	}

	// A redirect target that bears credentials is refused: it is identity
	// (redaction is impossible) and never a document.
	doc.FinalURL = "http://admin:S3cr3tPw@media.internal/x"
	if _, err := guardFetch(doc, config.GuardReject, true); err == nil {
		t.Fatal("a basic-auth redirect target must refuse the doc")
	}

	// An image src with credentials is redacted in place (a leak, not a
	// link); the redaction joins the audit trail.
	doc.FinalURL = ""
	doc.Images = []model.ImageRef{{URL: "http://admin:S3cr3tPw@cdn.example/i.png", Alt: "logo"}}
	redactions, err := guardFetch(doc, config.GuardReject, false)
	if err != nil {
		t.Fatalf("the image URL shape must be redacted, not refused: %v", err)
	}
	if strings.Contains(doc.Images[0].URL, "S3cr3tPw") {
		t.Errorf("the credential survived the image URL redaction: %s", doc.Images[0].URL)
	}
	reported := false
	for _, r := range redactions {
		if r.Field == "image 0 url" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the image URL redaction must be reported: %+v", redactions)
	}
}

// TestFetchGuardProjectScanned pins that the project label is scanned on the
// fetch write path (identity text in a shared store). The documented
// asymmetry: project is NOT HighOnly — a SHA-shaped label can refuse a fetch
// at reject without bypass (docs/setup names it).
func TestFetchGuardProjectScanned(t *testing.T) {
	doc := &model.Doc{
		Title: "P", URL: "https://x/y", Project: "p",
		Sections: []model.Section{{Order: 0, Heading: "H", Text: "clean"}},
		HTML:     "<html></html>",
	}
	if _, err := guardFetch(doc, config.GuardReject, false); err != nil {
		t.Fatalf("clean project must pass: %v", err)
	}
	doc.Project = "key=" + guardSentinel
	if _, err := guardFetch(doc, config.GuardReject, true); err == nil {
		t.Fatal("high-confidence shape in project must refuse at reject")
	}
}

// TestFetchGuardAuditOrder pins the audit-trail order: the html surgery is
// reported first, then the identity fields, then the sections. An agent
// reading doc.Redactions must see the storage-level edits before the
// field-level ones (the order the guard applies them).
func TestFetchGuardAuditOrder(t *testing.T) {
	doc := &model.Doc{
		Title:    "clean",
		URL:      "https://x/y",
		Project:  "p",
		HTML:     `<b>key="` + guardSentinel + `"</b>`,
		Sections: []model.Section{{Order: 0, Heading: "h", Text: "leaked: " + guardSentinel}},
	}
	reds, err := guardFetch(doc, config.GuardReject, false)
	if err != nil {
		t.Fatalf("redactable doc must not refuse: %v", err)
	}
	if len(reds) < 2 {
		t.Fatalf("redactions = %+v, want html and section entries", reds)
	}
	if reds[0].Field != "html" {
		t.Errorf("first audit entry = %q, want html", reds[0].Field)
	}
	seenHTML := false
	for _, r := range reds {
		if r.Field == "html" {
			seenHTML = true
			break
		}
	}
	if !seenHTML {
		t.Error("html redactions missing from the audit trail")
	}
}
