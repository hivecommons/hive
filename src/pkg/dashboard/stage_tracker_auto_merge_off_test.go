package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// projectbluefin/server#626 (opened 2026-11-05) and its 48 siblings on the
// projectbluefin hive's common, server, actions, website and documentation
// repos — auto-merge switched off there — read "New · waiting for the
// stage-label pass (→ 050 review pending) (next pass due now)" for days.
// Every merge lane skips a repo whose auto-merge is off before it records
// any stage evidence, so that pass never comes. The 000-new narration on
// such a repo must drop the countdown and the stage-label pass, name the
// person's merge as the last gate, and carry the merge verdict.
func autoMergeOffTrackerFixture(t *testing.T, now time.Time, level int, autoMerge *bool) (*Server, StageTrackerResponse) {
	t.Helper()
	s, store := stageTrackerFixture(t, now)
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Project.Org = "acme"
	if autoMerge != nil {
		cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "acme/console", AutoMerge: autoMerge}}
	}
	s.deps = &Dependencies{Config: cfg}
	opened := now.Add(-4 * 24 * time.Hour)
	s.statusMu.Lock()
	// The governor's kick already passed: the old caption read "next pass due now".
	s.status.Governor.NextKickAt = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		// Mergeable on GitHub; only the repo switch holds it (the merge
		// verdict #16815 writes for an auto-merge-off repo).
		FrontendPR{PullRequest: github.PullRequest{Number: 626, Title: "fix: stop false build safety failures", Author: "karibhou", HeadSHA: "eaf066b", BaseRef: "main", CreatedAt: opened, UpdatedAt: opened},
			MergeVerdict: &github.MergeVerdict{State: github.MergeVerdictOutstanding, HandMerge: true, Reason: "auto-merge is off for this repo (its auto-merge setting, or the hive is below ACMM L6) — a person merges it"}},
		// Red CI: GitHub refuses the merge; the author has work to do first.
		FrontendPR{PullRequest: github.PullRequest{Number: 645, Title: "fix: list both pcre2 copies", Author: "kylerankin", HeadSHA: "c0ffee1", BaseRef: "main", CreatedAt: opened, UpdatedAt: opened},
			MergeVerdict: &github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: "blocked — CI failing: zizmor"}},
	)
	s.statusMu.Unlock()
	return s, s.buildStageTracker(s.status, store, now)
}

func TestStageTrackerAutoMergeOffNewPRNamesThePersonsMerge(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	off := false
	_, resp := autoMergeOffTrackerFixture(t, now, 6, &off)

	ready := trackerItem(t, resp, "pr", 626)
	if ready.New == nil || ready.StageSlug != StageTrackerNewSlug || !ready.New.AutoMergeOff {
		t.Fatalf("#626 = slug %q new %+v, want 000-new on an auto-merge-off repo", ready.StageSlug, ready.New)
	}
	for _, it := range []StageTrackerItem{ready, trackerItem(t, resp, "pr", 645)} {
		if strings.Contains(it.Caption, "stage-label pass") || strings.Contains(it.Caption, "next pass") || strings.Contains(it.New.NextMoverText, "stage-label pass") {
			t.Errorf("#%d caption %q / next %q still promises a stage pass that never runs on an auto-merge-off repo", it.Number, it.Caption, it.New.NextMoverText)
		}
		if it.New.NextPassAt != nil || it.New.NextPassInSeconds != 0 {
			t.Errorf("#%d carries the governor's countdown %v / %ds; the SPA would print \"next hive pass due now\"", it.Number, it.New.NextPassAt, it.New.NextPassInSeconds)
		}
		var gate *StageTrackerGate
		for i := range it.New.Gates {
			if it.New.Gates[i].Name == StageTrackerGateHumanMerge {
				gate = &it.New.Gates[i]
			}
		}
		if gate == nil || gate.Text != stageTrackerAutoMergeOffGateText {
			t.Errorf("#%d gates = %+v, want the human-merge gate %q", it.Number, it.New.Gates, stageTrackerAutoMergeOffGateText)
		}
	}

	// Mergeable: a person's merge is the next move, and the row offers it.
	if ready.New.NextMover != StageTrackerMoverMaintainer || ready.Actor != StageTrackerMoverMaintainer || ready.Caption != "New · auto-merge off — a person merges it" {
		t.Fatalf("#626 mover %q actor %q caption %q, want the maintainer's merge", ready.New.NextMover, ready.Actor, ready.Caption)
	}
	if len(ready.New.Gates) == 0 || ready.New.Gates[0].Name != "merge-verdict" || !strings.Contains(ready.New.Gates[0].Text, "a person merges it") {
		t.Fatalf("#626 gates = %+v, want the merge verdict first", ready.New.Gates)
	}
	d := ready.DecisionNeeded
	if d == nil || len(d.Options) < 2 {
		t.Fatalf("#626 decision = %+v, want a merge decision", d)
	}
	if lead := d.Options[0]; lead.Action != decisionActionMergeNow || !lead.Enabled {
		t.Fatalf("#626 lead option = %+v, want an enabled merge-now", lead)
	}
	if q := decisionOptionByAction(t, d, decisionActionQueueMerge); q.Enabled || !strings.Contains(strings.ToLower(q.ReasonIfDisabled), "auto-merge is off") {
		t.Fatalf("#626 queue option = %+v, want it greyed: no merge pass runs here", q)
	}

	// Red CI: still CI / review, but no promise of a stage pass.
	red := trackerItem(t, resp, "pr", 645)
	if red.New.NextMover != StageTrackerMoverHive || red.DecisionNeeded != nil {
		t.Fatalf("#645 mover %q decision %+v, want CI / review and no merge ask", red.New.NextMover, red.DecisionNeeded)
	}
	if len(red.New.Gates) == 0 || !strings.Contains(red.New.Gates[0].Text, "CI failing: zizmor") {
		t.Fatalf("#645 gates = %+v, want the merge verdict on the review gate", red.New.Gates)
	}

	// The maintainer row has a real lead action in the SPA (the rule
	// TestStageTrackerMaintainerRowsHaveLeadAction pins for the fixture).
	raw, err := json.Marshal([]StageTrackerItem{ready})
	if err != nil {
		t.Fatal(err)
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, indexHTML(t))+`
const rows = `+string(raw)+`;
const ctx = { user: true, canHold: true, canHuman: true, login: 'octo', nowMs: `+itoa(int(now.UnixMilli()))+`, governor: null };
const lead = stageTrackerLeadAction(rows[0], ctx);
assert.ok(lead && lead.enabled && lead.id === 'decide:merge-now', JSON.stringify(lead));
`)
}

// Below ACMM L6 every repo reads auto-merge off (config.RepoAutoMergeEnabled)
// and no lane runs anywhere, so the same narration applies; at L6 with the
// switch on, the stage-label pass is still the honest next mover.
func TestStageTrackerAutoMergeOffNewFollowsTheEffectiveSwitch(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, below := autoMergeOffTrackerFixture(t, now, 5, nil)
	if it := trackerItem(t, below, "pr", 626); it.New == nil || !it.New.AutoMergeOff || strings.Contains(it.Caption, "stage-label pass") {
		t.Fatalf("below L6: new %+v caption %q, want the auto-merge-off narration", it.New, it.Caption)
	}
	_, on := autoMergeOffTrackerFixture(t, now, 6, nil)
	it := trackerItem(t, on, "pr", 626)
	if it.New == nil || it.New.AutoMergeOff || it.New.NextMover != StageTrackerMoverHive || !strings.Contains(it.Caption, "stage-label pass") || it.DecisionNeeded != nil {
		t.Fatalf("L6, auto-merge on: new %+v caption %q decision %+v, want the stage-label pass unchanged", it.New, it.Caption, it.DecisionNeeded)
	}
}
