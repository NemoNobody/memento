package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/NemoNobody/velruna/internal/client"
)

func TestPrintDashboardPush_GlobalSaysWhereToLook(t *testing.T) {
	var out, errOut bytes.Buffer
	res := &client.DashboardPush{Index: ".kibana_2", Global: true, Objects: 22, Indexed: 22}
	if err := printDashboardPush(&out, &errOut, res, "http://os:9200", nil); err != nil {
		t.Fatalf("printDashboardPush: %v", err)
	}
	got := out.String()
	for _, want := range []string{"22 saved object", ".kibana", "global tenant", "(write index .kibana_2 at probe time)", "Global tenant"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if errOut.Len() != 0 {
		t.Errorf("a clean run wrote to stderr: %s", errOut.String())
	}
}

// A failed refresh means the objects are not visible yet: the success hint must
// not appear before the error.
func TestPrintDashboardPush_NoHintWhenTheRunFailed(t *testing.T) {
	var out, errOut bytes.Buffer
	res := &client.DashboardPush{Index: ".kibana_2", Global: true, Indexed: 22}
	if err := printDashboardPush(&out, &errOut, res, "http://os:9200", errFake{}); err != nil {
		t.Fatalf("printDashboardPush: %v", err)
	}
	if strings.Contains(out.String(), "hard-reload") {
		t.Errorf("the success hint was printed for a failed run:\n%s", out.String())
	}
}

func TestPrintDashboardPush_ExplicitIndexIsNotCalledGlobal(t *testing.T) {
	var out, errOut bytes.Buffer
	res := &client.DashboardPush{Index: ".kibana_custom", Indexed: 2, Objects: 2}
	if err := printDashboardPush(&out, &errOut, res, "http://os:9200", nil); err != nil {
		t.Fatalf("printDashboardPush: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "global tenant") {
		t.Errorf("an explicit index was reported as the global tenant:\n%s", got)
	}
}

// The warning is the only visible sign that the target was never checked.
func TestPrintDashboardPush_UnverifiedWarns(t *testing.T) {
	var out, errOut bytes.Buffer
	res := &client.DashboardPush{Index: ".kibana_1", Indexed: 22, Objects: 22, Unverified: true}
	if err := printDashboardPush(&out, &errOut, res, "http://os:9200", nil); err != nil {
		t.Fatalf("printDashboardPush: %v", err)
	}
	if !strings.Contains(errOut.String(), "may be the wrong index") {
		t.Errorf("no warning about the unverified index:\nstdout: %s\nstderr: %s", out.String(), errOut.String())
	}
}

func TestPrintDashboardPush_TotalFailureNamesTheIndex(t *testing.T) {
	var out, errOut bytes.Buffer
	res := &client.DashboardPush{
		Index: ".kibana_typo", Indexed: 0, Objects: 2, Failed: 2,
		Errors: []string{"a: HTTP 403", "b: HTTP 403"},
	}
	if err := printDashboardPush(&out, &errOut, res, "http://os:9200", nil); err != nil {
		t.Fatalf("printDashboardPush: %v", err)
	}
	for _, want := range []string{"failed: a: HTTP 403", "2 object(s) failed", "nothing was written", ".kibana_typo"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
}

type errFake struct{}

func (errFake) Error() string { return "refresh failed" }
