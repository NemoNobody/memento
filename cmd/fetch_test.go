package cmd

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/model"
)

func testDoc() *model.Doc {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	return &model.Doc{
		URL:       "https://example.com/guide",
		URLHash:   "abc123def4567890",
		Title:     "Example Guide",
		Project:   "memento",
		FetchedAt: now.Add(-24 * time.Hour),
		ExpiresAt: now.Add(24 * time.Hour),
		Sections: []model.Section{
			{Order: 0, Heading: "Intro", Text: "first section text"},
			{Order: 1, Heading: "Usage", Text: "second section text"},
		},
	}
}

func TestPrintDocOutline(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := printDocOutline(&buf, testDoc(), true, false, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"url: https://example.com/guide",
		"hash: abc123def4567890",
		"title: Example Guide",
		"project: memento",
		"cached: true",
		"[0] Intro",
		"[1] Usage",
		"memento doc abc123def4567890 <n>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("outline lacks %q:\n%s", want, out)
		}
	}
	// The outline never carries section bodies.
	if strings.Contains(out, "first section text") {
		t.Errorf("outline leaked section text:\n%s", out)
	}
}

func TestPrintDocOutlineStale(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.ExpiresAt = now.Add(-time.Hour) // past TTL
	if err := printDocOutline(&buf, d, false, false, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "stale: true") {
		t.Errorf("expired doc must be marked stale:\n%s", buf.String())
	}
}

func TestPrintDocFullAndSection(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now().UTC()
	if err := printDocFull(&buf, testDoc(), now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "first section text") || !strings.Contains(out, "second section text") {
		t.Errorf("--full must print every section:\n%s", out)
	}
	if !strings.Contains(out, "section [1] Usage") {
		t.Errorf("section header missing:\n%s", out)
	}

	buf.Reset()
	if err := writeSection(&buf, testDoc(), testDoc().Sections[0], now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "first section text") {
		t.Errorf("section body missing:\n%s", buf.String())
	}
}

func TestPrintDocOutlineCapsRows(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.Sections = nil
	for i := 0; i < outlineRows+5; i++ {
		d.Sections = append(d.Sections, model.Section{
			Order:   i,
			Heading: "Section " + strconv.Itoa(i),
			Text:    "body",
		})
	}
	if err := printDocOutline(&buf, d, false, false, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "  [0] Section 0") {
		t.Errorf("outline lacks the first row:\n%s", out)
	}
	if strings.Contains(out, "  ["+strconv.Itoa(outlineRows)+"] ") {
		t.Errorf("outline listed a row past the cap:\n%s", out)
	}
	if !strings.Contains(out, "more 5 blocks, orders 60..64") {
		t.Errorf("outline lacks the hidden-block summary:\n%s", out)
	}
}

func TestPrintDocOutlineTextVolume(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := testDoc()
	d.Size = 120
	d.FullChars = 400
	d.Truncated = true
	if err := printDocOutline(&buf, d, false, false, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "text: 120 of 400 chars stored") {
		t.Errorf("outline lacks the stored/total volume:\n%s", out)
	}
	if !strings.Contains(out, "store the whole page with --max-chars 0") {
		t.Errorf("truncated outline lacks the whole-page hint:\n%s", out)
	}
}

func TestPrintDocHits(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	docs := []model.Doc{*testDoc()}
	docs[0].ExpiresAt = now.Add(-time.Hour)
	if err := printDocHits(&buf, docs, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"abc123def4567890", "Example Guide", "[stale]", "first section text"} {
		if !strings.Contains(out, want) {
			t.Errorf("hits lack %q:\n%s", want, out)
		}
	}
}

func TestSnippetCapsAndCollapses(t *testing.T) {
	got := snippet("a\n\n  b   c", 10)
	if got != "a b c" {
		t.Errorf("snippet collapse = %q", got)
	}
	long := strings.Repeat("x", 50)
	got = snippet(long, 10)
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) || !strings.HasSuffix(got, "…") {
		t.Errorf("snippet cap = %q", got)
	}
	if len([]rune(got)) != 11 { // 10 + ellipsis
		t.Errorf("snippet len = %d, want 11", len([]rune(got)))
	}
}
