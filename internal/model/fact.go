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
	// Embedding is reserved for semantic search; the knn plugin is not
	// registered yet, so it is never written (omitempty).
	Embedding []float32 `json:"embedding,omitempty" yaml:"embedding,omitempty"`
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
