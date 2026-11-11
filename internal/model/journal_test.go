package model

import "testing"

// TestValidOp pins the journal vocabulary: the three operations the store
// writes and the journal queries read back. An unknown op must be refused so
// a typo cannot create entries no filter will ever find.
func TestValidOp(t *testing.T) {
	for _, op := range []string{OpWrite, OpRediscovery, OpRetire, "WRITE", "Retire"} {
		if !ValidOp(op) {
			t.Errorf("ValidOp(%q) = false, want true", op)
		}
	}
	for _, op := range []string{"", "retired", "delete", "write "} {
		if ValidOp(op) {
			t.Errorf("ValidOp(%q) = true, want false", op)
		}
	}
}
