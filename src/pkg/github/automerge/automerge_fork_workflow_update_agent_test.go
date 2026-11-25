package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// GitHub refused the App's update-branch on a fork head because the base
// changed .github/workflows/ files (fork-workflow-update). The App cannot
// push those files to a fork, but an agent author's relay can push to its
// own branch: projectbluefin/utah-packages#1294's relay merged the base in
// and the PR recovered with no person, while #1296 and #1298 sat at 085 all
// night with nothing sent to their authors.

// fwuApprove is the head's approve verdict: the agent route needs it, and
// a green head (the tests set Engine.checkSummary to greenChecks).
var fwuApprove = ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}

// observeFWU is adoptHarness.observe for a fork-workflow-update refusal on
// a head whose reviewer verdict the lane noted (none when v is not
// Configured).
func observeFWU(h *adoptHarness, pr *gh.PullRequest, v ContributorLaneVerdict) {
	h.t.Helper()
	h.adopt.mu.Lock()
	h.adopt.heads[pr.GetNumber()] = pr.GetHead().GetSHA()
	h.adopt.mu.Unlock()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, autoMergeReasonForkWorkflowUpdate, false, nil)
	if v.Configured {
		h.e.noteStageVerdict("acme", "widget", pr.GetNumber(), v)
	}
	h.e.noteStageRole("acme", "widget", pr.GetNumber(), stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// nudgeDiagnosis is unchanged for the reason (the bare lane reason, as
// today); only the narrow agent case swaps in the ask, which names the merge
// the author can push. It never says rebase (a rebase drops the per-commit
// approvals some repositories rely on) and never asks for "allow edits by
// maintainers", which does not help here.
func TestNudgeDiagnosisForkWorkflowUpdate(t *testing.T) {
	checks, asks, human := nudgeDiagnosis(autoMergeReasonForkWorkflowUpdate, stageDetail{})
	if checks != nil || human || !reflect.DeepEqual(asks, []string{autoMergeReasonForkWorkflowUpdate}) {
		t.Fatalf("checks=%v asks=%v human=%v, want today's bare lane reason", checks, asks, human)
	}
	if got := withForkWorkflowUpdateAsk(asks); !reflect.DeepEqual(got, []string{forkWorkflowUpdateAsk}) {
		t.Fatalf("narrow asks = %v", got)
	}
	assertForkWorkflowUpdateAsk(t, forkWorkflowUpdateAsk)
	other := []string{"a reviewer requested changes"}
	if got := withForkWorkflowUpdateAsk(other); !reflect.DeepEqual(got, other) {
		t.Fatalf("other asks rewritten: %v", got)
	}
	obs := stageObservation{Reason: autoMergeReasonForkWorkflowUpdate, HeadSHA: "aaa"}
	owed := stagelabels.Record{AuthorUpdateOwedHead: "aaa", AuthorUpdateOwedAt: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	if !authorUpdateOwedNow(owed, obs) || authorUpdateOwedNow(stagelabels.Record{}, obs) || authorUpdateOwedNow(owed, stageObservation{Reason: autoMergeReasonForkWorkflowUpdate, HeadSHA: "bbb"}) || authorUpdateOwedNow(owed, stageObservation{Reason: "conflicting", HeadSHA: "aaa"}) {
		t.Fatal("authorUpdateOwedNow must hold only for the owed head under fork-workflow-update")
	}
}

// The stalled reminder does not promise an adoption the hive will never
// make for the narrow case; every other nudge keeps today's text.
func TestContributorNudgeStalledReminderForkWorkflowUpdate(t *testing.T) {
	n := ContributorNudge{Author: "mallory", HeadSHA: "aaa", Reason: autoMergeReasonForkWorkflowUpdate, ReviewAsks: []string{forkWorkflowUpdateAsk}, Nudge: 2, Stalled: true, NoAdoption: true}
	got := ContributorNudgeComment(n, ContributorNudgeOutcome{Mode: "comment"})
	if strings.Contains(got, "may adopt") || !strings.Contains(got, "a maintainer is asked to bring it up to date") {
		t.Fatalf("narrow stalled reminder:\n%s", got)
	}
	n.NoAdoption = false
	if got := ContributorNudgeComment(n, ContributorNudgeOutcome{Mode: "comment"}); !strings.Contains(got, "the hive may adopt the PR to finish it") {
		t.Fatalf("other stalled reminder changed:\n%s", got)
	}
}

// An agent's fork-workflow-update head outside the narrow case (here at
// 072 through a needs-rebase label, with no approve verdict) keeps today's
// nudge exactly: under the production withhold policy an offline relay's
// comment is withheld for the adoption, and the ask is the bare lane
// reason.
func TestForkWorkflowUpdateOutsideNarrowCaseKeepsTodaysNudge(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, withholdPolicy())
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.e.checkSummary = greenChecks
	observeFWU(h, forkPR(7, "aaa", "needs-rebase"), ContributorLaneVerdict{})
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.Stage != stagelabels.ReturnedToContributor || !rec.AuthorUpdateOwedSince("aaa").IsZero() {
		t.Fatalf("needs-rebase: %+v, want today's 072 with no owed clock", rec)
	}
	if len(nudgeComments(h.api)) != 0 {
		t.Fatalf("nudge comment posted outside the narrow case: %v", h.api.comments)
	}
	if len(h.sink.nudges) != 1 || !reflect.DeepEqual(h.sink.nudges[0].ReviewAsks, []string{autoMergeReasonForkWorkflowUpdate}) || h.sink.nudges[0].NoAdoption {
		t.Fatalf("relay nudge: %+v, want today's bare lane reason", h.sink.nudges)
	}
}

func assertForkWorkflowUpdateAsk(t *testing.T, text string) {
	t.Helper()
	for _, want := range []string{"`.github/workflows/`", "does not let the Hive App push to a fork", "merge the base branch into your branch and push", "`gh pr update-branch`"} {
		if !strings.Contains(text, want) {
			t.Errorf("ask missing %q:\n%s", want, text)
		}
	}
	lower := strings.ToLower(text)
	for _, bad := range []string{"rebase", "allow edits", "\u2014", "\u2013", "@"} {
		if strings.Contains(lower, bad) {
			t.Errorf("ask carries %q:\n%s", bad, text)
		}
	}
}

// An agent's approved, green fork head refused for a base workflow change
// goes to 072 and its relay is nudged with the merge ask, whether its relay is offline or
// not registered here: under the production policy
// (NudgeWithholdWhenAdopting) the GitHub comment posts at once, because no
// adoption note will ever speak for a fork head the hive cannot adopt. The
// hive cannot adopt it (the base sync carries a workflow file the App
// cannot push to the fork, and fork code is never moved into the base
// repository), so after the 072 dwell since the nudge it goes to a person
// with the #17549 note naming the update.
func TestForkWorkflowUpdateAgentPRNudgedThenEscalated(t *testing.T) {
	for name, presence := range map[string]ContributorPresence{
		"offline relay":       {Registered: true, Online: false},
		"unregistered author": {},
	} {
		t.Run(name, func(t *testing.T) {
			p := withholdPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.sink.presence = &presence
			h.e.checkSummary = greenChecks
			h.adopt.allowMaintainerEdits(7, true)
			h.adopt.compareFiles = []string{forkUpdateWorkflowFile}
			key := stageKey("acme", "widget", 7)
			returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			stageIs := func(want stagelabels.Stage, when string) stagelabels.Record {
				t.Helper()
				rec, _ := h.e.stageStoreOrShared().Get(key)
				if rec.Stage != want {
					t.Fatalf("%s: stage %s, want %s (reason %q)", when, rec.Stage, want, rec.Reason)
				}
				return rec
			}

			observeFWU(h, forkPR(7, "aaa"), fwuApprove)
			if rec := stageIs(stagelabels.ReturnedToContributor, "refused update"); rec.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("recorded reason %q, want %q", rec.Reason, autoMergeReasonForkWorkflowUpdate)
			}
			assertForkWorkflowUpdateNudgePosted(t, h)

			h.clock.advance(30 * time.Minute)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			if rec := stageIs(stagelabels.ReturnedToContributor, "adoption due"); !rec.ForkRehomeRefused("aaa") || rec.AdoptedHead != "" {
				t.Fatalf("the adoption attempt must decline the fork and record it: %+v", rec)
			}
			if len(adoptionComments(h.api)) != 0 || len(stalledComments(h.api)) != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
				t.Fatalf("fork PR adopted or parked early: comments=%v pulls=%v refs=%v", h.api.comments, h.adopt.pulls, h.adopt.refs)
			}

			h.clock.advance(23 * time.Hour)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			stageIs(stagelabels.ReturnedToContributor, "before the dwell")

			h.clock.advance(time.Hour)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			// The fork escalation is armed for this head, so it hands the PR
			// on with its own note; the stage-record fallback stands aside.
			want := stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused
			if rec := stageIs(stagelabels.NeedsDecision, "past the dwell"); rec.Reason != want {
				t.Fatalf("reason %q, want %q", rec.Reason, want)
			}
			notes := stalledComments(h.api)
			if len(notes) != 1 {
				t.Fatalf("escalation note: %v", h.api.comments)
			}
			for _, s := range []string{
				"reason=" + want,
				"lane reason `" + autoMergeReasonForkWorkflowUpdate + "`",
				"`v5` has changed `" + forkUpdateWorkflowFile + "` since this branch was cut",
				"not installed on the fork",
				"`gh pr update-branch 7 -R acme/widget`",
			} {
				if !strings.Contains(notes[0], s) {
					t.Fatalf("note missing %q:\n%s", s, notes[0])
				}
			}
			if len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
				t.Fatalf("fork PR adopted or re-homed: comments=%v pulls=%v refs=%v", h.api.comments, h.adopt.pulls, h.adopt.refs)
			}
		})
	}
}

// assertForkWorkflowUpdateNudgePosted checks the one nudge comment and the
// relay task carry the fork-workflow-update ask, and that the agent's PR
// did not get the maintainer's 085 entry comment.
func assertForkWorkflowUpdateNudgePosted(t *testing.T, h *adoptHarness) {
	t.Helper()
	nudges := nudgeComments(h.api)
	if len(nudges) != 1 {
		t.Fatalf("nudge comments: %v", h.api.comments)
	}
	from, to := strings.Index(nudges[0], "**Review asks:**"), strings.Index(nudges[0], "Reply here")
	if from < 0 || to < from || !strings.Contains(nudges[0], "- "+forkWorkflowUpdateAsk+"\n") {
		t.Fatalf("nudge does not list the fork-workflow-update ask:\n%s", nudges[0])
	}
	assertForkWorkflowUpdateAsk(t, nudges[0][from:to])
	if len(h.sink.nudges) != 1 || !slices.Contains(h.sink.nudges[0].ReviewAsks, forkWorkflowUpdateAsk) {
		t.Fatalf("relay nudge: %+v", h.sink.nudges)
	}
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) {
			t.Fatalf("an agent's PR got the maintainer's 085 entry comment:\n%s", c)
		}
	}
}

// An agent's fork-workflow-update PR is never left at 072 with nobody
// asked. When no fork escalation can arm (adoption off, the nudge off, or
// maintainer_policy holding a maintainer's agent PR), the stage record's
// own clock moves a head nobody updated back to the 085 hand-off after the
// 072 dwell spent at 072; on this approved, green head the human route
// parks it at 085 with today's entry comment naming the update.
func TestForkWorkflowUpdateAgentPRFallsBackTo085AfterDwell(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy func() StageLabelPolicy
		nudged bool
	}{
		{name: "adoption off", nudged: true, policy: func() StageLabelPolicy {
			p := withholdPolicy()
			p.Adoption = false
			return p
		}},
		{name: "nudge off", policy: func() StageLabelPolicy {
			p := withholdPolicy()
			p.ContributorNudge = false
			return p
		}},
		{name: "maintainer_policy never", nudged: true, policy: func() StageLabelPolicy {
			p := maintainerPolicyWith(config.AdoptionMaintainerPolicyNever)
			p.NudgeWithholdWhenAdopting = true
			return p
		}},
		{name: "maintainer_policy ask", nudged: true, policy: func() StageLabelPolicy {
			p := maintainerPolicyWith(config.AdoptionMaintainerPolicyAsk)
			p.NudgeWithholdWhenAdopting = true
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, tc.policy())
			defer done()
			h.sink.presence = &ContributorPresence{Registered: true, Online: false}
			h.adopt.permissions = map[string]string{"mallory": "admin"}
			h.adopt.allowMaintainerEdits(7, true)
			h.adopt.compareFiles = []string{forkUpdateWorkflowFile}
			h.e.checkSummary = greenChecks
			var logs bytes.Buffer
			h.e.logger = slog.New(slog.NewTextHandler(&logs, nil))
			key := stageKey("acme", "widget", 7)
			returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			stageIs := func(want stagelabels.Stage, when string) stagelabels.Record {
				t.Helper()
				rec, _ := h.e.stageStoreOrShared().Get(key)
				if rec.Stage != want {
					t.Fatalf("%s: stage %s, want %s (reason %q)", when, rec.Stage, want, rec.Reason)
				}
				return rec
			}

			observeFWU(h, forkPR(7, "aaa"), fwuApprove)
			stageIs(stagelabels.ReturnedToContributor, "refused update")
			if tc.nudged {
				assertForkWorkflowUpdateNudgePosted(t, h)
			} else if len(nudgeComments(h.api)) != 0 {
				t.Fatalf("nudged with the nudge off: %v", h.api.comments)
			}
			h.clock.advance(30 * time.Minute)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			if rec := stageIs(stagelabels.ReturnedToContributor, "after the grace"); rec.ForkRehomeRefused("aaa") && rec.NudgeHead == "aaa" {
				t.Fatalf("fork escalation armed; this case must exercise the fallback: %+v", rec)
			}
			h.clock.advance(23 * time.Hour)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			stageIs(stagelabels.ReturnedToContributor, "before the dwell")

			h.api.reset()
			h.clock.advance(30 * time.Minute)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			if rec := stageIs(stagelabels.NeedsDecision, "past the dwell"); rec.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("reason %q, want %q", rec.Reason, autoMergeReasonForkWorkflowUpdate)
			}
			var entries []string
			for _, c := range h.api.comments {
				if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) {
					entries = append(entries, c)
				}
			}
			if len(entries) != 1 || !strings.Contains(entries[0], decisionCause(autoMergeReasonForkWorkflowUpdate)) {
				t.Fatalf("085 entry comment = %q (all comments %v)", entries, h.api.comments)
			}
			if len(stalledComments(h.api)) != 0 || len(adoptionComments(h.api)) != 0 {
				t.Fatalf("fallback posted an adoption note: %v", h.api.comments)
			}

			// It holds at 085 on the same head with no second entry comment.
			h.api.reset()
			h.clock.advance(time.Hour)
			observeFWU(h, forkPR(7, "aaa", "stage/085-needs-decision", "role/contributor", "author/clanker"), fwuApprove)
			stageIs(stagelabels.NeedsDecision, "next pass")
			for _, c := range h.api.comments {
				if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) {
					t.Fatalf("entry comment repeated:\n%s", c)
				}
			}
			if n := strings.Count(logs.String(), "still owes the branch update past the 072 dwell"); n != 1 {
				t.Fatalf("lapse logged %d times, want once (on the move off 072):\n%s", n, logs.String())
			}

			// The relay pushes a new head: it is the agent's again, on a
			// fresh clock.
			h.clock.advance(time.Minute)
			observeFWU(h, forkPR(7, "bbb", "stage/085-needs-decision", "role/contributor", "author/clanker"), fwuApprove)
			stageIs(stagelabels.ReturnedToContributor, "new head")
		})
	}
}

// The owed clock counts time spent at 072 for this reason on this head. A
// head that left 072 (here a requires_human verdict parked it at 085 for
// days) starts afresh when it comes back, so it does not lapse at once; a
// pass that lost the head's verdict (a split drain) keeps the clock.
func TestForkWorkflowUpdateOwedClockCountsTimeAt072(t *testing.T) {
	p := withholdPolicy()
	p.Adoption = false
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.e.checkSummary = greenChecks
	key := stageKey("acme", "widget", 7)
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	parked := []string{"stage/085-needs-decision", "role/contributor", "author/clanker"}
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	at := func(want stagelabels.Stage, when string) stagelabels.Record {
		t.Helper()
		rec, _ := h.e.stageStoreOrShared().Get(key)
		if rec.Stage != want {
			t.Fatalf("%s: stage %s, want %s (reason %q)", when, rec.Stage, want, rec.Reason)
		}
		return rec
	}

	start := h.clock.now()
	observeFWU(h, forkPR(7, "aaa"), fwuApprove)
	if rec := at(stagelabels.ReturnedToContributor, "first refusal"); !rec.AuthorUpdateOwedSince("aaa").Equal(start) {
		t.Fatalf("owed clock %v, want %v", rec.AuthorUpdateOwedSince("aaa"), start)
	}
	// A pass that lost the verdict (a split drain) keeps 072 and the clock.
	h.clock.advance(time.Hour)
	observeFWU(h, forkPR(7, "aaa", returned...), ContributorLaneVerdict{})
	if rec := at(stagelabels.ReturnedToContributor, "verdict lost"); !rec.AuthorUpdateOwedSince("aaa").Equal(start) {
		t.Fatalf("a pass with no verdict moved the clock: %v", rec.AuthorUpdateOwedSince("aaa"))
	}
	h.clock.advance(time.Minute)
	observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
	if rec := at(stagelabels.ReturnedToContributor, "verdict back"); !rec.AuthorUpdateOwedSince("aaa").Equal(start) {
		t.Fatalf("clock %v, want it kept at %v", rec.AuthorUpdateOwedSince("aaa"), start)
	}

	// The reviewer asks for a person: the head leaves 072 for days.
	h.clock.advance(time.Hour)
	observeFWU(h, forkPR(7, "aaa", returned...), requiresHuman)
	if rec := at(stagelabels.NeedsDecision, "requires_human"); !rec.AuthorUpdateOwedSince("aaa").IsZero() {
		t.Fatalf("clock kept after leaving 072: %v", rec.AuthorUpdateOwedSince("aaa"))
	}
	h.clock.advance(72 * time.Hour)
	observeFWU(h, forkPR(7, "aaa", parked...), requiresHuman)
	at(stagelabels.NeedsDecision, "days later")

	// Approved again: back at 072 on a fresh clock, not lapsed at once.
	h.clock.advance(time.Minute)
	back := h.clock.now()
	observeFWU(h, forkPR(7, "aaa", parked...), fwuApprove)
	if rec := at(stagelabels.ReturnedToContributor, "back at 072"); !rec.AuthorUpdateOwedSince("aaa").Equal(back) {
		t.Fatalf("owed clock %v, want a fresh %v", rec.AuthorUpdateOwedSince("aaa"), back)
	}
	h.clock.advance(23 * time.Hour)
	observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
	at(stagelabels.ReturnedToContributor, "23h after the return")
	h.clock.advance(time.Hour)
	observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
	if rec := at(stagelabels.NeedsDecision, "24h after the return"); rec.Reason != autoMergeReasonForkWorkflowUpdate {
		t.Fatalf("reason %q, want %q", rec.Reason, autoMergeReasonForkWorkflowUpdate)
	}
}

// A split drain hands the stage pass an agent's head without its verdict.
// When the record already has this head at 072 for the update, the pass
// keeps it there (no label write, no entry comment) instead of flipping it
// 072, 085, 072. A new head, or a red check, takes the hand-off as before.
func TestForkWorkflowUpdateLostVerdictKeeps072OnTheSameHead(t *testing.T) {
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	redChecks := func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{Total: 3, Green: 2, Red: []string{"unit"}, DCO: 1}, nil
	}
	for _, tc := range []struct {
		name    string
		head    string
		red     bool
		keep072 bool
	}{
		{name: "same head", head: "aaa", keep072: true},
		{name: "new head", head: "bbb"},
		{name: "red check", head: "aaa", red: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, withholdPolicy())
			defer done()
			h.sink.presence = &ContributorPresence{Registered: true, Online: true}
			h.e.checkSummary = greenChecks
			key := stageKey("acme", "widget", 7)
			observeFWU(h, forkPR(7, "aaa"), fwuApprove)
			if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("approved, green: %+v, want 072", rec)
			}
			h.api.reset()
			h.clock.advance(3 * time.Minute)
			if tc.red {
				h.e.checkSummary = redChecks
				h.clock.advance(humanChecksTTL)
			}
			observeFWU(h, forkPR(7, tc.head, returned...), ContributorLaneVerdict{})
			rec, _ := h.e.stageStoreOrShared().Get(key)
			for _, c := range h.api.comments {
				if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) && tc.keep072 {
					t.Fatalf("entry comment on a kept 072:\n%s", c)
				}
			}
			if !tc.keep072 {
				if rec.Stage == stagelabels.ReturnedToContributor && rec.Reason == autoMergeReasonForkWorkflowUpdate {
					t.Fatalf("lost verdict kept the agent route: %+v", rec)
				}
				return
			}
			if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("lost verdict: %+v, want 072 kept", rec)
			}
			if len(h.api.added) != 0 || len(h.api.removed) != 0 {
				t.Fatalf("lost verdict wrote labels: added=%v removed=%v", h.api.added, h.api.removed)
			}
			h.clock.advance(3 * time.Minute)
			observeFWU(h, forkPR(7, "aaa", returned...), fwuApprove)
			if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("verdict back: %+v, want 072", rec)
			}
			if len(h.api.added) != 0 || len(h.api.removed) != 0 {
				t.Fatalf("verdict back wrote labels: added=%v removed=%v", h.api.added, h.api.removed)
			}
		})
	}
}

// Without an approve verdict (or with a red check) an agent's
// fork-workflow-update keeps today's hand-off: no 072, no nudge.
func TestForkWorkflowUpdateAgentPRNotApprovedKeepsHandOff(t *testing.T) {
	for name, setup := range map[string]func(h *adoptHarness) ContributorLaneVerdict{
		"no verdict": func(h *adoptHarness) ContributorLaneVerdict {
			h.e.checkSummary = greenChecks
			return ContributorLaneVerdict{}
		},
		"red check": func(h *adoptHarness) ContributorLaneVerdict {
			h.e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
				return hgithub.CommitChecksSummary{Total: 3, Green: 2, Red: []string{"unit"}, DCO: 1}, nil
			}
			return fwuApprove
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, withholdPolicy())
			defer done()
			observeFWU(h, forkPR(7, "aaa"), setup(h))
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if rec.Stage == stagelabels.ReturnedToContributor && rec.Reason == autoMergeReasonForkWorkflowUpdate {
				t.Fatalf("routed to the agent: %+v", rec)
			}
			if !rec.AuthorUpdateOwedSince("aaa").IsZero() {
				t.Fatalf("owed clock started: %+v", rec)
			}
			for _, n := range h.sink.nudges {
				if slices.Contains(n.ReviewAsks, forkWorkflowUpdateAsk) {
					t.Fatalf("nudged for the branch update: %+v", n)
				}
			}
		})
	}
}

// A sentinel alert on an agent's approved, green fork-workflow-update head
// is a person's to see: the PR keeps the 085 park and is not nudged.
func TestForkWorkflowUpdateAgentPRWithSentinelStaysAt085(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, withholdPolicy())
	defer done()
	h.e.checkSummary = greenChecks
	observeFWU(h, forkPR(7, "aaa", defaultSentinelLabel), fwuApprove)
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("stage %s (reason %q), want 085", rec.Stage, rec.Reason)
	}
	if len(nudgeComments(h.api)) != 0 || len(h.sink.nudges) != 0 {
		t.Fatalf("nudged past a sentinel alert: %v", h.api.comments)
	}
}

// A person's fork PR keeps today's 085 park: the entry comment tells the
// author, or a maintainer when edits are allowed, to run gh pr
// update-branch or merge the base in. Nobody is nudged.
func TestForkWorkflowUpdatePersonPRStaysAt085(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	pr := stagePR(13, "role/contributor", "author/human")
	pr.User = &gh.User{Login: gh.Ptr("alice")}
	pr.Body = gh.Ptr("Fixes the widget by hand.\n")
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("alice/widget")}
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, autoMergeReasonForkWorkflowUpdate, false, nil)
	e.noteStageVerdict("acme", "widget", 13, *verdict)
	e.noteStageRole("acme", "widget", 13, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stageOf(t, store, 13); got != stagelabels.NeedsDecision {
		t.Fatalf("stage %v, want 085", got)
	}
	if rec, _ := store.Get(stageKey("acme", "widget", 13)); rec.Reason != autoMergeReasonForkWorkflowUpdate {
		t.Fatalf("recorded reason %q, want %q", rec.Reason, autoMergeReasonForkWorkflowUpdate)
	}
	if len(nudgeComments(api)) != 0 {
		t.Fatalf("a person's PR was nudged: %v", api.comments)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.DecisionCommentMarker) || !strings.Contains(api.comments[0], decisionCause(autoMergeReasonForkWorkflowUpdate)) {
		t.Fatalf("085 entry comment = %q", api.comments)
	}
}
