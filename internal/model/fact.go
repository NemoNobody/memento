package model

import (
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
	FactID     string         `json:"fact_id" yaml:"fact_id"`
	Key        string         `json:"key" yaml:"key"`
	Value      string         `json:"value" yaml:"value"`
	Category   string         `json:"category" yaml:"category"`
	Project    string         `json:"project" yaml:"project"`
	Status     string         `json:"status" yaml:"status"`
	CreatedAt  time.Time      `json:"created_at" yaml:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at" yaml:"updated_at"`
	ObservedAt time.Time      `json:"observed_at" yaml:"observed_at"`
	Source     string         `json:"source" yaml:"source"`
	Notes      string         `json:"notes,omitempty" yaml:"notes,omitempty"`
	History    []HistoryEntry `json:"history,omitempty" yaml:"history,omitempty"`
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
var ttlDaysByCategory = map[string]int{
	"config":   14,
	"endpoint": 14,
	"location": 30,
	"behavior": 30,
}

// TTLDays returns the staleness TTL in days for a category, 0 for "never".
func TTLDays(category string) int {
	return ttlDaysByCategory[category]
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

// Stale reports whether the fact is past its category TTL and should be
// re-verified. An unknown observation time is stale by definition: the agent
// cannot tell how old it is, so it must re-read the source.
func (f *Fact) Stale(now time.Time) bool {
	ttl := TTLDays(f.Category)
	if ttl == 0 {
		return false
	}
	age := f.AgeDays(now)
	if age < 0 {
		return true
	}
	return age > ttl
}
