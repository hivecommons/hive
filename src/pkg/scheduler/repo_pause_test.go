package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func pauseSchedulerConfig(t *testing.T, paused ...string) *Scheduler {
	t.Helper()
	entries := make([]config.RepoPause, 0, len(paused))
	for _, repo := range paused {
		entries = append(entries, config.RepoPause{Repo: repo, By: "bketelsen", Reason: "release freeze"})
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "my-org",
			Repos:       []string{"console", "dashboard", "docs"},
			PrimaryRepo: "console",
			PausedRepos: entries,
		},
	}
	return New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// A paused repo drops out of AUTHORIZED REPOS but is NAMED as paused. Silently
// omitting it would read to an agent that has filed issues there for weeks as
// scope loss — the kind of thing agents file findings about.
func TestBuildReposSection_PausedRepoIsExcludedAndExplained(t *testing.T) {
	s := pauseSchedulerConfig(t, "dashboard")
	section := s.buildReposSection()

	authorized, _, ok := strings.Cut(section, "PAUSED BY THE OPERATOR")
	if !ok {
		t.Fatalf("paused repos are not named in the kick:\n%s", section)
	}
	if strings.Contains(authorized, "my-org/dashboard") {
		t.Errorf("paused repo listed as authorized:\n%s", section)
	}
	for _, want := range []string{"my-org/console", "my-org/docs"} {
		if !strings.Contains(authorized, want) {
			t.Errorf("active repo %q missing from AUTHORIZED REPOS:\n%s", want, section)
		}
	}
	if !strings.Contains(section, "my-org/dashboard") {
		t.Errorf("paused repo should still be named (as paused):\n%s", section)
	}
	// The agent must be told not to route around it or file an issue about it.
	if !strings.Contains(section, "NOT an outage") {
		t.Errorf("paused note does not tell the agent this is deliberate:\n%s", section)
	}
}

// The multi-repo rotation instruction counts ACTIVE repos and must never name a
// paused repo as the primary to fall back on — "all of them are in scope, not
// just the primary" while the primary is frozen is a contradiction the agent
// will try to resolve by writing there.
func TestBuildReposSection_RotationSkipsPausedPrimary(t *testing.T) {
	s := pauseSchedulerConfig(t, "console") // console is also PrimaryRepo
	section := s.buildReposSection()

	if !strings.Contains(section, "this project has 2 authorized repos") {
		t.Errorf("rotation note should count only active repos:\n%s", section)
	}
	if strings.Contains(section, "not just the primary (my-org/console)") {
		t.Errorf("rotation note named the PAUSED primary as the fallback:\n%s", section)
	}
	if !strings.Contains(section, "not just the primary (my-org/dashboard)") {
		t.Errorf("rotation note should fall back to the first ACTIVE repo:\n%s", section)
	}
}

// Pausing down to a single active repo drops the multi-repo rotation block:
// there is nothing to rotate between.
func TestBuildReposSection_SingleActiveRepoDropsRotation(t *testing.T) {
	s := pauseSchedulerConfig(t, "dashboard", "docs")
	section := s.buildReposSection()
	if strings.Contains(section, "MULTI-REPO COVERAGE") {
		t.Errorf("rotation instruction emitted with one active repo:\n%s", section)
	}
}

// Every repo paused is a legitimate (if drastic) state and must read as one,
// not as an empty authorized list the agent has to interpret.
func TestBuildReposSection_AllPaused(t *testing.T) {
	s := pauseSchedulerConfig(t, "console", "dashboard", "docs")
	section := s.buildReposSection()
	if !strings.Contains(section, "every repo this hive watches is currently paused") {
		t.Errorf("all-paused state not stated:\n%s", section)
	}
}

// A hive with no pauses must produce byte-identical kick text to before.
func TestBuildReposSection_NoPausesUnchanged(t *testing.T) {
	s := pauseSchedulerConfig(t)
	section := s.buildReposSection()
	if strings.Contains(section, "PAUSED BY THE OPERATOR") {
		t.Errorf("pause note emitted on a hive with no pauses:\n%s", section)
	}
	for _, want := range []string{"my-org/console", "my-org/dashboard", "my-org/docs"} {
		if !strings.Contains(section, want) {
			t.Errorf("repo %q missing:\n%s", want, section)
		}
	}
	if !strings.Contains(section, "this project has 3 authorized repos") {
		t.Errorf("rotation note should count all three:\n%s", section)
	}
}
