package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// An Actionable issue the scheduler withholds for an open GitHub "blocked
// by" dependency must say so: `blocked` modifier, the blocker keys, and a
// caption naming the wait (hivecommons/hive#14765). Once the dependency
// resolves, or the issue is past the gate (claimed, covered by a PR), the
// row is untouched.
func TestStageTrackerBlockedByDependencyIsSurfaced(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	s := newTestServer()

	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{
			github.Issue{Number: 14635, Title: "search: index", Author: "pat", AuthorIsHuman: true,
				Labels:    []string{"stage/010-actionable"},
				DependsOn: []github.IssueDependency{{Key: "hive#14633"}, {Key: "hivecommons/crux#19"}, {Key: "hive#14600", Resolved: true}}},
			github.Issue{Number: 14636, Title: "search: ready", Author: "pat", AuthorIsHuman: true,
				Labels:    []string{"stage/010-actionable"},
				DependsOn: []github.IssueDependency{{Key: "hive#14600", Resolved: true}}},
			github.Issue{Number: 14637, Title: "search: claimed", Author: "pat", AuthorIsHuman: true,
				Labels:    []string{"stage/020-claimed"},
				DependsOn: []github.IssueDependency{{Key: "hive#14633"}}},
			github.Issue{Number: 14638, Title: "search: labelled and linked", Author: "pat", AuthorIsHuman: true,
				Labels:    []string{"stage/010-actionable", "blocked"},
				DependsOn: []github.IssueDependency{{Key: "hive#14633"}}},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, s.stageStore, now)

	blocked := trackerItem(t, resp, "issue", 14635)
	if blocked.Stage != 10 || !hasMod(blocked, modBlocked) {
		t.Errorf("#14635 = stage %d mods %v, want 010 + blocked", blocked.Stage, blocked.Modifiers)
	}
	if got, want := strings.Join(blocked.BlockedOn, ","), "hive#14633,hivecommons/crux#19"; got != want {
		t.Errorf("#14635 blocked_on = %q, want %q (resolved blocker dropped, sorted)", got, want)
	}
	if !strings.HasSuffix(blocked.Caption, "waiting on #14633, hivecommons/crux#19") {
		t.Errorf("#14635 caption = %q, want it to end naming the blockers", blocked.Caption)
	}
	if blocked.Actor != "its blockers" {
		t.Errorf("#14635 actor = %q, want its blockers", blocked.Actor)
	}

	ready := trackerItem(t, resp, "issue", 14636)
	if hasMod(ready, modBlocked) || ready.BlockedOn != nil || strings.Contains(ready.Caption, "waiting on") {
		t.Errorf("#14636 with only resolved deps = mods %v blocked_on %v caption %q, want plain Actionable", ready.Modifiers, ready.BlockedOn, ready.Caption)
	}

	claimed := trackerItem(t, resp, "issue", 14637)
	if claimed.Stage != 20 || hasMod(claimed, modBlocked) || claimed.BlockedOn != nil {
		t.Errorf("#14637 past the gate = stage %d mods %v blocked_on %v, want 020 untouched", claimed.Stage, claimed.Modifiers, claimed.BlockedOn)
	}

	labelled := trackerItem(t, resp, "issue", 14638)
	if n := countMod(labelled, modBlocked); n != 1 {
		t.Errorf("#14638 with label and dependency carries blocked ×%d, want once: %v", n, labelled.Modifiers)
	}
}

func TestStageTrackerBlockerList(t *testing.T) {
	cases := []struct {
		repo string
		in   []string
		want string
	}{
		{"hive", []string{"hive#1"}, "#1"},
		{"hivecommons/hive", []string{"hive#1", "hivecommons/hive#2"}, "#1, #2"},
		{"hive", []string{"hivecommons/crux#19", "#3"}, "hivecommons/crux#19, #3"},
	}
	for _, c := range cases {
		if got := stageTrackerBlockerList(c.repo, c.in); got != c.want {
			t.Errorf("stageTrackerBlockerList(%q, %v) = %q, want %q", c.repo, c.in, got, c.want)
		}
	}
}

func countMod(it StageTrackerItem, m string) int {
	n := 0
	for _, x := range it.Modifiers {
		if x == m {
			n++
		}
	}
	return n
}
