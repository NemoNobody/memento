package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HistoryEntry is one past observation of a fact, kept when the value changes.
type HistoryEntry struct {
	Value      string    `json:"value"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
}

// Fact represents a stored fact in the memory store.
type Fact struct {
	FactID     string    `json:"fact_id" yaml:"fact_id"`
	Key        string    `json:"key" yaml:"key"`
	Value      string    `json:"value" yaml:"value"`
	Category   string    `json:"category" yaml:"category"`
	Project    string    `json:"project" yaml:"project"`
	Status     string    `json:"status" yaml:"status"`
	CreatedAt  time.Time `json:"created_at" yaml:"created_at"`
	UpdatedAt  time.Time `json:"updated_at" yaml:"updated_at"`
	ObservedAt time.Time `json:"observed_at" yaml:"observed_at"`
	Source     string    `json:"source" yaml:"source"`
	Notes      string    `json:"notes,omitempty" yaml:"notes,omitempty"`

	// TTLDays is an optional writer-chosen staleness window in days. It may
	// only shorten the category default (the category value is the cap a
	// writer may pick; 0-category categories are uncapped). 0 means "not set:
	// use the category default".
	TTLDays int            `json:"ttl_days,omitempty" yaml:"ttl_days,omitempty"`
	History []HistoryEntry `json:"history,omitempty" yaml:"history,omitempty"`

	// Summary is a short (table-of-contents) rendering of Value. It is
	// required once a value passes config.SummaryMinLen, so listings stay
	// skimmable; the client write path enforces the length bounds.
	Summary string `json:"summary,omitempty" yaml:"summary,omitempty"`

	// Authorship labels: who/what recorded the fact, on which host, in which
	// agent session. SELF-REPORTED by design — audit metadata, not an
	// authentication factor (the caller supplies them). Author/AuthorSource/
	// Model/Host/Session are written once at creation; every later write
	// moves UpdatedBy (the last toucher).
	Author       string `json:"author,omitempty" yaml:"author,omitempty"`
	AuthorSource string `json:"author_source,omitempty" yaml:"author_source,omitempty"` // security | config | os | none
	Model        string `json:"model,omitempty" yaml:"model,omitempty"`
	Host         string `json:"host,omitempty" yaml:"host,omitempty"`
	Session      string `json:"session,omitempty" yaml:"session,omitempty"`
	UpdatedBy    string `json:"updated_by,omitempty" yaml:"updated_by,omitempty"`

	// CurrentRef is the journal entry id of the current value: the invariant
	// "the journal contains the current value" as data. Facts created before
	// the journal (and before the migrate-history run) have it empty.
	CurrentRef string `json:"current_ref,omitempty" yaml:"current_ref,omitempty"`
}

// NewFact creates a new fact with a generated ID and timestamps.
// Timestamps are stored in UTC so every fact has a consistent format,
// regardless of the machine's timezone.
func NewFact(key, value, category, project, source, notes string) *Fact {
	now := time.Now().UTC()
	return &Fact{
		FactID:     uuid.New().String(),
		Key:        key,
		Value:      value,
		Category:   category,
		Project:    project,
		Status:     "verified",
		CreatedAt:  now,
		UpdatedAt:  now,
		ObservedAt: now,
		Source:     source,
		Notes:      notes,
	}
}

// MarkObsolete marks a fact obsolete because its subject is gone. The value and
// ObservedAt are intentionally left untouched: the last seen observation stays
// recoverable. The reason is appended to Notes so earlier qualifiers are kept.
func (f *Fact) MarkObsolete(reason string) {
	f.Status = "obsolete"
	f.UpdatedAt = time.Now().UTC()
	if reason != "" {
		if f.Notes == "" {
			f.Notes = reason
		} else {
			f.Notes = f.Notes + "\n" + reason
		}
	}
}

// Valid returns true if the fact is currently trusted.
func (f *Fact) Valid() bool {
	return f.Status == "verified"
}

// ttlDaysByCategory maps a category to the age (in days) after which a fact
// should be re-verified before being trusted. 0 (including unknown categories)
// means the fact never goes stale. The store provides the signal; the agent
// judges — staleness is an instruction to re-check, not to discard.
//
// The same number is also the cap on a writer-chosen per-fact window
// (Fact.TTLDays): a writer may shorten a TTL when it knows the fact rots
// faster, never lengthen it past what the category promises. A 0 category is
// uncapped precisely because it has no promise to break.
var ttlDaysByCategory = map[string]int{
	"config":   14,
	"endpoint": 14,
	"location": 30,
	"behavior": 30,
	"recipe":   180,
	"rule":     90,
}

// ValidCategory reports whether category is one of the known categories —
// the ones the staleness policy and the tool schemas document. Unknown
// categories silently never age, so a typo would create facts that rot in
// the dark; callers reject them at the edge.
func ValidCategory(category string) bool {
	switch strings.ToLower(category) {
	case "config", "endpoint", "location", "behavior", "context_map", "recipe", "rule":
		return true
	}
	return false
}

// TTLDays returns the staleness TTL in days for a category, 0 for "never".
// Matching is case-insensitive so facts written before the write edge began
// canonicalizing categories still age correctly.
func TTLDays(category string) int {
	return ttlDaysByCategory[strings.ToLower(category)]
}

// ValidTTLOverride checks a writer-chosen per-fact staleness window against
// the category cap. It is the single contract behind "boundaries may come
// from the writer, the category sets the maximum": an over-cap request would
// silently outlive what the category promises auditors.
func ValidTTLOverride(category string, ttl int) error {
	if ttl <= 0 {
		return fmt.Errorf("ttl_days must be positive, got %d", ttl)
	}
	if cap := TTLDays(category); cap > 0 && ttl > cap {
		return fmt.Errorf("ttl_days %d exceeds the max for category %q (%dd): a writer may shorten a staleness window, never lengthen it past the category cap", ttl, strings.ToLower(category), cap)
	}
	return nil
}

// AgeDays returns whole days since the last observation, or -1 when
// observed_at is unknown (facts created before the field existed).
func (f *Fact) AgeDays(now time.Time) int {
	if f.ObservedAt.IsZero() {
		return -1
	}
	d := int(now.Sub(f.ObservedAt) / (24 * time.Hour))
	if d < 0 {
		return 0
	}
	return d
}

// AgeHours returns whole hours since the last observation, or -1 when
// observed_at is unknown. Fresh facts age in hours, not days: age_days=0
// cannot tell "just stored" from "23 hours old", and the agent's
// trust/re-verify decision depends on that difference.
func (f *Fact) AgeHours(now time.Time) int {
	if f.ObservedAt.IsZero() {
		return -1
	}
	h := int(now.Sub(f.ObservedAt) / time.Hour)
	if h < 0 {
		return 0
	}
	return h
}

// EffectiveTTLDays returns the staleness window actually in force: the
// per-fact override when one was chosen, otherwise the category default.
// 0 means the fact never goes stale.
func (f *Fact) EffectiveTTLDays() int {
	if f.TTLDays > 0 {
		return f.TTLDays
	}
	return TTLDays(f.Category)
}

// Stale reports whether the fact is past its effective TTL and should be
// re-verified. An unknown observation time is stale by definition: the agent
// cannot tell how old it is, so it must re-read the source.
func (f *Fact) Stale(now time.Time) bool {
	ttl := f.EffectiveTTLDays()
	if ttl == 0 {
		return false
	}
	age := f.AgeDays(now)
	if age < 0 {
		return true
	}
	return age > ttl
}
