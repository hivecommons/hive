package holdguard

import (
	"path/filepath"
	"strings"
	"testing"
)

// An accepted merge-from-base is awaited for its own head and base only,
// expires after UpdateConfirmWindow, and is forgotten once the head moves.
// Settling leaves the baseline alone: the lift check judges the new head.
func TestUpdateRequestIsPendingUntilTheHeadMoves(t *testing.T) {
	s, clock := newTestStore(t)

	s.MarkUpdateRequested("r", 1, "h1", "b1") // untracked: no-op
	if pending, _ := s.UpdatePending("r", 1, "h1", "b1"); pending {
		t.Fatal("an untracked PR has no awaited update")
	}

	s.Snapshot("r", 1, "h1", nil)
	s.MarkUpdateRequested("r", 1, "", "b1") // empty head: no-op
	if pending, _ := s.UpdatePending("r", 1, "", "b1"); pending {
		t.Fatal("an empty head has no awaited update")
	}

	s.MarkUpdateRequested("r", 1, "h1", "b1")
	if pending, expired := s.UpdatePending("r", 1, "h1", "b1"); !pending || expired {
		t.Fatalf("UpdatePending right after the request = (%v, %v), want (true, false)", pending, expired)
	}
	for _, other := range [][2]string{{"h1", "b2"}, {"h2", "b1"}} {
		if pending, _ := s.UpdatePending("r", 1, other[0], other[1]); pending {
			t.Fatalf("UpdatePending(%s, %s) = true; the request was for h1 on b1", other[0], other[1])
		}
	}

	*clock = clock.Add(UpdateConfirmWindow)
	if pending, expired := s.UpdatePending("r", 1, "h1", "b1"); !pending || !expired {
		t.Fatalf("UpdatePending after UpdateConfirmWindow = (%v, %v), want (true, true)", pending, expired)
	}

	if s.SettleUpdateRequest("r", 1, "h1") || s.SettleUpdateRequest("r", 1, "") || s.SettleUpdateRequest("r", 2, "h2") {
		t.Fatal("SettleUpdateRequest settled although the head has not moved, is empty, or the PR is untracked")
	}
	if !s.SettleUpdateRequest("r", 1, "h2") {
		t.Fatal("SettleUpdateRequest must settle once the head moved off the requested head")
	}
	if pending, _ := s.UpdatePending("r", 1, "h1", "b1"); pending {
		t.Fatal("a settled request is still pending")
	}
	if s.SettleUpdateRequest("r", 1, "h3") {
		t.Fatal("a request settles once")
	}
	if rec, _ := s.Recorded("r", 1); rec.HeadSHA != "h1" {
		t.Fatalf("settling moved the baseline to %q; it must stay at h1", rec.HeadSHA)
	}
}

func TestAdvanceHygieneForgetsTheAwaitedUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	s.Snapshot("r", 1, "h1", nil)
	s.MarkUpdateRequested("r", 1, "h1", "b1")
	s.AdvanceHygiene("r", 1, "m1", []Commit{{SHA: "m1", Author: "hive"}})
	if pending, _ := s.UpdatePending("r", 1, "h1", "b1"); pending {
		t.Fatal("AdvanceHygiene must clear the awaited update it confirmed")
	}
}

// A stop holds for one head on one base and survives a restart; a record
// from a build that only knew the workflows refusal stops on its head alone.
func TestUpdateStoppedForOneHeadAndBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hold-guard.json")
	s := Load(path)

	s.MarkUpdateStopped("r", 1, "h1", "b1", UpdateStopNotLanded) // untracked: no-op
	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("MarkUpdateStopped must not create an entry for an untracked PR")
	}

	s.Snapshot("r", 1, "h1", nil)
	s.MarkUpdateRequested("r", 1, "h1", "b1")
	s.MarkUpdateStopped("r", 1, "h1", "b1", UpdateStopNotLanded)
	s.MarkUpdateStopped("r", 1, "", "b1", UpdateStopConflict) // empty head: no-op
	if pending, _ := s.UpdatePending("r", 1, "h1", "b1"); pending {
		t.Fatal("a stopped update is no longer awaited")
	}

	rec, _ := Load(path).Recorded("r", 1)
	if reason, ok := rec.UpdateStoppedFor("h1", "b1"); !ok || reason != UpdateStopNotLanded {
		t.Fatalf("UpdateStoppedFor(h1, b1) after reload = (%q, %v), want (%q, true)", reason, ok, UpdateStopNotLanded)
	}
	for _, other := range [][2]string{{"h1", "b2"}, {"h2", "b1"}, {"", "b1"}} {
		if _, ok := rec.UpdateStoppedFor(other[0], other[1]); ok {
			t.Fatalf("UpdateStoppedFor(%q, %q) = true; the stop was for h1 on b1", other[0], other[1])
		}
	}

	legacy := Entry{WorkflowRefusedHeadSHA: "h3"}
	if reason, ok := legacy.UpdateStoppedFor("h3", ""); !ok || reason != UpdateStopWorkflowPermission {
		t.Fatalf("legacy workflows refusal without a base = (%q, %v), want (%q, true)", reason, ok, UpdateStopWorkflowPermission)
	}
	if _, ok := legacy.UpdateStoppedFor("h3", "any-base"); ok {
		t.Fatal("a legacy record has no base, so a known base must re-evaluate rather than stop")
	}
}

func TestUpdateStopNotedIsPerHeadAndReason(t *testing.T) {
	s, _ := newTestStore(t)
	s.MarkUpdateStopNoted("r", 1, "h1", UpdateStopForkNotEditable) // untracked: no-op
	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("MarkUpdateStopNoted must not create an entry for an untracked PR")
	}

	s.Snapshot("r", 1, "h1", nil)
	s.MarkUpdateStopNoted("r", 1, "h1", UpdateStopForkNotEditable)
	rec, _ := s.Recorded("r", 1)
	if !rec.UpdateStopNoted("h1", UpdateStopForkNotEditable) {
		t.Fatal("the note posted for h1 must read as noted")
	}
	if rec.UpdateStopNoted("h1", UpdateStopNotLanded) || rec.UpdateStopNoted("h2", UpdateStopForkNotEditable) {
		t.Fatal("a note covers its own head and reason only")
	}
	if (Entry{}).UpdateStopNoted("", "") {
		t.Fatal("an empty head is never noted")
	}
}

func TestUpdateStoppedCommentBodySaysWhyAndWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   []string
	}{
		{UpdateStopWorkflowPermission, []string{"`.github/workflows/`", "use **Update branch**"}},
		{UpdateStopForkNotEditable, []string{"\"Allow edits by maintainers\" is off", "ask the author to merge the base branch in"}},
		{UpdateStopForkWorkflowUpdate, []string{"`.github/workflows/`", "the branch is in a fork", "ask the author to merge the base branch in"}},
		{UpdateStopNotLanded, []string{"had not moved 30 minutes later", "use **Update branch**"}},
		{UpdateStopUpToDate, []string{"GitHub did not complete the update.", "use **Update branch**"}},
	} {
		body := UpdateStoppedCommentBody(tc.reason)
		want := append([]string{"Hive could not update this branch", "will not try again until this branch or its base branch changes"}, tc.want...)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("UpdateStoppedCommentBody(%q) lacks %q:\n%s", tc.reason, w, body)
			}
		}
	}
}

// A fork head is out of the App's reach whatever it is granted, so the fork
// note never sends a person after the workflows permission
// (projectbluefin/utah#1001).
func TestUpdateStoppedCommentBodyForkWorkflowNamesNoGrant(t *testing.T) {
	if body := UpdateStoppedCommentBody(UpdateStopForkWorkflowUpdate); strings.Contains(body, "permission") {
		t.Fatalf("fork workflow note mentions a permission:\n%s", body)
	}
}
