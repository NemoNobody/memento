package cmd

import (
	"testing"
	"time"

	"github.com/NemoNobody/memento/internal/model"
)

// TestNeedsMigration pins the migration selection: unlinked facts and
// linked-but-array-carrying facts are work; fully migrated facts are not.
// The linked+array case is the half-done fact an early migration pass or a
// post-upgrade edit leaves behind — skipping it (the old rule) let the
// legacy array live forever.
func TestNeedsMigration(t *testing.T) {
	base := func() *model.Fact {
		return model.NewFact("k", "v", "config", "p", "s", "")
	}
	linked := base()
	linked.CurrentRef = "e1"
	linkedArr := base()
	linkedArr.CurrentRef = "e1"
	linkedArr.History = []model.HistoryEntry{{Value: "v0"}}

	cases := []struct {
		name string
		f    *model.Fact
		want bool
	}{
		{"unlinked, no array", base(), true},
		{"unlinked, array", func() *model.Fact { f := base(); f.History = []model.HistoryEntry{{Value: "v0"}}; return f }(), true},
		{"linked, no array", linked, false},
		{"linked, array still present", linkedArr, true},
	}
	for _, tc := range cases {
		if got := needsMigration(*tc.f); got != tc.want {
			t.Errorf("%s: needsMigration = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPlanMigrateEntries pins plan/execute agreement: the entry count must
// match exactly what MigrateFact appends — history entries always, the
// current-value entry only while the fact is unlinked.
func TestPlanMigrateEntries(t *testing.T) {
	mk := func(ref string, hist int) *model.Fact {
		f := model.NewFact("k", "v", "config", "p", "s", "")
		f.CurrentRef = ref
		for i := 0; i < hist; i++ {
			f.History = append(f.History, model.HistoryEntry{Value: "h", ObservedAt: time.Now().UTC()})
		}
		return f
	}
	facts := []model.Fact{
		*mk("", 2),   // full migration: 2 history + 1 current
		*mk("e1", 3), // linked half-migrant: only the 3 array entries
		*mk("e1", 0), // done
		*mk("", 0),   // journal-era fact with no array: current entry only
	}
	toMigrate, entries, skipped := planMigrate(facts)
	if toMigrate != 3 {
		t.Errorf("toMigrate = %d, want 3", toMigrate)
	}
	if entries != 7 {
		t.Errorf("entries = %d, want 7 (2+1, 3, 1)", entries)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
}
