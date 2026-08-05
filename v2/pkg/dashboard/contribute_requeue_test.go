package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #2568 (tractable slice) — operator MANUAL requeue / release of a wedged-but-held
// task. These tests pin the safe slice: the endpoint is owner/read-write ONLY, the
// release reuses the EXISTING #2492/#2557 failure-cooldown path (so a manual requeue
// cannot recreate the duplicate-assignment race), the item returns to the ready
// queue, and no lease/generation-token protocol is introduced.

// TestRequeueHub_ReleasesAndBooksCooldown drives the hub method directly: a
// connection holding a real issue task is released, currentTask is cleared, and the
// SAME short failure cooldown the disconnect/ready-abandon paths book is applied —
// which is exactly what keeps the issue from being instantly re-handed to a stale
// worker (the #2492/#2557 guarantee reused, not reinvented).
func TestRequeueHub_ReleasesAndBooksCooldown(t *testing.T) {
	hub, _ := covK2Hub(t)

	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "wedged", ContributorID: "c-wedged", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 42},
		lastPong:    time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-wedged"] = held
	hub.mu.Unlock()

	// Precondition: the issue is NOT in failure cooldown before the requeue.
	if hub.isTaskInFailureCooldown("myorg/repo1", 42) {
		t.Fatalf("issue unexpectedly in failure cooldown before requeue")
	}

	released := hub.RequeueContributorTask("c-wedged", "wedged: no progress")
	if released != 1 {
		t.Fatalf("expected 1 session released, got %d", released)
	}

	// The held task is cleared — the issue drops out of selectTask's activeIssues
	// guard so it can be re-offered per normal admission.
	held.mu.Lock()
	stillHeld := held.currentTask
	held.mu.Unlock()
	if stillHeld != nil {
		t.Fatalf("currentTask not cleared after requeue: %+v", stillHeld)
	}

	// The SAME short cooldown the auto-release paths book is now in effect, so the
	// issue is not instantly re-admissible (this is what prevents the dup-assign
	// race a naive release would open).
	if !hub.isTaskInFailureCooldown("myorg/repo1", 42) {
		t.Fatalf("requeue did not book the short failure cooldown via the existing path")
	}
}

// TestRequeueHub_NoInFlightTask_NoOp confirms requeuing a contributor with nothing
// in flight releases nothing (and books no cooldown) — the endpoint maps this to a
// 404 "nothing to requeue".
func TestRequeueHub_NoInFlightTask_NoOp(t *testing.T) {
	hub, _ := covK2Hub(t)
	idle := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "idle", ContributorID: "c-idle", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-idle"] = idle
	hub.mu.Unlock()

	if got := hub.RequeueContributorTask("c-idle", ""); got != 0 {
		t.Fatalf("expected 0 released for an idle clanker, got %d", got)
	}
	if got := hub.RequeueContributorTask("c-nobody", ""); got != 0 {
		t.Fatalf("expected 0 released for an unknown id, got %d", got)
	}
}

// TestRequeueHub_ReleasedTaskReturnsToQueue proves the released issue comes BACK to
// the ready queue once its short cooldown lapses — i.e. requeue returns work to the
// pool rather than dropping it. We assert admission via isTaskInFailureCooldown
// flipping back to false after the failure timestamp is aged past the short window
// (selectTask consults exactly this predicate before offering an issue).
func TestRequeueHub_ReleasedTaskReturnsToQueue(t *testing.T) {
	hub, _ := covK2Hub(t)
	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "w2", ContributorID: "c-w2", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: "t-2", Repo: "myorg/repo1", Number: 7},
		lastPong:    time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-w2"] = held
	hub.mu.Unlock()

	if hub.RequeueContributorTask("c-w2", "") != 1 {
		t.Fatalf("expected release")
	}
	key := "myorg/repo1#7"
	if !hub.isTaskInFailureCooldown("myorg/repo1", 7) {
		t.Fatalf("expected short cooldown right after requeue")
	}
	// Age the failure timestamp beyond the short cooldown window (one failure ⇒ the
	// short failedTaskCooldownMinutes applies, well under the quarantine window), so
	// the issue is admissible again — back in the queue.
	hub.completedMu.Lock()
	hub.failedTasks[key] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if hub.isTaskInFailureCooldown("myorg/repo1", 7) {
		t.Fatalf("issue should be admissible again (back in the queue) after the short cooldown lapses")
	}
}

// TestRequeueHub_NoDoubleAssignSameIdentity is the anti-race assertion: after a
// manual requeue, a SECOND live session for the same identity that immediately asks
// for work must NOT be handed the just-released issue (its short cooldown excludes
// it), exactly as the auto-release paths guarantee. We assert the cooldown predicate
// selectTask uses is active for that issue.
func TestRequeueHub_NoDoubleAssignSameIdentity(t *testing.T) {
	hub, _ := covK2Hub(t)
	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "dup", ContributorID: "c-dup", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: "t-3", Repo: "myorg/repo1", Number: 99},
		lastPong:    time.Now(),
	}
	second := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "dup", ContributorID: "c-dup", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-held"] = held
	hub.connections["conn-second"] = second
	hub.mu.Unlock()

	hub.RequeueContributorTask("c-dup", "")
	// The freshly-released issue is in the short cooldown — selectTask's admission
	// scan (isTaskInFailureCooldown) excludes it, so the second session cannot be
	// re-handed the same issue in the reconnect window. No duplicate assignment.
	if !hub.isTaskInFailureCooldown("myorg/repo1", 99) {
		t.Fatalf("released issue not in cooldown — a stale worker could be re-handed it (dup-assign race)")
	}
}

// TestRequeueEndpoint_OwnerReleases exercises the HTTP endpoint end to end: an owner
// POST releases the held task (200 + released count), and a subsequent requeue of a
// now-idle contributor is a 404 (nothing to requeue). Read-viewer 403 is covered by
// the shared TestContributorWrite_ReadViewerForbidden table (requeue row added).
func TestRequeueEndpoint_OwnerReleases(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "contributor",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes() // wires s.contributeHub

	// Inject a live connection holding a task for carol.
	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: "t-c", Repo: "myorg/repo1", Number: 5},
		lastPong:    time.Now(),
	}
	s.contributeHub.mu.Lock()
	s.contributeHub.connections["conn-carol"] = held
	s.contributeHub.mu.Unlock()

	// Owner requeues carol's task → 200, released == 1.
	req := httptest.NewRequest(http.MethodPost, "/api/contributors/carol/requeue", strings.NewReader(""))
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner requeue got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		OK       bool `json:"ok"`
		Released int  `json:"released"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.Released != 1 {
		t.Fatalf("unexpected requeue response: %+v (body %s)", resp, w.Body.String())
	}
	if !s.contributeHub.isTaskInFailureCooldown("myorg/repo1", 5) {
		t.Fatalf("endpoint did not book the existing short cooldown on release")
	}

	// carol is now idle — a second requeue is a 404 (nothing in flight).
	req2 := httptest.NewRequest(http.MethodPost, "/api/contributors/carol/requeue", strings.NewReader(""))
	req2.Header.Set("X-Hive-Role", "owner")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("requeue of idle contributor got %d, want 404 (body: %s)", w2.Code, w2.Body.String())
	}
}
