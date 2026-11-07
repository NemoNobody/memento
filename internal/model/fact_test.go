package model

import (
	"testing"
	"time"
)

func TestNewFact(t *testing.T) {
	f := NewFact("test:key", "some value", "endpoint", "web", "user:confirmed", "checked dev only")

	if f.Key != "test:key" {
		t.Errorf("Key = %q, want %q", f.Key, "test:key")
	}
	if f.Value != "some value" {
		t.Errorf("Value = %q, want %q", f.Value, "some value")
	}
	if f.Category != "endpoint" {
		t.Errorf("Category = %q, want %q", f.Category, "endpoint")
	}
	if f.Project != "web" {
		t.Errorf("Project = %q, want %q", f.Project, "web")
	}
	if f.Source != "user:confirmed" {
		t.Errorf("Source = %q, want %q", f.Source, "user:confirmed")
	}
	if f.Notes != "checked dev only" {
		t.Errorf("Notes = %q, want %q", f.Notes, "checked dev only")
	}
	if f.Status != "verified" {
		t.Errorf("Status = %q, want %q", f.Status, "verified")
	}
	if f.FactID == "" {
		t.Error("FactID should be a generated UUID, got empty")
	}
	if f.ObservedAt.IsZero() {
		t.Error("ObservedAt should be set for a new fact")
	}
	if loc := f.ObservedAt.Location(); loc != time.UTC {
		t.Errorf("ObservedAt location = %v, want UTC", loc)
	}
}

func TestNewFactUniqueIDs(t *testing.T) {
	f1 := NewFact("k", "v", "c", "p", "s", "")
	f2 := NewFact("k", "v", "c", "p", "s", "")
	if f1.FactID == f2.FactID {
		t.Errorf("NewFact should generate unique IDs, got %q for both", f1.FactID)
	}
}

// TestTimestampsAreUTC pins the fix for mixed time formats: `add` used to write
// local time while `obsolete` wrote UTC, so the same fact showed both "+03:00"
// and "Z" — same instant, but it reads like two different times.
func TestTimestampsAreUTC(t *testing.T) {
	f := NewFact("k", "v", "c", "p", "s", "")
	if loc := f.CreatedAt.Location(); loc != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", loc)
	}
	if loc := f.UpdatedAt.Location(); loc != time.UTC {
		t.Errorf("UpdatedAt location = %v, want UTC", loc)
	}
	if loc := f.ObservedAt.Location(); loc != time.UTC {
		t.Errorf("ObservedAt location = %v, want UTC", loc)
	}

	f.MarkObsolete("reason")
	if loc := f.UpdatedAt.Location(); loc != time.UTC {
		t.Errorf("UpdatedAt after MarkObsolete = %v, want UTC", loc)
	}
}

func TestValid(t *testing.T) {
	f := NewFact("k", "v", "c", "p", "s", "")
	if !f.Valid() {
		t.Errorf("new fact should be valid, Status=%q", f.Status)
	}

	f.Status = "obsolete"
	if f.Valid() {
		t.Errorf("obsolete fact should not be valid")
	}
}

func TestMarkObsolete(t *testing.T) {
	f := NewFact("k", "old value", "c", "p", "s", "checked partially")
	oldUpdated := f.UpdatedAt
	oldObserved := f.ObservedAt

	f.MarkObsolete("subject deleted")

	if f.Status != "obsolete" {
		t.Errorf("Status = %q, want obsolete", f.Status)
	}
	// The last seen value must stay recoverable: obsolete is not a value change.
	if f.Value != "old value" {
		t.Errorf("Value = %q, want %q (must not be overwritten)", f.Value, "old value")
	}
	if !f.ObservedAt.Equal(oldObserved) {
		t.Error("ObservedAt must pin the last observation; obsolete must not move it")
	}
	if want := "checked partially\nsubject deleted"; f.Notes != want {
		t.Errorf("Notes = %q, want %q", f.Notes, want)
	}
	if f.Valid() {
		t.Error("MarkObsolete should set status to obsolete (not valid)")
	}
	if f.UpdatedAt.Before(oldUpdated) {
		t.Error("UpdatedAt went backwards")
	}
}

func TestMarkObsoleteWithoutReasonKeepsNotes(t *testing.T) {
	f := NewFact("k", "v", "c", "p", "s", "note")
	f.MarkObsolete("")
	if f.Notes != "note" {
		t.Errorf("Notes = %q, want %q (an empty reason must not add a line)", f.Notes, "note")
	}
}

func TestTTLDays(t *testing.T) {
	cases := map[string]int{
		"config":      14,
		"endpoint":    14,
		"location":    30,
		"behavior":    30,
		"context_map": 0,
		"decision":    0,
		"":            0,
		"nonsense":    0,
	}
	for category, want := range cases {
		if got := TTLDays(category); got != want {
			t.Errorf("TTLDays(%q) = %d, want %d", category, got, want)
		}
	}
}

func TestAgeDays(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	fresh := &Fact{Category: "location", ObservedAt: now}
	if got := fresh.AgeDays(now); got != 0 {
		t.Errorf("observed now: AgeDays = %d, want 0", got)
	}

	// A few hours short of four days must still count as three.
	recent := &Fact{Category: "location", ObservedAt: now.Add(-100 * time.Hour)}
	if got := recent.AgeDays(now); got != 4 {
		t.Errorf("observed 100h ago: AgeDays = %d, want 4", got)
	}

	// A clock skew must not produce a negative age.
	future := &Fact{Category: "location", ObservedAt: now.Add(time.Hour)}
	if got := future.AgeDays(now); got != 0 {
		t.Errorf("observed in the future: AgeDays = %d, want 0", got)
	}

	// Pre-observed_at facts report -1: the age is unknown, not zero.
	legacy := &Fact{Category: "location"}
	if got := legacy.AgeDays(now); got != -1 {
		t.Errorf("zero observed_at: AgeDays = %d, want -1", got)
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		category string
		observed time.Time
		want     bool
	}{
		{"config at TTL boundary is not stale", "config", now.Add(-14 * 24 * time.Hour), false},
		{"config past TTL is stale", "config", now.Add(-15 * 24 * time.Hour), true},
		{"endpoint past TTL is stale", "endpoint", now.Add(-40 * 24 * time.Hour), true},
		{"location inside TTL is fresh", "location", now.Add(-29 * 24 * time.Hour), false},
		{"location past TTL is stale", "location", now.Add(-31 * 24 * time.Hour), true},
		// Categories without a TTL never go stale: a decision stays valid
		// until something contradicts it, no matter its age.
		{"no-TTL category never stales", "context_map", now.Add(-3650 * 24 * time.Hour), false},
		// An unknown observation time is stale by definition for TTL'd
		// categories: the agent cannot tell how old the fact is.
		{"unknown observation stales TTL'd category", "behavior", time.Time{}, true},
		{"unknown observation keeps no-TTL category fresh", "context_map", time.Time{}, false},
	}
	for _, tc := range cases {
		f := &Fact{Category: tc.category, ObservedAt: tc.observed}
		if got := f.Stale(now); got != tc.want {
			t.Errorf("%s: Stale = %v, want %v", tc.name, got, tc.want)
		}
	}
}
