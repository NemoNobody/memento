package dashboards

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The embedded bundle is a release artifact: if the generator produced a type
// the writer does not know, the push must fail here rather than half-install a
// dashboard.
func TestBundleParsesAndEveryTypeIsSupported(t *testing.T) {
	objs, err := Parse(Bundle())
	if err != nil {
		t.Fatalf("Parse(Bundle()): %v", err)
	}
	if len(objs) < 10 {
		t.Fatalf("bundle has %d objects, expected the full overview set", len(objs))
	}

	var haveDashboard bool
	now := time.Now()
	for _, o := range objs {
		if _, err := o.Document(now); err != nil {
			t.Errorf("embedded object %s: %v", o.DocumentID(), err)
		}
		if o.Type == "dashboard" && o.ID == "velruna-store-dashboard" {
			haveDashboard = true
		}
	}
	if !haveDashboard {
		t.Errorf("bundle has no dashboard:velruna-store-dashboard object")
	}
}

func TestDocumentShape(t *testing.T) {
	bundle := []byte(`{"id":"dash","type":"dashboard","attributes":{"title":"X"},"references":[]}`)
	objs, err := Parse(bundle)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	now := time.Date(2026, 11, 21, 10, 0, 0, 123456789, time.UTC)
	raw, err := objs[0].Document(now)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rendered document is not JSON: %v", err)
	}
	for _, key := range []string{"type", "updated_at", "migrationVersion", "references", "dashboard"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("document has no %q key: %s", key, raw)
		}
	}
	if string(doc["type"]) != `"dashboard"` {
		t.Errorf("type = %s, want \"dashboard\"", doc["type"])
	}
	if string(doc["updated_at"]) != `"2026-11-21T10:00:00.123Z"` {
		t.Errorf("updated_at = %s, want millisecond UTC", doc["updated_at"])
	}
	if string(doc["migrationVersion"]) != `{"dashboard":"7.9.3"}` {
		t.Errorf("migrationVersion = %s", doc["migrationVersion"])
	}
	if string(doc["references"]) != `[]` {
		t.Errorf("references = %s, want an empty array (the UI shape)", doc["references"])
	}
	if string(doc["dashboard"]) != `{"title":"X"}` {
		t.Errorf("attributes not nested under the type key: %s", doc["dashboard"])
	}
}

func TestDocumentRejectsUnknownType(t *testing.T) {
	objs, err := Parse([]byte(`{"id":"x","type":"lens","attributes":{}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = objs[0].Document(time.Now())
	if err == nil {
		t.Fatal("Document accepted an unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported saved object type") {
		t.Errorf("error = %v, want it to name the unsupported type", err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name   string
		bundle string
		want   string
	}{
		{"empty", "\n \n", "bundle is empty"},
		{"bad json", "{not json}\n", "bundle line 1"},
		{"no id", `{"type":"dashboard","attributes":{}}` + "\n", "without id or type"},
		{"no attributes", `{"id":"x","type":"dashboard"}` + "\n", "without attributes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.bundle))
			if err == nil {
				t.Fatalf("Parse accepted %q", tc.bundle)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseSkipsBlankLines(t *testing.T) {
	objs, err := Parse([]byte("\n" + `{"id":"a","type":"dashboard","attributes":{"t":1}}` + "\n\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("Parse returned %d objects, want 1", len(objs))
	}
}
