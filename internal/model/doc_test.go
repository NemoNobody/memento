package model

import (
	"encoding/json"
	"strings"
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

// TestDocNullExpiryIsNeverStale pins the reader side of a cleared expiry: a
// partial update writes "expires_at": null (OpenSearch keeps the key, not an
// absent field), and the doc must read back as "never expires" instead of
// erroring or looking stale.
func TestDocNullExpiryIsNeverStale(t *testing.T) {
	var d Doc
	raw := `{"url":"https://example.test/p","url_hash":"h","fetched_at":"2020-01-01T00:00:00Z","expires_at":null}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal doc with null expiry: %v", err)
	}
	if !d.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero for a null", d.ExpiresAt)
	}
	if d.Stale(time.Now().UTC()) {
		t.Error("a null expiry must mean never expires")
	}
}

// TestDocZeroExpirySerializesAbsent pins the writer side: a never-expiring
// snapshot must not carry an expiry at all. time.Time is a struct, so
// omitempty left year 1 in the document, and both a range filter and a reader
// then saw an expiry in the distant past.
func TestDocZeroExpirySerializesAbsent(t *testing.T) {
	raw, err := json.Marshal(&Doc{URL: "u"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "expires_at") {
		t.Errorf("zero expiry must be omitted, got: %s", raw)
	}

	// The legacy shape (year 1 stored verbatim by older builds) must read back
	// as "never expires", not as a date in the past.
	var legacy Doc
	if err := json.Unmarshal([]byte(`{"url":"u","expires_at":"0001-01-01T00:00:00Z"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.Stale(time.Now().UTC()) {
		t.Error("a year-1 expiry means \"never set\": it must not be stale")
	}
}
