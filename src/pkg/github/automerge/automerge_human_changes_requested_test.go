package automerge

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
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
// own PR head sends it to fixing (reason human-changes-requested, a
// stageFixingReasons member) and records one request for the planner.
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
			if !stageFixingReasons[reason] {
				t.Fatalf("%q is not a fixing reason; the PR would not reach 070", reason)
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
