package model

import (
	"testing"
	"time"
)

func TestDocStale(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	if (&Doc{}).Stale(now) {
		t.Error("a doc without an expiry must never be stale")
	}
	if !(&Doc{ExpiresAt: now.Add(-time.Second)}).Stale(now) {
		t.Error("a doc past expires_at must be stale (the web moved; re-fetch)")
	}
	if (&Doc{ExpiresAt: now.Add(time.Hour)}).Stale(now) {
		t.Error("a fresh doc must not be stale")
	}
}
