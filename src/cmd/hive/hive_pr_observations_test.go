package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func observationsTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "hivecommons", AIAuthor: "hive-bee"},
	}
}

// hivePRObservations feeds the escalation ledger, so its author filter is a
// gate: a HUMAN-authored PR must never become an escalation observation — the
// fix-loop reaper re-dispatching agents onto a human's in-progress branch is
// exactly the interference the filter exists to prevent.
func TestHivePRObservationsExcludesHumanAuthors(t *testing.T) {
	cfg := observationsTestConfig()
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "hive", Number: 1, Author: "hive-bee", HeadSHA: "aaa"},
		{Repo: "hive", Number: 2, Author: "human-dev", HeadSHA: "bbb"},
		{Repo: "hive", Number: 3, Author: "dependabot[bot]", HeadSHA: "ccc"},
	}}}

	obs := hivePRObservations(cfg, actionable)

	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2 (agent + bot only)", len(obs))
	}
	for _, o := range obs {
		if o.Number == 2 {
			t.Errorf("human-authored PR #2 leaked into escalation observations: %+v", o)
		}
	}
}

// Observations must carry FULLY-QUALIFIED repos: the escalation store keys on
// them, and a bare "hive" beside a "hivecommons/hive" for the same PR would
// split its attempt count across two ledger entries and defeat the breaker.
func TestHivePRObservationsQualifiesRepos(t *testing.T) {
	cfg := observationsTestConfig()
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "hive", Number: 1, Author: "hive-bee"},
		{Repo: "otherorg/console", Number: 2, Author: "hive-bee"},
	}}}

	obs := hivePRObservations(cfg, actionable)

	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2", len(obs))
	}
	if obs[0].Repo != "hivecommons/hive" {
		t.Errorf("bare repo = %q, want qualified %q", obs[0].Repo, "hivecommons/hive")
	}
	if obs[1].Repo != "otherorg/console" {
		t.Errorf("already-qualified repo rewritten to %q, want untouched %q", obs[1].Repo, "otherorg/console")
	}
}

// Red must mean "a required check failed" — CIStatus "failure" WITHOUT a named
// failing check must not read as red (HasFailingRequiredCheck's
// belt-and-suspenders guard), and the CI failure excerpt must ride along as
// the fix agent's evidence.
func TestHivePRObservationsRedRequiresFailingCheck(t *testing.T) {
	cfg := observationsTestConfig()
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "hive", Number: 1, Author: "hive-bee", HeadSHA: "aaa",
			CIStatus: "failure", FailingChecks: []string{"build"}, CIFailureExcerpt: "compile error"},
		{Repo: "hive", Number: 2, Author: "hive-bee", HeadSHA: "bbb", CIStatus: "failure"},
		{Repo: "hive", Number: 3, Author: "hive-bee", HeadSHA: "ccc", CIStatus: "success"},
	}}}

	obs := hivePRObservations(cfg, actionable)

	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3", len(obs))
	}
	if !obs[0].Red || obs[0].Excerpt != "compile error" || obs[0].HeadSHA != "aaa" {
		t.Errorf("red PR with failing check misprojected: %+v", obs[0])
	}
	if obs[1].Red {
		t.Error("CIStatus failure without a named failing check must not read as red")
	}
	if obs[2].Red {
		t.Error("green PR read as red")
	}
}

// Pending must be threaded through so a CI-in-flight window is a no-op
// observation for the escalation ledger (#5617, gap G2) — neither red (no
// attempt counted) nor green (no history/staleness wiped).
func TestHivePRObservationsMarksPendingWindows(t *testing.T) {
	cfg := observationsTestConfig()
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "hive", Number: 1, Author: "hive-bee", HeadSHA: "aaa", CIStatus: "pending"},
		{Repo: "hive", Number: 2, Author: "hive-bee", HeadSHA: "bbb", CIStatus: "success"},
		{Repo: "hive", Number: 3, Author: "hive-bee", HeadSHA: "ccc",
			CIStatus: "failure", FailingChecks: []string{"build"}},
	}}}

	obs := hivePRObservations(cfg, actionable)

	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3", len(obs))
	}
	if !obs[0].Pending || obs[0].Red {
		t.Errorf("CI-pending PR must project as Pending, not Red: %+v", obs[0])
	}
	if obs[1].Pending || obs[2].Pending {
		t.Error("settled (green/red) PRs must not project as Pending")
	}
}

// A nil enumeration yields nil — the eval cycle calls this before the first
// successful GitHub pass.
func TestHivePRObservationsNilActionable(t *testing.T) {
	if obs := hivePRObservations(observationsTestConfig(), nil); obs != nil {
		t.Errorf("hivePRObservations(nil) = %v, want nil", obs)
	}
}
