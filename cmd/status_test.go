package cmd

import (
	"strings"
	"testing"

	"github.com/NemoNobody/memento/internal/client"
)

func TestStorageNote(t *testing.T) {
	tests := []struct {
		name string
		st   *client.StorageState
		want string
	}{
		{"nil", nil, ""},
		{
			"absent",
			&client.StorageState{ClusterHealth: "yellow", IndexAbsent: true},
			"index does not exist yet: it is created on the first write",
		},
		{"red", &client.StorageState{IndexHealth: "red"}, "some primary shards are unassigned: parts of the index are unavailable"},
		{"yellow", &client.StorageState{IndexHealth: "yellow"}, "all primary shards are active; some replica shards are unassigned, so redundancy is reduced"},
		{"green", &client.StorageState{IndexHealth: "green"}, "all shards are active"},
		{"no health collected", &client.StorageState{ClusterHealth: "green"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := storageNote(tt.st); got != tt.want {
				t.Errorf("storageNote() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrintStorage_IndexAbsent(t *testing.T) {
	rep := statusReport{
		Storage:     &client.StorageState{ClusterHealth: "yellow", IndexAbsent: true, DataNodes: 1},
		StorageNote: storageNote(&client.StorageState{IndexAbsent: true}),
	}
	var b strings.Builder
	if err := printStorage(&b, rep); err != nil {
		t.Fatalf("printStorage: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "index=absent") {
		t.Errorf("output must mark the index absent, got:\n%s", out)
	}
	if strings.Contains(out, "index= ") {
		t.Errorf("empty index health leaked into the output:\n%s", out)
	}
	if !strings.Contains(out, "index does not exist yet") {
		t.Errorf("missing explanatory note:\n%s", out)
	}
}
