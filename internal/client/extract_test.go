package client

import (
	"strings"
	"testing"
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
