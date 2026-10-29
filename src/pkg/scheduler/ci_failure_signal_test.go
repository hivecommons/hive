package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A webhook CI-failure signal reaches the agent's fix-before-new section
// before ci-failing.json knows the PR, carrying the check name and the log
// pointer; a signal for a PR the file already lists adds to that row; a
// signal whose check is red on the base branch too is dropped.
func TestFormatRedPRFixDataWithSignals_MergesWebhookSignals(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	signals := []github.CIFailureSignal{
		{Repo: "test-org/console", Number: 22815, HeadSHA: "abc", Check: "lint", Conclusion: "failure", DetailsURL: "https://ci/lint/1", Agent: "scanner", At: at},
		{Repo: "test-org/console", Number: 500, HeadSHA: "def", Check: "unit", Conclusion: "timed_out", DetailsURL: "https://ci/unit/2", Title: "fresh red PR", Agent: "scanner", At: at},
		{Repo: "test-org/console", Number: 600, HeadSHA: "fed", Check: "build-gate", Agent: "scanner", At: at},
	}
	data := []byte(`{"ci_failing":[
	  {"number":22815,"repo":"test-org/console","title":"[Refactor] split tests","agent":"scanner","failing_checks":["build-gate"],"excerpt":"old evidence"},
	  {"number":600,"repo":"test-org/console","title":"base is red","agent":"scanner","failing_checks":["build-gate"],"base_red":true,"base_red_checks":["build-gate"]}
	]}`)
	out := formatRedPRFixDataWithSignals(data, "scanner", signals)
	for _, want := range []string{
		"failing CI or merge conflicts (2)",
		"#22815 test-org/console",
		"failing: build-gate, lint",
		"old evidence",
		"webhook: lint failed — log: https://ci/lint/1",
		"#500 test-org/console — fresh red PR",
		"failing: unit",
		"webhook: unit failed (timed_out) — log: https://ci/unit/2",
		"do NOT watch, poll, or sleep on CI",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("section missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#600 test-org/console — base is red") {
		t.Errorf("base-red check must stay excluded even when the webhook re-reports it:\n%s", out)
	}
	if got := formatRedPRFixDataWithSignals(nil, "scanner", signals[1:2]); !strings.Contains(got, "#500 ") {
		t.Errorf("signals alone (no ci-failing.json yet) must still render:\n%s", got)
	}
	if got := formatRedPRFixDataWithSignals(nil, "quality", signals); got != "" {
		t.Errorf("another agent's signals must not render, got:\n%s", got)
	}
}

// CIFailureKickMessage is the targeted kick: a one-line trigger header that
// restates the no-polling rule, then the same fix-before-new section a
// regular kick carries, sourced from the signal store when the file is
// absent. Advisory agents get nothing.
func TestCIFailureKickMessage(t *testing.T) {
	t.Cleanup(github.ResetCIFailureSignalsForTest(nil))
	orig := ciFailingPath
	ciFailingPath = filepath.Join(t.TempDir(), "missing.json")
	t.Cleanup(func() { ciFailingPath = orig })

	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Mode: "ISSUES_AND_PRS"},
				"advisor": {Mode: "ADVISORY"},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := New(cfg, logger)

	sig := github.CIFailureSignal{Repo: "test-org/console", Number: 77, HeadSHA: "abc", Check: "build-gate", DetailsURL: "https://ci/77", Title: "scanner PR", Agent: "scanner"}
	if got := s.CIFailureKickMessage("scanner", sig); got != "" {
		t.Fatalf("no recorded signal and no file: expected empty message, got:\n%s", got)
	}
	github.CIFailureSignals().Record(sig)
	got := s.CIFailureKickMessage("scanner", sig)
	for _, want := range []string{
		"CI failed on test-org/console#77 (build-gate)",
		"do not watch or poll CI yourself",
		"FIX-BEFORE-NEW",
		"#77 test-org/console — scanner PR",
		"log: https://ci/77",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kick missing %q:\n%s", want, got)
		}
	}
	if got := s.CIFailureKickMessage("advisor", sig); got != "" {
		t.Errorf("advisory agent must not get a CI kick, got:\n%s", got)
	}
	// The regular kick path carries the same signal until ci-failing.json
	// catches up, so nothing is lost between the webhook and the sweep.
	if msg := s.addRedPRFixFirst("scanner", "[agent:scanner]\nWORK\n"); !strings.Contains(msg, "#77 test-org/console") {
		t.Errorf("regular kick must fold the webhook signal in:\n%s", msg)
	}
}

// Agents never watch or poll CI themselves: detection is out-of-band
// (webhook, or the sweep as fallback). The reviewer lane's prompt is the
// contract that says so, and must keep saying so.
func TestReviewerLanePromptKeepsDoNotWaitForCI(t *testing.T) {
	s := reviewerTestScheduler(t, reviewerCloseACMMLevel, reviewerFixture)
	workList := s.buildReviewerWorkList()
	if workList == "" {
		t.Fatal("fixture produced no work list")
	}
	for _, msg := range []string{
		s.buildReviewerMessageHardcoded("adjudicator", reviewerCloseACMMLevel, workList),
		s.renderReviewerLaneTemplate("adjudicator", &github.ActionableResult{}, reviewerCloseACMMLevel, workList),
	} {
		for _, want := range []string{"Do Not Wait for CI", "Never watch, poll, or sleep on\nCI"} {
			if !strings.Contains(msg, want) {
				t.Errorf("reviewer prompt lost %q:\n%s", want, msg)
			}
		}
	}
}
