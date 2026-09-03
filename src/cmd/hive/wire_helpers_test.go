package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestBuildRepoActivityWireEmpty(t *testing.T) {
	if got := buildRepoActivityWire(nil); got != nil {
		t.Errorf("buildRepoActivityWire(nil) = %v, want nil", got)
	}
	if got := buildRepoActivityWire([]dashboard.RepoActivity{}); got != nil {
		t.Errorf("buildRepoActivityWire(empty) = %v, want nil", got)
	}
}

func TestBuildRepoActivityWireMapsEveryStat(t *testing.T) {
	at := func(i int) string { return fmt.Sprintf("2026-08-26T04:0%d:00Z", i) }
	in := []dashboard.RepoActivity{
		{
			Repo:       "hivecommons/hive",
			Issues:     dashboard.ActivityActionStat{Count: 1, NewestAt: at(0)},
			PRs:        dashboard.ActivityActionStat{Count: 2, NewestAt: at(1)},
			Comments:   dashboard.ActivityActionStat{Count: 3, NewestAt: at(2)},
			Merges:     dashboard.ActivityActionStat{Count: 4, NewestAt: at(3)},
			Claims:     dashboard.ActivityActionStat{Count: 5, NewestAt: at(4)},
			Reviews:    dashboard.ActivityActionStat{Count: 6, NewestAt: at(5)},
			Advisory:   dashboard.ActivityActionStat{Count: 7, NewestAt: at(6)},
			Reconciled: dashboard.ActivityActionStat{Count: 9, NewestAt: at(8)},
			Agents: []dashboard.AgentRepoActivity{{
				Agent:      "quality",
				Issues:     dashboard.ActivityActionStat{Count: 8, NewestAt: at(7)},
				Reconciled: dashboard.ActivityActionStat{Count: 10, NewestAt: at(9)},
			}},
		},
		{Repo: "kubestellar/other"},
	}

	out := buildRepoActivityWire(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}

	got := out[0]
	if got.Repo != "hivecommons/hive" {
		t.Errorf("Repo = %q", got.Repo)
	}
	checks := []struct {
		name      string
		count     int
		wantCount int
		newestAt  string
		wantAt    string
	}{
		{"Issues", got.Issues.Count, 1, got.Issues.NewestAt, at(0)},
		{"PRs", got.PRs.Count, 2, got.PRs.NewestAt, at(1)},
		{"Comments", got.Comments.Count, 3, got.Comments.NewestAt, at(2)},
		{"Merges", got.Merges.Count, 4, got.Merges.NewestAt, at(3)},
		{"Claims", got.Claims.Count, 5, got.Claims.NewestAt, at(4)},
		{"Reviews", got.Reviews.Count, 6, got.Reviews.NewestAt, at(5)},
		{"Advisory", got.Advisory.Count, 7, got.Advisory.NewestAt, at(6)},
		{"Reconciled", got.Reconciled.Count, 9, got.Reconciled.NewestAt, at(8)},
	}
	for _, c := range checks {
		if c.count != c.wantCount {
			t.Errorf("%s.Count = %d, want %d", c.name, c.count, c.wantCount)
		}
		if len(got.Agents) != 1 || got.Agents[0].Agent != "quality" || got.Agents[0].Issues.Count != 8 || got.Agents[0].Reconciled.Count != 10 {
			t.Errorf("agent activity not mapped: %+v", got.Agents)
		}
		if c.newestAt != c.wantAt {
			t.Errorf("%s.NewestAt = %q, want %q", c.name, c.newestAt, c.wantAt)
		}
	}

	// Zero-valued input maps to zero-valued wire, not to garbage.
	if out[1].Repo != "kubestellar/other" || out[1].Issues.Count != 0 || out[1].Issues.NewestAt != "" {
		t.Errorf("zero-valued repo mapped incorrectly: %+v", out[1])
	}
}

func TestConvertKnowledgeLayers(t *testing.T) {
	if got := convertKnowledgeLayers(nil); len(got) != 0 {
		t.Errorf("convertKnowledgeLayers(nil) = %v, want empty", got)
	}

	in := []config.KnowledgeLayer{
		{Type: "local", Path: "/data/kb", Shared: false},
		{Type: "remote", URL: "https://kb.example.com", Shared: true},
	}
	out := convertKnowledgeLayers(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Type != knowledge.LayerType("local") || out[0].Path != "/data/kb" || out[0].URL != "" || out[0].Shared {
		t.Errorf("layer[0] = %+v", out[0])
	}
	if out[1].Type != knowledge.LayerType("remote") || out[1].URL != "https://kb.example.com" || out[1].Path != "" || !out[1].Shared {
		t.Errorf("layer[1] = %+v", out[1])
	}
}

func TestRandomNameShape(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		name := randomName()
		parts := strings.Split(name, "-")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			t.Fatalf("randomName() = %q, want adjective-noun", name)
		}
		if name != strings.ToLower(name) {
			t.Fatalf("randomName() = %q, want lowercase", name)
		}
		seen[name] = true
	}
	// 48x48 combinations: 64 draws yielding a single name would mean the
	// random source is broken (fallback path returns a constant).
	if len(seen) < 2 {
		t.Errorf("randomName() produced no variety across 64 draws: %v", seen)
	}
}
