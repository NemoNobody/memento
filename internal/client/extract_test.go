package client

import (
	"fmt"
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

// TestContentLinesKeepsCJK (finding 126): the junk filter counts RUNES now.
// A byte limit silently ate CJK content (one ideograph = 3 bytes); ASCII
// keeps the old short-menu-debris threshold.
func TestContentLinesKeepsCJK(t *testing.T) {
	lines := contentLines("安装\n说明文档\nFAQ\n→\nok")
	joined := strings.Join(lines, "|")
	for _, want := range []string{"安装", "说明文档"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CJK line %q was dropped: %q", want, joined)
		}
	}
	for _, junk := range []string{"FAQ", "→", "ok"} {
		if strings.Contains(joined, junk) {
			t.Errorf("junk %q survived the filter: %q", junk, joined)
		}
	}
}

// TestExtractPrefersMainContent pins the content-root rule: when the markup
// names the article (<main>, role="main", <article>), the link rail and the
// sidebar prose in front of it are neither extracted nor counted against the
// document budget. The sidebar is a plain div on purpose: elements dropped by
// dropTags would disappear even without the content root.
func TestExtractPrefersMainContent(t *testing.T) {
	page := `<html><body>
<div class="sidebar"><h2>Menu</h2><p>Sidebar prose that is not a link at all.</p></div>
<main><h1>Guide</h1><p>The real content of the page.</p>
<h2>Details</h2><p>More detail here.</p></main>
<footer><h2>Footer</h2><p><a href="/x">X</a></p></footer>
</body></html>`
	secs, _, _ := ExtractSections(page)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if strings.Join(headings, ",") != "Guide,Details" {
		t.Errorf("headings = %v, want the main content only (Guide, Details)", headings)
	}
}

// TestExtractPrefersRoleMainNested covers a main container that is a plain div
// with nested divs: the span must follow the matching close tag, not the first
// </div> in sight — the section after the nested div is inside the root and
// must survive.
func TestExtractPrefersRoleMainNested(t *testing.T) {
	page := `<html><body>
<div class="sidebar"><h2>Nav</h2><p><a href="/a">A</a></p></div>
<div role="main"><div class="wrap"><h2>Article</h2>
<p>Body text that matters and is long enough to keep.</p></div>
<div class="pager"><p><a href="/next">Next</a></p></div>
<h2>After</h2><p>Text after the nested div must survive the span.</p></div>
</body></html>`
	secs, _, _ := ExtractSections(page)
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if strings.Join(headings, ",") != "Article,After" {
		t.Fatalf("headings = %v, want Article,After", headings)
	}
	if !strings.Contains(secs[0].Text, "Body text that matters") {
		t.Errorf("article text missing: %q", secs[0].Text)
	}
}

// TestExtractDropsShortLinkChrome: without a content root, a short block that
// is mostly anchor text is a rail, not documentation.
func TestExtractDropsShortLinkChrome(t *testing.T) {
	page := `<html><body>
<div><h2>Home</h2><p><a href="/">Home</a> <a href="/docs">Docs</a> <a href="/news">News</a></p></div>
<div><h2>Tutorial</h2><p>` + strings.Repeat("real documentation text ", 20) + `</p></div>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Tutorial" {
		t.Fatalf("sections = %+v, want only the Tutorial section", secs)
	}
}

// TestExtractLinkRatioIgnoresWhitespace pins the denominator of the chrome
// ratio: block tags inject a newline per element, and counting those padded the
// block until a rail of links looked like prose.
func TestExtractLinkRatioIgnoresWhitespace(t *testing.T) {
	page := `<html><body>
<div><h2>Chapters</h2><p><a href="/a">Alpha</a></p><p><a href="/b">Beta</a></p><p><a href="/c">Gamma</a></p></div>
<div><h2>Body</h2><p>` + strings.Repeat("substantive text ", 20) + `</p></div>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Body" {
		t.Fatalf("sections = %+v, want only the Body section", secs)
	}
}

// TestExtractKeepsChromeWhenEverythingIsChrome: a page that is nothing but
// links is still a page — dropping every section would store an empty document.
func TestExtractKeepsChromeWhenEverythingIsChrome(t *testing.T) {
	page := `<html><body><div><h2>Index</h2><p><a href="/a">Alpha</a> <a href="/b">Beta</a></p></div></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) == 0 {
		t.Fatal("an all-link page must keep its sections")
	}
}

// TestExtractStripsNavAsideFooter pins that the chrome elements are removed
// even when the page has no content root to prefer.
func TestExtractStripsNavAsideFooter(t *testing.T) {
	page := `<html><body>
<nav><h2>Menu</h2><p>nav text</p></nav>
<aside><p>related reading</p></aside>
<h2>Body</h2><p>the article</p>
<footer><p>copyright</p></footer>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Body" {
		t.Fatalf("sections = %+v, want only Body", secs)
	}
	for _, s := range secs {
		for _, bad := range []string{"nav text", "related reading", "copyright"} {
			if strings.Contains(s.Text, bad) {
				t.Errorf("chrome %q survived: %+v", bad, s)
			}
		}
	}
}

// TestExtractAnchorTargetDoesNotPoisonSections: <a id="x"/> is an anchor
// target, not an opener. Counting it as one turned the rest of the paragraph
// into link text and dropped a real section without a truncation flag.
func TestExtractAnchorTargetDoesNotPoisonSections(t *testing.T) {
	page := `<html><body>
<h2>Install</h2><p><a id="after-install"/>Run the installer and point it at the archive.</p>
<h2>Verify</h2><p>Check that status reports the version.</p>
<h2>Usage</h2><p>Fetch a page and read the stored sections.</p>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 3 {
		t.Fatalf("sections = %+v, want Install, Verify and Usage", secs)
	}
	if secs[1].Heading != "Verify" || secs[2].Heading != "Usage" {
		t.Errorf("headings = %q, %q; want Verify, Usage", secs[1].Heading, secs[2].Heading)
	}
}

// TestExtractEmptyRootFallsThrough: an empty container is not the article; the
// next candidate (here a lone <article>) still gets its turn.
func TestExtractEmptyRootFallsThrough(t *testing.T) {
	page := `<html><body>
<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>
<main></main>
<article><h2>Article</h2><p>Article body text.</p></article>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 1 || secs[0].Heading != "Article" {
		t.Fatalf("sections = %+v, want just Article", secs)
	}
}

// TestExtractUnclosedAnchorStopsAtSection pins the blast radius of a broken
// anchor: hand-written HTML leaves <a> unclosed, and one such tag must not
// reach past the section it sits in.
func TestExtractUnclosedAnchorStopsAtSection(t *testing.T) {
	page := `<html><body>
<h2>Install</h2><p>Unclosed anchor follows.<a href="/docs">Docs
<h2>Verify</h2><p>Check that status reports the version.</p>
</body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[1].Heading != "Verify" {
		t.Fatalf("sections = %+v, want Install and Verify", secs)
	}
}

// TestExtractIgnoresCommentedCloseTag: a commented-out </main> is markup prose
// inside a comment, not the end of the article.
func TestExtractIgnoresCommentedCloseTag(t *testing.T) {
	page := `<html><body><main>
<h2>First</h2><p>First paragraph with enough text to keep.</p>
<!-- </main> -->
<h2>Second</h2><p>Second paragraph that must survive extraction.</p></main></body></html>`
	secs, _, _ := ExtractSections(page)
	if len(secs) != 2 || secs[1].Heading != "Second" {
		t.Fatalf("sections = %+v, want First and Second", secs)
	}
}

// TestExtractRoleMainVariants: role is an attribute with a token list — spacing
// around '=' and an extra token are still the main landmark, and data-role is a
// different attribute that must not capture the root.
func TestExtractRoleMainVariants(t *testing.T) {
	sidebar := `<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>`
	for _, tc := range []struct {
		name  string
		inner string
		want  string
	}{
		{"spaced equals", `<div role = "main"><h2>Article</h2><p>Article text here.</p></div>`, "Article"},
		{"token list", `<div role="main navigation"><h2>Article</h2><p>Article text here.</p></div>`, "Article"},
		{"newline after tag name", "<div\nrole=\"main\"><h2>Article</h2><p>Article text here.</p></div>", "Article"},
		{"tab after tag name", "<div\trole=\"main\"><h2>Article</h2><p>Article text here.</p></div>", "Article"},
		{"boolean attribute first", `<div hidden role="main"><h2>Article</h2><p>Article text here.</p></div>`, "Article"},
		{"data-role is not role", `<div data-role="main"><h2>Wrapper</h2><p>Wrapper prose here.</p></div>`, "Side,Wrapper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secs, _, _ := ExtractSections(`<html><body>` + sidebar + tc.inner + `</body></html>`)
			var headings []string
			for _, s := range secs {
				headings = append(headings, s.Heading)
			}
			if got := strings.Join(headings, ","); got != tc.want {
				t.Errorf("headings = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractSeveralArticlesMeanListing: an index page has one <article> per
// teaser; extracting the first one would store a single teaser as the document.
// A lone <article> is still the article.
func TestExtractSeveralArticlesMeanListing(t *testing.T) {
	sidebar := `<div class="sidebar"><h2>Side</h2><p>Sidebar prose that is not a link at all.</p></div>`
	three := `<article><h2>Article one</h2><p>First teaser text.</p></article>` +
		`<article><h2>Article two</h2><p>Second teaser text.</p></article>` +
		`<article><h2>Article three</h2><p>Third teaser text.</p></article>`
	secs, _, _ := ExtractSections(`<html><body>` + sidebar + three + `</body></html>`)
	if len(secs) != 4 {
		t.Fatalf("sections = %+v, want the listing (Side and three teasers)", secs)
	}

	one := `<div><h2>Comments</h2><p>Reader comments area.</p></div>` +
		`<article><h2>Post</h2><p>Post body text.</p></article>`
	secs, _, _ = ExtractSections(`<html><body>` + sidebar + one + `</body></html>`)
	if len(secs) != 1 || secs[0].Heading != "Post" {
		t.Fatalf("sections = %+v, want just Post", secs)
	}
}

// TestExtractDroppedChromeIsNotTruncation: the flag means "text was cut", not
// "blocks were dropped" — a stored document that lost nothing must not claim
// it is a prefix.
func TestExtractDroppedChromeIsNotTruncation(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < MaxSections; i++ {
		b.WriteString(`<div><h2>Rail</h2><p><a href="/x">Alpha</a> <a href="/y">Beta</a></p></div>`)
		fmt.Fprintf(&b, `<div><h2>Section %d</h2><p>%s</p></div>`, i, strings.Repeat("real text ", 10))
	}
	b.WriteString("</body></html>")
	secs, _, truncated := ExtractSections(b.String())
	if len(secs) != MaxSections {
		t.Fatalf("sections = %d, want %d", len(secs), MaxSections)
	}
	if truncated {
		t.Error("nothing was cut: the dropped blocks were rails, not text")
	}
}

// TestExtractSectionCapReportsTruncation is the other side of the same flag: a
// section past the cap is a real loss.
func TestExtractSectionCapReportsTruncation(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < MaxSections+2; i++ {
		fmt.Fprintf(&b, `<div><h2>Section %d</h2><p>%s</p></div>`, i, strings.Repeat("real text ", 10))
	}
	b.WriteString("</body></html>")
	secs, _, truncated := ExtractSections(b.String())
	if len(secs) != MaxSections {
		t.Fatalf("sections = %d, want %d", len(secs), MaxSections)
	}
	if !truncated {
		t.Error("two real sections were cut and the flag must say so")
	}
}

// TestExtractContentScanLinear guards the content-root scan. Two shapes made it
// quadratic before the guards: a run of open tags followed by unrelated close
// tags (every close walked the stack), and a deep stack with many candidates
// (every rejected candidate walked the stack below it). Both fit in
// fetch_max_bytes, so both must stay linear — the quadratic version took 15 s
// and 3.5 s on these inputs.
func TestExtractContentScanLinear(t *testing.T) {
	cases := []struct {
		name string
		page string
	}{
		{"unclosed main", strings.Repeat("<main>", 100000)},
		{"unmatched close tags", strings.Repeat("<div>", 50000) + strings.Repeat("</span>", 200000)},
		{"candidates in a deep stack", strings.Repeat("<div>", 20000) + "<main>" +
			strings.Repeat("<article>x</article>", 90000) + "</main>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{}, 1)
			go func() {
				ExtractSections(tc.page)
				done <- struct{}{}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the content scan is not linear on this input")
			}
		})
	}
}
