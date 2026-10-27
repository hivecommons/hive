package dashboard

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hive:unauthorized — the hive's GitHub App lacks the permission for the
// item's next step (hivecommons/hive#14589) — is a tracker modifier like
// hold: it rides the label, outranks the needs-* chips and counts under
// Needs attention, since only a person can clear it.
func TestStageTrackerModifiers_Unauthorized(t *testing.T) {
	got := stageTrackerModifiers([]string{stagelabels.UnauthorizedLabel, "needs-human", "bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false)
	if len(got) != 2 || got[0] != modUnauthorized || got[1] != modNeedsHuman {
		t.Fatalf("modifiers = %v, want [%s %s]", got, modUnauthorized, modNeedsHuman)
	}
	if got := stageTrackerModifiers([]string{"hold", stagelabels.UnauthorizedLabel}, "hv", stagelabels.Record{}, false, nil, time.Now(), false); len(got) != 2 || got[0] != modHold || got[1] != modUnauthorized {
		t.Fatalf("hold (a human instruction) must lead: %v", got)
	}
}

func TestStageTrackerUnauthorizedFromReason(t *testing.T) {
	approve := prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns
	for _, tc := range []struct {
		name, reason, wantStep, wantTip string
	}{
		{name: "approve workflow runs", reason: approve, wantStep: "approve workflow runs", wantTip: "`actions: write`"},
		{name: "human-approved suffix is stripped", reason: approve + ":human-approved@pat", wantStep: "approve workflow runs", wantTip: "Approve and run"},
		{name: "another step", reason: prstage.StageReasonUnauthorizedPrefix + "update-branch", wantStep: "update branch", wantTip: "permission to update branch"},
		{name: "label without a reason", reason: "contributor-lane:soaking(1h)", wantTip: "lacks a permission for the next step"},
		{name: "empty", reason: "", wantTip: "lacks a permission for the next step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step, tip := stageTrackerUnauthorized(tc.reason)
			if step != tc.wantStep || !strings.Contains(tip, tc.wantTip) || !strings.Contains(tip, "hive:unauthorized") {
				t.Fatalf("stageTrackerUnauthorized(%q) = %q, %q; want step %q tip containing %q", tc.reason, step, tip, tc.wantStep, tc.wantTip)
			}
		})
	}
	if got := humanizeStageReason(approve); !strings.Contains(got, "not permitted to approve workflow runs") {
		t.Fatalf("humanizeStageReason = %q", got)
	}
}

// End to end through the payload: a PR parked on 085 by
// `unauthorized:approve-workflow-runs` and wearing hive:unauthorized carries
// the modifier, the step words for the gate cell, the tooltip, and a caption
// naming what the App may not do; a PR without the label carries none.
func TestStageTrackerUnauthorizedPayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	store.Observe(stagelabels.Key("acme/console", 130), stagelabels.NeedsDecision, prstage.StageReasonUnauthorizedPrefix+prstage.UnauthorizedStepApproveWorkflowRuns)
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 130, Title: "Audit deps", Author: "pat", Labels: []string{stagelabels.NeedsDecision.Label(), stagelabels.UnauthorizedLabel}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "pr", 130)
	if it.Stage != 85 || !hasMod(it, modUnauthorized) || it.UnauthorizedStep != "approve workflow runs" || !strings.Contains(it.Unauthorized, "`actions: write`") {
		t.Fatalf("row = stage %d mods %v step %q tip %q", it.Stage, it.Modifiers, it.UnauthorizedStep, it.Unauthorized)
	}
	if !strings.Contains(it.Caption, "not permitted to approve workflow runs") || it.Actor != "maintainer" {
		t.Fatalf("caption %q actor %q", it.Caption, it.Actor)
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"unauthorized_step":"approve workflow runs"`) || !strings.Contains(string(raw), `"unauthorized":"The hive's GitHub App lacks`) {
		t.Fatalf("payload lacks the unauthorized fields: %s", raw)
	}
	if clean := trackerItem(t, resp, "pr", 101); clean.UnauthorizedStep != "" || clean.Unauthorized != "" || hasMod(clean, modUnauthorized) {
		t.Fatalf("PR 101 must not carry unauthorized fields: %+v", clean)
	}
}

// The frontend: the pill is muted and explains itself on hover, it counts
// under Needs attention, and the 085 chevron reads "Awaiting human: <step>".
func TestStageTrackerUnauthorizedJS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".ham-badge-mod.ham-mod-unauthorized { color: var(--text-muted);",
		"'hold', 'unauthorized', 'needs-human'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html lacks %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — unauthorized pill was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		"var _stageTracker = { stages: [{ number: 85, short: 'Needs decision' }] };",
		"function agentNowClockTime(ms, nowMs) { return 'T' + ms; }",
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsFunc(t, html, "stageTrackerStageWord"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		stageTrackerManualMergeJS(t, html),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
	}, "\n") + `
const tip = "The hive's GitHub App lacks \u0060actions: write\u0060, so it cannot approve the workflow runs.";
const item = { kind: 'pr', stage: 85, modifiers: ['unauthorized'], unauthorized_step: 'approve workflow runs', unauthorized: tip };
const pill = stageTrackerChipsHtml(item);
assert.ok(pill.includes('ham-mod-unauthorized') && pill.includes('>unauthorized<'), pill);
assert.ok(pill.includes('title="' + esc(tip) + '"'), pill);
assert.ok(STAGE_TRACKER_HUMAN_MODS.includes('unauthorized') && STAGE_TRACKER_MOTION_HUMAN_MODS.includes('unauthorized'));
assert.ok(stageTrackerNeedsAttention(item));
// unauthorized outranks the needs-* chips but yields to hold, a human instruction.
assert.ok(stageTrackerChipsHtml({ stage: 85, modifiers: ['needs-decision', 'unauthorized'] }).includes('>unauthorized +1<'));
assert.ok(stageTrackerChipsHtml({ stage: 10, modifiers: ['unauthorized', 'hold'] }).includes('>hold +1<'));
// The 085 chevron names the human step owed; without a step the generic word stays.
assert.strictEqual(stageTrackerUnauthorizedWord(item), 'Awaiting human: approve workflow runs');
assert.strictEqual(stageTrackerUnauthorizedWord({ stage: 85, modifiers: ['unauthorized'] }), '');
assert.strictEqual(STAGE_TRACKER_HUMAN_GATED, 85);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
