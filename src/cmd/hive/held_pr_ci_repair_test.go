package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// The deadlock this fixes (hivecommons/hive#7438): the ACMM level gate holds
// an agent's PR for human review, the hold takes the PR out of the actionable
// population, and from there nothing ever notices its CI is red. It stays red,
// so it stays held, so no agent is ever told to repair it. A held red PR must
// land in ci_failing (flagged held) — and must still never be merge-eligible.
func TestWriteMergeEligible_HeldRedPRIsRepairableButNeverEligible(t *testing.T) {
	heldRed := github.PullRequest{
		Repo:             "widget",
		Number:           188,
		Title:            "fix: git diff gate",
		Author:           "acme-bot[bot]",
		CIStatus:         "failure",
		HeadSHA:          "deadbeef",
		HeadRef:          "hive/sec-check/188",
		HeadRepo:         "acme/widget",
		FailingChecks:    []string{"Python Unit Tests"},
		CIFailureExcerpt: "tests/test_git_diff_gate.py::test_gate FAILED",
		Mergeable:        github.MergeableYes,
	}

	eligible, failing := runWriteMergeEligible(t, nil, mergeEligibleInputs{
		heldPRs: []github.PullRequest{heldRed},
		org:     "acme",
	})

	if len(eligible) != 0 {
		t.Fatalf("eligible = %+v, want empty — the hold is a merge checkpoint and must hold", eligible)
	}
	if len(failing) != 1 {
		t.Fatalf("ci_failing = %+v, want the held red PR", failing)
	}
	got := failing[0]
	if got.Number != 188 || got.Repo != "acme/widget" {
		t.Errorf("ci_failing[0] = %d %s, want 188 acme/widget", got.Number, got.Repo)
	}
	if !got.Held {
		t.Error("ci_failing[0].held = false, want true — consumers need it to say 'fix CI, keep the hold'")
	}
	if got.HeadSHA != "deadbeef" || got.ReachableAction != "push" {
		t.Errorf("ci_failing[0] head_sha=%q reachable_action=%q, want deadbeef/push", got.HeadSHA, got.ReachableAction)
	}
	if len(got.FailingChecks) != 1 || got.FailingChecks[0] != "Python Unit Tests" {
		t.Errorf("ci_failing[0].failing_checks = %v, want the CI evidence carried through", got.FailingChecks)
	}
}

// A GREEN held PR gains nothing from this change: it is not red, so it is not
// repair work, and the hold keeps it out of the merge bucket. It must appear
// in neither bucket.
func TestWriteMergeEligible_HeldGreenPRIsInNeitherBucket(t *testing.T) {
	eligible, failing := runWriteMergeEligible(t, nil, mergeEligibleInputs{
		heldPRs: []github.PullRequest{{
			Repo:      "widget",
			Number:    200,
			Title:     "feat: held and green",
			CIStatus:  "success",
			HeadSHA:   "cafe",
			Mergeable: github.MergeableYes,
		}},
		org: "acme",
	})
	if len(eligible) != 0 {
		t.Errorf("eligible = %+v, want empty — a held PR may never merge", eligible)
	}
	if len(failing) != 0 {
		t.Errorf("ci_failing = %+v, want empty — a green PR is not repair work", failing)
	}
}

// The same rule applies when the hold arrives through the hold SNAPSHOT rather
// than through PRs.Held (a PR labelled after enumeration, or a cached
// actionable result): red still classifies as red, and merge eligibility is
// still refused.
func TestWriteMergeEligible_HoldSnapshotRedPRIsRepairableButNeverEligible(t *testing.T) {
	red := github.PullRequest{
		Repo:          "widget",
		Number:        7,
		Title:         "fix: something",
		CIStatus:      "failure",
		HeadSHA:       "abc123",
		FailingChecks: []string{"build-gate"},
	}
	green := github.PullRequest{
		Repo:      "widget",
		Number:    8,
		Title:     "feat: held green",
		CIStatus:  "success",
		HeadSHA:   "def456",
		Mergeable: github.MergeableYes,
	}

	eligible, failing := runWriteMergeEligible(t, []github.PullRequest{red, green}, mergeEligibleInputs{
		org: "acme",
		hold: github.HoldResult{Items: []github.HoldItem{
			{Repo: "widget", Number: 7, Type: "pr"},
			{Repo: "widget", Number: 8, Type: "pr"},
		}},
	})

	if len(eligible) != 0 {
		t.Fatalf("eligible = %+v, want empty — both PRs are held", eligible)
	}
	if len(failing) != 1 || failing[0].Number != 7 || !failing[0].Held {
		t.Fatalf("ci_failing = %+v, want only #7 with held=true", failing)
	}
}

// A PR present in BOTH populations is classified once. Without the dedupe the
// same red PR would be listed twice in every agent's fix block.
func TestWriteMergeEligible_HeldPRIsNotDoubleCounted(t *testing.T) {
	red := github.PullRequest{
		Repo:          "widget",
		Number:        42,
		Title:         "fix: dup",
		CIStatus:      "failure",
		HeadSHA:       "aaa",
		FailingChecks: []string{"build-gate"},
	}
	_, failing := runWriteMergeEligible(t, []github.PullRequest{red}, mergeEligibleInputs{
		heldPRs: []github.PullRequest{red},
		org:     "acme",
		hold:    github.HoldResult{Items: []github.HoldItem{{Repo: "widget", Number: 42, Type: "pr"}}},
	})
	if len(failing) != 1 {
		t.Fatalf("ci_failing = %+v, want exactly one row for #42", failing)
	}
	if !failing[0].Held {
		t.Error("ci_failing[0].held = false, want true")
	}
}

// An UNHELD red PR must keep held=false: the flag has to mean something, or
// every red PR would carry the "do not remove the hold" note.
func TestWriteMergeEligible_UnheldRedPRIsNotFlaggedHeld(t *testing.T) {
	_, failing := runWriteMergeEligible(t, []github.PullRequest{{
		Repo:          "widget",
		Number:        9,
		Title:         "fix: ordinary red",
		CIStatus:      "failure",
		HeadSHA:       "bbb",
		FailingChecks: []string{"build-gate"},
	}}, mergeEligibleInputs{org: "acme"})

	if len(failing) != 1 {
		t.Fatalf("ci_failing = %+v, want one row", failing)
	}
	if failing[0].Held {
		t.Error("ci_failing[0].held = true for a PR with no hold, want false")
	}
}
