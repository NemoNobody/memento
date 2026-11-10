package client

import (
	"encoding/json"
	"testing"
)

func TestBuildSearchBody_NoFilters(t *testing.T) {
	got := buildSearchBody("hello world", "", "", "", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello world" {
		t.Errorf("query = %v, want hello world", mm["query"])
	}
	fields := mm["fields"].([]string)
	if len(fields) != 3 || fields[0] != "value" || fields[1] != "notes" || fields[2] != "summary" {
		t.Errorf("fields = %v, want [value notes summary]", fields)
	}
	if _, ok := boolQ["filter"]; ok {
		t.Error("filter present, want none")
	}
	if size, ok := got["size"].(int); !ok || size != NavSizeCap {
		// limit 0 = "no cap": an explicit size beats the server's implicit
		// default of 10, which silently truncated the result.
		t.Errorf("size = %v, want NavSizeCap (%d) for limit 0", got["size"], NavSizeCap)
	}
}

func TestBuildSearchBody_Limit(t *testing.T) {
	got := buildSearchBody("hello", "", "", "", 7)
	if size, ok := got["size"].(int); !ok || size != 7 {
		t.Errorf("size = %v, want 7", got["size"])
	}
}

// Navigation queries must carry an explicit size: without it OpenSearch
// returns an arbitrary 10-hit subset with no truncation signal.
func TestBuildNavBodiesSizeCapped(t *testing.T) {
	bodies := map[string]map[string]interface{}{
		"project": buildProjectBody("p", "", "key"),
		"keys":    buildKeysBody("p:", "", "", ""),
	}
	for name, body := range bodies {
		if size, ok := body["size"].(int); !ok || size != NavSizeCap {
			t.Errorf("%s: size = %v, want NavSizeCap (%d)", name, body["size"], NavSizeCap)
		}
	}
}

func TestBuildSearchBody_AllFilters(t *testing.T) {
	got := buildSearchBody("hello", "proj1", "config", "verified", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello" {
		t.Errorf("query = %v, want hello", mm["query"])
	}
	fields := mm["fields"].([]string)
	if len(fields) != 3 || fields[0] != "value" || fields[1] != "notes" || fields[2] != "summary" {
		t.Errorf("fields = %v, want [value notes summary]", fields)
	}
	filters, ok := boolQ["filter"].([]map[string]interface{})
	if !ok {
		t.Error("filter missing")
		return
	}
	if len(filters) != 3 {
		t.Errorf("filter count = %d, want 3", len(filters))
		return
	}
	for i, m := range filters {
		term := m["term"].(map[string]interface{})
		switch i {
		case 0:
			if term["project"].(string) != "proj1" {
				t.Errorf("filter[0] project = %v, want proj1", term["project"])
			}
		case 1:
			if term["category"].(string) != "config" {
				t.Errorf("filter[1] category = %v, want config", term["category"])
			}
		case 2:
			if term["status"].(string) != "verified" {
				t.Errorf("filter[2] status = %v, want verified", term["status"])
			}
		}
	}
}

func TestBuildSearchBody_SingleFilter(t *testing.T) {
	got := buildSearchBody("hello", "", "config", "", 0)
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello" {
		t.Errorf("query = %v, want hello", mm["query"])
	}
	filters, ok := boolQ["filter"].([]map[string]interface{})
	if !ok {
		t.Error("filter missing")
		return
	}
	if len(filters) != 1 {
		t.Errorf("filter count = %d, want 1", len(filters))
		return
	}
	m := filters[0]
	term := m["term"].(map[string]interface{})
	if term["category"].(string) != "config" {
		t.Errorf("filter[0] category = %v, want config", term["category"])
	}
}

func TestBuildProjectBody(t *testing.T) {
	got := buildProjectBody("proj1", "", "key")
	q := got["query"].(map[string]interface{})
	b := q["bool"].(map[string]interface{})
	filters := b["filter"].([]map[string]interface{})
	if len(filters) != 1 {
		t.Fatalf("filters = %d, want 1 (no prefix)", len(filters))
	}
	term := filters[0]["term"].(map[string]interface{})
	if term["project"].(string) != "proj1" {
		t.Errorf("project = %v, want proj1", term["project"])
	}

	got = buildProjectBody("proj1", "proj1:state:", "key")
	q = got["query"].(map[string]interface{})
	b = q["bool"].(map[string]interface{})
	filters = b["filter"].([]map[string]interface{})
	if len(filters) != 2 {
		t.Fatalf("filters = %d, want 2 (project + prefix)", len(filters))
	}
	pfx := filters[1]["prefix"].(map[string]interface{})
	if pfx["key"].(string) != "proj1:state:" {
		t.Errorf("prefix key = %v, want proj1:state:", pfx["key"])
	}
}

func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"state:", "state:", false},
		{"state:*", "state:", false},
		{" proj:foo:loc:* ", "proj:foo:loc:", false},
		{"proj:*:loc", "", true},
		{"*abc", "", true},
	}
	for _, tt := range tests {
		got, err := NormalizePrefix(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("normalizePrefix(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizePrefix(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildKeysBody(t *testing.T) {
	got := buildKeysBody("proj:", "state", "memento", "verified")
	q := got["query"].(map[string]interface{})
	b := q["bool"].(map[string]interface{})
	filters := b["filter"].([]map[string]interface{})
	if len(filters) != 4 {
		t.Fatalf("filters = %d, want 4 (prefix, wildcard, project, status)", len(filters))
	}
	if pfx := filters[0]["prefix"].(map[string]interface{}); pfx["key"] != "proj:" {
		t.Errorf("prefix = %v, want proj:", pfx["key"])
	}
	if wc := filters[1]["wildcard"].(map[string]interface{}); wc["key"] != "*state*" {
		t.Errorf("wildcard = %v, want *state*", wc["key"])
	}
	if term := filters[2]["term"].(map[string]interface{}); term["project"] != "memento" {
		t.Errorf("project filter = %v, want memento", term["project"])
	}
	if term := filters[3]["term"].(map[string]interface{}); term["status"] != "verified" {
		t.Errorf("status filter = %v, want verified", term["status"])
	}
	sort := got["sort"].([]map[string]interface{})
	if order := sort[0]["key"].(map[string]interface{}); order["order"] != "asc" {
		t.Errorf("sort = %v, want key asc", sort[0])
	}
}

func TestBuildIndexBody_Mappings(t *testing.T) {
	got := buildIndexBody()
	props := got["mappings"].(map[string]interface{})["properties"].(map[string]interface{})

	expectedTypes := map[string]string{
		"fact_id":       "keyword",
		"key":           "keyword",
		"value":         "text",
		"category":      "keyword",
		"project":       "keyword",
		"status":        "keyword",
		"created_at":    "date",
		"updated_at":    "date",
		"observed_at":   "date",
		"source":        "keyword",
		"notes":         "text",
		"summary":       "text",
		"current_ref":   "keyword",
		"author":        "keyword",
		"author_source": "keyword",
		"model":         "keyword",
		"host":          "keyword",
		"session":       "keyword",
		"updated_by":    "keyword",
	}

	for field, expectedType := range expectedTypes {
		if _, ok := props[field]; !ok {
			t.Errorf("missing mapping for %q", field)
			continue
		}
		m := props[field].(map[string]interface{})
		if m["type"].(string) != expectedType {
			t.Errorf("%q type = %v, want %s", field, m["type"], expectedType)
		}
	}

	// history carries enabled instead of just a type, so it is checked
	// separately from the loop above: stored, never indexed.
	history, ok := props["history"].(map[string]interface{})
	if !ok {
		t.Fatal("missing mapping for \"history\"")
	}
	if history["type"] != "object" {
		t.Errorf("history type = %v, want object", history["type"])
	}
	if history["enabled"] != false {
		t.Errorf("history enabled = %v, want false (stored, not indexed)", history["enabled"])
	}

	if len(props) != 20 {
		t.Errorf("expected 20 mappings, got %d", len(props))
	}
}

func TestBuildIndexBody_ValidJSON(t *testing.T) {
	got := buildIndexBody()
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal index body: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal index body: %v", err)
	}
	if _, ok := m["mappings"]; !ok {
		t.Error("missing mappings in index body")
	}
}

// TestBuildProjectBodySort pins the list sort contract: sort=updated orders
// by last activity (newest first, undated docs last — legacy facts must not
// hijack the top), anything else keeps the stable key-ascending navigation.
func TestBuildProjectBodySort(t *testing.T) {
	got := buildProjectBody("p", "", "updated")
	sort, ok := got["sort"].([]map[string]interface{})
	if !ok || len(sort) != 1 {
		t.Fatalf("sort = %v, want one clause", got["sort"])
	}
	updated, ok := sort[0]["updated_at"].(map[string]interface{})
	if !ok || updated["order"] != "desc" || updated["missing"] != "_last" {
		t.Errorf("updated sort clause = %v, want desc with missing:_last", sort[0])
	}

	got = buildProjectBody("p", "", "key")
	sort = got["sort"].([]map[string]interface{})
	key, ok := sort[0]["key"].(map[string]interface{})
	if !ok || key["order"] != "asc" {
		t.Errorf("key sort clause = %v, want asc", sort[0])
	}
}
