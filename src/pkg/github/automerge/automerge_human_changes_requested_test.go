package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func humanReview(id int64, login, state, commit, body string) map[string]any {
	return map[string]any{
		"id":           id,
		"state":        state,
		"commit_id":    commit,
		"body":         body,
		"html_url":     "https://example.invalid/review/" + login,
		"submitted_at": "2026-10-09T17:29:00Z",
		"user":         map[string]any{"login": login},
	}
}

// TestTrySweepSelfAuthoredPRHonoursHumanChangesRequested pins
// hivecommons/hive#13041: a trusted human's changes_requested on the App's
// own PR head sends it to fixing (reason human-changes-requested) and
// records one request for the planner.
func TestTrySweepSelfAuthoredPRHonoursHumanChangesRequested(t *testing.T) {
	const body = "Please (1) gate it behind the flag and (2) drop the retry loop."
	tests := []struct {
		name       string
		reviews    []map[string]any
		wantReason string
		wantRecord bool
	}{
		{
			name:       "trusted human changes_requested on head parks at fixing and records the request",
			reviews:    []map[string]any{humanReview(1, "bob", "CHANGES_REQUESTED", "sha7", body)},
			wantReason: autoMergeReasonHumanChangesRequested,
			wantRecord: true,
		},
		{
			name:    "dismissed review does not count",
			reviews: []map[string]any{humanReview(1, "bob", "CHANGES_REQUESTED", "sha7", body), humanReview(2, "bob", "DISMISSED", "sha7", "")},
		},
		{
			name:    "later approval by the same reviewer clears it",
			reviews: []map[string]any{humanReview(1, "bob", "CHANGES_REQUESTED", "sha7", body), humanReview(2, "bob", "APPROVED", "sha7", "")},
		},
		{
			name:    "untrusted reviewer is ignored",
			reviews: []map[string]any{humanReview(1, "mallory", "CHANGES_REQUESTED", "sha7", body)},
		},
		{
			name:    "bot reviewer is ignored",
			reviews: []map[string]any{humanReview(1, "scanner[bot]", "CHANGES_REQUESTED", "sha7", body)},
		},
		{
			name:    "hive app review is ignored",
			reviews: []map[string]any{humanReview(1, testHiveAppBotLogin, "CHANGES_REQUESTED", "sha7", body)},
		},
		{
			name:    "changes_requested on an older head is outdated",
			reviews: []map[string]any{humanReview(1, "bob", "CHANGES_REQUESTED", "sha6", body)},
		},
		{
			name:    "comment-only review is not a change request",
			reviews: []map[string]any{humanReview(1, "bob", "COMMENTED", "sha7", body)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, reviews: tt.reviews})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			store := review.NewHumanChangeRequests("")
			c.humanChangeRequests = store

			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			req, recorded := store.For("acme/widget", 7, "sha7")
			if recorded != tt.wantRecord {
				t.Fatalf("recorded = %v (%+v), want %v", recorded, req, tt.wantRecord)
			}
			if !tt.wantRecord {
				if event.MergeSHA != "merge7" {
					t.Fatalf("event = %+v, want the green PR merged when no human change request stands", event)
				}
				return
			}
			if event.MergeSHA != "" {
				t.Fatalf("event = %+v, want no merge over a maintainer's changes_requested", event)
			}
			if req.Actor != "bob" || req.Body != body || req.Source != review.HumanChangeRequestSourceReview || req.SourceID != 1 {
				t.Fatalf("recorded request = %+v", req)
			}
			if got := classifyStage(stageObservation{Lane: stageLaneSelf, Reason: reason}); got.Stage != stagelabels.Fixing {
				t.Fatalf("%q routes to %v, want fixing (070)", reason, got.Stage)
			}
			if _, asks, _ := nudgeDiagnosis(reason, stageDetail{}); len(asks) != 1 || !strings.Contains(asks[0], "maintainer requested changes") {
				t.Fatalf("nudge prose for %q = %q", reason, asks)
			}

			// Second sweep, same head and review: no duplicate record, same reason.
			_, reason2, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil || reason2 != tt.wantReason {
				t.Fatalf("second sweep = (%q, %v)", reason2, err)
			}
			if n := store.Len(); n != 1 {
				t.Fatalf("store entries after second sweep = %d, want 1", n)
			}
		})
	}
}

// TestTrySweepSelfAuthoredPRHumanChangesRequestedWithoutStore pins the
// degraded mode: no store wired still means no merge over the review.
func TestTrySweepSelfAuthoredPRHumanChangesRequestedWithoutStore(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, reviews: []map[string]any{humanReview(1, "bob", "CHANGES_REQUESTED", "sha7", "nope")}})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err != nil || reason != autoMergeReasonHumanChangesRequested || event.MergeSHA != "" {
		t.Fatalf("got (%+v, %q, %v)", event, reason, err)
	}
}

// TestTrySweepSelfAuthoredPRHonoursRecordedDecision pins the text path of
// hivecommons/hive#13041: a `/hive decision` the un-park sweep recorded for
// this head (no changes_requested review exists) is read by the lane as
// changes requested — same reason, same 070 route, no merge — and a stale
// decision on an older head is not.
func TestTrySweepSelfAuthoredPRHonoursRecordedDecision(t *testing.T) {
	for _, tt := range []struct {
		name       string
		head       string
		wantReason string
	}{
		{name: "decision on the current head parks at fixing", head: "sha7", wantReason: autoMergeReasonHumanChangesRequested},
		{name: "decision on an older head is superseded", head: "sha6"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			store := review.NewHumanChangeRequests("")
			c.humanChangeRequests = store
			if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: tt.head, Source: review.HumanChangeRequestSourceDecision, SourceID: 99, Actor: "bob", Body: "drop the retry loop"}); err != nil {
				t.Fatal(err)
			}
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if tt.wantReason == "" {
				if event.MergeSHA != "merge7" {
					t.Fatalf("event = %+v, want merge when the decision is on an older head", event)
				}
				return
			}
			if event.MergeSHA != "" {
				t.Fatalf("event = %+v, want no merge over a recorded /hive decision", event)
			}
			if n := store.Len(); n != 1 {
				t.Fatalf("store entries = %d, want the decision untouched", n)
			}
		})
	}
}

// hivecommons/hive#14679: a recorded `/hive decision` on the current head
// is noted on the stage detail as the standing human change requester, so
// the stage derives fixing over the swarm's decision-requested verdict on
// that head; a bare approval recorded there is not a change request and
// does not hold the lane.
func TestTrySweepSelfAuthoredPRRecordedDecisionNotesStageAndApprovalReleases(t *testing.T) {
	for _, tt := range []struct {
		name          string
		kind          string
		wantReason    string
		wantRequester string
	}{
		{name: "instruction holds the lane and notes the requester", kind: review.HumanChangeRequestKindChanges, wantReason: autoMergeReasonHumanChangesRequested, wantRequester: "bob"},
		{name: "bare approval releases the lane", kind: review.HumanChangeRequestKindApprove},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			c.stageTracker = NewStageTracker()
			c.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }
			store := review.NewHumanChangeRequests("")
			c.humanChangeRequests = store
			if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 99, Actor: "bob", Body: "approve", Kind: tt.kind}); err != nil {
				t.Fatal(err)
			}
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			c.stageTracker.mu.Lock()
			detail := c.stageTracker.details[stageKey("acme", "widget", 7)]
			c.stageTracker.mu.Unlock()
			if detail.HumanChangeRequester != tt.wantRequester || (tt.wantRequester != "" && detail.HumanChangeRequestHead != "sha7") {
				t.Fatalf("stage detail = %+v, want requester %q on sha7", detail, tt.wantRequester)
			}
			if tt.wantReason == "" && event.MergeSHA != "merge7" {
				t.Fatalf("event = %+v, want the merge to proceed over a bare approval", event)
			}
			if tt.wantReason != "" {
				o := stageObservation{Lane: stageLaneSelf, Reason: reason, HeadSHA: "sha7", Detail: detail}
				o.Detail.Verdict, o.Detail.VerdictKnown = ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}, true
				if v := classifyStage(o); v.Stage != stagelabels.Fixing || v.Reason != autoMergeReasonHumanChangesRequested {
					t.Fatalf("stage with the decision-requested verdict = %+v, want 070 on the recorded decision", v)
				}
			}
		})
	}
}

func TestHumanChangeRequestClearedByLaterApprovalWithoutNewHead(t *testing.T) {
	for _, tc := range []struct {
		name, login, head, state, submitted string
		wantHeld                            bool
	}{
		{"later trusted approval", "bob", "sha7", "APPROVED", "2026-10-28T02:00:00Z", false},
		{"old approval", "bob", "sha7", "APPROVED", "2026-10-28T00:00:00Z", true},
		{"approval on another head", "bob", "sha6", "APPROVED", "2026-10-28T02:00:00Z", true},
		{"untrusted approval", "mallory", "sha7", "APPROVED", "2026-10-28T02:00:00Z", true},
		{"still requesting changes", "bob", "sha7", "CHANGES_REQUESTED", "2026-10-28T02:00:00Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv := humanReview(2, tc.login, tc.state, tc.head, "metadata fix checked")
			rv["submitted_at"] = tc.submitted
			api := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: []map[string]any{rv}})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			store := review.NewHumanChangeRequests("")
			c.humanChangeRequests = store
			at, err := time.Parse(time.RFC3339, "2026-10-28T01:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 99, Actor: "bob", Body: "edit body", RecordedAt: at}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if held := c.humanChangesRequestedForHead(context.Background(), "acme", "widget", 7, "sha7"); held != tc.wantHeld {
					t.Fatalf("held=%v, want %v", held, tc.wantHeld)
				}
				if scheduled := review.HumanChangeRequestFor(store, "acme/widget", 7, "sha7") != nil; scheduled != tc.wantHeld {
					t.Fatalf("planner still schedules=%v, want %v", scheduled, tc.wantHeld)
				}
			}
		})
	}
}

func TestTrustedHumanApprovalUsesLatestOpinion(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: []map[string]any{
		humanReview(1, "bob", "APPROVED", "sha7", ""),
		humanReview(2, "bob", "CHANGES_REQUESTED", "sha7", "please fix"),
	}})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	if login, ok := c.latestTrustedHumanApproval(context.Background(), "acme", "widget", 7, "sha7"); ok {
		t.Fatalf("obsolete approval from %s accepted", login)
	}
}

// A same-head APPROVED review persisted as an approval-kind decision must
// not outlive the review: once it is dismissed (or the reviewer's latest
// opinion on the head is no longer APPROVED), the record is dropped, the
// earlier instruction stands again and requires_human is unanswered. A
// comment-sourced approval is durable and is not revalidated.
func TestReviewBackedApprovalRevokedOnDismissal(t *testing.T) {
	for _, tc := range []struct {
		name, revoke, wantBody string
	}{
		{"dismissed", "DISMISSED", "edit body"},
		// The fresh change request is now the newest instruction.
		{"changes requested again", "CHANGES_REQUESTED", "metadata fix checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approval := humanReview(2, "bob", "APPROVED", "sha7", "metadata fix checked")
			approval["submitted_at"] = "2026-10-28T02:00:00Z"
			api := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: []map[string]any{approval}})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			store := review.NewHumanChangeRequests("")
			c.humanChangeRequests = store
			at, err := time.Parse(time.RFC3339, "2026-10-28T01:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 99, Actor: "bob", Body: "edit body", RecordedAt: at}); err != nil {
				t.Fatal(err)
			}
			if c.humanChangesRequestedForHead(context.Background(), "acme", "widget", 7, "sha7") {
				t.Fatal("approval on the same head did not release the request")
			}
			if login, ok := c.trustedHumanApprovalForRequiresHuman(context.Background(), "acme", "widget", 7, "sha7", ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}); !ok || login != "bob" {
				t.Fatalf("approval not seen: %q %v", login, ok)
			}

			// The reviewer takes the approval back without a new head.
			approval["state"] = tc.revoke
			approval["submitted_at"] = "2026-10-28T03:00:00Z"
			if login, ok := c.trustedHumanApprovalForRequiresHuman(context.Background(), "acme", "widget", 7, "sha7", ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}); ok {
				t.Fatalf("revoked review approval still answers requires_human as %q", login)
			}
			if !c.humanChangesRequestedForHead(context.Background(), "acme", "widget", 7, "sha7") {
				t.Fatal("instruction recorded before the revoked approval no longer holds the lane")
			}
			req := review.HumanChangeRequestFor(store, "acme/widget", 7, "sha7")
			if req == nil || req.Body != tc.wantBody {
				t.Fatalf("planner instruction = %+v, want %q", req, tc.wantBody)
			}
			if got, ok := store.Lookup("acme/widget", 7, review.HumanChangeRequestSourceReview, 2); ok && got.IsApproval() {
				t.Fatal("revoked review approval still persisted")
			}
		})
	}
}

func TestCommentApprovalSurvivesReviewDismissal(t *testing.T) {
	dismissed := humanReview(2, "bob", "DISMISSED", "sha7", "")
	api := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: []map[string]any{dismissed}})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	store := review.NewHumanChangeRequests("")
	c.humanChangeRequests = store
	if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Source: review.HumanChangeRequestSourceDecision, SourceID: 4242, Actor: "bob", Body: "approve", Kind: review.HumanChangeRequestKindApprove}); err != nil {
		t.Fatal(err)
	}
	if login, ok := c.trustedHumanApprovalForRequiresHuman(context.Background(), "acme", "widget", 7, "sha7", ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}); !ok || login != "bob" {
		t.Fatalf("durable /hive decision approve dropped: %q %v", login, ok)
	}
	if c.humanChangesRequestedForHead(context.Background(), "acme", "widget", 7, "sha7") {
		t.Fatal("durable approval read as a change request")
	}
}
