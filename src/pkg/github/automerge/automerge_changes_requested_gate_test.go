package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewgate"
)

// The GitHub fakes of this package's older tests predate the open
// change-request gate and do not all serve the review listing, so the
// package's tests run with a gate that sees no change requests; the gate's
// own tests below restore the real check.
func init() {
	changesRequestedCheck = func(context.Context, *gh.Client, string, string, int) ([]reviewgate.Block, error) { return nil, nil }
}

func realChangesRequestedGate(t *testing.T) {
	t.Helper()
	prev := changesRequestedCheck
	changesRequestedCheck = reviewgate.CheckGitHub
	t.Cleanup(func() { changesRequestedCheck = prev })
}

// TestSweepDoesNotMergeOverOpenChangeRequest pins hivecommons/hive#20067:
// a green PR whose reviewer with write access has CHANGES_REQUESTED as their
// latest review is not merged, whatever lane reached the merge, and it is
// released by that reviewer's approval or a dismissal. "maint" is not a
// configured trusted merger: write access alone is what counts. The change
// request sits on an older head, which GitHub still treats as open.
func TestSweepDoesNotMergeOverOpenChangeRequest(t *testing.T) {
	realChangesRequestedGate(t)
	writer := map[string]string{"maint": "write"}
	for _, tt := range []struct {
		name        string
		reviews     []map[string]any
		permissions map[string]string
		wantMerge   bool
	}{
		{
			name:        "open change request from a writer blocks the merge",
			reviews:     []map[string]any{humanReview(42, "maint", "CHANGES_REQUESTED", "sha6", "not yet")},
			permissions: writer,
		},
		{
			name:        "a later comment does not clear it",
			reviews:     []map[string]any{humanReview(42, "maint", "CHANGES_REQUESTED", "sha6", "not yet"), laterReview(43, "maint", "COMMENTED")},
			permissions: writer,
		},
		{
			name:        "approval from the same reviewer releases it",
			reviews:     []map[string]any{humanReview(42, "maint", "CHANGES_REQUESTED", "sha6", "not yet"), laterReview(43, "maint", "APPROVED")},
			permissions: writer,
			wantMerge:   true,
		},
		{
			name:        "a dismissed review releases it",
			reviews:     []map[string]any{humanReview(42, "maint", "DISMISSED", "sha6", "")},
			permissions: writer,
			wantMerge:   true,
		},
		{
			name:        "a change request from a non-writer does not block",
			reviews:     []map[string]any{humanReview(42, "passerby", "CHANGES_REQUESTED", "sha6", "nit")},
			permissions: map[string]string{"passerby": "read"},
			wantMerge:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mergeCalls := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, reviews: tt.reviews, permissions: tt.permissions, mergeCalls: &mergeCalls})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantMerge {
				if event.MergeSHA != "merge7" || mergeCalls != 1 {
					t.Fatalf("event = %+v, reason = %q, merge calls = %d; want the PR merged", event, reason, mergeCalls)
				}
				return
			}
			if mergeCalls != 0 || event.MergeSHA != "" {
				t.Fatalf("merge calls = %d, event = %+v; want no merge over an open change request", mergeCalls, event)
			}
			if reason != autoMergeReasonHumanChangesRequested {
				t.Fatalf("reason = %q, want %q", reason, autoMergeReasonHumanChangesRequested)
			}
		})
	}
}

// TestExecuteMergeEffectRefusesOpenChangeRequest pins the shared gate at the
// engine's one merge primitive: the merge function never runs, and the
// refusal maps onto the human-changes-requested skip reason.
func TestExecuteMergeEffectRefusesOpenChangeRequest(t *testing.T) {
	realChangesRequestedGate(t)
	api := newSelfSweepGuardAPI(t, selfSweepFixture{
		reviews:     []map[string]any{humanReview(42, "maint", "CHANGES_REQUESTED", "sha7", "")},
		permissions: map[string]string{"maint": "admin"},
	})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	ran := false
	_, err := c.executeMergeEffect(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestMerge, Target: "7"}, "acme", "widget", 7, "sha7",
		func(context.Context) (effects.Result, error) {
			ran = true
			return effects.Result{Provenance: "merge7"}, nil
		})
	if ran {
		t.Fatal("merge ran over an open change request")
	}
	var blocked *reviewgate.BlockedError
	if !errors.As(err, &blocked) || len(blocked.Blocks) != 1 || blocked.Blocks[0].ReviewID != 42 {
		t.Fatalf("err = %v, want a BlockedError naming review 42", err)
	}
	if reason, ok := changesRequestedSkipReason(err); !ok || reason != autoMergeReasonHumanChangesRequested {
		t.Fatalf("skip reason = %q, %v", reason, ok)
	}
}

// TestChangesRequestedOverrideReleasesEngineMerge pins the operator path: an
// override recorded for exactly the open change request lets the merge run.
func TestChangesRequestedOverrideReleasesEngineMerge(t *testing.T) {
	realChangesRequestedGate(t)
	api := newSelfSweepGuardAPI(t, selfSweepFixture{
		reviews:     []map[string]any{humanReview(42, "maint", "CHANGES_REQUESTED", "sha7", "")},
		permissions: map[string]string{"maint": "write"},
	})
	defer api.Close()
	t.Cleanup(func() { reviewgate.ClearOverride("acme/widget", 7) })
	if _, err := reviewgate.RecordOverride("acme/widget", 7, "owner", "verified out of band", []reviewgate.Block{{Reviewer: "maint", ReviewID: 42}}); err != nil {
		t.Fatal(err)
	}
	c := newAutoMergeSweepClient(api.URL)
	if err := c.gateChangesRequested(context.Background(), "acme", "widget", 7, "merge"); err != nil {
		t.Fatalf("gate with override = %v, want nil", err)
	}
}

func TestChangesRequestedSkipReasonUnreadable(t *testing.T) {
	err := &reviewgate.BlockedError{Repo: "acme/widget", Number: 7, Cause: errors.New("503")}
	if reason, ok := changesRequestedSkipReason(err); !ok || reason != autoMergeReasonChangesRequestedUnreadable {
		t.Fatalf("skip reason = %q, %v", reason, ok)
	}
	if _, ok := changesRequestedSkipReason(errors.New("other")); ok {
		t.Fatal("unrelated error mapped to a change-request skip reason")
	}
	if !strings.Contains(err.Error(), "could not read reviews") {
		t.Fatalf("error = %q", err)
	}
}

// laterReview is a review submitted after humanReview's fixed time.
func laterReview(id int64, login, state string) map[string]any {
	rv := humanReview(id, login, state, "sha7", "")
	rv["submitted_at"] = "2026-10-09T18:00:00Z"
	return rv
}

// TestQueuedLaneStopsOnSentinelAlert: a sentinel alert that lands after a
// merger queued the PR stops the merge-train lane like a hold does
// (hivecommons/hive#20067, after #20064 merged with sentinel-alert on).
func TestQueuedLaneStopsOnSentinelAlert(t *testing.T) {
	merges := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open", "mergeable_state": "clean", "mergeable": true,
				"user":   map[string]string{"login": "alice"},
				"head":   map[string]string{"sha": "sha7"},
				"base":   map[string]string{"ref": "main", "sha": "base1"},
				"labels": []map[string]string{{"name": hgithub.AutoMergeQueuedLabel}, {"name": "sentinel-alert"}},
			})
		case r.Method == http.MethodPut:
			merges++
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err != nil || reason != "sentinel" {
		t.Fatalf("trySweepQueuedPR reason=%q err=%v, want sentinel", reason, err)
	}
	if merges != 0 {
		t.Fatalf("merges = %d, want none", merges)
	}
}
