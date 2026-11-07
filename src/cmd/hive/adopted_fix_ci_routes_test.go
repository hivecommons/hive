package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An adopted red PR whose head does not move is the same red episode on every
// eligibility pass. projectbluefin/hplip-printer-app#199 and
// ghostscript-printer-app#190 (FSDK builds red on an fsdk-containers bug,
// heads unchanged for a day) were "routed to fixer" 61 times each, and stayed
// in the scanner's FIX-BEFORE-NEW list after the hive had parked them at 085
// saying it had nothing to push.

// useMemoryFixCIRoutes gives the test its own route ledger.
func useMemoryFixCIRoutes(t *testing.T) {
	t.Helper()
	prev := adoptedFixCIRoutes
	adoptedFixCIRoutes = &adoptedFixCIRouteLedger{}
	t.Cleanup(func() { adoptedFixCIRoutes = prev })
}

// sweepFixCILogged runs one writeMergeEligible pass and returns the
// ci-failing rows and the log text.
func sweepFixCILogged(t *testing.T, prs []github.PullRequest) ([]failingEntry, string) {
	t.Helper()
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	defer func() { mergeEligiblePath, ciFailingPath = origMerge, origFail }()
	var logs bytes.Buffer
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: prs}}
	writeMergeEligible(actionable, github.HoldResult{}, "projectbluefin", nil, false, nil, false, nil, nil, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	var payload struct {
		CIFailing []failingEntry `json:"ci_failing"`
	}
	readJSON(t, ciFailingPath, &payload)
	return payload.CIFailing, logs.String()
}

func adoptedRedPrinterPR(head string, checks ...string) github.PullRequest {
	return github.PullRequest{Number: 199, Repo: "hplip-printer-app", Author: "Karibhou", HeadSHA: head,
		HeadRef: "slim-printing", HeadRepo: "Karibhou/hplip-printer-app", FromFork: true, MaintainerCanModify: true,
		Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "failure",
		FailingChecks: checks,
		Labels:        []string{"sentinel-alert", "stage/074-adopted-by-hive", "author/clanker"}}
}

const fixCIRouted = "adopted PR routed to fixer"

func TestWriteMergeEligible_AdoptedFixCIRoutedOncePerHeadAndCheckSet(t *testing.T) {
	useMemoryFixCIRoutes(t)
	const repo = "projectbluefin/hplip-printer-app"
	adoptInPlace(t, repo, 199, "2b9a441")
	pr := adoptedRedPrinterPR("2b9a441", "FSDK image and socket-sink (x86_64)", "FSDK image and socket-sink (aarch64)")

	failing, logs := sweepFixCILogged(t, []github.PullRequest{pr})
	if len(failing) != 1 || strings.Count(logs, fixCIRouted) != 1 {
		t.Fatalf("first sweep: rows=%+v routed=%d, want the red PR listed and routed once", failing, strings.Count(logs, fixCIRouted))
	}
	// Same head, same failing checks (in another order): still listed for
	// the fixer's kick, but not routed again.
	pr.FailingChecks = []string{"FSDK image and socket-sink (aarch64)", "FSDK image and socket-sink (x86_64)"}
	failing, logs = sweepFixCILogged(t, []github.PullRequest{pr})
	if len(failing) != 1 || strings.Count(logs, fixCIRouted) != 0 {
		t.Fatalf("second sweep: rows=%+v routed=%d, want listed but not re-routed", failing, strings.Count(logs, fixCIRouted))
	}
	// A newly failing check on the same head is a new episode.
	pr.FailingChecks = append(pr.FailingChecks, "lint")
	if _, logs = sweepFixCILogged(t, []github.PullRequest{pr}); strings.Count(logs, fixCIRouted) != 1 {
		t.Fatalf("new failing check: routed=%d, want once", strings.Count(logs, fixCIRouted))
	}
	// A new head routes once, then goes quiet again.
	adoptInPlace(t, repo, 199, "3c0b552")
	pr.HeadSHA = "3c0b552"
	if _, logs = sweepFixCILogged(t, []github.PullRequest{pr}); strings.Count(logs, fixCIRouted) != 1 {
		t.Fatalf("new head: routed=%d, want once", strings.Count(logs, fixCIRouted))
	}
	if _, logs = sweepFixCILogged(t, []github.PullRequest{pr}); strings.Count(logs, fixCIRouted) != 0 {
		t.Fatalf("new head re-sweep: routed=%d, want none", strings.Count(logs, fixCIRouted))
	}
}

func TestWriteMergeEligible_AdoptedPRParkedAt085IsNotFixCIWork(t *testing.T) {
	useMemoryFixCIRoutes(t)
	const repo = "projectbluefin/hplip-printer-app"
	const reason = "adoption-stalled:check-failing:FSDK image and socket-sink (aarch64)"
	adoptInPlace(t, repo, 199, "2b9a441")
	key := stagelabels.Key(repo, 199)
	stagelabels.Shared().MarkAdoptionEscalated(key, "2b9a441", reason)
	pr := adoptedRedPrinterPR("2b9a441", "FSDK image and socket-sink (x86_64)", "FSDK image and socket-sink (aarch64)")

	for sweep := 1; sweep <= 2; sweep++ {
		failing, logs := sweepFixCILogged(t, []github.PullRequest{pr})
		if len(failing) != 0 || strings.Contains(logs, fixCIRouted) {
			t.Fatalf("sweep %d: rows=%+v routed=%v, want a PR parked at 085 off the fixer list", sweep, failing, strings.Contains(logs, fixCIRouted))
		}
		if got, want := strings.Count(logs, "fix-ci not routed"), map[int]int{1: 1, 2: 0}[sweep]; got != want {
			t.Fatalf("sweep %d: park lines=%d, want %d", sweep, got, want)
		}
	}
	// The escalation is released (blocker cleared, a recovery attempt, an
	// operator's intent): the fixer gets the head again.
	stagelabels.Shared().ClearAdoptionEscalation(key)
	if failing, logs := sweepFixCILogged(t, []github.PullRequest{pr}); len(failing) != 1 || strings.Count(logs, fixCIRouted) != 1 {
		t.Fatalf("released: rows=%+v routed=%d, want listed and routed once", failing, strings.Count(logs, fixCIRouted))
	}
	// A new head is not parked by the old head's escalation.
	stagelabels.Shared().MarkAdoptionEscalated(key, "2b9a441", reason)
	adoptInPlace(t, repo, 199, "3c0b552")
	pr.HeadSHA = "3c0b552"
	if failing, logs := sweepFixCILogged(t, []github.PullRequest{pr}); len(failing) != 1 || strings.Count(logs, fixCIRouted) != 1 {
		t.Fatalf("new head: rows=%+v routed=%d, want listed and routed once", failing, strings.Count(logs, fixCIRouted))
	}
}
