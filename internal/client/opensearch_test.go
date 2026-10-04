package client

import (
	"encoding/json"
	"testing"
)

func TestBuildSearchBody_NoFilters(t *testing.T) {
	got := buildSearchBody("hello world", "", "", "")
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello world" {
		t.Errorf("query = %v, want hello world", mm["query"])
	}
	fields := mm["fields"].([]string)
	if len(fields) != 2 || fields[0] != "value" || fields[1] != "notes" {
		t.Errorf("fields = %v, want [value notes]", fields)
	}
	if _, ok := boolQ["filter"]; ok {
		t.Error("filter present, want none")
	}
}

func TestBuildSearchBody_AllFilters(t *testing.T) {
	got := buildSearchBody("hello", "proj1", "config", "verified")
	q := got["query"].(map[string]interface{})
	boolQ := q["bool"].(map[string]interface{})
	must := boolQ["must"].(map[string]interface{})
	mm := must["multi_match"].(map[string]interface{})
	if mm["query"].(string) != "hello" {
		t.Errorf("query = %v, want hello", mm["query"])
	}
	fields := mm["fields"].([]string)
	if len(fields) != 2 || fields[0] != "value" || fields[1] != "notes" {
		t.Errorf("fields = %v, want [value notes]", fields)
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
	got := buildSearchBody("hello", "", "config", "")
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
	got := buildProjectBody("proj1")
	q := got["query"].(map[string]interface{})
	term := q["term"].(map[string]interface{})
	if term["project"].(string) != "proj1" {
		t.Errorf("project = %v, want proj1", term["project"])
	}
}

func TestBuildIndexBody_Mappings(t *testing.T) {
	got := buildIndexBody()
	props := got["mappings"].(map[string]interface{})["properties"].(map[string]interface{})

	expectedTypes := map[string]string{
		"fact_id":     "keyword",
		"key":         "keyword",
		"value":       "text",
		"category":    "keyword",
		"project":     "keyword",
		"status":      "keyword",
		"created_at":  "date",
		"updated_at":  "date",
		"observed_at": "date",
		"source":      "keyword",
		"notes":       "text",
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

	if len(props) != 12 {
		t.Errorf("expected 12 mappings, got %d", len(props))
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
