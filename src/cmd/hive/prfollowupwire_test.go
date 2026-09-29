package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prfollowup"
)

type fakePRFollowUpSessions map[string]string

func (f fakePRFollowUpSessions) SessionID(name string) (string, bool) {
	s, ok := f[name]
	return s, ok
}

func prFollowUpTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(prfollowup.DirEnvVar, dir)
	return dir
}

func redFollowUpActionable() *github.ActionableResult {
	return &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{{
		Repo: "hivecommons/hive", Number: 9, CIStatus: "failure", HeadSHA: "abc1234def",
	}}}}
}

func TestPRFollowUpWiring_DefaultOffIsInert(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "")
	dir := prFollowUpTestDir(t)
	cfg := &config.Config{}

	recordPRFollowUpPointer(cfg, fakePRFollowUpSessions{"scanner": "s1"}, "scanner", "hivecommons/hive", 9, "", time.Now(), discardLogger())
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("default-off hook wrote %d pointer file(s)", len(entries))
	}
	if out := routePRFollowUps(context.Background(), cfg, redFollowUpActionable(), nil, nil, discardLogger()); out != nil {
		t.Fatalf("default-off router produced outcomes: %+v", out)
	}
}

func TestPRFollowUpWiring_EnabledRecordsAndRoutes(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	prFollowUpTestDir(t)
	threadsPath := redirectReviewThreadsPath(t)
	cfg := &config.Config{}

	if err := github.WriteReviewThreadsReport(threadsPath, github.ReviewThreadsReport{Enabled: true, PRs: []github.ReviewThreadPR{{
		Repo: "hivecommons/hive", Number: 9, Threads: []github.ReviewThread{{ThreadID: "PRRT_x", Path: "a.go", Author: "bot", Body: "fix"}},
	}}}); err != nil {
		t.Fatal(err)
	}

	recordPRFollowUpPointer(cfg, fakePRFollowUpSessions{"scanner": "s1"}, "scanner", "hivecommons/hive", 9, "https://example.invalid/pr/9", time.Now(), discardLogger())

	// No agent manager: the pointer makes the PR eligible, both the red CI
	// and the review-bot thread are detected, and it falls back cleanly.
	out := routePRFollowUps(context.Background(), cfg, redFollowUpActionable(), nil, nil, discardLogger())
	if len(out) != 1 || out[0].Route != prfollowup.RouteFallback || out[0].Reason != prfollowup.ReasonNoResumer || len(out[0].Events) != 2 {
		t.Fatalf("outcomes = %+v, want one fallback with CI + thread events", out)
	}

	// An escalated PR is a human's now: never routed.
	escalated := map[string]bool{escalation.Key("hivecommons/hive", 9): true}
	next := redFollowUpActionable()
	next.PRs.Items[0].HeadSHA = "new0000sha"
	if out := routePRFollowUps(context.Background(), cfg, next, escalated, nil, discardLogger()); len(out) != 0 {
		t.Fatalf("escalated PR routed: %+v", out)
	}
	if out := routePRFollowUps(context.Background(), cfg, nil, nil, nil, discardLogger()); out != nil {
		t.Fatalf("nil actionable routed: %+v", out)
	}
}

func TestPRFollowUpResumer_AdaptsManager(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{}, discardLogger(), agent.ProjectContext{})
	r := prFollowUpResumer{mgr: mgr}
	if _, ok := r.SessionID("ghost"); ok {
		t.Fatal("unknown agent must have no session")
	}
	err := r.SendResumeKick("ghost", "msg", "s1")
	if !errors.Is(err, agent.ErrResumeSessionGone) || errors.Is(err, prfollowup.ErrBusy) {
		t.Fatalf("err = %v, want session-gone (a fallback), not busy", err)
	}
}
