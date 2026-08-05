package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_lease_test.go covers the kubestellar/hive#2568 completion: the hub-owned
// task LEASE (auto-expiry backstop), the operator revoke/requeue REASON plumbing, and
// the assignment-GENERATION guard (the Gate — a stale worker whose task was reassigned
// must not overwrite the new owner's state). These build on the #2492/#2557 cooldown
// path and the manual-requeue slice already covered by contribute_requeue_test.go.

// --- generationAccepted (the Gate primitive) ---------------------------------

func TestGenerationAccepted_FencesStaleWorker(t *testing.T) {
	// Current owner's generation is 5.
	if !generationAccepted(5, 5) {
		t.Fatalf("the current generation must be accepted")
	}
	// A stale worker still echoing 3 (its task was revoked and reassigned, bumping
	// the connection past it) must be rejected — this is the fence.
	if generationAccepted(3, 5) {
		t.Fatalf("a stale generation must be rejected (the Gate)")
	}
	// An unversioned relay that never learned a generation echoes 0 and is accepted,
	// falling back to the pre-existing TaskID identity match (backward compatibility).
	if !generationAccepted(0, 5) {
		t.Fatalf("an unversioned relay (gen 0) must be accepted for backward compatibility")
	}
}

// --- lease TTL backstop: reclaimExpiredLeases --------------------------------

// TestLeaseExpiry_WedgedTaskAutoReleasedAndCooledDown proves the backstop: a task
// whose lease has not been renewed within wsTaskTimeout is auto-released through the
// SAME short cooldown the disconnect/requeue paths book, its currentTask is cleared,
// and its generation is bumped (fencing the wedged worker).
func TestLeaseExpiry_WedgedTaskAutoReleasedAndCooledDown(t *testing.T) {
	hub, _ := covK2Hub(t)

	now := time.Now()
	wedged := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "wedged", ContributorID: "c-wedged", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-wedged", Repo: "myorg/repo1", Number: 42},
		currentTaskGen: 7,
		// Lease last renewed well past the TTL → wedged, connected but not progressing.
		lastLeaseRenew: now.Add(-(wsTaskTimeout + time.Minute)),
		lastPong:       now, // heartbeat still ALIVE — the connection is not stale.
	}
	hub.mu.Lock()
	hub.connections["conn-wedged"] = wedged
	hub.mu.Unlock()

	if hub.isTaskInFailureCooldown("myorg/repo1", 42) {
		t.Fatalf("issue unexpectedly in cooldown before lease expiry")
	}

	released := hub.reclaimExpiredLeases(now)
	if released != 1 {
		t.Fatalf("expected 1 expired lease reclaimed, got %d", released)
	}

	wedged.mu.Lock()
	still := wedged.currentTask
	gen := wedged.currentTaskGen
	wedged.mu.Unlock()
	if still != nil {
		t.Fatalf("currentTask not cleared on lease expiry: %+v", still)
	}
	if gen == 7 {
		t.Fatalf("generation not bumped on lease expiry — a wedged worker would not be fenced")
	}
	if !hub.isTaskInFailureCooldown("myorg/repo1", 42) {
		t.Fatalf("lease expiry did not book the short failure cooldown via the existing path")
	}
}

// TestLeaseExpiry_ClearsPendingCredential proves that lease expiry clears the
// pendingToken and credentialDelivered state, matching the cleanup in
// RequeueContributorTask. Without this, a stale credential could leak to a
// now-idle connection. (kubestellar/hive#2675)
func TestLeaseExpiry_ClearsPendingCredential(t *testing.T) {
	hub, _ := covK2Hub(t)

	now := time.Now()
	wedged := &ContributorConnection{
		profile:            &ContributorProfile{GitHubUsername: "cred-leak", ContributorID: "c-cred", TrustTier: "contributor"},
		currentTask:        &WSTaskAssign{TaskID: "t-cred", Repo: "myorg/repo1", Number: 55},
		currentTaskGen:     9,
		lastLeaseRenew:     now.Add(-(wsTaskTimeout + time.Minute)),
		lastPong:           now,
		pendingToken:       "ghp_LEAKED_TOKEN_12345",
		credentialDelivered: false,
	}
	hub.mu.Lock()
	hub.connections["conn-cred"] = wedged
	hub.mu.Unlock()

	released := hub.reclaimExpiredLeases(now)
	if released != 1 {
		t.Fatalf("expected 1 expired lease reclaimed, got %d", released)
	}

	wedged.mu.Lock()
	tok := wedged.pendingToken
	delivered := wedged.credentialDelivered
	wedged.mu.Unlock()

	if tok != "" {
		t.Fatalf("pendingToken not cleared on lease expiry: %q (credential leak)", tok)
	}
	if delivered {
		t.Fatalf("credentialDelivered not cleared on lease expiry")
	}
}

// TestLeaseExpiry_ClearsDeliveredCredentialFlag ensures credentialDelivered=true is
// also reset on expiry, so a subsequent task assignment starts clean.
func TestLeaseExpiry_ClearsDeliveredCredentialFlag(t *testing.T) {
	hub, _ := covK2Hub(t)

	now := time.Now()
	wedged := &ContributorConnection{
		profile:            &ContributorProfile{GitHubUsername: "delivered", ContributorID: "c-del", TrustTier: "contributor"},
		currentTask:        &WSTaskAssign{TaskID: "t-del", Repo: "myorg/repo1", Number: 56},
		currentTaskGen:     4,
		lastLeaseRenew:     now.Add(-(wsTaskTimeout + 2*time.Minute)),
		lastPong:           now,
		pendingToken:       "",
		credentialDelivered: true,
	}
	hub.mu.Lock()
	hub.connections["conn-del"] = wedged
	hub.mu.Unlock()

	hub.reclaimExpiredLeases(now)

	wedged.mu.Lock()
	delivered := wedged.credentialDelivered
	wedged.mu.Unlock()
	if delivered {
		t.Fatalf("credentialDelivered not cleared — next task would skip credential delivery")
	}
}

// TestLeaseExpiry_ProgressingTaskNotReclaimed is the NO-FALSE-RECLAIM guarantee: a
// task that is "working slowly" but still renewing its lease (recent lastLeaseRenew)
// is NEVER auto-released. This is what keeps the conservative backstop from stealing a
// legitimately long-running task.
func TestLeaseExpiry_ProgressingTaskNotReclaimed(t *testing.T) {
	hub, _ := covK2Hub(t)

	now := time.Now()
	working := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "slow", ContributorID: "c-slow", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-slow", Repo: "myorg/repo1", Number: 7},
		currentTaskGen: 3,
		// Lease renewed one second ago — the worker is alive and reporting progress.
		lastLeaseRenew: now.Add(-time.Second),
		lastPong:       now,
	}
	hub.mu.Lock()
	hub.connections["conn-slow"] = working
	hub.mu.Unlock()

	if got := hub.reclaimExpiredLeases(now); got != 0 {
		t.Fatalf("a progressing task must NOT be reclaimed, but %d were", got)
	}
	working.mu.Lock()
	still := working.currentTask
	working.mu.Unlock()
	if still == nil {
		t.Fatalf("a progressing task was wrongly released (false reclaim)")
	}
	if hub.isTaskInFailureCooldown("myorg/repo1", 7) {
		t.Fatalf("a progressing task must not be put into cooldown")
	}
}

// TestLeaseExpiry_IdleConnectionIgnored confirms a connection with no active lease
// (zero lastLeaseRenew / no currentTask) is never a reclaim target, so an idle clanker
// is untouched by the backstop.
func TestLeaseExpiry_IdleConnectionIgnored(t *testing.T) {
	hub, _ := covK2Hub(t)
	idle := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "idle", ContributorID: "c-idle", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-idle"] = idle
	hub.mu.Unlock()
	if got := hub.reclaimExpiredLeases(time.Now()); got != 0 {
		t.Fatalf("an idle connection must not be reclaimed, got %d", got)
	}
}

// --- operator requeue: reason + generation bump ------------------------------

// TestRequeue_RecordsReasonAndBumpsGeneration proves the operator requeue records the
// supplied reason in the activity log (auditable, visible to operators) and bumps the
// held connection's generation (fencing the revoked worker).
func TestRequeue_RecordsReasonAndBumpsGeneration(t *testing.T) {
	hub, _ := covK2Hub(t)

	held := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "wedged", ContributorID: "c-w", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 5},
		currentTaskGen: 11,
		lastLeaseRenew: time.Now(),
		lastPong:       time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-w"] = held
	hub.mu.Unlock()

	if released, _ := hub.RequeueContributorTask("c-w", "wedged: no progress for an hour"); released != 1 {
		t.Fatalf("expected 1 released")
	}

	held.mu.Lock()
	gen := held.currentTaskGen
	held.mu.Unlock()
	if gen == 11 {
		t.Fatalf("generation not bumped on requeue — revoked worker would not be fenced")
	}

	// The reason is recorded in the activity log (auditable).
	hub.activityMu.RLock()
	found := false
	for _, e := range hub.activity {
		if strings.Contains(e.Action, "wedged: no progress for an hour") {
			found = true
			break
		}
	}
	hub.activityMu.RUnlock()
	if !found {
		t.Fatalf("operator reason not recorded in the activity log")
	}
}

// TestRequeue_BlankReasonFallsBack confirms an empty reason falls back to the default
// recovery label rather than recording an empty string, so the release stays auditable.
func TestRequeue_BlankReasonFallsBack(t *testing.T) {
	hub, _ := covK2Hub(t)
	held := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "w2", ContributorID: "c-w2", TrustTier: "contributor"},
		currentTask:    &WSTaskAssign{TaskID: "t-2", Repo: "myorg/repo1", Number: 9},
		lastLeaseRenew: time.Now(),
		lastPong:       time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-w2"] = held
	hub.mu.Unlock()

	hub.RequeueContributorTask("c-w2", "   ")
	hub.activityMu.RLock()
	found := false
	for _, e := range hub.activity {
		// The yank release records "yanked by operator: <defaultYankReason>" when the
		// operator passes a blank reason, so the release stays auditable.
		if strings.Contains(e.Action, defaultYankReason) {
			found = true
			break
		}
	}
	hub.activityMu.RUnlock()
	if !found {
		t.Fatalf("blank reason did not fall back to the default recovery label")
	}
}

// --- endpoint: reason plumbing + auth ----------------------------------------

// TestRequeueEndpoint_ReasonAndAuth exercises the HTTP endpoint: an owner may pass a
// reason in the JSON body (200 + released), and a read viewer is forbidden (403 — the
// #2562 owner/read-write boundary, so this is not an unauthenticated control like the
// class of bug #2610 fixed).
func TestRequeueEndpoint_ReasonAndAuth(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "contributor",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	inject := func() {
		held := &ContributorConnection{
			profile:        &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "contributor"},
			currentTask:    &WSTaskAssign{TaskID: "t-d", Repo: "myorg/repo1", Number: 5},
			lastLeaseRenew: time.Now(),
			lastPong:       time.Now(),
		}
		s.contributeHub.mu.Lock()
		s.contributeHub.connections["conn-dave"] = held
		s.contributeHub.mu.Unlock()
	}

	// A read viewer is forbidden — the control requires owner/read-write.
	inject()
	reqRV := httptest.NewRequest(http.MethodPost, "/api/contributors/dave/requeue",
		strings.NewReader(`{"reason":"nope"}`))
	reqRV.Header.Set("X-Hive-Role", "read")
	wRV := httptest.NewRecorder()
	s.mux.ServeHTTP(wRV, reqRV)
	if wRV.Code != http.StatusForbidden {
		t.Fatalf("read viewer requeue got %d, want 403 (unauthed control would be the #2610 bug)", wRV.Code)
	}

	// An owner with a reason body releases the task (200, released == 1).
	req := httptest.NewRequest(http.MethodPost, "/api/contributors/dave/requeue",
		strings.NewReader(`{"reason":"wedged: connected but idle"}`))
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("owner requeue with reason got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		OK       bool `json:"ok"`
		Released int  `json:"released"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.Released != 1 {
		t.Fatalf("unexpected requeue response: %+v", resp)
	}
}

// --- THE GATE: stale-generation guard end to end -----------------------------

// TestStaleGeneration_RevokedWorkerCannotOverwriteNewOwner is the most important test
// (the Gate). It drives the real WebSocket handlers:
//
//  1. A worker is assigned a task and learns its generation (task_gen) from task_assign.
//  2. The operator revokes that task (RequeueContributorTask bumps the connection's
//     generation and books the cooldown).
//  3. The SAME worker, now stale, sends task_complete carrying the OLD task_gen.
//  4. The guard REJECTS it: the completion is not recorded (TasksCompleted stays 0),
//     so a revoked worker cannot overwrite the reassigned owner's state.
//
// A control leg confirms that a completion carrying the CURRENT generation IS recorded,
// so the guard fences only stale workers.
func TestStaleGeneration_RevokedWorkerCannotOverwriteNewOwner(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Register + connect a worker.
	body := `{"github_username":"gateuser"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	// Give the worker a task; capture its generation from the assign message.
	setStatusIssues(s, intgIssue(300, "Gate issue", "someone", nil))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	if assign.TaskGen == 0 {
		t.Fatalf("task_assign did not carry a generation token (the Gate needs it)")
	}
	staleGen := assign.TaskGen
	staleTaskID := assign.TaskID

	// Operator revokes the task: this bumps the connection's generation past staleGen.
	if released, _ := s.contributeHub.RequeueContributorTask(reg["contributor_id"], "wedged: no progress"); released != 1 {
		t.Fatalf("expected the operator revoke to release the held task")
	}
	// The relay would receive a task_revoke here; drain it so it doesn't confuse reads.
	_ = readMsg(t, conn) // task_revoke

	// The now-STALE worker wakes and reports completion with the OLD generation.
	conn.WriteJSON(WSMessage{
		Type:    "task_complete",
		TaskID:  staleTaskID,
		TaskGen: staleGen,
		Result:  "pr_created",
		PRURL:   "https://github.com/myorg/repo1/pull/999",
	})
	time.Sleep(60 * time.Millisecond)

	p := findContributor(reg["contributor_id"])
	if p == nil {
		t.Fatalf("contributor vanished")
	}
	if p.TasksCompleted != 0 {
		t.Fatalf("THE GATE FAILED: a stale-generation task_complete was recorded (TasksCompleted=%d) — a revoked worker overwrote state", p.TasksCompleted)
	}

	// Control: a fresh assignment + a completion carrying the CURRENT generation IS
	// recorded, proving the guard fences only stale workers, not all completions.
	setStatusIssues(s, intgIssue(301, "Gate control issue", "someone", nil))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	assign2 := readMsg(t, conn)
	if assign2.Type != "task_assign" {
		t.Fatalf("expected a fresh task_assign for the control leg, got %+v", assign2)
	}
	if assign2.TaskGen == staleGen {
		t.Fatalf("the reassignment reused the stale generation — generations must be monotonic")
	}
	conn.WriteJSON(WSMessage{
		Type:    "task_complete",
		TaskID:  assign2.TaskID,
		TaskGen: assign2.TaskGen,
		Result:  "no_pr",
		PRURL:   "",
	})
	time.Sleep(60 * time.Millisecond)

	p = findContributor(reg["contributor_id"])
	if p.TasksCompleted != 1 {
		t.Fatalf("a current-generation completion was not recorded (TasksCompleted=%d) — the guard is over-fencing", p.TasksCompleted)
	}
}

// TestAtMostOneAuthoritativeLease is the concurrency half of the Gate: after a task is
// assigned to one connection, the in-flight guard (activeIssueKeys) excludes that issue
// so a second connection asking for work cannot be handed the SAME issue — at most one
// authoritative lease exists at a time.
func TestAtMostOneAuthoritativeLease(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, intgIssue(400, "Only issue", "someone", nil))

	first := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "first", ContributorID: "c-first", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-first"] = first
	hub.mu.Unlock()

	assign := hub.selectTask(first)
	if assign == nil || assign.Type != "task_assign" {
		t.Fatalf("first connection should get the only issue, got %+v", assign)
	}
	// First now holds an authoritative lease (currentTask + generation set by selectTask).
	first.mu.Lock()
	gen := first.currentTaskGen
	first.mu.Unlock()
	if gen == 0 {
		t.Fatalf("selectTask did not stamp an assignment generation")
	}

	// A second connection asks for work: the only issue is in-flight, so it must NOT
	// be handed out again — no second authoritative lease on the same issue.
	second := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "second", ContributorID: "c-second", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(second)
	if msg != nil && msg.Type == "task_assign" {
		t.Fatalf("two authoritative leases on the same issue: %s#%d handed out twice", msg.Repo, msg.Number)
	}
}
