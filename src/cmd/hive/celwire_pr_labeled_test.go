package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestCELPRLabeledFiresWhenOptInLabelAdded pins hivecommons/hive#18538: a
// maintainer adding the review opt-in label to an already-seen PR kicks the
// reviewer on the next eval, once, instead of waiting for its cadence.
func TestCELPRLabeledFiresWhenOptInLabelAdded(t *testing.T) {
	cfg := &config.Config{
		Triggers: []config.TriggerRule{
			{Name: "review-on-label", Expr: `event.kind == "pr.labeled" && "hive-review" in event.added_labels`, Agent: "reviewer"},
		},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"reviewer": enabledAgent()},
		},
	}
	engine := celEngineFor(cfg, celTestLogger())
	gov := newTestGovernor(cfg.Agents)
	notPaused := func(string) bool { return false }
	snap := func(labels ...string) *github.ActionableResult {
		r := &github.ActionableResult{}
		r.PRs.Items = append(r.PRs.Items, github.PullRequest{Repo: "o/r", Number: 7, Labels: labels})
		return r
	}
	var seen celSeenSet
	run := func(a *github.ActionableResult) int {
		return len(celMatchedAgentsOnce(engine, a, cfg, gov, notPaused, celTestLogger(), &seen))
	}

	steps := []struct {
		name   string
		labels []string
		want   int
	}{
		{"first sight is pr.opened, not labeled", []string{"bug"}, 0},
		{"unchanged labels", []string{"bug"}, 0},
		{"unrelated label added", []string{"bug", "area/attn"}, 0},
		{"opt-in label added", []string{"bug", "area/attn", "hive-review"}, 1},
		{"label still there next eval", []string{"bug", "area/attn", "hive-review"}, 0},
		{"label removed", []string{"bug"}, 0},
		{"label added back", []string{"bug", "hive-review"}, 1},
	}
	for _, s := range steps {
		if got := run(snap(s.labels...)); got != s.want {
			t.Fatalf("%s: matched %d agents, want %d", s.name, got, s.want)
		}
	}
}

func TestCELSeenSetObserveLabels(t *testing.T) {
	var s celSeenSet
	if first, added := s.observeLabels("k", []string{"a"}); !first || added != nil {
		t.Fatalf("first observe = %v %v", first, added)
	}
	if first, added := s.observeLabels("k", []string{"A", "b"}); first || len(added) != 1 || added[0] != "b" {
		t.Fatalf("label compare must be case-insensitive and report only b: %v %v", first, added)
	}
	// A key first recorded by firstSight has no label baseline yet.
	var t2 celSeenSet
	t2.firstSight("k")
	if first, added := t2.observeLabels("k", []string{"x"}); first || added != nil {
		t.Fatalf("baseline after firstSight = %v %v", first, added)
	}
}

func TestPRLabeledEvent(t *testing.T) {
	ev := prLabeledEvent(github.PullRequest{Repo: "o/r", Number: 3, Labels: []string{"a", "b"}}, []string{"b"})
	if ev.Kind != celtrigger.KindPRLabeled || len(ev.AddedLabels) != 1 || ev.AddedLabels[0] != "b" || len(ev.Labels) != 2 || ev.Number != 3 {
		t.Fatalf("prLabeledEvent = %+v", ev)
	}
}
