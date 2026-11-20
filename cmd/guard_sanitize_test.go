package cmd

import (
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/config"
	"github.com/NemoNobody/velruna/internal/secretguard"
)

// The guard's write policy separates two classes: a high-confidence credential
// refuses the write (the store is plaintext), while a medium-confidence
// heuristic hit — long hex, base64, a weak assignment — is replaced in place.
// The distinction matters because the heuristics fire on legitimate content
// (commit hashes, checksums, config names), and refusing those made a
// perfectly good fact unstorable with no escape at secret_guard=reject.
const mediumHex = "0123456789abcdef0123456789abcdef0123456789abcdef"

func TestGuardSanitizeRedactsMediumConfidence(t *testing.T) {
	value := "deployment id " + mediumHex
	notes := "checked by hand"
	redactions, err := guardSanitize(config.GuardReject, false,
		textField("value", &value),
		textField("notes", &notes))
	if err != nil {
		t.Fatalf("reject must redact a heuristic hit, not refuse: %v", err)
	}
	if strings.Contains(value, mediumHex) {
		t.Errorf("the flagged span survived: %q", value)
	}
	if !strings.Contains(value, "[velruna-redacted:long-hex]") {
		t.Errorf("marker missing: %q", value)
	}
	if !strings.Contains(value, "deployment id ") {
		t.Errorf("redaction destroyed the surrounding claim: %q", value)
	}
	if notes != "checked by hand" {
		t.Errorf("clean field rewritten: %q", notes)
	}
	if len(redactions) != 1 {
		t.Fatalf("redactions = %+v, want one row", redactions)
	}
	r := redactions[0]
	if r.Field != "value" || r.Pattern != "long-hex" || r.Chars != len(mediumHex) {
		t.Errorf("audit row = %+v, want value/long-hex/%d", r, len(mediumHex))
	}
	// The audit must never carry the text itself.
	if strings.Contains(r.Pattern, mediumHex) {
		t.Error("audit row leaked the flagged text")
	}
}

func TestGuardSanitizeRefusesHighConfidence(t *testing.T) {
	value := "the admin key is " + guardSentinel
	redactions, err := guardSanitize(config.GuardReject, false, textField("value", &value))
	if err == nil {
		t.Fatal("a high-confidence credential must refuse the write")
	}
	if redactions != nil {
		t.Errorf("a refused write must report no redactions: %+v", redactions)
	}
	if value != "the admin key is "+guardSentinel {
		t.Error("a refused field must be left untouched for the message")
	}
	if strings.Contains(err.Error(), guardSentinel) {
		t.Errorf("refusal echoes the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "aws-access-key") {
		t.Errorf("refusal must name the pattern: %v", err)
	}
	// reject has no confirmation flag.
	if _, err := guardSanitize(config.GuardReject, true, textField("value", &value)); err == nil {
		t.Fatal("reject bypassed with the confirmation flag")
	}
}

func TestGuardSanitizeIdentityFieldsRefuse(t *testing.T) {
	// A key is identity: it is matched on, so a redacted key would silently
	// become a different fact. Even a medium shape refuses.
	key := "proj:fact:" + mediumHex
	if _, err := guardSanitize(config.GuardReject, false, identityField("key", &key)); err == nil {
		t.Fatal("a medium shape in the key must refuse at reject")
	}
	// At ask the human can confirm: the operator decides the key is fine.
	if _, err := guardSanitize(config.GuardAsk, true, identityField("key", &key)); err != nil {
		t.Fatalf("confirmed identity finding must proceed: %v", err)
	}
	// Same for the source label and a tag set.
	src := "tool:" + mediumHex
	if _, err := guardSanitize(config.GuardReject, false, identityField("source", &src)); err == nil {
		t.Fatal("a medium shape in source must refuse")
	}
	tags := "recipe " + mediumHex
	if _, err := guardSanitize(config.GuardReject, false, identityField("keywords", &tags)); err == nil {
		t.Fatal("a medium shape in keywords must refuse")
	}
}

func TestGuardSanitizeAskConfirmationStoresRaw(t *testing.T) {
	value := "the admin key is " + guardSentinel
	redactions, err := guardSanitize(config.GuardAsk, true, textField("value", &value))
	if err != nil {
		t.Fatalf("confirmed false positive must be stored as written: %v", err)
	}
	if value != "the admin key is "+guardSentinel {
		t.Errorf("confirmed write was rewritten: %q", value)
	}
	if len(redactions) != 0 {
		t.Errorf("a confirmed write reports no redactions: %+v", redactions)
	}
	// Without the confirmation ask refuses, same as reject.
	if _, err := guardSanitize(config.GuardAsk, false, textField("value", &value)); err == nil {
		t.Fatal("unconfirmed finding at ask must refuse")
	}
}

func TestGuardSanitizeAllowDoesNothing(t *testing.T) {
	value := "the admin key is " + guardSentinel
	if _, err := guardSanitize(config.GuardAllow, false, textField("value", &value)); err != nil {
		t.Fatalf("allow mode must not refuse: %v", err)
	}
	if value != "the admin key is "+guardSentinel {
		t.Errorf("allow mode rewrote the value: %q", value)
	}
}

// A value that was nothing but the flagged span has nothing left to store: a
// fact whose value is a bare marker claims "something was here", which reads
// as a complete fact and is worse than refusing.
func TestGuardSanitizeRefusesWhenNothingIsLeft(t *testing.T) {
	value := mediumHex
	_, err := guardSanitize(config.GuardReject, false, textField("value", &value))
	if err == nil {
		t.Fatal("a value made only of a flagged span must refuse")
	}
	if !strings.Contains(err.Error(), "nothing but a long-hex span") {
		t.Errorf("the refusal must explain why: %v", err)
	}
	if !strings.Contains(err.Error(), "secret_guard=allow") {
		t.Errorf("the refusal must name the way out: %v", err)
	}
}

// The assignment heuristic matches the key name too; replacing the whole match
// would store "velruna-redacted" and lose what the fact was about.
func TestGuardSanitizeAssignmentKeepsKeyName(t *testing.T) {
	value := "password = Sup3rS3cret!"
	redactions, err := guardSanitize(config.GuardReject, false, textField("value", &value))
	if err != nil {
		t.Fatalf("a weak assignment is a heuristic hit, not a fatal one: %v", err)
	}
	if !strings.HasPrefix(value, "password = ") {
		t.Errorf("the key name was lost: %q", value)
	}
	if strings.Contains(value, "Sup3rS3cret") {
		t.Errorf("the value survived: %q", value)
	}
	if !strings.Contains(value, "[velruna-redacted:credential-assignment]") {
		t.Errorf("marker missing: %q", value)
	}
	if len(redactions) != 1 || redactions[0].Chars != len("Sup3rS3cret!") {
		t.Errorf("audit = %+v, want the value span only (%d chars)", redactions, len("Sup3rS3cret!"))
	}
}

// Redaction runs on stored text; a second pass over its own marker must find
// nothing (the marker is not a credential shape).
func TestRedactAllIsIdempotent(t *testing.T) {
	value := "deployment id " + mediumHex
	once, applied := secretguard.RedactAll("value", value)
	if len(applied) != 1 {
		t.Fatalf("first pass applied %d findings, want 1", len(applied))
	}
	twice, appliedAgain := secretguard.RedactAll("value", once)
	if len(appliedAgain) != 0 {
		t.Errorf("second pass re-redacted the marker: %q", twice)
	}
	if twice != once {
		t.Errorf("redaction is not idempotent: %q vs %q", once, twice)
	}
}

func TestHasContent(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"[velruna-redacted:long-hex]", false},
		{"  [velruna-redacted:long-hex]  ", false},
		{"[velruna-redacted:long-hex] [velruna-redacted:long-base64]", false},
		{"id [velruna-redacted:long-hex]", true},
		{"plain text", true},
	}
	for _, tc := range cases {
		if got := secretguard.HasContent(tc.text); got != tc.want {
			t.Errorf("HasContent(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
