package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// delegatedAsked is the reviewer verdict the delegate answered: the swarm
// handed the head to a person.
var delegatedAsked = ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, HiveAuthoredKnown: true, HiveAuthored: true}

// delegatedDirectionFixingStage is where a standing direction routes a
// contributor's agent-written PR (author/clanker, like the live fork PR):
// ClassifyStage's HumanChangeRequestStage, FixingStage for a non-self lane.
var delegatedDirectionFixingStage = stagelabels.FixingStageFor(false, authorkind.Clanker)

// delegatedDirectionObservations is one stage pass's lanes on such a PR: the
// trusted lane noted nothing, and the contributor lane folded the recorded
// answer into its verdict and noted the requester or approver it read,
// before the stage pass.
func delegatedDirectionObservations(owner, repo string, number int, head string, answer ContributorLaneVerdict, requester, approver string) []stageObservation {
	contributor := stageDetail{VerdictKnown: true, Verdict: answer}
	if requester != "" {
		contributor.HumanChangeRequester, contributor.HumanChangeRequestHead = requester, head
	}
	if approver != "" {
		contributor.HumanApprover, contributor.HumanApprovalHead = approver, head
	}
	return []stageObservation{
		{Lane: stageLaneTrusted, Owner: owner, Repo: repo, Number: number, HeadSHA: head, AuthorKind: authorkind.Clanker, Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true, Verdict: delegatedAsked}},
		{Lane: stageLaneContributor, Owner: owner, Repo: repo, Number: number, HeadSHA: head, AuthorKind: authorkind.Clanker, Reason: contributorLaneReasonNoVerdict, Detail: contributor},
	}
}

func delegatedReview(id int64, login, state, commit, at string) map[string]any {
	rv := humanReview(id, login, state, commit, "")
	rv["submitted_at"] = at
	return rv
}

func recordDirection(t *testing.T, store *review.HumanChangeRequests, repo string, number int, head, actor string, sourceID int64, delegated bool, at time.Time) {
	t.Helper()
	if err := store.Record(review.HumanChangeRequest{
		Repo: repo, Number: number, HeadSHA: head,
		Source: review.HumanChangeRequestSourceDecision, SourceID: sourceID,
		Actor: actor, Body: "Change `Closes #12` to `Refs #12` in the PR body.",
		RecordedAt: at, Delegated: delegated,
	}); err != nil {
		t.Fatal(err)
	}
}

// assertDirectionStands checks that the head still routes to the author's
// fixing stage on the direction (decision-answered: the direction answered
// the requires_human verdict), that every lane names a requester, and that
// the planner still holds the direction with no approval over it.
func assertDirectionStands(t *testing.T, store *review.HumanChangeRequests, list []stageObservation, repo string, number int, head, wantRequester string) {
	t.Helper()
	got, _, obs := resolveStage(list)
	if got.Stage != delegatedDirectionFixingStage || got.Reason != stageReasonDecisionAnswered {
		t.Fatalf("stage=%s reason=%q (lane %s), want %s %q", got.Stage, got.Reason, obs.Lane, delegatedDirectionFixingStage, stageReasonDecisionAnswered)
	}
	named := false
	for _, o := range list {
		if o.Detail.HumanChangeRequester == "" || o.Detail.HumanChangeRequestHead != head {
			t.Fatalf("lane %s lost the requester: %+v", o.Lane, o.Detail)
		}
		named = named || o.Detail.HumanChangeRequester == wantRequester
	}
	if !named {
		t.Fatalf("no lane names %q as requesting changes: %+v", wantRequester, list)
	}
	if req := review.HumanChangeRequestFor(store, repo, number, head); req == nil || req.Source != review.HumanChangeRequestSourceDecision || req.IsApproval() {
		t.Fatalf("the planner lost the direction: %+v", req)
	}
	if a := review.HumanApprovalFor(store, repo, number, head); a != nil {
		t.Fatalf("an approval stands over the direction: %+v", a)
	}
}

// A fork PR the hive adopted sat 2.4 days at 085
// adoption-stalled:human-changes-requested with the dashboard's Approve
// disabled: the operator clicked "Let hive decide", the delegate recorded a
// direction (a PR-body edit, so no commit ever followed), and the next day
// the same operator APPROVED the same head on GitHub. The record carried the
// operator as its actor, so the stage read the hive's own direction as the
// operator's change request (the rule hivecommons/hive#15063 set for the
// decision comment). A trusted approval of the head after a delegated
// direction now supersedes it; everything else still blocks as before.
func TestFoldRecordedDecisionDelegatedDirectionYieldsToLaterApproval(t *testing.T) {
	recorded := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// delegated marks the record as the delegate's; sourceID is the
		// comment a typed `/hive decision <text>` came from (0: dashboard).
		delegated bool
		sourceID  int64
		reviews   []map[string]any
		// laneRequester is who the lane noted as requesting changes on the
		// head before the stage pass (the recorded actor, or a reviewer).
		laneRequester string
		// approver is the superseding review's author; "" when the
		// direction stands, and wantRequester names who blocks.
		approver      string
		wantRequester string
	}{
		{
			name:      "delegated direction, the operator approves later: superseded",
			delegated: true, reviews: []map[string]any{delegatedReview(2, "bob", "APPROVED", "sha7", "2026-10-28T02:00:00Z")},
			laneRequester: "bob", approver: "bob",
		},
		{
			name:      "delegated direction, another trusted merger approves later: superseded",
			delegated: true, reviews: []map[string]any{delegatedReview(2, "carol", "APPROVED", "sha7", "2026-10-28T02:00:00Z")},
			laneRequester: "bob", approver: "carol",
		},
		{
			name:      "delegated direction, approval before it: the direction stands",
			delegated: true, reviews: []map[string]any{delegatedReview(2, "bob", "APPROVED", "sha7", "2026-10-28T00:30:00Z")},
			laneRequester: "bob", wantRequester: "bob",
		},
		{
			name:      "delegated direction with a person's changes_requested on the head: the review still blocks",
			delegated: true,
			reviews: []map[string]any{
				delegatedReview(3, "carol", "CHANGES_REQUESTED", "sha7", "2026-10-28T01:30:00Z"),
				delegatedReview(2, "bob", "APPROVED", "sha7", "2026-10-28T02:00:00Z"),
			},
			laneRequester: "carol", wantRequester: "carol",
		},
		{
			// Pins the narrow scope on this path: the owner has not ruled
			// that a person's approval supersedes their own typed direction.
			name:     "typed /hive decision <text>, then that person approves: unchanged",
			sourceID: 4242, reviews: []map[string]any{delegatedReview(2, "bob", "APPROVED", "sha7", "2026-10-28T02:00:00Z")},
			laneRequester: "bob", wantRequester: "bob",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: tc.reviews})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			store := review.NewHumanChangeRequests("")
			c.humanChangeRequests = store
			recordDirection(t, store, "acme/widget", 7, "sha7", "bob", tc.sourceID, tc.delegated, recorded)
			answered := delegatedAsked.WithDecisionAnswer("bob", "sha7", review.DecisionDirectiveChanges)
			list := delegatedDirectionObservations("acme", "widget", 7, "sha7", answered, tc.laneRequester, "")
			c.foldRecordedDecision(context.Background(), list)
			if tc.approver == "" {
				assertDirectionStands(t, store, list, "acme/widget", 7, "sha7", tc.wantRequester)
				return
			}
			// The approval answers the reviewer's requires_human question
			// and nothing else is known about the head: the lane's own
			// no-verdict reason classifies it, 050 with no stage reason.
			got, _, obs := resolveStage(list)
			if got.Stage != stagelabels.ReviewPending || got.Reason != "" {
				t.Fatalf("stage=%s reason=%q (lane %s), want %s with no reason: a later approval left the delegated direction blocking the head", got.Stage, got.Reason, obs.Lane, stagelabels.ReviewPending)
			}
			for _, o := range list {
				if o.Detail.HumanChangeRequester != "" {
					t.Fatalf("lane %s still names %q as requesting changes", o.Lane, o.Detail.HumanChangeRequester)
				}
				if o.Detail.HumanApprover != tc.approver || o.Detail.HumanApprovalHead != "sha7" {
					t.Fatalf("lane %s does not carry %s's approval: %+v", o.Lane, tc.approver, o.Detail)
				}
				if v := o.Detail.Verdict; !v.DecisionAnswered || v.DecisionDirective != review.DecisionDirectiveApprove || v.DecisionAnsweredBy != tc.approver {
					t.Fatalf("lane %s verdict answer = %+v, want approve by %s", o.Lane, v, tc.approver)
				}
			}
			// Durable for every reader: the planner stops kicking the fixer
			// for the direction, and the next pass reads the approval.
			if req := review.HumanChangeRequestFor(store, "acme/widget", 7, "sha7"); req != nil {
				t.Fatalf("the planner still reads the direction: %+v", req)
			}
			a := review.HumanApprovalFor(store, "acme/widget", 7, "sha7")
			if a == nil || a.Source != review.HumanChangeRequestSourceReview || a.SourceID != 2 || a.Actor != tc.approver || !a.SupersedesDelegated {
				t.Fatalf("approval not recorded over the direction: %+v", a)
			}
		})
	}
}

// supersededDelegatedDirection is pass 1 of the revocation tests: bob's
// approval at 02:00 supersedes the delegated direction recorded at 01:00 on
// sha7. approval is the review the fixture serves; failReviews makes the
// review list answer 500.
func supersededDelegatedDirection(t *testing.T) (c *Engine, store *review.HumanChangeRequests, approval map[string]any, failReviews *atomic.Bool) {
	t.Helper()
	approval = delegatedReview(2, "bob", "APPROVED", "sha7", "2026-10-28T02:00:00Z")
	inner := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: []map[string]any{approval}})
	t.Cleanup(inner.Close)
	failReviews = &atomic.Bool{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failReviews.Load() && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews" {
			http.Error(w, "reviews unavailable", http.StatusInternalServerError)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	c = newAutoMergeSweepClient(api.URL)
	store = review.NewHumanChangeRequests("")
	c.humanChangeRequests = store
	recordDirection(t, store, "acme/widget", 7, "sha7", "bob", 0, true, time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC))
	first := delegatedDirectionObservations("acme", "widget", 7, "sha7", delegatedAsked.WithDecisionAnswer("bob", "sha7", review.DecisionDirectiveChanges), "bob", "")
	c.foldRecordedDecision(context.Background(), first)
	if a := review.HumanApprovalFor(store, "acme/widget", 7, "sha7"); a == nil || !a.SupersedesDelegated || a.SourceID != 2 {
		t.Fatalf("pass 1: approval not recorded over the direction: %+v", a)
	}
	return c, store, approval, failReviews
}

// The approval that superseded a delegated direction binds only while the
// review stands: once its reviewer dismisses it, the next stage pass drops
// the record and the direction blocks the head again, with the lanes' stale
// read of the approval taken off. Either the stage pass notices first, or
// (as in production) a lane's requires_human release reconciles it before
// the pass, after the lane already folded the approval into its verdict.
func TestFoldRecordedDecisionDismissedApprovalRestoresDelegatedDirection(t *testing.T) {
	for _, lanesReconcile := range []bool{false, true} {
		name := map[bool]string{false: "the stage pass reconciles", true: "a lane reconciles first"}[lanesReconcile]
		t.Run(name, func(t *testing.T) {
			c, store, approval, _ := supersededDelegatedDirection(t)
			// bob dismisses the approval; the head does not move.
			approval["state"] = "DISMISSED"
			approval["submitted_at"] = "2026-10-28T03:00:00Z"
			if lanesReconcile {
				// The contributor lane reads its verdict (with the recorded
				// approval folded in) before trustedHumanApprovalForRequiresHuman
				// reconciles the dismissed review away.
				if login, ok := c.trustedHumanApprovalForRequiresHuman(context.Background(), "acme", "widget", 7, "sha7", delegatedAsked); ok {
					t.Fatalf("dismissed approval still releases requires_human as %q", login)
				}
			}
			// The lanes read the approval before it was dropped.
			second := delegatedDirectionObservations("acme", "widget", 7, "sha7", delegatedAsked.WithDecisionAnswer("bob", "sha7", review.DecisionDirectiveApprove), "", "bob")
			c.foldRecordedDecision(context.Background(), second)
			assertDirectionStands(t, store, second, "acme/widget", 7, "sha7", "bob")
			for _, o := range second {
				if o.Detail.HumanApprover != "" {
					t.Fatalf("lane %s still carries the dismissed approval by %q", o.Lane, o.Detail.HumanApprover)
				}
			}
			if got, ok := store.Lookup("acme/widget", 7, review.HumanChangeRequestSourceReview, 2); ok {
				t.Fatalf("dismissed approval still recorded: %+v", got)
			}
		})
	}
}

// A review list GitHub cannot serve (a rate-limit window, an incident) says
// nothing about the approval: the record that superseded the delegated
// direction stays, for the lanes' requires_human release and for the stage
// pass alike, and the head does not fall back to the author's stage.
func TestFoldRecordedDecisionReviewReadFailureKeepsSupersedingApproval(t *testing.T) {
	c, store, _, failReviews := supersededDelegatedDirection(t)
	failReviews.Store(true)
	login, ok := c.trustedHumanApprovalForRequiresHuman(context.Background(), "acme", "widget", 7, "sha7", delegatedAsked)
	if !ok || login != "bob" {
		t.Fatalf("a failed review read revoked the recorded approval: %q %v", login, ok)
	}
	second := delegatedDirectionObservations("acme", "widget", 7, "sha7", delegatedAsked.WithDecisionAnswer("bob", "sha7", review.DecisionDirectiveApprove), "", "bob")
	c.foldRecordedDecision(context.Background(), second)
	got, _, obs := resolveStage(second)
	if got.Stage != stagelabels.ReviewPending || got.Reason != "" {
		t.Fatalf("stage=%s reason=%q (lane %s), want %s with no reason", got.Stage, got.Reason, obs.Lane, stagelabels.ReviewPending)
	}
	for _, o := range second {
		if o.Detail.HumanChangeRequester != "" {
			t.Fatalf("lane %s names %q as requesting changes after a failed read", o.Lane, o.Detail.HumanChangeRequester)
		}
	}
	if a := review.HumanApprovalFor(store, "acme/widget", 7, "sha7"); a == nil || a.SourceID != 2 || !a.SupersedesDelegated {
		t.Fatalf("approval record = %+v, want it kept", a)
	}
	if req := review.HumanChangeRequestFor(store, "acme/widget", 7, "sha7"); req != nil {
		t.Fatalf("the planner reads the direction again: %+v", req)
	}
}

// The record's own review being dismissed in a list GitHub did serve settles
// it, even while another trusted approval's reach to the head cannot be
// read: that other approval can never make the dismissed one live again, so
// the record is dropped and the direction stands.
func TestFoldRecordedDecisionDismissedApprovalDroppedDespiteOtherUnreadReach(t *testing.T) {
	approval := delegatedReview(2, "bob", "APPROVED", "sha7", "2026-10-28T02:00:00Z")
	other := delegatedReview(3, "carol", "COMMENTED", "sha7", "2026-10-28T01:30:00Z")
	inner := newSelfSweepGuardAPI(t, selfSweepFixture{reviews: []map[string]any{approval, other}})
	t.Cleanup(inner.Close)
	failCommit := &atomic.Bool{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failCommit.Load() && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7" {
			http.Error(w, "commit unavailable", http.StatusInternalServerError)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	c := newAutoMergeSweepClient(api.URL)
	store := review.NewHumanChangeRequests("")
	c.humanChangeRequests = store
	recordDirection(t, store, "acme/widget", 7, "sha7", "bob", 0, true, time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC))
	first := delegatedDirectionObservations("acme", "widget", 7, "sha7", delegatedAsked.WithDecisionAnswer("bob", "sha7", review.DecisionDirectiveChanges), "bob", "")
	c.foldRecordedDecision(context.Background(), first)
	if a := review.HumanApprovalFor(store, "acme/widget", 7, "sha7"); a == nil || !a.SupersedesDelegated || a.SourceID != 2 {
		t.Fatalf("pass 1: approval not recorded over the direction: %+v", a)
	}

	// bob dismisses; carol approves, but her approval's reach to the head
	// cannot be read (the head commit read fails).
	approval["state"] = "DISMISSED"
	approval["submitted_at"] = "2026-10-28T03:00:00Z"
	other["state"] = "APPROVED"
	other["submitted_at"] = "2026-10-28T03:30:00Z"
	failCommit.Store(true)
	second := delegatedDirectionObservations("acme", "widget", 7, "sha7", delegatedAsked.WithDecisionAnswer("bob", "sha7", review.DecisionDirectiveApprove), "", "bob")
	c.foldRecordedDecision(context.Background(), second)
	if got, ok := store.Lookup("acme/widget", 7, review.HumanChangeRequestSourceReview, 2); ok {
		t.Fatalf("dismissed approval kept because another approval's reach could not be read: %+v", got)
	}
	assertDirectionStands(t, store, second, "acme/widget", 7, "sha7", "bob")
}

// A person's change request the hive carried over its own base merge stands
// on the new head as theirs, so a later approval does not supersede the
// delegated direction beside it: the head keeps waiting on the author.
func TestFoldRecordedDecisionDelegatedDirectionKeepsCarriedChangeRequest(t *testing.T) {
	const oldHead, newHead, baseTip = "aaaa", "72246f98", "b45e"
	api := &baseRefreshAPI{mergeAuthor: testHiveAppBotLogin, oldHead: oldHead, newHead: newHead, baseTip: baseTip, newFiles: refreshFiles("blob1")}
	inner := api.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/15649/reviews") {
			// clubanderson asked for changes on the old head; carol's later
			// approval of it reaches the new head over the same App merge.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 77, "state": "CHANGES_REQUESTED", "commit_id": oldHead, "body": "please fix the parser",
					"user": map[string]string{"login": "clubanderson", "type": "User"}, "submitted_at": "2026-11-21T02:54:00Z"},
				{"id": 78, "state": "APPROVED", "commit_id": oldHead, "body": "",
					"user": map[string]string{"login": "carol", "type": "User"}, "submitted_at": "2026-11-21T04:00:00Z"},
			})
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer server.Close()
	store := review.NewHumanChangeRequests("")
	client := hgithub.NewClient("token", "hivecommons", []string{"hive"}, nil, server.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), HumanChangeRequests: store,
		MergerAuthorizer: func(login string) bool { return login == "clubanderson" || login == "carol" }})
	recordDirection(t, store, "hivecommons/hive", 15649, newHead, "carol", 0, true, time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC))

	list := delegatedDirectionObservations("hivecommons", "hive", 15649, newHead, delegatedAsked.WithDecisionAnswer("carol", newHead, review.DecisionDirectiveChanges), "carol", "")
	c.foldRecordedDecision(context.Background(), list)
	assertDirectionStands(t, store, list, "hivecommons/hive", 15649, newHead, "carol")
}
