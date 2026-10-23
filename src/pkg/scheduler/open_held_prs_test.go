package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func openHeldCapScheduler() *Scheduler {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "my-org", Repos: []string{"console", "docs"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Mode: "ISSUES_AND_PRS"},
				"advisor": {Mode: "ADVISORY"},
			},
		},
	}
	return New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// A PR-opening agent is told, with the exact capacity line, which of its
// repos are at their max_open_held_prs cap (hivecommons/hive#13645); repos
// below the cap, advisory agents and an unconfigured hive see nothing.
func TestAddOpenHeldPRCapNotice(t *testing.T) {
	s := openHeldCapScheduler()
	a := &github.ActionableResult{OpenHeldPRs: map[string]github.OpenHeldPRs{
		"my-org/console": {Count: 12, Max: 12},
		"my-org/docs":    {Count: 3, Max: 12},
	}}

	got := s.addOpenHeldPRCapNotice("scanner", a, "BASE")
	for _, want := range []string{"BASE", "PR CAPACITY", "my-org/console: 12 open held hive PRs (max_open_held_prs 12)", "\n    " + github.RepoAtCapacityLine, "issue filing is not affected"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "my-org/docs") {
		t.Errorf("a repo below its cap must not be named:\n%s", got)
	}

	if got := s.addOpenHeldPRCapNotice("advisor", a, "BASE"); got != "BASE" {
		t.Errorf("an agent that cannot open PRs got the notice: %q", got)
	}
	if got := s.addOpenHeldPRCapNotice("scanner", &github.ActionableResult{}, "BASE"); got != "BASE" {
		t.Errorf("no cap configured changed the kick: %q", got)
	}
	if got := s.addOpenHeldPRCapNotice("scanner", nil, "BASE"); got != "BASE" {
		t.Errorf("nil snapshot changed the kick: %q", got)
	}
	below := &github.ActionableResult{OpenHeldPRs: map[string]github.OpenHeldPRs{"my-org/console": {Count: 11, Max: 12}}}
	if got := s.addOpenHeldPRCapNotice("scanner", below, "BASE"); got != "BASE" {
		t.Errorf("a repo below its cap changed the kick: %q", got)
	}
}
