package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prfollowup"
)

// seedLiveHandoff records a pointer with a handoff note for scanner's PR and
// routes one red-CI follow-up with no resumer, so the PR is live and its
// follow-up fell back to the fresh-dispatch path.
func seedLiveHandoff(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := prFollowUpDir
	prFollowUpDir = func() string { return dir }
	t.Cleanup(func() { prFollowUpDir = orig })
	now := time.Now()
	if err := prfollowup.RecordWithNote(context.Background(), dir, "scanner", "test-org/console", 77, "https://example.invalid/pull/77", "s1", "Why: the original reasoning", now); err != nil {
		t.Fatal(err)
	}
	red := github.PullRequest{Repo: "test-org/console", Number: 77, CIStatus: "failure", HeadSHA: "abcdef0123"}
	out := prfollowup.Route(context.Background(), []github.PullRequest{red}, nil, prfollowup.Options{Dir: dir}, now)
	if len(out) != 1 || out[0].Route != prfollowup.RouteFallback {
		t.Fatalf("seed route = %+v, want a fallback", out)
	}
	return dir
}

func handoffTestScheduler() *Scheduler {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {Mode: "ISSUES_AND_PRS"},
			"advisor": {Mode: "ADVISORY"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// With the toggle on, the fresh kick of the agent that opened the PR carries
// the handoff note right below the header; nobody else's does.
func TestAddPRFollowUpHandoff_InjectsNoteWhenEnabled(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	seedLiveHandoff(t)
	s := handoffTestScheduler()

	msg := s.addPRFollowUpHandoff("scanner", "[agent:scanner]\nWORK LIST\n")
	if !strings.HasPrefix(msg, "[agent:scanner]\n\n## 🧭 PR HANDOFF") {
		t.Fatalf("section must land directly below the kick header:\n%s", msg)
	}
	if !strings.Contains(msg, "Why: the original reasoning") || !strings.Contains(msg, "test-org/console#77") || !strings.HasSuffix(msg, "WORK LIST\n") {
		t.Fatalf("kick = %s", msg)
	}
	if got := s.addPRFollowUpHandoff("advisor", "[agent:advisor]\nADVISE\n"); strings.Contains(got, "PR HANDOFF") {
		t.Error("an advisory agent cannot act on a PR handoff")
	}
	if got := s.addPRFollowUpHandoff("scanner", ""); got != "" {
		t.Error("an empty (fail-closed) message must stay empty")
	}
	if got := s.addPRFollowUpHandoff("scanner", "no header\n"); !strings.HasPrefix(got, "\n## 🧭 PR HANDOFF") {
		t.Errorf("a headerless message gets the section prepended:\n%s", got)
	}
}

// Toggle off: kicks are byte-for-byte what they were before the feature,
// even with a live handoff on disk.
func TestAddPRFollowUpHandoff_NoChangeWhenDisabled(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "")
	seedLiveHandoff(t)
	s := handoffTestScheduler()
	const kick = "[agent:scanner]\nWORK LIST\n"
	if got := s.addPRFollowUpHandoff("scanner", kick); got != kick {
		t.Fatalf("toggle off changed the kick:\n%s", got)
	}
	var nilCfg Scheduler
	if got := nilCfg.addPRFollowUpHandoff("scanner", kick); got != kick {
		t.Fatal("a scheduler without config must not change the kick")
	}
}

// An agent with no live PR follow-ups gets no section.
func TestAddPRFollowUpHandoff_NothingToHandOff(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := t.TempDir()
	orig := prFollowUpDir
	prFollowUpDir = func() string { return dir }
	t.Cleanup(func() { prFollowUpDir = orig })
	s := handoffTestScheduler()
	const kick = "[agent:scanner]\nWORK LIST\n"
	if got := s.addPRFollowUpHandoff("scanner", kick); got != kick {
		t.Fatalf("empty store changed the kick:\n%s", got)
	}
}
