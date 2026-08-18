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

// Yank repurposes the operator manual-requeue control: it still RELEASES the held task
// (same cooldown + generation-bump machinery — pinned by contribute_requeue_test.go and
// contribute_lease_test.go), and it ADDS an immediate reassignment of the SAME clanker
// to its next-priority item, with a brief per-(clanker, issue) self-exclusion so the
// clanker moves to genuinely different work while the released issue stays offerable to
// every OTHER contributor immediately. These tests pin exactly those additions.

// TestYank_ReleasesAndReassignsToDifferentIssue proves the core yank behavior: a clanker
// holding issue #1 is yanked and, in the SAME call, reassigned issue #2 (a DIFFERENT
// admissible issue) rather than being left idle. The released #1 is booked into the
// short cooldown (the reused release machinery), and its assignment generation is bumped.
func TestYank_ReleasesAndReassignsToDifferentIssue(t *testing.T) {
	hub, s := covK2Hub(t)

	held := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "worker", ContributorID: "c-worker", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 1},
		currentTaskGen: 7,
		lastLeaseRenew: time.Now(),
		lastPong:       time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-worker"] = held
	hub.mu.Unlock()

	// Two admissible issues; #1 is the held one, #2 is different work.
	setStatusIssues(s,
		intgIssue(1, "held issue", "someone", nil),
		intgIssue(2, "other issue", "someone", nil),
	)

	released, assigned := hub.RequeueContributorTask("c-worker", "wedged: no progress")
	if released != 1 {
		t.Fatalf("expected 1 session released, got %d", released)
	}
	// The clanker was IMMEDIATELY reassigned (not left idle) to a DIFFERENT issue.
	if assigned == nil || assigned.Type != "task_assign" {
		t.Fatalf("expected an immediate reassignment task_assign, got %+v", assigned)
	}
	if assigned.Number != 2 {
		t.Fatalf("yank should reassign to a DIFFERENT issue (#2), got #%d", assigned.Number)
	}

	// The connection now holds the reassigned #2, and its generation was bumped off 7.
	held.mu.Lock()
	cur := held.currentTask
	gen := held.currentTaskGen
	held.mu.Unlock()
	if cur == nil || cur.Number != 2 {
		t.Fatalf("connection did not adopt the reassigned issue #2: %+v", cur)
	}
	if gen == 7 {
		t.Fatalf("assignment generation not bumped — a stale worker would not be fenced")
	}

	// The released #1 is booked into the short failure cooldown (reused release path).
	if !hub.isTaskInFailureCooldown("myorg/repo1", 1) {
		t.Fatalf("yank did not book the short cooldown on the released issue via the existing path")
	}
}

// TestYank_SelfExcludesReleasedIssueFromSameClanker proves the self-exclusion: the
// just-yanked issue is skipped for THIS clanker for the TTL, so even when it is the ONLY
// otherwise-admissible issue the reassignment does not re-grab it (the clanker ends up
// idle rather than back on the same work). After the TTL elapses the issue is selectable
// by this clanker again.
func TestYank_SelfExcludesReleasedIssueFromSameClanker(t *testing.T) {
	hub, s := covK2Hub(t)

	held := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "solo", ContributorID: "c-solo", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 1},
		lastLeaseRenew: time.Now(),
		lastPong:       time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-solo"] = held
	hub.mu.Unlock()

	// ONLY issue #1 exists — the very issue being yanked off.
	setStatusIssues(s, intgIssue(1, "only issue", "someone", nil))

	released, assigned := hub.RequeueContributorTask("c-solo", "")
	if released != 1 {
		t.Fatalf("expected release, got %d", released)
	}
	// #1 is self-excluded for this clanker AND still in its fresh cooldown, so there is
	// no admissible reassignment: released + idle (the requeue-only fallback).
	if assigned != nil && assigned.Type == "task_assign" {
		t.Fatalf("clanker was re-handed the same yanked issue: %+v", assigned)
	}

	// Verify the self-exclusion is what would block it even absent the cooldown: age the
	// failure cooldown out, then confirm selectTask STILL will not offer #1 to this
	// clanker while the exclusion holds.
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#1"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()

	msg := hub.selectTask(held)
	if msg != nil && msg.Type == "task_assign" && msg.Number == 1 {
		t.Fatalf("self-exclusion did not hold: #1 was offered back to the same clanker before the TTL")
	}

	// Expire the self-exclusion; #1 becomes selectable by this clanker again.
	hub.mu.Lock()
	hub.yankExclusions[yankExcludeKey("c-solo", "myorg/repo1", 1)] = time.Now().Add(-time.Second)
	hub.mu.Unlock()
	msg = hub.selectTask(held)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 1 {
		t.Fatalf("after the self-exclusion TTL elapsed, #1 should be selectable again, got %+v", msg)
	}
}

// TestYank_ReleasedIssueOfferableToOtherClankerImmediately proves the self-exclusion is
// SCOPED to the yanked clanker: a DIFFERENT contributor may be offered the released
// issue right away (only the short global cooldown applies to everyone; once it lapses,
// the other clanker gets it while the yanked clanker is still excluded).
func TestYank_ReleasedIssueOfferableToOtherClankerImmediately(t *testing.T) {
	hub, s := covK2Hub(t)

	held := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "yanked", ContributorID: "c-yanked", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 1},
		lastLeaseRenew: time.Now(),
		lastPong:       time.Now(),
	}
	other := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "other", ContributorID: "c-other", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-yanked"] = held
	hub.connections["conn-other"] = other
	hub.mu.Unlock()

	setStatusIssues(s, intgIssue(1, "only issue", "someone", nil))

	if released, _ := hub.RequeueContributorTask("c-yanked", ""); released != 1 {
		t.Fatalf("expected release")
	}

	// The global short cooldown applies to everyone right after release; age it out so we
	// isolate the SELF-exclusion (which is per-clanker, not global).
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#1"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()

	// The yanked clanker is still self-excluded from #1...
	if excluded := func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return hub.isYankSelfExcludedLocked("c-yanked", "myorg/repo1", 1)
	}(); !excluded {
		t.Fatalf("yanked clanker should still be self-excluded from #1")
	}
	// ...but a DIFFERENT clanker is offered #1 immediately.
	msg := hub.selectTask(other)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 1 {
		t.Fatalf("released issue #1 must be offerable to a DIFFERENT clanker immediately, got %+v", msg)
	}
}

// TestYankEndpoint_NoHeldTaskAndAuth exercises the (unchanged) requeue endpoint under the
// yank behavior: a read viewer is forbidden (403 — the owner/read-write boundary), and a
// contributor holding no in-flight task is a 4xx guard (nothing to yank). The happy-path
// release + reassignment is covered at the hub level above.
func TestYankEndpoint_NoHeldTaskAndAuth(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "erin", ContributorID: "c-erin", TrustTier: "contributor",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// Read viewer is forbidden even when a held task exists.
	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "erin", ContributorID: "c-erin", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: "t-e", Repo: "myorg/repo1", Number: 5},
		lastPong:    time.Now(),
	}
	s.contributeHub.mu.Lock()
	s.contributeHub.connections["conn-erin"] = held
	s.contributeHub.mu.Unlock()

	reqRV := httptest.NewRequest(http.MethodPost, "/api/contributors/erin/requeue", strings.NewReader(""))
	reqRV.Header.Set("X-Hive-Role", "read")
	wRV := httptest.NewRecorder()
	s.mux.ServeHTTP(wRV, reqRV)
	if wRV.Code != http.StatusForbidden {
		t.Fatalf("read viewer yank got %d, want 403", wRV.Code)
	}

	// Owner yanks the held task (no other issue configured → released, idle). The
	// response reports reassigned==false but ok==true.
	req := httptest.NewRequest(http.MethodPost, "/api/contributors/erin/requeue", strings.NewReader(""))
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner yank got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		OK         bool `json:"ok"`
		Released   int  `json:"released"`
		Reassigned bool `json:"reassigned"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.Released != 1 {
		t.Fatalf("unexpected yank response: %+v (body %s)", resp, w.Body.String())
	}

	// erin now holds nothing — a second yank is the no-held-task guard.
	req2 := httptest.NewRequest(http.MethodPost, "/api/contributors/erin/requeue", strings.NewReader(""))
	req2.Header.Set("X-Hive-Role", "owner")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code == http.StatusOK {
		t.Fatalf("yank of a clanker holding no task should not be 200, got %d (body: %s)", w2.Code, w2.Body.String())
	}
}
