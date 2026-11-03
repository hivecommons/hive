package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rebasemerge"
)

func useRebaseAttemptsStore(t *testing.T) *rebasemerge.Attempts {
	t.Helper()
	store := rebasemerge.NewAttempts("")
	prev := rebaseAttemptsStore
	rebaseAttemptsStore = func() *rebasemerge.Attempts { return store }
	t.Cleanup(func() { rebaseAttemptsStore = prev })
	return store
}

func useRebaseMergeStore(t *testing.T) *rebasemerge.Store {
	t.Helper()
	store := rebasemerge.NewStore(filepath.Join(t.TempDir(), "rm.json"))
	prev := rebaseMergeStore
	rebaseMergeStore = func() *rebasemerge.Store { return store }
	t.Cleanup(func() { rebaseMergeStore = prev })
	return store
}

// "Merge now as maintainer" on a PR GitHub reports dirty is not a
// 405: the approval is recorded first (/lgtm + /approve), the
// follow-through is armed, the rebase note is left, nothing is merged, and
// the answer is 202 rebase-queued (hivecommons/hive#14822).
func TestRepoItemDecisionMergeNowOnDirtyPRArmsRebaseThenMerge(t *testing.T) {
	store := useRebaseMergeStore(t)
	mock := &repoItemDecisionMock{prForeign: true, prMergeState: "dirty", prLabels: []string{"stage/085-needs-decision", "needs-rebase"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr","comment":"ship it"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("merge-now on dirty: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != repoItemStatusRebaseQd || resp.Head != "feedfacefeedfacefeedfacefeedfacefeedface" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Result == nil || resp.Result.Action != rowActionRebaseQueued || !strings.Contains(resp.Result.Outcome, "approval is on record") || !strings.Contains(resp.Result.Outcome, "rebasing #8 onto main") {
		t.Fatalf("result = %+v", resp.Result)
	}
	if len(mock.merges) != 0 {
		t.Fatalf("nothing merges on a dirty head: %v", mock.merges)
	}
	if len(mock.comments) < 3 || !strings.HasPrefix(mock.comments[0], "/lgtm\n") || !strings.HasPrefix(mock.comments[1], "/approve\n") || !strings.Contains(mock.comments[2], "Rebase, then merge as maintainer — @writer approved #8") || !strings.Contains(mock.comments[2], "> ship it") {
		t.Fatalf("comments = %q", mock.comments)
	}
	f, ok := store.Get("testorg/testrepo", 8)
	if !ok || f.State != rebasemerge.StateRebasing || f.Actor != "writer" || f.Base != "main" || f.ApprovedHead != "feedfacefeedfacefeedfacefeedfacefeedface" {
		t.Fatalf("follow-through = %+v (ok=%v)", f, ok)
	}
	if len(f.Steps) != 2 || f.Steps[0].Step != rebasemerge.StepApprovalRecorded || f.Steps[1].Step != rebasemerge.StepRebaseKicked {
		t.Fatalf("steps = %+v", f.Steps)
	}
	if f.HiveAuthored {
		t.Fatal("a contributor PR must not read as hive-authored")
	}
	// A second click while it runs is a no-op that says so.
	w = postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "Already queued") {
		t.Fatalf("repeat: code=%d body=%s", w.Code, w.Body.String())
	}
}

// The driver: a rebased head that GitHub reports clean merges with the
// approval on record and settles the ledger; a dirty head past the deadline
// fails with the files and the two options, and the PR gets the note.
func TestDriveRebaseMergeGreenMergesAndConflictFails(t *testing.T) {
	store := useRebaseMergeStore(t)
	mock := &repoItemDecisionMock{prForeign: true, commitBy: map[string]string{"0123456789abcdef0123456789abcdef01234567": "hive[bot]"}}
	srv := newRepoItemDecisionServer(t, mock)
	srv.operatorActionStore = newOperatorActionStore(filepath.Join(t.TempDir(), "ops.json"))
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	prevNow := rebaseMergeNow
	rebaseMergeNow = func() time.Time { return now }
	t.Cleanup(func() { rebaseMergeNow = prevNow })

	f := rebasemerge.Arm("testorg/testrepo", 8, "writer", "main", "feedfacefeedfacefeedfacefeedfacefeedface", false, now)
	f.Kicked(now, "fixer")
	if err := store.Put(f); err != nil {
		t.Fatal(err)
	}
	_ = srv.operatorActions().Record("testorg/testrepo", 8, OperatorAction{Action: operatorActionMergeNow, Actor: "writer", At: now, Outcome: operatorActionOutcomePending, Detail: repoItemStatusRebaseQd, Stage: 85})

	// New head, CI pending: no merge yet.
	now = now.Add(5 * time.Minute)
	srv.driveRebaseMerge(context.Background(), store, f, ghpkg.PRMergeState{State: "open", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "main", MergeableState: "blocked"})
	f, _ = store.Get("testorg/testrepo", 8)
	if f.State != rebasemerge.StateCIPending || len(mock.merges) != 0 {
		t.Fatalf("after rebase: state=%q merges=%v", f.State, mock.merges)
	}
	// Clean: merge pinned to the rebased head.
	now = now.Add(5 * time.Minute)
	srv.driveRebaseMerge(context.Background(), store, f, ghpkg.PRMergeState{State: "open", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "main", MergeableState: "clean"})
	f, _ = store.Get("testorg/testrepo", 8)
	if f.State != rebasemerge.StateMerged || f.MergeSHA != "0ddba11" {
		t.Fatalf("after clean: %+v", f)
	}
	if len(mock.merges) != 1 || mock.merges[0]["sha"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("merge must pin the rebased head: %v", mock.merges)
	}
	acts := srv.operatorActions().List("testorg/testrepo", 8)
	if len(acts) != 1 || acts[0].Outcome != operatorActionOutcomeApplied || !strings.Contains(acts[0].Detail, "merged 0ddba11 after rebase") {
		t.Fatalf("ledger = %+v", acts)
	}

	// Conflict path on another follow-through.
	g := rebasemerge.Arm("testorg/testrepo", 9, "writer", "main", "feedfacefeedfacefeedfacefeedfacefeedface", true, now)
	g.Kicked(now, "fixer")
	_ = store.Put(g)
	_ = srv.operatorActions().Record("testorg/testrepo", 9, OperatorAction{Action: operatorActionMergeNow, Actor: "writer", At: now, Outcome: operatorActionOutcomePending, Stage: 85})
	before := len(mock.comments)
	now = now.Add(rebasemerge.DefaultRebaseDeadline)
	srv.driveRebaseMerge(context.Background(), store, g, ghpkg.PRMergeState{State: "open", HeadSHA: "feedfacefeedfacefeedfacefeedfacefeedface", BaseRef: "main", MergeableState: "dirty"})
	g, _ = store.Get("testorg/testrepo", 9)
	if g.State != rebasemerge.StateFailed || g.FailReason != rebasemerge.FailConflict || len(g.Options) != 2 {
		t.Fatalf("conflict: %+v", g)
	}
	acts = srv.operatorActions().List("testorg/testrepo", 9)
	if len(acts) != 1 || acts[0].Outcome != operatorActionOutcomeFailed || !strings.Contains(acts[0].Detail, "conflicts") {
		t.Fatalf("ledger = %+v", acts)
	}
	if len(mock.comments) != before+1 || !strings.Contains(mock.comments[before], "did not land") || !strings.Contains(mock.comments[before], rebasemerge.OptionAdoptFreshLabel) || !strings.Contains(mock.comments[before], rebasemerge.OptionCloseSupersLabel) {
		t.Fatalf("failure note = %q", mock.comments[before:])
	}
	res := rebaseMergeFailedResult(g)
	if res.Action != rowActionRefused || len(res.Options) != 2 || res.Options[0].Action != decisionActionAdopt || res.Options[1].Action != decisionActionCloseSuperseded {
		t.Fatalf("failed result = %+v", res)
	}
}

// The row: while the follow-through runs the caption reads "Rebasing →
// merge queued (approved by @you)" and the ledger's followed[] carries the
// steps; the menu offers "Rebase, then merge as maintainer" at 085 and in
// place of merge-now at 090 when the row needs a rebase.
func TestRebaseMergeRowCaptionFollowedAndOptions(t *testing.T) {
	store := useRebaseMergeStore(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	f := rebasemerge.Arm("hivecommons/hive", 14822, "clubanderson", "v5", "b0f5f80", false, now)
	f.Kicked(now.Add(time.Second), "fixer")
	_ = store.Put(f)
	srv := &Server{}
	items := []StageTrackerItem{{Repo: "hivecommons/hive", Number: 14822, Kind: "pr", Stage: 85, Caption: "Needs human decision", Modifiers: []string{modNeedsRebase},
		LastOperatorAction: &StageTrackerOperatorAction{Action: operatorActionMergeNow, Actor: "clubanderson", At: now, Followed: []StageTrackerHop{{Number: 85, Slug: "needs-decision", At: now.Add(time.Hour)}}}}}
	srv.applyRebaseMergeFollowThrough(items, now.Add(time.Minute))
	it := items[0]
	if it.RebaseMerge == nil || it.RebaseMerge.State != rebasemerge.StateRebasing || it.Caption != "Rebasing → merge queued (approved by @clubanderson)" {
		t.Fatalf("row = %+v caption=%q", it.RebaseMerge, it.Caption)
	}
	followed := it.LastOperatorAction.Followed
	if len(followed) != 3 || followed[0].Slug != rebasemerge.StepApprovalRecorded || followed[1].Slug != rebasemerge.StepRebaseKicked || followed[2].Number != 85 {
		t.Fatalf("followed = %+v", followed)
	}

	// 085 options: Let hive decide, Approve, Rebase-then-merge, …
	d := decisionForPR(it, nil, "", false)
	if len(d.Options) < 3 || d.Options[2].Action != decisionActionMergeNow || d.Options[2].Label != rebaseMergeFollowThroughLabel || !strings.Contains(d.Options[2].Consequence, "rebases #14822 onto its base branch") {
		t.Fatalf("085 options = %+v", d.Options)
	}
	// Without needs-rebase the 085 menu is unchanged.
	plain := decisionForPR(StageTrackerItem{Repo: "o/r", Number: 1, Kind: "pr", Stage: 85}, nil, "", false)
	for _, o := range plain.Options {
		if o.Action == decisionActionMergeNow {
			t.Fatalf("merge-now offered at 085 without needs-rebase: %+v", plain.Options)
		}
	}
	// 090 relabel: merge-now is the secondary answer behind "Approve and
	// let hive merge", renamed for the rebase.
	hm := decisionForHumanMerge(StageTrackerItem{Repo: "o/r", Number: 7, Kind: "pr", Stage: 90, BaseRef: "v5", Labels: []string{"needs-rebase"}}, false)
	if hm.Options[0].Action != decisionActionQueueMerge || hm.Options[1].Action != decisionActionMergeNow || hm.Options[1].Label != rebaseMergeFollowThroughLabel || hm.Options[1].Consequence != rebaseMergeFollowThroughConsequence(7, "v5") {
		t.Fatalf("090 options = %+v", hm.Options[:2])
	}
	hmPlain := decisionForHumanMerge(StageTrackerItem{Repo: "o/r", Number: 7, Kind: "pr", Stage: 90, BaseRef: "v5"}, false)
	if hmPlain.Options[1].Label != "Merge now as maintainer" {
		t.Fatalf("090 plain option = %+v", hmPlain.Options[1])
	}
	// Settled: the caption is the lane's again; a stale record is dropped.
	f.Merged(now.Add(2*time.Minute), "abc")
	_ = store.Update(f)
	items[0].Caption = "Merged"
	srv.applyRebaseMergeFollowThrough(items, now.Add(3*time.Minute))
	if items[0].Caption != "Merged" || items[0].RebaseMerge == nil || items[0].RebaseMerge.State != rebasemerge.StateMerged {
		t.Fatalf("settled row = %+v caption=%q", items[0].RebaseMerge, items[0].Caption)
	}
	items[0].RebaseMerge = nil
	srv.applyRebaseMergeFollowThrough(items, now.Add(3*time.Hour))
	if items[0].RebaseMerge != nil {
		t.Fatalf("stale settled record still on the row: %+v", items[0].RebaseMerge)
	}
}

// hivecommons/hive#15103: the fixer's rebase loop guard shows on the row
// ("Rebasing · attempt n/2", then "needs a person"), and a follow-through
// whose rebased head is still dirty retries while attempts remain and
// fails with the conflict once they are exhausted.
func TestRebaseAttemptsPillAndFollowThroughExhaustion(t *testing.T) {
	attempts := useRebaseAttemptsStore(t)
	store := useRebaseMergeStore(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	rebaseMergeNow = func() time.Time { return now }
	t.Cleanup(func() { rebaseMergeNow = time.Now })

	items := []StageTrackerItem{{Repo: "hivecommons/hive", Number: 15087, Kind: "pr", Stage: 90, Modifiers: []string{modNeedsRebase}}, {Repo: "hivecommons/hive", Number: 1, Kind: "pr", Stage: 90}}
	applyRebaseAttempts(items)
	if items[0].Rebase != nil || items[1].Rebase != nil {
		t.Fatalf("no episode, no pill: %+v %+v", items[0].Rebase, items[1].Rebase)
	}
	attempts.Observe("hivecommons/hive", 15087, "aaa1111aaa1111", now)
	applyRebaseAttempts(items)
	if r := items[0].Rebase; r == nil || r.Attempt != 1 || r.Max != 2 || r.Exhausted || r.Head != "aaa1111" || r.Text != "Rebasing · attempt 1/2" {
		t.Fatalf("attempt 1 pill = %+v", r)
	}
	attempts.Observe("hivecommons/hive", 15087, "bbb2222", now)
	attempts.Observe("hivecommons/hive", 15087, "ccc3333", now)
	applyRebaseAttempts(items)
	if r := items[0].Rebase; r == nil || !r.Exhausted || r.Attempt != 2 || r.Text != "Rebase failed · 2/2 — needs a person" {
		t.Fatalf("exhausted pill = %+v", r)
	}

	// Follow-through on the same PR: still dirty after the fixer's first
	// push → back to rebasing; exhausted → failed with the files.
	mock := &repoItemDecisionMock{commitBy: map[string]string{"bbb2222": "hive[bot]", "ccc3333": "hive[bot]"}}
	srv := newRepoItemDecisionServer(t, mock)
	srv.operatorActionStore = newOperatorActionStore(filepath.Join(t.TempDir(), "ops.json"))
	attempts.Clear("hivecommons/hive", 15087)
	attempts.Observe("hivecommons/hive", 15087, "aaa1111", now)
	f := rebasemerge.Arm("hivecommons/hive", 15087, "clubanderson", "v5", "aaa1111", false, now)
	f.Kicked(now, "scanner")
	_ = store.Put(f)
	now = now.Add(time.Minute)
	attempts.Observe("hivecommons/hive", 15087, "bbb2222", now)
	srv.driveRebaseMerge(context.Background(), store, f, ghpkg.PRMergeState{State: "open", HeadSHA: "bbb2222", BaseRef: "v5", MergeableState: "dirty"})
	f, _ = store.Get("hivecommons/hive", 15087)
	if f.State != rebasemerge.StateRebasing || f.Steps[len(f.Steps)-1].Step != rebasemerge.StepStillConflicting {
		t.Fatalf("after first still-dirty head: %+v", f)
	}
	now = now.Add(time.Minute)
	attempts.Observe("hivecommons/hive", 15087, "ccc3333", now)
	srv.driveRebaseMerge(context.Background(), store, f, ghpkg.PRMergeState{State: "open", HeadSHA: "ccc3333", BaseRef: "v5", MergeableState: "dirty"})
	f, _ = store.Get("hivecommons/hive", 15087)
	if f.State != rebasemerge.StateFailed || f.FailReason != rebasemerge.FailConflict || len(f.Options) != 2 {
		t.Fatalf("after exhausting attempts: %+v", f)
	}
}

// Approve on a PR GitHub reports dirty (hivecommons/hive#15103, live
// 17:59): the toast must not promise the merge gate "takes it from here"
// (it skips a conflicting PR for good). The approval is recorded, the
// fixer's rebase episode opens in the operator's name (attempt 1/2) so
// the row reads "Rebasing · attempt 1/2", and the toast offers "Rebase,
// then merge as maintainer". A hive-authored PR that is merely behind
// gets update-branch on the spot.
func TestRepoItemDecisionApproveOnDirtyPRQueuesRebase(t *testing.T) {
	attempts := useRebaseAttemptsStore(t)
	mock := &repoItemDecisionMock{prForeign: true, prMergeState: "dirty", prLabels: []string{"stage/085-needs-decision", "needs-rebase"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"approve","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve on dirty: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "approval-commented" || resp.Rebase == nil || resp.Rebase.Attempt != 1 || resp.Rebase.Max != 2 || resp.Rebase.Text != "Rebasing · attempt 1/2" {
		t.Fatalf("response = %+v rebase=%+v", resp, resp.Rebase)
	}
	if len(mock.comments) < 2 || !strings.HasPrefix(mock.comments[0], "/lgtm\n") || !strings.HasPrefix(mock.comments[1], "/approve\n") {
		t.Fatalf("the approval is recorded first: %q", mock.comments)
	}
	res := resp.Result
	if res == nil || res.Action != rowActionApproved || res.Outcome != "Approved — #8 conflicts with main; hive will rebase it next pass (attempt 1/2)." {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.Next, "takes it from here;") || !strings.Contains(res.Next, "skips a conflicting PR") {
		t.Fatalf("next = %q", res.Next)
	}
	if len(res.Options) != 1 || res.Options[0].Action != decisionActionMergeNow || res.Options[0].Label != rebaseMergeFollowThroughLabel || res.Options[0].Consequence != rebaseMergeFollowThroughConsequence(8, "main") {
		t.Fatalf("options = %+v", res.Options)
	}
	a, ok := attempts.Get("testorg/testrepo", 8)
	if !ok || a.RequestedBy != "writer" || a.Count() != 1 || a.Head() != "feedfacefeedfacefeedfacefeedfacefeedface" {
		t.Fatalf("episode = %+v ok=%v", a, ok)
	}
	if mock.updates != 0 {
		t.Fatalf("a dirty head gets no update-branch: %d", mock.updates)
	}
	items := []StageTrackerItem{{Repo: "testorg/testrepo", Number: 8, Kind: "pr", Stage: 85}}
	applyRebaseAttempts(items)
	if items[0].Rebase == nil || items[0].Rebase.Text != "Rebasing · attempt 1/2" {
		t.Fatalf("row = %+v", items[0].Rebase)
	}

	// Hive's own PR, merely behind: update-branch now, and the toast says so.
	mock2 := &repoItemDecisionMock{prMergeState: "behind", prLabels: []string{"stage/085-needs-decision", "needs-rebase"}}
	srv2 := newRepoItemDecisionServer(t, mock2)
	w = postRepoItemDecisionAs(t, srv2, 8, "writer", "merger", `{"decision":"approve","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve on behind: code=%d body=%s", w.Code, w.Body.String())
	}
	resp = repoItemDecisionResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if mock2.updates != 1 || resp.Result == nil || !strings.Contains(resp.Result.Outcome, "is behind main; hive is updating the branch") {
		t.Fatalf("behind: updates=%d result=%+v", mock2.updates, resp.Result)
	}

	// Clean: the plain approve toast is unchanged.
	mock3 := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision"}}
	srv3 := newRepoItemDecisionServer(t, mock3)
	w = postRepoItemDecisionAs(t, srv3, 8, "writer", "merger", `{"decision":"approve","type":"pr"}`)
	resp = repoItemDecisionResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Rebase != nil || resp.Result == nil || len(resp.Result.Options) != 0 || !strings.Contains(resp.Result.Next, "merge sweep merges it") {
		t.Fatalf("clean approve: code=%d resp=%+v", w.Code, resp.Result)
	}
}

// The approval is bound to the head the operator read. When a contributor
// PR's head moves and GitHub says the hive did not commit it, the
// follow-through fails closed — nothing merges, the ledger says why, the
// note on the PR asks for a fresh approval — and the row's way out is to
// approve the new head (hivecommons/hive#15104 security review).
func TestDriveRebaseMergeForeignHeadFailsClosed(t *testing.T) {
	store := useRebaseMergeStore(t)
	mock := &repoItemDecisionMock{prForeign: true}
	srv := newRepoItemDecisionServer(t, mock)
	srv.operatorActionStore = newOperatorActionStore(filepath.Join(t.TempDir(), "ops.json"))
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	prevNow := rebaseMergeNow
	rebaseMergeNow = func() time.Time { return now }
	t.Cleanup(func() { rebaseMergeNow = prevNow })

	f := rebasemerge.Arm("testorg/testrepo", 8, "writer", "main", "feedfacefeedfacefeedfacefeedfacefeedface", false, now)
	f.Kicked(now, "fixer")
	_ = store.Put(f)
	_ = srv.operatorActions().Record("testorg/testrepo", 8, OperatorAction{Action: operatorActionMergeNow, Actor: "writer", At: now, Outcome: operatorActionOutcomePending, Detail: repoItemStatusRebaseQd, Stage: 85})

	now = now.Add(5 * time.Minute)
	srv.driveRebaseMerge(context.Background(), store, f, ghpkg.PRMergeState{State: "open", HeadSHA: "0123456789abcdef0123456789abcdef01234567", BaseRef: "main", MergeableState: "clean"})
	f, _ = store.Get("testorg/testrepo", 8)
	if f.State != rebasemerge.StateFailed || f.FailReason != rebasemerge.FailForeignHead || f.ForeignHead != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("foreign head: %+v", f)
	}
	if len(mock.merges) != 0 {
		t.Fatalf("a head the hive did not produce must not merge: %v", mock.merges)
	}
	acts := srv.operatorActions().List("testorg/testrepo", 8)
	if len(acts) != 1 || acts[0].Outcome != operatorActionOutcomeFailed || !strings.Contains(acts[0].Detail, "someone pushed 0123456") {
		t.Fatalf("ledger = %+v", acts)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "approve it again") || !strings.Contains(mock.comments[0], rebasemerge.OptionReapproveLabel) || strings.Contains(mock.comments[0], rebasemerge.OptionAdoptFreshLabel) {
		t.Fatalf("failure note = %q", mock.comments)
	}
	res := rebaseMergeFailedResult(f)
	if res.Action != rowActionRefused || res.Why != rebasemerge.FailForeignHead || len(res.Options) != 2 || res.Options[0].Action != decisionActionMergeNow || res.Options[1].Action != decisionActionCloseSuperseded {
		t.Fatalf("failed result = %+v", res)
	}
}

// A maintainer-editable fork with workflow changes must not enter the fixer
// queue, even when its author is the hive's App. Approval cannot grant push
// permissions, and a fresh-branch adoption is fenced for these paths too.
func TestRebaseMergeWorkflowForkRefusesBeforeArming(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign=", foreign), func(t *testing.T) {
			store := useRebaseMergeStore(t)
			mock := &repoItemDecisionMock{prForeign: foreign, prFork: true, prMergeState: "dirty", workflowPath: ".github/workflows/ci.yml"}
			srv := newRepoItemDecisionServer(t, mock)
			w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var resp repoItemDecisionResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Result == nil || resp.Result.Why != rebasemerge.FailWorkflowPush || !strings.Contains(resp.Result.Outcome, mock.workflowPath) || len(resp.Result.Options) != 0 {
				t.Fatalf("result = %+v", resp.Result)
			}
			if store.Armed("testorg/testrepo", 8) || len(mock.comments) != 0 || len(mock.merges) != 0 || mock.updates != 0 || len(mock.reviews) != 0 {
				t.Fatalf("refused request armed or wrote to GitHub: store=%v mock=%+v", store.Live(), mock)
			}
		})
	}
}

// A fork head without maintainer edits is only unreachable for a
// contributor's PR: the hive pushes its own heads with its own credentials,
// so a hive-authored fork PR still arms as it did before the workflow fence.
func TestRebaseMergeForkWithoutMaintainerEditsKeepsHiveAuthoredBypass(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign=", foreign), func(t *testing.T) {
			store := useRebaseMergeStore(t)
			mock := &repoItemDecisionMock{prForeign: foreign, prFork: true, prNoMaintainerEdits: true, prMergeState: "dirty"}
			srv := newRepoItemDecisionServer(t, mock)
			w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr"}`)
			var resp repoItemDecisionResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if !foreign {
				f, ok := store.Get("testorg/testrepo", 8)
				if w.Code != http.StatusAccepted || resp.Status != repoItemStatusRebaseQd || !ok || !f.HiveAuthored || f.State != rebasemerge.StateRebasing {
					t.Fatalf("hive-authored fork must still arm: code=%d body=%s follow-through=%+v ok=%v", w.Code, w.Body.String(), f, ok)
				}
				return
			}
			if w.Code != http.StatusOK || resp.Result == nil || resp.Result.Why != rebasemerge.FailNotReachable {
				t.Fatalf("contributor fork must be refused as unreachable: code=%d result=%+v", w.Code, resp.Result)
			}
			if store.Armed("testorg/testrepo", 8) || len(mock.comments) != 0 || len(mock.merges) != 0 {
				t.Fatalf("refused request armed or wrote to GitHub: store=%v mock=%+v", store.Live(), mock)
			}
		})
	}
}

func TestRebaseMergeWorkflowDriftStopsExistingFollowThrough(t *testing.T) {
	store := useRebaseMergeStore(t)
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)
	now := time.Now()
	f := rebasemerge.Arm("testorg/testrepo", 8, "writer", "main", "feedface", false, now)
	f.Kicked(now, "fixer")
	_ = store.Put(f)
	_ = srv.operatorActions().Record(f.Repo, f.Number, OperatorAction{Action: operatorActionMergeNow, Actor: "writer", At: now, Outcome: operatorActionOutcomePending, Stage: 85})
	srv.driveRebaseMerge(context.Background(), store, f, ghpkg.PRMergeState{State: "open", HeadSHA: f.ApprovedHead, BaseRef: "main", MergeableState: "dirty", FromFork: true, MaintainerCanModify: true, WorkflowPushPaths: []string{".github/workflows/new.yml"}})
	got, _ := store.Get(f.Repo, f.Number)
	if got.Live() || got.FailReason != rebasemerge.FailWorkflowPush || store.Armed(f.Repo, f.Number) {
		t.Fatalf("follow-through = %+v", got)
	}
	acts := srv.operatorActions().List(f.Repo, f.Number)
	if len(acts) != 1 || acts[0].Outcome != operatorActionOutcomeFailed || len(mock.comments) != 1 || len(mock.merges) != 0 || strings.Contains(mock.comments[0], rebasemerge.OptionAdoptFreshLabel) {
		t.Fatalf("ledger=%+v comments=%v merges=%v", acts, mock.comments, mock.merges)
	}
}
