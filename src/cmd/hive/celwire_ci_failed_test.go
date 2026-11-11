package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type ciFailedTestKicker struct {
	kicks map[string]string
}

func (k *ciFailedTestKicker) SendKick(name, message string) error {
	if k.kicks == nil {
		k.kicks = map[string]string{}
	}
	k.kicks[name] = message
	return nil
}

func ciFailedTestRun() github.CIFailedRun {
	return github.CIFailedRun{
		Repo:       "widget",
		Branch:     "main",
		Check:      "build",
		HeadSHA:    "0123456789abcdef0123",
		Conclusion: "failure",
		URL:        "https://github.test/acme/widget/runs/21",
	}
}

func TestCIFailedEventFields(t *testing.T) {
	ev := ciFailedEvent(ciFailedTestRun())
	want := celtrigger.NormalizedEvent{Kind: celtrigger.KindCIFailed, Repo: "widget", Title: "build", HeadBranch: "main", State: "failure"}
	if ev.Kind != want.Kind || ev.Repo != want.Repo || ev.Title != want.Title || ev.HeadBranch != want.HeadBranch || ev.State != want.State || ev.Number != 0 {
		t.Fatalf("ciFailedEvent = %+v, want %+v", ev, want)
	}
	msg := ciFailedKickReason(ciFailedTestRun())
	for _, part := range []string{"ci.failed", `"build"`, "failure", "main", "widget", "0123456789ab", "https://github.test/acme/widget/runs/21"} {
		if !strings.Contains(msg, part) {
			t.Errorf("kick reason %q lacks %q", msg, part)
		}
	}
	if strings.Contains(msg, "0123456789abcdef0123") {
		t.Errorf("kick reason should shorten the SHA: %q", msg)
	}
}

// A ci.failed rule kicks its agent through the shared CEL kick path; a rule
// that never names ci.failed is not evaluated for it, and a paused or
// disabled agent is not kicked.
func TestCIFailedTriggerKicksThroughCELPath(t *testing.T) {
	cfg := &config.Config{
		Triggers: []config.TriggerRule{
			{Name: "main-red", Expr: `event.kind == "ci.failed"`, Agent: "ci-maintainer"},
			{Name: "repo-wide", Expr: `event.repo == "widget"`, Agent: "scanner"},
			{Name: "paused", Expr: `event.kind == "ci.failed" && event.title == "build"`, Agent: "paused-agent"},
			{Name: "disabled", Expr: `event.kind == "ci.failed"`, Agent: "off"},
		},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"ci-maintainer": enabledAgent(),
				"scanner":       enabledAgent(),
				"paused-agent":  enabledAgent(),
				"off":           {Enabled: false},
			},
		},
	}
	resetCELCache()
	defer resetCELCache()
	engine := celEngineFor(cfg, celTestLogger()).ForKind(celtrigger.KindCIFailed)
	if engine.Len() != 3 {
		t.Fatalf("ForKind(ci.failed) kept %d rules, want 3", engine.Len())
	}
	kicker := &ciFailedTestKicker{}
	gov := newTestGovernor(cfg.Agents)
	isPaused := func(agent string) bool { return agent == "paused-agent" }
	run := ciFailedTestRun()
	kicked := celTriggerKickAgents(context.Background(), engine, ciFailedEvent(run), cfg, gov, isPaused, kicker, ciFailedKickReason(run), celTestLogger())
	if len(kicked) != 1 || kicked[0] != "ci-maintainer" {
		t.Fatalf("kicked %v, want [ci-maintainer]", kicked)
	}
	if msg := kicker.kicks["ci-maintainer"]; !strings.Contains(msg, "ci.failed") || !strings.Contains(msg, run.URL) {
		t.Fatalf("kick message = %q", msg)
	}
	if _, ok := kicker.kicks["scanner"]; ok {
		t.Fatal("a rule that never names ci.failed must not fire on a CI failure")
	}
}

// With no rule naming ci.failed the boot hook reports false, so the red-main
// sweep makes no ci.failed reads; with one it reports true.
func TestCIFailedTriggerWanted(t *testing.T) {
	resetCELCache()
	defer resetCELCache()
	b := &boot{logger: celTestLogger(), cfg: &config.Config{}}
	if b.ciFailedTriggerWanted() {
		t.Fatal("no triggers: want must be false")
	}
	b.cfg = &config.Config{Triggers: []config.TriggerRule{{Name: "issues", Expr: `event.kind == "issue.opened"`, Agent: "scanner"}}}
	if b.ciFailedTriggerWanted() {
		t.Fatal("no rule names ci.failed: want must be false")
	}
	b.cfg = &config.Config{Triggers: []config.TriggerRule{{Name: "main-red", Expr: `event.kind == "ci.failed"`, Agent: "ci-maintainer"}}}
	if !b.ciFailedTriggerWanted() {
		t.Fatal("a ci.failed rule: want must be true")
	}
	var nilBoot *boot
	if nilBoot.ciFailedTriggerWanted() {
		t.Fatal("nil boot: want must be false")
	}
	// No agent runtime yet: emit is a no-op, not a panic.
	b.emitCIFailedTrigger(context.Background(), ciFailedTestRun())
}
