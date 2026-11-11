package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A PR the blocked sweep holds on its Depends-on target names it on its row,
// the same way a blocked issue does: blocked_on_pr with the cached state and
// a "Blocked on PR #N — lifts when it merges" caption clause. The MCP
// stage_tracker_read tool reads this same /api/stage-tracker payload. A PR
// whose `blocked` no marker explains keeps the bare pill.
func TestStageTrackerBlockedPRNamesItsDependsOnTarget(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := newTestServer()
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "widget", Full: "acme/widget",
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 9201, Title: "Client for the new API", Author: "alice", State: "open", Labels: []string{"blocked"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 9202, Title: "The new API", Author: "bob", State: "open"}},
			FrontendPR{PullRequest: github.PullRequest{Number: 9203, Title: "Held by a person", Author: "carol", State: "open", Labels: []string{"blocked"}}},
		},
	}}
	t.Cleanup(github.NoteBlockedOnSeenForTest("acme/widget", 9201, "acme/widget", 9202))
	s.UpdateStatus(status)
	tr := s.buildStageTracker(status, nil, now)

	it := trackerItem(t, tr, "pr", 9201)
	if it.BlockedOnPR == nil || it.BlockedOnPR.Repo != "acme/widget" || it.BlockedOnPR.Number != 9202 || it.BlockedOnPR.State != blockedOnPRStateOpen {
		t.Fatalf("9201 blocked_on_pr = %+v", it.BlockedOnPR)
	}
	if !hasMod(it, modBlocked) || !strings.Contains(it.Caption, "Blocked on PR #9202 — lifts when it merges") {
		t.Fatalf("9201 mods=%v caption=%q", it.Modifiers, it.Caption)
	}

	it = trackerItem(t, tr, "pr", 9203)
	if it.BlockedOnPR != nil || !hasMod(it, modBlocked) {
		t.Fatalf("9203 (no marker read) blocked_on_pr=%+v mods=%v", it.BlockedOnPR, it.Modifiers)
	}
}

// The dashboard's human-merge shortcuts never merge past `blocked`.
func TestDelegatedHumanMergeGatedByBlocked(t *testing.T) {
	ready := stagelabels.ReadyForHumanMerge.Label()
	if !delegatedHumanMergeGated(delegatedecision.Input{Kind: delegatedecision.KindPR, Labels: []string{ready, "Blocked"}}) {
		t.Fatal("a blocked 090 PR must be gated")
	}
	if delegatedHumanMergeGated(delegatedecision.Input{Kind: delegatedecision.KindPR, Labels: []string{ready}}) {
		t.Fatal("a plain 090 PR must not be gated")
	}
}
