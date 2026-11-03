package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

func celTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// enabledAgent is a minimal enabled, governor-kickable agent config.
func enabledAgent() config.AgentConfig {
	return config.AgentConfig{Enabled: true}
}

// newTestGovernor builds a governor whose named agents are all eligible for a
// CEL kick (no cadence-pause, no budget limit, not on-demand, default kick
// channel). This is the default state; we only need the agents map populated.
func newTestGovernor(agents map[string]config.AgentConfig) *governor.Governor {
	return governor.New(config.GovernorConfig{}, agents, celTestLogger())
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// TestCELRuleFiresAgent proves the core wiring: a config trigger rule whose CEL
// expression matches an enumerated actionable issue routes its agent into the
// due set, UNIONED with the governor's own selection (which is preserved), while
// a non-matching event adds nothing.
func TestCELRuleFiresAgent(t *testing.T) {
	// One rule: kick "reviewer" for any bug-labeled issue.
	cfg := &config.Config{
		Triggers: []config.TriggerRule{
			{
				Name:  "bug-to-reviewer",
				Expr:  `event.kind == "issue.opened" && hasLabel(event.labels, "bug")`,
				Agent: "reviewer",
			},
		},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"reviewer": enabledAgent(),
				"fixer":    enabledAgent(),
			},
		},
	}

	engine := celEngineFor(cfg, celTestLogger())
	if engine == nil {
		t.Fatal("expected a compiled CEL engine for a valid trigger rule")
	}

	gov := newTestGovernor(cfg.Agents)
	notPaused := func(string) bool { return false }

	// The governor's own selection this cycle.
	governorDue := []string{"fixer"}

	t.Run("matching event unions the CEL agent", func(t *testing.T) {
		actionable := &github.ActionableResult{
			Issues: github.IssueResult{
				Count: 1,
				Items: []github.Issue{
					{Repo: "org/repo", Number: 7, Title: "crash", Labels: []string{"bug"}},
				},
			},
		}

		celAgents := celMatchedAgents(engine, actionable, cfg, gov, notPaused, celTestLogger())
		if !contains(celAgents, "reviewer") {
			t.Fatalf("expected CEL to match 'reviewer', got %v", celAgents)
		}

		union := unionAgents(append([]string{}, governorDue...), celAgents)
		if !contains(union, "reviewer") {
			t.Errorf("expected 'reviewer' in due set after union, got %v", union)
		}
		// Additive: the governor's own agent must still be present.
		if !contains(union, "fixer") {
			t.Errorf("union dropped governor-selected 'fixer', got %v", union)
		}
	})

	t.Run("non-matching event adds nothing", func(t *testing.T) {
		actionable := &github.ActionableResult{
			Issues: github.IssueResult{
				Count: 1,
				Items: []github.Issue{
					// No "bug" label → rule does not match.
					{Repo: "org/repo", Number: 8, Title: "docs", Labels: []string{"documentation"}},
				},
			},
		}

		celAgents := celMatchedAgents(engine, actionable, cfg, gov, notPaused, celTestLogger())
		if len(celAgents) != 0 {
			t.Fatalf("expected no CEL matches for a non-bug issue, got %v", celAgents)
		}

		union := unionAgents(append([]string{}, governorDue...), celAgents)
		if contains(union, "reviewer") {
			t.Errorf("non-matching event must not add 'reviewer', got %v", union)
		}
		if !contains(union, "fixer") {
			t.Errorf("union dropped governor-selected 'fixer', got %v", union)
		}
	})
}

// TestCELGatingRespectsPauseAndBudget proves a CEL match never bypasses the
// governor's hard gates: a config-paused agent, a manager-paused agent, and a
// budget-exhausted agent are all excluded even though the rule matches.
func TestCELGatingRespectsPauseAndBudget(t *testing.T) {
	matchingIssue := &github.ActionableResult{
		Issues: github.IssueResult{
			Count: 1,
			Items: []github.Issue{
				{Repo: "org/repo", Number: 1, Labels: []string{"bug"}},
			},
		},
	}
	rule := config.TriggerRule{
		Name:  "bug-rule",
		Expr:  `hasLabel(event.labels, "bug")`,
		Agent: "reviewer",
	}

	t.Run("config-paused agent excluded", func(t *testing.T) {
		cfg := &config.Config{
			Triggers: []config.TriggerRule{rule},
			AgentRuntimeConfig: config.AgentRuntimeConfig{
				Agents: map[string]config.AgentConfig{
					"reviewer": {Enabled: true, Paused: true},
				},
			},
		}
		engine := celEngineFor(cfg, celTestLogger())
		gov := newTestGovernor(cfg.Agents)
		got := celMatchedAgents(engine, matchingIssue, cfg, gov, func(string) bool { return false }, celTestLogger())
		if len(got) != 0 {
			t.Errorf("config-paused agent must be excluded, got %v", got)
		}
	})

	t.Run("manager-paused agent excluded", func(t *testing.T) {
		cfg := &config.Config{
			Triggers: []config.TriggerRule{rule},
			AgentRuntimeConfig: config.AgentRuntimeConfig{
				Agents: map[string]config.AgentConfig{"reviewer": enabledAgent()},
			},
		}
		engine := celEngineFor(cfg, celTestLogger())
		gov := newTestGovernor(cfg.Agents)
		paused := func(name string) bool { return name == "reviewer" }
		got := celMatchedAgents(engine, matchingIssue, cfg, gov, paused, celTestLogger())
		if len(got) != 0 {
			t.Errorf("manager-paused agent must be excluded, got %v", got)
		}
	})

	t.Run("budget-exhausted agent excluded", func(t *testing.T) {
		cfg := &config.Config{
			Triggers: []config.TriggerRule{rule},
			AgentRuntimeConfig: config.AgentRuntimeConfig{
				Agents: map[string]config.AgentConfig{"reviewer": enabledAgent()},
			},
		}
		engine := celEngineFor(cfg, celTestLogger())
		gov := newTestGovernor(cfg.Agents)
		// Exhaust the budget: spend >= limit with no exemption.
		gov.SetBudgetLimit(100)
		gov.SeedBudget(200, nil, nil, gov.GetBudget().ResetAt)
		got := celMatchedAgents(engine, matchingIssue, cfg, gov, func(string) bool { return false }, celTestLogger())
		if len(got) != 0 {
			t.Errorf("budget-exhausted agent must be excluded, got %v", got)
		}
	})

	t.Run("unknown agent excluded", func(t *testing.T) {
		cfg := &config.Config{
			Triggers: []config.TriggerRule{{Name: "r", Expr: `hasLabel(event.labels, "bug")`, Agent: "ghost"}},
			AgentRuntimeConfig: config.AgentRuntimeConfig{
				Agents: map[string]config.AgentConfig{"reviewer": enabledAgent()},
			},
		}
		engine := celEngineFor(cfg, celTestLogger())
		gov := newTestGovernor(cfg.Agents)
		got := celMatchedAgents(engine, matchingIssue, cfg, gov, func(string) bool { return false }, celTestLogger())
		if len(got) != 0 {
			t.Errorf("unknown agent must be excluded, got %v", got)
		}
	})
}

// TestCELEngineFailClosed proves a malformed rule disables CEL routing (nil
// engine) instead of crashing, and an empty triggers list is a total no-op.
func TestCELEngineFailClosed(t *testing.T) {
	t.Run("empty triggers → nil engine", func(t *testing.T) {
		resetCELCache()
		cfg := &config.Config{}
		if e := celEngineFor(cfg, celTestLogger()); e != nil {
			t.Errorf("expected nil engine for empty triggers, got %v", e)
		}
	})

	t.Run("malformed rule → nil engine (fail-closed)", func(t *testing.T) {
		resetCELCache()
		cfg := &config.Config{
			Triggers: []config.TriggerRule{
				{Name: "bad", Expr: `event.nonexistent_field == "x"`, Agent: "a"},
			},
		}
		if e := celEngineFor(cfg, celTestLogger()); e != nil {
			t.Errorf("expected nil engine for malformed rule, got %v", e)
		}
	})
}

// TestCELMatchedAgentsNilSafe proves the zero-overhead guards.
func TestCELMatchedAgentsNilSafe(t *testing.T) {
	if got := celMatchedAgents(nil, nil, nil, nil, nil, celTestLogger()); got != nil {
		t.Errorf("nil inputs must return nil, got %v", got)
	}
	// Nil actionable with a real engine is still a no-op.
	cfg := &config.Config{
		Triggers: []config.TriggerRule{{Name: "r", Expr: "true", Agent: "a"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"a": enabledAgent()},
		},
	}
	engine := celEngineFor(cfg, celTestLogger())
	if got := celMatchedAgents(engine, nil, cfg, nil, nil, celTestLogger()); got != nil {
		t.Errorf("nil actionable must return nil, got %v", got)
	}
}

func TestUnionAgents(t *testing.T) {
	base := []string{"a", "b"}
	got := unionAgents(base, []string{"b", "c"})
	for _, want := range []string{"a", "b", "c"} {
		if !contains(got, want) {
			t.Errorf("expected %q in union %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("expected de-duplicated union of length 3, got %v", got)
	}
	// Empty add returns base unchanged.
	if u := unionAgents([]string{"x"}, nil); len(u) != 1 || u[0] != "x" {
		t.Errorf("empty add must return base unchanged, got %v", u)
	}
	// A repo-scoped governor target already covers the agent for this cycle;
	// CEL must not add a second aggregate kick for the same agent.
	if u := unionAgents([]string{config.CadenceTargetKey("scanner", "org/repo")}, []string{"scanner"}); len(u) != 1 {
		t.Errorf("repo-scoped base target should dedupe aggregate CEL add, got %v", u)
	}
}

// resetCELCache clears the process-wide engine cache so tests that assert on a
// fresh compile do not read a value cached by an earlier test with matching
// (empty) triggers.
func resetCELCache() {
	globalCELEngine.mu.Lock()
	defer globalCELEngine.mu.Unlock()
	globalCELEngine.sig = ""
	globalCELEngine.engine = nil
	globalCELEngine.built = false
}

// Compile-time assertion that the event mappers produce the expected kinds.
func TestEventMapperKinds(t *testing.T) {
	if ev := issueToEvent(github.Issue{Repo: "o/r", Number: 1}); ev.Kind != celtrigger.KindIssueOpened {
		t.Errorf("issue kind = %q, want %q", ev.Kind, celtrigger.KindIssueOpened)
	}
	if ev := prToEvent(github.PullRequest{Repo: "o/r", Number: 2, Draft: true}); ev.Kind != celtrigger.KindPROpened || !ev.IsDraft {
		t.Errorf("pr event mapping wrong: %+v", ev)
	}
}

// TestCELOpenedFiresOncePerItem verifies issue.opened / pr.opened are
// edge-triggered: the same snapshot evaluated twice matches once, and a newly
// appearing item still fires.
func TestCELOpenedFiresOncePerItem(t *testing.T) {
	cfg := &config.Config{
		Triggers: []config.TriggerRule{
			{Name: "issue", Expr: `event.kind == "issue.opened"`, Agent: "reviewer"},
			{Name: "pr", Expr: `event.kind == "pr.opened"`, Agent: "fixer"},
		},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"reviewer": enabledAgent(), "fixer": enabledAgent()},
		},
	}
	engine := celEngineFor(cfg, celTestLogger())
	gov := newTestGovernor(cfg.Agents)
	notPaused := func(string) bool { return false }
	snap := func(issues []int, prs []int) *github.ActionableResult {
		r := &github.ActionableResult{}
		for _, n := range issues {
			r.Issues.Items = append(r.Issues.Items, github.Issue{Repo: "o/r", Number: n})
		}
		for _, n := range prs {
			r.PRs.Items = append(r.PRs.Items, github.PullRequest{Repo: "o/r", Number: n})
		}
		return r
	}
	var seen celSeenSet
	run := func(a *github.ActionableResult) int {
		return len(celMatchedAgentsOnce(engine, a, cfg, gov, notPaused, celTestLogger(), &seen))
	}

	if got := run(snap([]int{1}, []int{2})); got != 2 {
		t.Fatalf("first eval: want 2 agents, got %d", got)
	}
	if got := run(snap([]int{1}, []int{2})); got != 0 {
		t.Fatalf("second eval of same snapshot: want 0 agents, got %d", got)
	}
	if got := run(snap([]int{1, 3}, []int{2})); got != 1 {
		t.Fatalf("new issue: want 1 agent, got %d", got)
	}
	if got := run(snap([]int{1, 3}, []int{2, 4})); got != 1 {
		t.Fatalf("new PR: want 1 agent, got %d", got)
	}
	if got := len(celMatchedAgents(engine, snap([]int{1}, nil), cfg, gov, notPaused, celTestLogger())); got != 1 {
		t.Fatalf("level-triggered wrapper must still match, got %d", got)
	}
}
