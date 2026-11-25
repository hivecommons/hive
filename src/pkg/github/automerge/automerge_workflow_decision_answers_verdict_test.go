package automerge

import (
	"context"
	"strconv"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

// Danathar/zfs-aurora-complex#254, 2026-10-31: the owner's own PR edits a
// workflow file, the swarm returned requires_human with a question for the
// owner, and the 085 comment named `/hive decision approve` as releasing
// the fence and settling the verdict. The owner typed it on head d2ff9f4;
// the fence released ("merges this PR itself once the usual gates … are
// clear") and the lane then skipped the PR 163 times as
// contributor-lane:no-verdict, because nothing recorded the comment as the
// answer to the reviewer's question. GitHub refuses the Approve review the
// owner would otherwise give on their own PR.
const workflowAskOwner = "danathar"

func workflowAskVerdict(string, int, string) ContributorLaneVerdict {
	return ContributorLaneVerdict{
		Configured: true, Verdict: "requires_human", DecisionRequested: true,
		DecisionQuestion: "@Danathar should decide whether to merge this tightening of the triage job's `if:`",
		DecisionFinding:  "the reviewer marked this change high-risk",
	}
}

// workflowAskFixture is #254's shape: owner-authored, a workflow file in the
// diff, green, and a typed `/hive decision approve` posted after the head.
func workflowAskFixture(comments ...map[string]any) contributorLaneFixture {
	fx := validContributorFixture()
	fx.author = workflowAskOwner
	fx.issueComments[0]["body"] = contributorClaimMarker(workflowAskOwner, "contributor", contributorLaneTestHive)
	fx.files = workflowPathFiles()
	fx.headCommittedAt = "2026-10-31T14:00:00Z"
	fx.prComments = comments
	return fx
}

func typedDecisionApprove(id int, login, at string) map[string]any {
	return map[string]any{"id": id, "user": map[string]string{"login": login}, "body": "/hive decision approve", "created_at": at, "updated_at": at, "html_url": "https://github.com/acme/widget/pull/7#issuecomment-" + strconv.Itoa(id)}
}

// editedInto is comment after someone rewrote its body at editedAt: GitHub
// keeps created_at and moves updated_at.
func editedInto(comment map[string]any, editedAt string) map[string]any {
	comment["updated_at"] = editedAt
	return comment
}

// stageHumanApprover is what the stage engine sees as the trusted approver
// for the PR after one pass of the lane.
func stageHumanApprover(engine *Engine) (string, string) {
	engine.stageTracker.mu.Lock()
	defer engine.stageTracker.mu.Unlock()
	d := engine.stageTracker.details[stageKey("acme", "widget", 7)]
	return d.HumanApprover, d.HumanApprovalHead
}

func runWorkflowAsk(t *testing.T, fx *contributorLaneFixture, store *review.HumanChangeRequests, verdict ContributorLaneVerdictFunc) (string, *Engine) {
	t.Helper()
	api := newContributorLaneAPI(t, fx)
	t.Cleanup(api.Close)
	policy := defaultContributorLanePolicy()
	policy.OwnerApprovalReleasesWorkflowFence = true
	clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-31T15:30:00Z")}
	opts := Options{ContributorSoak: soakedTracker(clock), ContributorLaneVerdict: verdict}
	if store != nil {
		opts.HumanChangeRequests = store
	}
	engine := newContributorLaneEngine(api.URL, policy, clock, opts)
	engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, workflowAskOwner) })
	engine.stageTracker = NewStageTracker()
	engine.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	return reason, engine
}

func workflowApprovalAudit(t *testing.T, fx *contributorLaneFixture) string {
	t.Helper()
	for _, body := range fx.allComments {
		if strings.HasPrefix(body, workflowApprovalMarkerPrefix) {
			return body
		}
	}
	t.Fatalf("no workflow-approval comment among %v", fx.allComments)
	return ""
}

func TestTypedDecisionApproveAnswersTheRequiresHumanVerdictOnAWorkflowPR(t *testing.T) {
	fx := workflowAskFixture(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"))
	store := review.NewHumanChangeRequests("")
	reason, engine := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
	if reason != "" || fx.merges != 1 {
		t.Fatalf("reason=%q merges=%d, want the merge the owner approved (comments=%v)", reason, fx.merges, fx.allComments)
	}
	req, ok := review.DecisionAnswerFor(store, "acme/widget", 7, "sha7")
	if !ok || !req.IsApproval() || !strings.EqualFold(req.Actor, workflowAskOwner) || req.SourceID != 5 || req.Source != review.HumanChangeRequestSourceDecision {
		t.Fatalf("recorded = %+v, %v; want the owner's typed approval (comment 5) on sha7", req, ok)
	}
	// Ordered by when the owner typed it, not by when the lane saw it, so a
	// decision recorded in between keeps its place as the newer one.
	if want := mustParseTime(t, "2026-10-31T15:06:57Z"); !req.RecordedAt.Equal(want) {
		t.Fatalf("recorded at %s, want the comment's time %s", req.RecordedAt, want)
	}
	// The record is what every later pass and the stage engine read: the
	// question stays answered on this head after a restart.
	if v := engine.contributorLaneVerdictFor("acme/widget", 7, "sha7"); !v.DecisionAnswered || !strings.EqualFold(v.DecisionAnsweredBy, workflowAskOwner) {
		t.Fatalf("verdict after the record = %+v, want answered by the owner", v)
	}
	audit := workflowApprovalAudit(t, &fx)
	if !strings.Contains(audit, "answers the review swarm's `requires_human` question on this head") || strings.Contains(audit, "review verdict) are clear") {
		t.Fatalf("audit comment = %q, want it to say the approval answered the verdict", audit)
	}
}

// Without a decision store the approval cannot be recorded: the fence still
// releases, and the same reply names what answers the verdict instead of
// promising a merge.
func TestTypedDecisionApproveWithoutAStoreSaysWhatStillHoldsThePR(t *testing.T) {
	fx := workflowAskFixture(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"))
	reason, _ := runWorkflowAsk(t, &fx, nil, workflowAskVerdict)
	if reason != contributorLaneReasonNoVerdict || fx.merges != 0 {
		t.Fatalf("reason=%q merges=%d, want no-verdict and no merge", reason, fx.merges)
	}
	audit := workflowApprovalAudit(t, &fx)
	for _, want := range []string{"`requires_human` verdict on this head still holds the PR", "`/hive decision approve`", "`/hive approve` reply posted after the verdict"} {
		if !strings.Contains(audit, want) {
			t.Fatalf("audit comment missing %q:\n%s", want, audit)
		}
	}
}

// A person's later decision on the head stands: an earlier typed approval
// is not recorded over it.
func TestTypedDecisionApproveNeverOverwritesALaterDecision(t *testing.T) {
	fx := workflowAskFixture(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"))
	store := review.NewHumanChangeRequests("")
	later := review.HumanChangeRequest{
		Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 9,
		Actor: workflowAskOwner, Body: "drop the second condition", Kind: review.HumanChangeRequestKindChanges,
		RecordedAt: mustParseTime(t, "2026-10-31T15:20:00Z"),
	}
	if err := store.Record(later); err != nil {
		t.Fatal(err)
	}
	reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
	if fx.merges != 0 || reason == "" {
		t.Fatalf("reason=%q merges=%d, want the later instruction to hold the PR", reason, fx.merges)
	}
	if got, _ := store.For("acme/widget", 7, "sha7"); got.SourceID != 9 || got.IsApproval() {
		t.Fatalf("newest decision = %+v, want the later instruction kept", got)
	}
}

// What stays as it was: a verdict that asks for nobody records nothing and
// keeps the old reply; a GitHub Approve review already answers the verdict
// and records nothing new; a typed approval older than the head releases
// neither gate.
func TestTypedDecisionApproveRecordsOnlyWhenItAnswersAnOpenQuestion(t *testing.T) {
	t.Run("verdict approve", func(t *testing.T) {
		fx := workflowAskFixture(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"))
		store := review.NewHumanChangeRequests("")
		reason, _ := runWorkflowAsk(t, &fx, store, approvedVerdict)
		if reason != "" || fx.merges != 1 || store.Len() != 0 {
			t.Fatalf("reason=%q merges=%d records=%d, want a merge and no record", reason, fx.merges, store.Len())
		}
		if audit := workflowApprovalAudit(t, &fx); !strings.Contains(audit, "(CI, soak, holds, review verdict) are clear") {
			t.Fatalf("audit comment = %q, want the unchanged wording", audit)
		}
	})
	t.Run("approve review by another trusted merger", func(t *testing.T) {
		fx := workflowAskFixture()
		fx.reviews = []map[string]any{ownerApprovedReview("danathar", "sha7")}
		store := review.NewHumanChangeRequests("")
		reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
		if reason != "" || fx.merges != 1 {
			t.Fatalf("reason=%q merges=%d, want the review to answer both gates", reason, fx.merges)
		}
		if req, ok := store.For("acme/widget", 7, "sha7"); ok && req.Source == review.HumanChangeRequestSourceDecision {
			t.Fatalf("a review must not be recorded as a typed decision: %+v", req)
		}
		if audit := workflowApprovalAudit(t, &fx); !strings.Contains(audit, "answers the review swarm's `requires_human` question") {
			t.Fatalf("audit comment = %q, want it to say the verdict is answered", audit)
		}
	})
	t.Run("typed approve older than the head", func(t *testing.T) {
		fx := workflowAskFixture(typedDecisionApprove(5, "Danathar", "2026-10-31T13:00:00Z"))
		store := review.NewHumanChangeRequests("")
		reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
		if reason != contributorLaneReasonWorkflowPath || fx.merges != 0 || store.Len() != 0 {
			t.Fatalf("reason=%q merges=%d records=%d, want the fence to hold and nothing recorded", reason, fx.merges, store.Len())
		}
	})
	t.Run("typed approve by someone who is not a trusted merger", func(t *testing.T) {
		fx := workflowAskFixture(typedDecisionApprove(5, "passerby", "2026-10-31T15:06:57Z"))
		store := review.NewHumanChangeRequests("")
		reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
		if reason != contributorLaneReasonWorkflowPath || store.Len() != 0 {
			t.Fatalf("reason=%q records=%d, want the fence to hold and nothing recorded", reason, store.Len())
		}
	})
}

// hivecommons/hive#16121: the same typed approval on an owner's PR that
// touches no workflow file. No fence trips, so the fence release never read
// the comment, and the un-park sweep that also records it reads only
// stage-labelled PRs: the lane reads it for the verdict itself.
func TestTypedDecisionApproveAnswersTheRequiresHumanVerdictOnANonFencedPR(t *testing.T) {
	nonFenced := func(comments ...map[string]any) contributorLaneFixture {
		fx := workflowAskFixture(comments...)
		fx.files = nil
		return fx
	}
	t.Run("trusted merger after the head", func(t *testing.T) {
		fx := nonFenced(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"))
		store := review.NewHumanChangeRequests("")
		reason, engine := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
		if reason != "" || fx.merges != 1 {
			t.Fatalf("reason=%q merges=%d, want the merge the owner approved (comments=%v)", reason, fx.merges, fx.allComments)
		}
		req, ok := review.DecisionAnswerFor(store, "acme/widget", 7, "sha7")
		if !ok || !req.IsApproval() || !strings.EqualFold(req.Actor, workflowAskOwner) || req.SourceID != 5 || req.Source != review.HumanChangeRequestSourceDecision {
			t.Fatalf("recorded = %+v, %v; want the owner's typed approval (comment 5) on sha7", req, ok)
		}
		if want := mustParseTime(t, "2026-10-31T15:06:57Z"); !req.RecordedAt.Equal(want) {
			t.Fatalf("recorded at %s, want the comment's time %s", req.RecordedAt, want)
		}
		if v := engine.contributorLaneVerdictFor("acme/widget", 7, "sha7"); !v.DecisionAnswered || !strings.EqualFold(v.DecisionAnsweredBy, workflowAskOwner) {
			t.Fatalf("verdict after the record = %+v, want answered by the owner", v)
		}
		// noteStageVerdict ran before the record existed; the stage engine
		// must still see the approver on this pass, not the next sweep's.
		if login, head := stageHumanApprover(engine); !strings.EqualFold(login, workflowAskOwner) || head != "sha7" {
			t.Fatalf("stage approver = (%q, %q), want the owner on sha7 this tick", login, head)
		}
		for _, body := range fx.allComments {
			if strings.HasPrefix(body, workflowApprovalMarkerPrefix) {
				t.Fatalf("no fence tripped, yet a workflow-approval comment was posted: %q", body)
			}
		}
	})
	t.Run("a later decision stands", func(t *testing.T) {
		fx := nonFenced(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"))
		store := review.NewHumanChangeRequests("")
		later := review.HumanChangeRequest{
			Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 9,
			Actor: workflowAskOwner, Body: "drop the second condition", Kind: review.HumanChangeRequestKindChanges,
			RecordedAt: mustParseTime(t, "2026-10-31T15:20:00Z"),
		}
		if err := store.Record(later); err != nil {
			t.Fatal(err)
		}
		reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
		if fx.merges != 0 || reason == "" {
			t.Fatalf("reason=%q merges=%d, want the later instruction to hold the PR", reason, fx.merges)
		}
		if got, _ := store.For("acme/widget", 7, "sha7"); got.SourceID != 9 || got.IsApproval() {
			t.Fatalf("newest decision = %+v, want the later instruction kept", got)
		}
	})
	for _, tc := range []struct {
		name    string
		comment map[string]any
		verdict ContributorLaneVerdictFunc
		reason  string
	}{
		{name: "typed approve older than the head", comment: typedDecisionApprove(5, "Danathar", "2026-10-31T13:00:00Z"), verdict: workflowAskVerdict, reason: contributorLaneReasonNoVerdict},
		{name: "typed approve by someone who is not a trusted merger", comment: typedDecisionApprove(5, "passerby", "2026-10-31T15:06:57Z"), verdict: workflowAskVerdict, reason: contributorLaneReasonNoVerdict},
		// A collaborator with write access can edit the merger's benign
		// post-head comment into the command; an edit is not an instruction.
		{name: "typed approve edited into an existing comment", comment: editedInto(typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"), "2026-10-31T15:10:00Z"), verdict: workflowAskVerdict, reason: contributorLaneReasonNoVerdict},
		{name: "verdict asks nobody", comment: typedDecisionApprove(5, "Danathar", "2026-10-31T15:06:57Z"), verdict: approvedVerdict, reason: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := nonFenced(tc.comment)
			store := review.NewHumanChangeRequests("")
			reason, _ := runWorkflowAsk(t, &fx, store, tc.verdict)
			if reason != tc.reason || store.Len() != 0 {
				t.Fatalf("reason=%q records=%d, want %q and nothing recorded", reason, store.Len(), tc.reason)
			}
		})
	}
}

// typedDecisionApproveWith is typedDecisionApprove with its own body, for the
// `/hive decision approve <why>` form the PR reply options teach.
func typedDecisionApproveWith(id int, login, at, body string) map[string]any {
	comment := typedDecisionApprove(id, login, at)
	comment["body"] = body
	return comment
}

// The live shape behind this fix: a trusted merger typed the approval with
// the reason on the command line. The un-park sweep recorded it ("Approval
// recorded by ... for head 27c88f5"), but the fence and the lane's own read
// wanted the command alone on the first line, so the workflow-path PR sat
// at 090 with no audit comment. It now releases the fence, answers the
// verdict, and records the decision text the sweep records for the comment.
func TestTypedDecisionApproveWithTheReasonOnTheSameLine(t *testing.T) {
	const body = "/hive decision approve The hive's review of 27c88f5b found it correct.\n\nThe new job only reads."
	const recorded = "approve The hive's review of 27c88f5b found it correct.\n\nThe new job only reads."
	nonFenced := func(comments ...map[string]any) contributorLaneFixture {
		fx := workflowAskFixture(comments...)
		fx.files = nil
		return fx
	}
	for _, tc := range []struct {
		name    string
		fixture func(...map[string]any) contributorLaneFixture
		fenced  bool
	}{
		{name: "workflow-path PR", fixture: workflowAskFixture, fenced: true},
		{name: "PR no fence trips", fixture: nonFenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fixture(typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", body))
			store := review.NewHumanChangeRequests("")
			reason, engine := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
			if reason != "" || fx.merges != 1 {
				t.Fatalf("reason=%q merges=%d, want the merge the owner approved (comments=%v)", reason, fx.merges, fx.allComments)
			}
			req, ok := review.DecisionAnswerFor(store, "acme/widget", 7, "sha7")
			if !ok || !req.IsApproval() || !strings.EqualFold(req.Actor, workflowAskOwner) || req.SourceID != 5 || req.Source != review.HumanChangeRequestSourceDecision {
				t.Fatalf("recorded = %+v, %v; want the owner's typed approval (comment 5) on sha7", req, ok)
			}
			if req.Body != recorded {
				t.Fatalf("recorded body = %q, want the un-park sweep's decision text %q", req.Body, recorded)
			}
			if v := engine.contributorLaneVerdictFor("acme/widget", 7, "sha7"); !v.DecisionAnswered || !strings.EqualFold(v.DecisionAnsweredBy, workflowAskOwner) {
				t.Fatalf("verdict after the record = %+v, want answered by the owner", v)
			}
			audited := false
			for _, c := range fx.allComments {
				if strings.HasPrefix(c, workflowApprovalMarkerPrefix) {
					audited = true
				}
			}
			if audited != tc.fenced {
				t.Fatalf("workflow-approval comment posted = %v, want %v (comments=%v)", audited, tc.fenced, fx.allComments)
			}
		})
	}
}

// The un-park sweep may have recorded the same comment first. The lane then
// reads that record as the person's answer (the verdict is already answered,
// so it never reaches recordTypedApprovalDecision), merges, and leaves the
// record as the sweep wrote it. TestRecordTypedApprovalDecisionKeepsTheSweepsRecord
// covers the record path itself.
func TestTypedDecisionApproveWithAReasonMergesOnTheSweepsRecord(t *testing.T) {
	const body = "/hive decision approve the pin bump is intended"
	for _, tc := range []struct {
		name  string
		files bool
	}{
		{name: "workflow-path PR", files: true},
		{name: "PR no fence trips"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := workflowAskFixture(typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", body))
			if !tc.files {
				fx.files = nil
			}
			store := review.NewHumanChangeRequests("")
			swept := review.HumanChangeRequest{
				Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 5,
				Actor: workflowAskOwner, Body: "approve the pin bump is intended", Kind: review.HumanChangeRequestKindApprove,
				URL:        "https://github.com/acme/widget/pull/7#issuecomment-5",
				RecordedAt: mustParseTime(t, "2026-10-31T15:10:00Z"),
			}
			if err := store.Record(swept); err != nil {
				t.Fatal(err)
			}
			reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
			if reason != "" || fx.merges != 1 {
				t.Fatalf("reason=%q merges=%d, want the merge the owner approved (comments=%v)", reason, fx.merges, fx.allComments)
			}
			got, ok := store.For("acme/widget", 7, "sha7")
			if !ok || store.Len() != 1 || got.SourceID != swept.SourceID || got.Body != swept.Body || got.Kind != swept.Kind || !got.RecordedAt.Equal(swept.RecordedAt) {
				t.Fatalf("store = %+v (len %d), want the sweep's record unchanged %+v", got, store.Len(), swept)
			}
		})
	}
}

// countingChangeRequests counts the writes recordTypedApprovalDecision makes.
type countingChangeRequests struct {
	*review.HumanChangeRequests
	records int
}

func (s *countingChangeRequests) Record(req review.HumanChangeRequest) error {
	s.records++
	return s.HumanChangeRequests.Record(req)
}

// recordTypedApprovalDecision on a comment the un-park sweep already
// recorded on the head reports the approval and writes nothing; on a head
// with no record it writes the sweep's own decision text for the comment.
func TestRecordTypedApprovalDecisionKeepsTheSweepsRecord(t *testing.T) {
	typedAt := mustParseTime(t, "2026-10-31T15:06:57Z")
	comment := &gh.IssueComment{
		ID: gh.Ptr(int64(5)), Body: gh.Ptr("/hive decision approve the pin bump is intended"),
		HTMLURL: gh.Ptr("https://github.com/acme/widget/pull/7#issuecomment-5"), CreatedAt: &gh.Timestamp{Time: typedAt},
	}
	approval := workflowApproval{Login: workflowAskOwner, Typed: comment}
	swept := review.HumanChangeRequest{
		Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 5,
		Actor: workflowAskOwner, Body: "approve the pin bump is intended", Kind: review.HumanChangeRequestKindApprove,
		URL:        "https://github.com/acme/widget/pull/7#issuecomment-5",
		RecordedAt: mustParseTime(t, "2026-10-31T15:10:00Z"),
	}
	t.Run("the sweep recorded the comment", func(t *testing.T) {
		store := &countingChangeRequests{HumanChangeRequests: review.NewHumanChangeRequests("")}
		if err := store.HumanChangeRequests.Record(swept); err != nil {
			t.Fatal(err)
		}
		e := &Engine{humanChangeRequests: store}
		if !e.recordTypedApprovalDecision("widget", "acme", "widget", 7, "sha7", approval) {
			t.Fatal("the sweep's approval of the same comment must read as the head's approval")
		}
		got, ok := store.For("acme/widget", 7, "sha7")
		if store.records != 0 || !ok || store.Len() != 1 || got.Body != swept.Body || !got.RecordedAt.Equal(swept.RecordedAt) {
			t.Fatalf("writes=%d record=%+v (len %d), want no write and the sweep's record %+v", store.records, got, store.Len(), swept)
		}
	})
	t.Run("no record yet", func(t *testing.T) {
		store := &countingChangeRequests{HumanChangeRequests: review.NewHumanChangeRequests("")}
		e := &Engine{humanChangeRequests: store}
		if !e.recordTypedApprovalDecision("widget", "acme", "widget", 7, "sha7", approval) {
			t.Fatal("a typed approval on a head with no decision must be recorded")
		}
		got, ok := store.For("acme/widget", 7, "sha7")
		if store.records != 1 || !ok || !got.IsApproval() || got.SourceID != 5 || got.Body != swept.Body || !got.RecordedAt.Equal(typedAt) {
			t.Fatalf("writes=%d record=%+v, want one approval of comment 5 with the sweep's body %q at the comment's time", store.records, got, swept.Body)
		}
	})
}

// Only what the un-park sweep records as an approval, spelled `approve`,
// answers the verdict on a PR no fence trips; nothing is recorded otherwise.
func TestTypedDecisionApproveLookalikesAnswerNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		comment map[string]any
	}{
		{name: "approved", comment: typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", "/hive decision approved")},
		{name: "approvefoo", comment: typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", "/hive decision approvefoo")},
		{name: "another decision", comment: typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", "/hive decision reject x")},
		{name: "quoted", comment: typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", "> /hive decision approve the pin bump")},
		{name: "fenced", comment: typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", "```\n/hive decision approve the pin bump\n```")},
		{name: "with a reason, older than the head", comment: typedDecisionApproveWith(5, "Danathar", "2026-10-31T13:00:00Z", "/hive decision approve the pin bump")},
		{name: "with a reason, edited", comment: editedInto(typedDecisionApproveWith(5, "Danathar", "2026-10-31T15:06:57Z", "/hive decision approve the pin bump"), "2026-10-31T15:10:00Z")},
		{name: "with a reason, untrusted", comment: typedDecisionApproveWith(5, "passerby", "2026-10-31T15:06:57Z", "/hive decision approve the pin bump")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := workflowAskFixture(tc.comment)
			fx.files = nil
			store := review.NewHumanChangeRequests("")
			reason, _ := runWorkflowAsk(t, &fx, store, workflowAskVerdict)
			if reason != contributorLaneReasonNoVerdict || fx.merges != 0 || store.Len() != 0 {
				t.Fatalf("reason=%q merges=%d records=%d, want %q, no merge and nothing recorded", reason, fx.merges, store.Len(), contributorLaneReasonNoVerdict)
			}
		})
	}
}
