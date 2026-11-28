package reviewgate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

// writers answers WriterFunc from a fixed set; anyone else is not a writer.
func writers(logins ...string) WriterFunc {
	set := map[string]bool{}
	for _, l := range logins {
		set[strings.ToLower(l)] = true
	}
	return func(_ context.Context, login string) (bool, error) { return set[strings.ToLower(login)], nil }
}

func TestEvaluateOpenChangeRequestFromWriterBlocks(t *testing.T) {
	reviews := []Review{
		{ID: 1, Reviewer: "maint", State: "APPROVED", CommitID: "a", SubmittedAt: at(0)},
		{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", CommitID: "a", SubmittedAt: at(5)},
	}
	blocks := Evaluate(context.Background(), reviews, writers("maint"))
	if len(blocks) != 1 || blocks[0].Reviewer != "maint" || blocks[0].ReviewID != 2 {
		t.Fatalf("blocks = %+v, want maint's review 2", blocks)
	}
}

func TestEvaluateApprovalFromSameReviewerReleases(t *testing.T) {
	reviews := []Review{
		{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", CommitID: "a", SubmittedAt: at(0)},
		{ID: 3, Reviewer: "Maint", State: "APPROVED", CommitID: "b", SubmittedAt: at(10)},
	}
	if blocks := Evaluate(context.Background(), reviews, writers("maint")); len(blocks) != 0 {
		t.Fatalf("blocks = %+v, want none after the same reviewer approved", blocks)
	}
}

func TestEvaluateApprovalFromAnotherReviewerDoesNotRelease(t *testing.T) {
	reviews := []Review{
		{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", SubmittedAt: at(0)},
		{ID: 3, Reviewer: "other", State: "APPROVED", SubmittedAt: at(10)},
	}
	if blocks := Evaluate(context.Background(), reviews, writers("maint", "other")); len(blocks) != 1 {
		t.Fatalf("blocks = %+v, want maint's change request to stand", blocks)
	}
}

func TestEvaluateDismissedReviewReleases(t *testing.T) {
	for name, rv := range map[string]Review{
		// GitHub reports a dismissal in the state.
		"github state": {ID: 2, Reviewer: "maint", State: "DISMISSED", SubmittedAt: at(0)},
		// Forgejo / Gitea keep the state and set dismissed.
		"forgejo flag": {ID: 2, Reviewer: "maint", State: "REQUEST_CHANGES", Dismissed: true, SubmittedAt: at(0)},
	} {
		t.Run(name, func(t *testing.T) {
			if blocks := Evaluate(context.Background(), []Review{rv}, writers("maint")); len(blocks) != 0 {
				t.Fatalf("blocks = %+v, want none for a dismissed review", blocks)
			}
		})
	}
}

func TestEvaluateChangeRequestFromNonWriterDoesNotBlock(t *testing.T) {
	reviews := []Review{{ID: 2, Reviewer: "drive-by", State: "CHANGES_REQUESTED", SubmittedAt: at(0)}}
	if blocks := Evaluate(context.Background(), reviews, writers("maint")); len(blocks) != 0 {
		t.Fatalf("blocks = %+v, want none from a reviewer without write access", blocks)
	}
}

func TestEvaluateCommentDoesNotClearChangeRequest(t *testing.T) {
	reviews := []Review{
		{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", SubmittedAt: at(0)},
		{ID: 3, Reviewer: "maint", State: "COMMENTED", SubmittedAt: at(5)},
		{ID: 4, Reviewer: "maint", State: "PENDING", SubmittedAt: at(6)},
	}
	if blocks := Evaluate(context.Background(), reviews, writers("maint")); len(blocks) != 1 {
		t.Fatalf("blocks = %+v, want the change request to stand after a comment", blocks)
	}
}

func TestEvaluateChangeRequestOnOlderHeadStillBlocks(t *testing.T) {
	// GitHub never marks a change request stale on a push.
	reviews := []Review{{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", CommitID: "old", SubmittedAt: at(0)}}
	blocks := Evaluate(context.Background(), reviews, writers("maint"))
	if len(blocks) != 1 || blocks[0].CommitID != "old" {
		t.Fatalf("blocks = %+v, want the change request on the older head", blocks)
	}
}

func TestEvaluateForgejoSpelling(t *testing.T) {
	reviews := []Review{{ID: 9, Reviewer: "maint", State: "REQUEST_CHANGES", SubmittedAt: at(0)}}
	if blocks := Evaluate(context.Background(), reviews, writers("maint")); len(blocks) != 1 {
		t.Fatalf("blocks = %+v, want REQUEST_CHANGES to block", blocks)
	}
}

func TestEvaluateOrderIndependent(t *testing.T) {
	// Listed newest first: the later approval still wins.
	reviews := []Review{
		{ID: 3, Reviewer: "maint", State: "APPROVED", SubmittedAt: at(10)},
		{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", SubmittedAt: at(0)},
	}
	if blocks := Evaluate(context.Background(), reviews, writers("maint")); len(blocks) != 0 {
		t.Fatalf("blocks = %+v, want none", blocks)
	}
}

func TestEvaluatePermissionErrorFailsClosed(t *testing.T) {
	reviews := []Review{{ID: 2, Reviewer: "maint", State: "CHANGES_REQUESTED", SubmittedAt: at(0)}}
	failing := func(context.Context, string) (bool, error) { return false, errors.New("boom") }
	blocks := Evaluate(context.Background(), reviews, failing)
	if len(blocks) != 1 || !blocks[0].PermissionUnverified {
		t.Fatalf("blocks = %+v, want an unverified block", blocks)
	}
	if blocks := Evaluate(context.Background(), reviews, nil); len(blocks) != 1 {
		t.Fatalf("nil WriterFunc: blocks = %+v, want a block", blocks)
	}
}

func TestBlockedErrorMatches(t *testing.T) {
	err := error(&BlockedError{Repo: "o/r", Number: 7, Blocks: []Block{{Reviewer: "maint", ReviewID: 2}}})
	if !errors.Is(err, ErrChangesRequested) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("errors.Is mismatch for %v", err)
	}
	if !strings.Contains(err.Error(), "maint (review 2)") {
		t.Fatalf("error %q does not name the reviewer and review", err)
	}
	unreadable := error(&BlockedError{Repo: "o/r", Number: 7, Cause: errors.New("503")})
	if !errors.Is(unreadable, ErrChangesRequested) || !errors.Is(unreadable, ErrUnreadable) {
		t.Fatalf("errors.Is mismatch for %v", unreadable)
	}
}

func TestOverrideCoversExactlyTheChangeRequestsShown(t *testing.T) {
	t.Cleanup(func() { ClearOverride("o/r", 7) })
	shown := []Block{{Reviewer: "maint", ReviewID: 2}}
	if _, err := RecordOverride("o/r", 7, "owner", "hotfix agreed in standup", shown); err != nil {
		t.Fatal(err)
	}
	d := Decide("O/R", 7, shown)
	if !d.Allowed() || !d.Overridden || d.Override.Reason != "hotfix agreed in standup" || d.Override.Actor != "owner" {
		t.Fatalf("decision = %+v, want an override with the actor and reason", d)
	}
	// A new change request is not covered.
	more := append(shown, Block{Reviewer: "other", ReviewID: 5})
	if d := Decide("o/r", 7, more); d.Allowed() {
		t.Fatalf("decision = %+v, want a new change request to block", d)
	}
	// Another PR is not covered.
	if d := Decide("o/r", 8, shown); d.Allowed() {
		t.Fatalf("decision = %+v, want another PR to block", d)
	}
}

func TestOverrideNeedsActorAndReason(t *testing.T) {
	shown := []Block{{Reviewer: "maint", ReviewID: 2}}
	for _, tc := range []struct{ actor, reason string }{{"", "why"}, {"owner", " "}} {
		if _, err := RecordOverride("o/r", 7, tc.actor, tc.reason, shown); !errors.Is(err, ErrOverrideIncomplete) {
			t.Fatalf("RecordOverride(%q, %q) = %v, want ErrOverrideIncomplete", tc.actor, tc.reason, err)
		}
	}
	if _, err := RecordOverride("o/r", 7, "owner", "why", nil); !errors.Is(err, ErrOverrideIncomplete) {
		t.Fatalf("RecordOverride without blocks = %v, want ErrOverrideIncomplete", err)
	}
}

func TestOverrideExpires(t *testing.T) {
	t.Cleanup(func() { ClearOverride("o/r", 7); nowFn = time.Now })
	now := t0
	nowFn = func() time.Time { return now }
	shown := []Block{{Reviewer: "maint", ReviewID: 2}}
	if _, err := RecordOverride("o/r", 7, "owner", "why", shown); err != nil {
		t.Fatal(err)
	}
	now = now.Add(OverrideTTL + time.Minute)
	if d := Decide("o/r", 7, shown); d.Allowed() {
		t.Fatalf("decision = %+v, want an expired override to block", d)
	}
}

func TestDecideWithoutBlocksAllows(t *testing.T) {
	if d := Decide("o/r", 7, nil); !d.Allowed() || d.Overridden {
		t.Fatalf("decision = %+v, want allowed without an override", d)
	}
}

func TestNormalizeState(t *testing.T) {
	for in, want := range map[string]string{
		"REQUEST_CHANGES": StateChangesRequested, "changes_requested": StateChangesRequested,
		"APPROVE": StateApproved, "approved": StateApproved, "DISMISSED": StateDismissed,
		"COMMENT": StateCommented, "commented": StateCommented, " pending ": StatePending,
	} {
		if got := NormalizeState(in, false); got != want {
			t.Errorf("NormalizeState(%q) = %q, want %q", in, got, want)
		}
	}
	if got := NormalizeState("APPROVED", true); got != StateDismissed {
		t.Errorf("dismissed approval = %q, want %q", got, StateDismissed)
	}
}

func TestEvaluateSortsBlocksAndDescribes(t *testing.T) {
	reviews := []Review{
		{ID: 7, Reviewer: "zed", State: "CHANGES_REQUESTED", SubmittedAt: at(1)},
		{ID: 5, Reviewer: "Amy", State: "CHANGES_REQUESTED", SubmittedAt: at(2)},
		{ID: 9, Reviewer: "", State: "CHANGES_REQUESTED", SubmittedAt: at(3)},
	}
	blocks := Evaluate(context.Background(), reviews, writers("zed", "amy"))
	if len(blocks) != 2 || blocks[0].Reviewer != "Amy" || blocks[1].Reviewer != "zed" {
		t.Fatalf("blocks = %+v, want Amy then zed (an anonymous review is ignored)", blocks)
	}
	if got := Describe(blocks); got != "Amy (review 5), zed (review 7)" {
		t.Fatalf("Describe = %q", got)
	}
	if got := Reviewers(blocks); got != "Amy,zed" {
		t.Fatalf("Reviewers = %q", got)
	}
	if got := ReviewIDs(blocks); got != "5,7" {
		t.Fatalf("ReviewIDs = %q", got)
	}
	unverified := Block{Reviewer: "x", ReviewID: 1, PermissionUnverified: true}
	if !strings.Contains(unverified.String(), "access unverified") {
		t.Fatalf("String = %q", unverified.String())
	}
}

func TestBlockedErrorUnwrapAndClear(t *testing.T) {
	cause := errors.New("503")
	err := &BlockedError{Repo: "o/r", Number: 7, Cause: cause}
	if !errors.Is(err, cause) || err.Unwrap() != cause {
		t.Fatal("BlockedError does not unwrap its cause")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("Error = %q", err.Error())
	}
	shown := []Block{{Reviewer: "maint", ReviewID: 2}}
	if _, err := RecordOverride("o/r", 9, "owner", "why", shown); err != nil {
		t.Fatal(err)
	}
	ClearOverride("o/r", 9)
	if _, ok := OverrideFor("o/r", 9, shown); ok {
		t.Fatal("override survived ClearOverride")
	}
	if _, ok := OverrideFor("o/r", 9, nil); ok {
		t.Fatal("OverrideFor with no blocks reported an override")
	}
	if _, err := RecordOverride("", 9, "owner", "why", shown); !errors.Is(err, ErrOverrideIncomplete) {
		t.Fatalf("RecordOverride without a repo = %v", err)
	}
}
