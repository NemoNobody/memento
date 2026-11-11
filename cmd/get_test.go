package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/model"
)

func testFact() *model.Fact {
	return &model.Fact{
		Key:        "proj:x:fact:a",
		Value:      "v",
		Category:   "config",
		Project:    "x",
		Status:     "verified",
		Source:     "tool:test",
		ObservedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC),
	}
}

// --history must say what it looked at. A journal-era fact has an empty
// in-doc array; printing nothing would read as "this fact never changed"
// (finding 138: the flag was silently dead for exactly those facts).
func TestPrintFactHistoryShapes(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

	f := testFact()
	f.History = []model.HistoryEntry{{Value: "old", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "tool:mcp"}}
	var buf bytes.Buffer
	if err := printFact(&buf, f, now, true, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "history:\n") || !strings.Contains(buf.String(), `"old"`) {
		t.Errorf("legacy in-doc history not rendered: %s", buf.String())
	}

	f = testFact()
	entries := []model.JournalEntry{
		{Op: "write", Value: "v", CreatedAt: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)},
		{Op: "write", Value: "older", CreatedAt: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)},
	}
	buf.Reset()
	if err := printFact(&buf, f, now, true, entries, 5, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "history (journal, newest first): 5 entries") || !strings.Contains(out, "showing the newest 2") {
		t.Errorf("journal history not rendered with honest counts: %s", out)
	}
	if !strings.Contains(out, "[\"write\"] value=\"older\"") {
		t.Errorf("journal entry values missing: %s", out)
	}

	buf.Reset()
	if err := printFact(&buf, testFact(), now, true, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "history: none") {
		t.Errorf("empty history must be stated, not silent: %s", buf.String())
	}

	buf.Reset()
	if err := printFact(&buf, testFact(), now, true, nil, 0, errors.New("cluster down")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "history: journal read failed: cluster down") {
		t.Errorf("journal read failure must be reported under the fact: %s", buf.String())
	}

	buf.Reset()
	if err := printFact(&buf, testFact(), now, false, entries, 5, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "history") {
		t.Errorf("history must stay out without the flag: %s", buf.String())
	}
}

// Every rendered timestamp is UTC and says so; a zero updated_at (legacy
// rows) must render "unknown", not year 1 (finding 141).
func TestDisplayTimeUTCAndZero(t *testing.T) {
	if got := displayTime(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)); got != "2026-11-01 12:00:00Z" {
		t.Errorf("displayTime = %q, want a UTC-marked stamp", got)
	}
	if got := displayTime(time.Time{}); got != "unknown" {
		t.Errorf("zero displayTime = %q, want unknown", got)
	}
	f := testFact()
	f.UpdatedAt = time.Time{}
	var buf bytes.Buffer
	if err := printFact(&buf, f, time.Now().UTC(), false, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "updated_at: unknown") {
		t.Errorf("zero updated_at must render unknown: %s", buf.String())
	}
}
