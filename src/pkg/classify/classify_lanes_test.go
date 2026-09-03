package classify

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestSetLanesCustom(t *testing.T) {
	defer SetLanes(nil) // restore defaults after test

	custom := []LaneConfig{
		{Name: "security", Keywords: []string{"cve", "vulnerability", "exploit"}},
		{Name: "docs", Keywords: []string{"documentation", "readme", "typo"}},
	}
	SetLanes(custom)

	lanes := activeLanes()
	if len(lanes) != 2 {
		t.Errorf("expected 2 custom lanes, got %d", len(lanes))
	}
	if lanes[0].Name != "security" {
		t.Errorf("first lane = %q, want 'security'", lanes[0].Name)
	}
}

func TestClassifyWithCustomLanes(t *testing.T) {
	defer SetLanes(nil)

	SetLanes([]LaneConfig{
		{Name: "infra", Keywords: []string{"kubernetes", "deploy", "helm"}},
	})

	issue := github.Issue{
		Title:  "Fix kubernetes deployment manifest",
		Labels: []string{"kind/bug"},
	}
	c := Classify(issue)
	if c.Lane != "infra" {
		t.Errorf("lane = %q, want 'infra'", c.Lane)
	}
}

func TestActiveLanesDefaultWhenEmpty(t *testing.T) {
	defer SetLanes(nil)

	SetLanes(nil)
	lanes := activeLanes()
	if len(lanes) == 0 {
		t.Error("should return default lanes when configured is nil")
	}
}

func TestActiveLanesDefaultWhenEmptySlice(t *testing.T) {
	defer SetLanes(nil)

	SetLanes([]LaneConfig{})
	lanes := activeLanes()
	if len(lanes) == 0 {
		t.Error("should return default lanes when configured is empty")
	}
}
