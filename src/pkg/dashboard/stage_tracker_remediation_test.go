package dashboard

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// A hive-authored 070 PR whose head was pushed BEFORE the review asked for
// changes: the recorded push is the reviewed head, not a fix.
func remediationRedPR(now time.Time) *StageTrackerItem {
	pushed := now.Add(-3 * time.Hour)
	red := now.Add(-40 * time.Minute)
	stageSince := now.Add(-39 * time.Minute)
	it := &StageTrackerItem{Repo: "org/repo", Kind: "pr", Number: 7, Head: "abc1234def", Author: "hive[bot]", AuthorKind: authorKindHive,
		Stage: trackerNumberFixing, StageSince: &stageSince, Caption: "New head pushed · CI and re-review",
		CIActivity:     &StageTrackerActivity{Head: "abc1234def", State: "passing"},
		ReviewActivity: &StageTrackerActivity{Head: "abc1234def", State: "changes_requested", Since: &red},
		Push:           &StageTrackerPush{State: pushStatePushed, Login: "hive[bot]", Since: &pushed, Sha: "abc1234def"},
		Claimant:       &StageTrackerClaimant{Kind: claimantKindHiveAgent, Login: "hive[bot]", Source: claimantSourceAuthor, Since: &stageSince}}
	stageTrackerApplyCodeActivity(it, review.DispatchState{}, nil, now)
	return it
}

func TestStageTrackerRemediationUnassignedWithoutEvidence(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	it := remediationRedPR(now)
	if it.CodeActivity.State != "pushed" || it.DisplayColumn != trackerNumberReviewPending {
		t.Fatalf("precondition: stale push marker expected, got %s col %d", it.CodeActivity.State, it.DisplayColumn)
	}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.Remediation != nil {
		t.Fatalf("no evidence must yield no remediation record: %+v", it.Remediation)
	}
	ca := it.CodeActivity
	if ca.State != codeStateUnassigned || ca.Caption != "Changes requested · unassigned · next: scanner" || it.DisplayColumn != trackerNumberPRInProgress {
		t.Fatalf("unassigned word: state=%s caption=%q col=%d", ca.State, ca.Caption, it.DisplayColumn)
	}
	if !strings.Contains(ca.Detail, "switches to Remediating when") || ca.Since == nil || !ca.Since.Equal(now.Add(-40*time.Minute)) {
		t.Fatalf("detail should explain pickup triggers and date the red signal: %q since=%v", ca.Detail, ca.Since)
	}
	if it.Caption != ca.Caption {
		t.Fatalf("070 caption follows the code word: %q", it.Caption)
	}
}

func TestStageTrackerRemediationCIFailingWordAndPersonNext(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	it := remediationRedPR(now)
	it.ReviewActivity = &StageTrackerActivity{Head: it.Head, State: "unknown"}
	red := now.Add(-10 * time.Minute)
	it.CIActivity = &StageTrackerActivity{Head: it.Head, State: "failing", Since: &red}
	it.AuthorKind, it.Author, it.Claimant = authorKindHuman, "alice", nil
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if got := it.CodeActivity.Caption; got != "CI failing · unassigned · next: @alice" {
		t.Fatalf("CI-only red names CI and the author: %q", got)
	}
}

func TestStageTrackerRemediationEvidenceKickFromFixDispatch(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	it := remediationRedPR(now)
	dispatched := now.Add(-4 * time.Minute)
	dispatch := review.DispatchState{Fixes: []review.PendingFix{{Repo: it.Repo, Number: it.Number, HeadSHA: it.Head, Agent: "scanner", Dispatched: dispatched}}}
	stageTrackerApplyRemediation(it, dispatch, nil, "scanner", now)
	ev := it.Remediation
	if ev == nil || ev.Kind != remediationKindKick || ev.Actor != "scanner" || ev.Source != "fix-dispatch" || !ev.Since.Equal(dispatched) {
		t.Fatalf("fix dispatch is kick evidence: %+v", ev)
	}
	ca := it.CodeActivity
	if ca.State != codeStateRemediating || ca.Caption != "Remediating · scanner · 4m" || !ca.Active || it.DisplayColumn != trackerNumberPRInProgress {
		t.Fatalf("remediating word: state=%s caption=%q active=%v col=%d", ca.State, ca.Caption, ca.Active, it.DisplayColumn)
	}
	if !strings.Contains(ca.Detail, "evidence: kick, fix-dispatch, since "+dispatched.Format(time.RFC3339)) {
		t.Fatalf("tooltip carries evidence kind and time: %q", ca.Detail)
	}
	// An expired dispatch is no evidence.
	dispatch.Fixes[0].Dispatched = now.Add(-review.DefaultPendingFixTTL - time.Minute)
	it = remediationRedPR(now)
	stageTrackerApplyRemediation(it, dispatch, nil, "scanner", now)
	if it.Remediation != nil || it.CodeActivity.State != codeStateUnassigned {
		t.Fatalf("expired dispatch must not read as pickup: %+v %s", it.Remediation, it.CodeActivity.State)
	}
}

func TestStageTrackerRemediationEvidenceKickFromAgentNow(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	it := remediationRedPR(now)
	at := now.Add(-2 * time.Minute)
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "reviewer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: it.Repo, Number: it.Number, Action: github.AuditActionPRReviewed, Current: true, At: at.Format(time.RFC3339Nano)}},
		{Name: "scanner", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: it.Repo, Number: it.Number, Action: github.AuditActionAgentCommentCreated, Current: true, At: at.Format(time.RFC3339Nano)}},
	}}
	stageTrackerApplyRemediation(it, review.DispatchState{}, status, "scanner", now)
	if ev := it.Remediation; ev == nil || ev.Kind != remediationKindKick || ev.Actor != "scanner" || ev.Source != "agent-now" {
		t.Fatalf("working agent on the PR after the red signal is kick evidence: %+v", ev)
	}
	// The reviewer alone is the review swarm, not a fix; an action older
	// than the red signal is the earlier turn.
	status.Agents = status.Agents[:1]
	it = remediationRedPR(now)
	stageTrackerApplyRemediation(it, review.DispatchState{}, status, "scanner", now)
	if it.Remediation != nil {
		t.Fatalf("review action is not pickup: %+v", it.Remediation)
	}
	status.Agents = []FrontendAgent{{Name: "scanner", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: it.Repo, Number: it.Number, Action: github.AuditActionAgentPRCreated, Current: true, At: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)}}}
	it = remediationRedPR(now)
	stageTrackerApplyRemediation(it, review.DispatchState{}, status, "scanner", now)
	if it.Remediation != nil {
		t.Fatalf("pre-signal action is not pickup: %+v", it.Remediation)
	}
}

func TestStageTrackerRemediationEvidenceClaim(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	since := now.Add(-9 * time.Minute)
	it := remediationRedPR(now)
	it.Rework = &StageTrackerRework{State: reworkStateInProgress, Actor: "bob", Evidence: reworkEvidenceClaim, Since: &since}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if ev := it.Remediation; ev == nil || ev.Kind != remediationKindClaim || ev.Actor != "@bob's clanker" || ev.Source != "contributor-fix-relay" {
		t.Fatalf("relay claim is claim evidence: %+v", ev)
	}
	if got := it.CodeActivity.Caption; got != "Remediating · @bob's clanker · 9m" {
		t.Fatalf("claim word: %q", got)
	}
	// A clanker claim on the PR after the signal counts; the authorship
	// claimant never does.
	it = remediationRedPR(now)
	it.Claimant = &StageTrackerClaimant{Kind: claimantKindClanker, Login: "carol", Source: claimantSourceClaimComment, Since: &since}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if ev := it.Remediation; ev == nil || ev.Kind != remediationKindClaim || ev.Actor != "@carol's clanker" {
		t.Fatalf("claim comment is claim evidence: %+v", ev)
	}
	before := now.Add(-2 * time.Hour)
	it = remediationRedPR(now)
	it.Claimant = &StageTrackerClaimant{Kind: claimantKindClanker, Login: "carol", Source: claimantSourceClaimLedger, Since: &before}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.Remediation != nil {
		t.Fatalf("claim older than the red signal is not pickup: %+v", it.Remediation)
	}
	// A queued relay fix is a waiting word with its queue position.
	it = remediationRedPR(now)
	it.Rework = &StageTrackerRework{State: reworkStateRequested, Actor: "bob", Evidence: reworkEvidenceQueued, Ahead: 3}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.Remediation != nil || it.CodeActivity.Caption != "Changes requested · unassigned · fixer queued, 3 ahead · next: @bob's clanker" {
		t.Fatalf("queued fix is waiting, not remediating: %+v %q", it.Remediation, it.CodeActivity.Caption)
	}
}

func TestStageTrackerRemediationEvidencePush(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	it := remediationRedPR(now)
	pushed := now.Add(-2 * time.Minute)
	it.Push = &StageTrackerPush{State: pushStatePushed, Login: "hive[bot]", Since: &pushed, Sha: "f269349abcdef"}
	it.Head = "f269349abcdef"
	it.CIActivity = &StageTrackerActivity{Head: it.Head, State: "running"}
	it.ReviewActivity = &StageTrackerActivity{Head: it.Head, State: "changes_requested", Since: it.ReviewActivity.Since}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if ev := it.Remediation; ev == nil || ev.Kind != remediationKindPush || ev.Actor != "@hive[bot]" || ev.Head != "f269349abcdef" || ev.Source != "recorded-push" {
		t.Fatalf("push after the signal is push evidence: %+v", ev)
	}
	ca := it.CodeActivity
	if ca.State != codeStateFixPushed || ca.Caption != "Fix pushed f269349 · CI pending" || ca.Active || it.DisplayColumn != trackerNumberReviewPending {
		t.Fatalf("fix pushed word: state=%s caption=%q active=%v col=%d", ca.State, ca.Caption, ca.Active, it.DisplayColumn)
	}
	it.CIActivity.State = "passing"
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if got := it.CodeActivity.Caption; got != "Fix pushed f269349 · re-review pending" {
		t.Fatalf("green CI leaves the re-review: %q", got)
	}
	// 072: the author's head after the ask (rework pushed) is push evidence.
	it = remediationRedPR(now)
	it.Stage, it.Author, it.AuthorKind = trackerNumberReturned, "dave", authorKindClanker
	it.Push = nil
	it.Rework = &StageTrackerRework{State: reworkStatePushed, Actor: "dave", Evidence: reworkEvidenceCommit, Head: "0123456789", Since: &pushed}
	stageTrackerApplyCodeActivity(it, review.DispatchState{}, nil, now)
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if ev := it.Remediation; ev == nil || ev.Kind != remediationKindPush || ev.Actor != "@dave" || it.CodeActivity.Caption != "Fix pushed 0123456 · re-review pending" {
		t.Fatalf("rework push: %+v %q", ev, it.CodeActivity.Caption)
	}
}

func TestStageTrackerRemediationKeepsOwnerWaitAndObservedWork(t *testing.T) {
	now := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	it := remediationRedPR(now)
	it.Stage, it.Author, it.AuthorKind, it.Push = trackerNumberReturned, "erin", authorKindClanker, nil
	stageTrackerApplyCodeActivity(it, review.DispatchState{}, nil, now)
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	ca := it.CodeActivity
	if ca.State != "waiting_owner" || ca.Caption != "Waiting for @erin · changes requested" || it.DisplayColumn != trackerNumberPRInProgress || it.Remediation != nil {
		t.Fatalf("person-owned wait keeps its word with the red reason: state=%s caption=%q col=%d ev=%+v", ca.State, ca.Caption, it.DisplayColumn, it.Remediation)
	}
	// Observed code work already reads Remediating · agent; untouched.
	it = remediationRedPR(now)
	it.CodeActivity = &StageTrackerActivity{Head: it.Head, State: "fixing", Caption: "Remediating · scanner", Active: true, Source: "fix-dispatch+successful-edit+verified-git-worktree"}
	it.DisplayColumn = trackerNumberPRInProgress
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.CodeActivity.State != "fixing" || it.CodeActivity.Caption != "Remediating · scanner" {
		t.Fatalf("observed work keeps its word: %+v", it.CodeActivity)
	}
	// Not red, not in a fix stage: nothing changes.
	it = remediationRedPR(now)
	it.Stage = trackerNumberReviewPending
	it.ReviewActivity = &StageTrackerActivity{Head: it.Head, State: "reviewing"}
	stageTrackerApplyCodeActivity(it, review.DispatchState{}, nil, now)
	before := it.CodeActivity
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.Remediation != nil || it.CodeActivity != before {
		t.Fatalf("green PR untouched: %+v", it.CodeActivity)
	}
}

func TestStageTrackerRemediationJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	if output, err := exec.Command(node, "--test", "stage_tracker_remediation_chevron.test.js").CombinedOutput(); err != nil {
		t.Fatalf("remediation chevron JavaScript: %v\n%s", err, output)
	}
}
