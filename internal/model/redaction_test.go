package model

import "testing"

// TestMergeRedactions pins the qualifier-write rule: a row of a field the write
// OVERWRITES is dropped (the field may have lost nothing this time), rows of
// untouched fields survive, and appended text accumulates rows — the earlier
// line is still in the notes.
func TestMergeRedactions(t *testing.T) {
	stored := []Redaction{{Field: "value", Pattern: "long-hex", Chars: 48}}

	got := MergeRedactions(stored, nil, []Redaction{{Field: "notes", Pattern: "long-base64", Chars: 40}})
	if len(got) != 2 || got[0].Field != "value" || got[1].Field != "notes" {
		t.Fatalf("merged = %+v, want the value row first, then the note row", got)
	}

	// Overwriting a field drops its stored row whether or not the new text lost
	// anything: a clean rewrite must not leave a stale claim behind.
	got = MergeRedactions(
		[]Redaction{{Field: "value", Pattern: "long-hex", Chars: 48}, {Field: "notes", Pattern: "long-hex", Chars: 40}},
		[]string{"notes"}, nil)
	if len(got) != 1 || got[0].Field != "value" {
		t.Errorf("clean note rewrite = %+v, want only the value row", got)
	}

	// A row for a replaced field is replaced, not duplicated.
	got = MergeRedactions(stored, []string{"value"}, []Redaction{{Field: "value", Pattern: "long-base64", Chars: 41}})
	if len(got) != 1 || got[0].Pattern != "long-base64" {
		t.Errorf("same-field merge = %+v, want one replaced row", got)
	}

	// Appended text accumulates: the previous line is still in notes.
	got = MergeRedactions(
		[]Redaction{{Field: "notes_append", Pattern: "long-hex", Chars: 40}},
		nil,
		[]Redaction{{Field: "notes_append", Pattern: "long-base64", Chars: 41}})
	if len(got) != 2 {
		t.Errorf("append merge = %+v, want both rows", got)
	}

	// Replacing notes erases the appended lines (and a retire reason) folded
	// into it, so their rows go with the text.
	got = MergeRedactions(
		[]Redaction{
			{Field: "value", Pattern: "long-hex", Chars: 48},
			{Field: "notes_append", Pattern: "long-hex", Chars: 40},
			{Field: "reason", Pattern: "long-hex", Chars: 41},
		},
		[]string{"notes"}, nil)
	if len(got) != 1 || got[0].Field != "value" {
		t.Errorf("notes replace = %+v, want only the value row", got)
	}

	if got := MergeRedactions(stored, nil, nil); len(got) != 1 || got[0].Field != "value" {
		t.Errorf("no-op merge must keep the stored rows: %+v", got)
	}
	if got := MergeRedactions(nil, nil, []Redaction{{Field: "notes", Pattern: "long-hex", Chars: 40}}); len(got) != 1 {
		t.Errorf("empty stored must take the new rows: %+v", got)
	}
}
