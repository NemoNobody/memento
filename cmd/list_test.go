package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/model"
)

// The summary is the store's table of contents: a one-line listing must
// show it instead of a long value (facts stored before summaries existed
// keep showing the value).
func TestPrintFactsPrefersSummary(t *testing.T) {
	var buf bytes.Buffer
	facts := []model.Fact{
		{Key: "k1", Value: strings.Repeat("v", 100), Summary: "toc of k1", Status: "verified"},
		{Key: "k2", Value: "plain value", Status: "verified"},
	}
	if err := printFacts(&buf, facts, time.Now()); err != nil {
		t.Fatalf("printFacts: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "k1: toc of k1 [summary]") {
		t.Errorf("a summarized fact must list its summary, got: %s", out)
	}
	if !strings.Contains(out, "k2: plain value") {
		t.Errorf("a fact without a summary must list its value, got: %s", out)
	}
	if strings.Contains(out, strings.Repeat("v", 100)) {
		t.Errorf("a summarized value must not bloat the listing, got: %s", out)
	}
}

// get shows the summary next to the value when one is stored.
func TestPrintFactShowsSummary(t *testing.T) {
	var buf bytes.Buffer
	f := &model.Fact{Key: "k", Value: "v", Summary: "toc", Status: "verified", Category: "config", Project: "p"}
	if err := printFact(&buf, f, time.Now(), false); err != nil {
		t.Fatalf("printFact: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "value: v\nsummary: toc\n") {
		t.Errorf("the summary must render right after the value, got: %s", out)
	}
}
