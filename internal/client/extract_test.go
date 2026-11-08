package client

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NemoNobody/memento/internal/model"
)

func TestExtractTitle(t *testing.T) {
	body := `<html><head><title>Hello &amp; World</title></head><body>x</body></html>`
	if got := ExtractTitle(body); got != "Hello & World" {
		t.Errorf("ExtractTitle = %q, want %q", got, "Hello & World")
	}
	if got := ExtractTitle("<html><body>x</body></html>"); got != "" {
		t.Errorf("missing title = %q, want empty", got)
	}
}

func TestExtractSkipsDoctypeHeadComments(t *testing.T) {
	// Live-shape page: a doctype, a head with a title, a comment. None of
	// their text may reach the sections (a doctype line once became a
	// document's heading; head text is noise — ExtractTitle owns the title).
	body := `<!doctype html><html lang=en><head><meta charset=utf-8>` +
		`<title>Doc Title</title><style>b{}</style></head>` +
		`<body><!-- an author note, long enough to survive the min-line filter -->` +
		`<h1>Real Head</h1><p>Real paragraph text here.</p></body></html>`
	secs, _, _ := ExtractSections(body)
	if len(secs) != 1 {
		t.Fatalf("sections = %d, want 1: %+v", len(secs), secs)
	}
	if secs[0].Heading != "Real Head" {
		t.Errorf("heading = %q, want the real h1 (doctype/head leaked?)", secs[0].Heading)
	}
	all := secs[0].Heading + secs[0].Text
	for _, bad := range []string{"doctype", "Doc Title", "author note"} {
		if strings.Contains(all, bad) {
			t.Errorf("%q leaked into sections: %q", bad, all)
		}
	}
}

func TestExtractSectionsBasics(t *testing.T) {
	body := `<html><body>` +
		`<style>h1{display:none}</style>` +
		`<script>var s="<h1>fake</h1>";</script>` +
		`<h1>Main</h1><p>Intro &amp; co.</p>` +
		`<h2>Second</h2><ul><li>alpha item</li><li>beta item</li></ul>` +
		`<img src="/img/a.png" alt="Pic" title="t1">` +
		`<img src="data:image/png;base64,AAA" alt="skip">` +
		`<div>nav</div></body></html>`

	secs, imgs, trunc := ExtractSections(body)
	if trunc {
		t.Error("truncated = true, want false for a small page")
	}
	if len(secs) != 2 {
		t.Fatalf("sections = %d, want 2: %+v", len(secs), secs)
	}
	if secs[0].Heading != "Main" || secs[0].Text != "Intro & co." {
		t.Errorf("sec[0] = %+v", secs[0])
	}
	if secs[1].Heading != "Second" || secs[1].Text != "alpha item\nbeta item" {
		t.Errorf("sec[1] = %+v", secs[1])
	}
	all := secs[0].Text + secs[1].Text
	if strings.Contains(all, "display:none") || strings.Contains(all, "fake") {
		t.Errorf("script/style content leaked into text: %q", all)
	}
	if len(imgs) != 1 {
		t.Fatalf("images = %d, want 1 (data: skipped): %+v", len(imgs), imgs)
	}
	want := model.ImageRef{URL: "/img/a.png", Alt: "Pic", Title: "t1"}
	if imgs[0] != want {
		t.Errorf("image = %+v, want %+v", imgs[0], want)
	}
}

func TestExtractSectionsTruncatedByLimit(t *testing.T) {
	var b strings.Builder
	// Headings must be >= minLineChars long: contentLines drops shorter lines,
	// and a 1-char heading collapses every section to a textless heading.
	for range MaxSections + 5 {
		b.WriteString("<h1>Section head</h1><p>paragraph text line</p>")
	}
	secs, _, trunc := ExtractSections(b.String())
	if len(secs) != MaxSections {
		t.Errorf("sections = %d, want cap %d", len(secs), MaxSections)
	}
	if !trunc {
		t.Error("truncated = false; limit hits must be reported, never silent")
	}
}

func TestExtractPlainTextLimits(t *testing.T) {
	body := strings.Repeat("word ", 2000) // one huge paragraph
	secs, trunc := ExtractPlainText(body)
	if len(secs) == 0 {
		t.Fatal("no sections")
	}
	if utf8.RuneCountInString(secs[0].Text) > MaxSectionChars {
		t.Errorf("section text %d runes, want cap %d",
			utf8.RuneCountInString(secs[0].Text), MaxSectionChars)
	}
	if !trunc {
		t.Error("oversized paragraph must mark truncated")
	}
}

// Unicode case folding changes byte length (U+2126 Ω shrinks, U+023A grows),
// which desynchronizes the body/lower index sharing in the extractor. These
// characters appear in ordinary technical prose; extraction must neither panic
// nor slice content at shifted byte offsets.
func TestExtractUnicodeLoweringStable(t *testing.T) {
	for _, u := range []string{"\u2126", "\u0130", "\u212A", "\u023A", "\u023E", "\u1E9E"} {
		body := "<p>" + strings.Repeat(u, 200) + "x</p>" // over the heading-length threshold
		secs, _, _ := ExtractSections(body)
		if len(secs) == 0 {
			t.Fatalf("%q: no sections", u)
		}
		if !strings.Contains(secs[0].Text, "x") {
			t.Fatalf("%q: text lost: %q", u, secs[0].Text)
		}
	}
}

func TestExtractTitleUnicodeLoweringStable(t *testing.T) {
	title := ExtractTitle("<title>\u2126 \u0130 page</title>")
	if title != "\u2126 \u0130 page" {
		t.Fatalf("title mangled by lowering: %q", title)
	}
}

// An unclosed (or self-closed) drop tag must remove only the tag itself: the
// previous behavior deleted the whole document tail after it, silently losing
// every section the page had left.
func TestStripElementsUnclosedTag(t *testing.T) {
	secs, _, _ := ExtractSections("<p>alpha</p><script src=x>tail-kept</p>")
	joined := strings.Join(textOfSections(secs), " ")
	if !strings.Contains(joined, "tail-kept") {
		t.Fatalf("content after unclosed <script> deleted: %q", joined)
	}
	secs, _, _ = ExtractSections("<p>alpha</p><svg/>tail2-kept</p>")
	joined = strings.Join(textOfSections(secs), " ")
	if !strings.Contains(joined, "tail2-kept") {
		t.Fatalf("content after self-closed <svg/> deleted: %q", joined)
	}
}

// TestStripElementsNested pins the removal range for nested same-name tags:
// the element spans to the first matching close (the old code's behavior —
// "y</style>" survives), the inner open is consumed with the outer element.
func TestStripElementsNested(t *testing.T) {
	secs, _, _ := ExtractSections("<style><style>x</style>y</style><h1>Head text</h1><p>kept text</p>")
	joined := strings.Join(textOfSections(secs), " ")
	if !strings.Contains(joined, "kept") {
		t.Fatalf("content after nested <style> deleted: %q", joined)
	}
	if strings.Contains(joined, "y") {
		t.Errorf("nested-drop semantics changed (text between the closes survived): %q", joined)
	}
}

// TestStripElementsLinear is the DoS regression: the old per-occurrence
// string rebuild was quadratic, so a page of many dropTags (all legal under
// fetch_max_bytes) burned minutes of CPU in the long-lived MCP server. The
// single pass must finish a 50k-element page in well under a second.
func TestStripElementsLinear(t *testing.T) {
	const n = 50000
	var b strings.Builder
	b.Grow(n * 12)
	for i := 0; i < n; i++ {
		b.WriteString("<style>x</style>")
	}
	out := stripElements(lowerASCII(b.String()), b.String())
	if out != "" {
		t.Fatalf("all style elements must be dropped, got %d bytes", len(out))
	}
}

// TestStripElementsUnclosedLinear is the second DoS regression: the linear
// pass cached nothing about missing closing tags, so a page of repeated
// unclosed <style> made every tag re-scan the whole tail (quadratic: ~75s at
// the 2 MB fetch cap in the long-lived server, driven by one untrusted link).
// The no-close cache must make the whole run a single pass; 300ms is a wide
// margin over the linear cost and a hard fail for the quadratic one (~2s here).
func TestStripElementsUnclosedLinear(t *testing.T) {
	const n = 50000
	var b strings.Builder
	b.Grow(n * 7)
	for i := 0; i < n; i++ {
		b.WriteString("<style>")
	}
	body := b.String()
	done := make(chan string, 1)
	go func() { done <- stripElements(lowerASCII(body), body) }()
	select {
	case out := <-done:
		if out != "" {
			t.Fatalf("style tags must be dropped, got %d bytes", len(out))
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("stripElements on unclosed drop-tags is not linear (tail re-scan)")
	}
}

// TestExtractTagSoupLinear guards the same invariant for the text loop: a
// giant run of unterminated "<a" must not re-scan the tail per tag (parseTag
// reports the EOF end once, consuming the soup in one scan).
func TestExtractTagSoupLinear(t *testing.T) {
	const n = 200000
	var b strings.Builder
	b.Grow(n * 2)
	for i := 0; i < n; i++ {
		b.WriteString("<a")
	}
	done := make(chan struct{}, 1)
	go func() {
		ExtractSections(b.String())
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("ExtractSections on a '<a' soup is not linear")
	}
}

// cleanText strips control bytes AFTER entity unescaping: html.UnescapeString
// turns &#27; into ESC, so untested markup (and stored pages) can inject ANSI
// escapes into titles/sections that every later terminal render executes.
func TestCleanTextStripsControl(t *testing.T) {
	title := ExtractTitle("<title>Head &#27;[31mRED&#27;[0m</title>")
	if strings.ContainsRune(title, 0x1b) {
		t.Errorf("ESC survived into the title: %q", title)
	}
	if title != "Head [31mRED[0m" {
		t.Errorf("unexpected title: %q", title)
	}
	secs, _, _ := ExtractSections("<p>plain &#0;and&#128; control</p>")
	joined := strings.Join(textOfSections(secs), " ")
	if strings.ContainsRune(joined, 0x00) || strings.ContainsRune(joined, 0x80) {
		t.Errorf("control bytes survived in section text: %q", joined)
	}
}

func textOfSections(secs []model.Section) []string {
	var out []string
	for _, s := range secs {
		out = append(out, s.Text)
	}
	return out
}
