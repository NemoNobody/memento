package cmd

import (
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/config"
)

const guardSentinel = "AKIAIOSFODNN7EXAMPLE" // the AWS documentation example key

func fields() []guardField {
	return []guardField{
		{"value", "the admin key is " + guardSentinel},
		{"notes", "checked by hand"},
	}
}

func TestGuardCheckReject(t *testing.T) {
	if err := guardCheck(config.GuardReject, false, fields()...); err == nil {
		t.Fatal("reject mode stored credential-like text")
	}
	// reject has no confirmation: the flag must not open a bypass.
	if err := guardCheck(config.GuardReject, true, fields()...); err == nil {
		t.Fatal("reject mode bypassed with the confirmation flag")
	}
	if err := guardCheck(config.GuardReject, false, guardField{"value", "plain fact text"}); err != nil {
		t.Errorf("reject blocked clean text: %v", err)
	}
}

func TestGuardCheckAsk(t *testing.T) {
	err := guardCheck(config.GuardAsk, false, fields()...)
	if err == nil {
		t.Fatal("ask mode stored credential-like text without confirmation")
	}
	if !strings.Contains(err.Error(), "--allow-secret") {
		t.Errorf("ask message must name the confirmation flag: %v", err)
	}
	if err := guardCheck(config.GuardAsk, true, fields()...); err != nil {
		t.Errorf("ask with confirmation must proceed, got: %v", err)
	}
}

func TestGuardCheckAllow(t *testing.T) {
	if err := guardCheck(config.GuardAllow, false, fields()...); err != nil {
		t.Errorf("allow mode must not block, got: %v", err)
	}
}

// TestGuardCheckMessageIsMasked is the leak guarantee: the guard error is the
// only channel that reports a hit, and it must carry masked previews only.
func TestGuardCheckMessageIsMasked(t *testing.T) {
	err := guardCheck(config.GuardReject, false, fields()...)
	if err == nil {
		t.Fatal("want a guard error")
	}
	if strings.Contains(err.Error(), guardSentinel) {
		t.Errorf("guard message echoes the secret: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"field value", "aws-access-key", "****"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}
