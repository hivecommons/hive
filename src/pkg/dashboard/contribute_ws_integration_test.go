package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/config"
)

// contribute_ws_integration_test.go is the INTEGRATION test called for by
// kubestellar/hive#2413: src/pkg/dashboard/contribute_ws.go's selectTask is
// modified by a chain of PRs, each of which added its own isolated unit tests
// (contribute_ws_more_coverage_test.go for #2408/#2409, provision_approve_
// coverage_test.go for #2410, contribute_ws_trust_controls_test.go for #2436,
// contribute_ws_livelock_test.go for #2435). None of them exercises the
// COMBINED behavior of a queue that mixes every kind of candidate at once.
// This file does that: it builds one status payload/hub state carrying own
// work, cooldown issues (with and without a reported PR), quarantined/
// recently-failed issues, and trust-tier restrictions together, then asserts
// selectTask's priority order and gating end to end.
//
// Scope note on #2410 (ClaimDelivered reset): issue #2413 lists #2410 as a
// third PR modifying selectTask's queue/state lifecycle. It does not. #2410
// ("hub: re-arm ClaimDelivered on approve-provision (re)assignment") touches
// SaaSHive.ClaimDelivered in src/pkg/hub/saas.go — the hub/spoke SaaS
// provisioning claim handshake — a completely different subsystem from the
// contributor task queue in this package. contribute_ws.go's selectTask has
// no ClaimDelivered field or recycled-placeholder concept; grepping this
// package for "ClaimDelivered" and "SaaSHive" turns up nothing. There is
// nothing in selectTask's combined behavior to integration-test for #2410,
// so no scenario below references it. #2410 already has its own regression
// coverage in src/pkg/hub/provision_approve_coverage_test.go
// (TestHandleApproveProvisionRearmClaimDelivered).
//
// PRs actually covered here, all confirmed by reading contribute_ws.go at
// HEAD (v2 branch) before writing this file:
//   #2408 — conditional cooldown by pr_url (completedTaskCooldownHours=168h
//           with a PR, completedNoPRCooldownHours=4h without)
//   #2409 — own-authored candidates ordered ahead of everyone else's work
//   #2436 (via #2456 + #2437) — disabled_tiers refusal, tier_limits.
//           MaxConcurrent enforced per identity, and auto-promotion gated on
//           TasksWithPR (a PR-less task_complete does not promote)
//   #2435 (via #2455) — task_failed records a short failure cooldown,
//           consecutive failures quarantine an issue for the longer window,
//           and the failure-aware stable sort deprioritizes a recently-failed
//           issue behind never-failed peers so the queue cannot livelock

// intgIssue is a small builder for the map[string]any shape selectTask reads
// out of FrontendRepo.ActionableIssues (see oneActionableIssue /
// twoIssueStatus in the sibling _test.go files for the same shape used in
// isolation).
func intgIssue(number int, title, author string, assignees []string) map[string]any {
	issue := map[string]any{
		"number": float64(number),
		"title":  title,
		"url":    "https://github.com/myorg/repo1/issues/" + itoa(number),
		"author": author,
	}
	if len(assignees) > 0 {
		anyAssignees := make([]any, len(assignees))
		for i, a := range assignees {
			anyAssignees[i] = a
		}
		issue["assignees"] = anyAssignees
	}
	return issue
}

// itoa avoids pulling in strconv just for this file's URL-building.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// setStatusIssues installs a status with exactly the given actionable issues
// in one repo (myorg/repo1), replacing whatever was there before. Centralizing
// this (rather than repeating the StatusPayload literal per case) keeps the
// table cases in this file focused on WHICH issues are present, not on
// plumbing.
func setStatusIssues(s *Server, issues ...map[string]any) {
	anyIssues := make([]any, len(issues))
	for i, iss := range issues {
		anyIssues[i] = iss
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name:             "repo1",
				Full:             "myorg/repo1",
				ActionableIssues: anyIssues,
			},
		},
	}
	s.statusMu.Unlock()
}

// contributorPollTimeout/contributorPollInterval bound the poll loops below.
// task_complete is handled asynchronously relative to the test goroutine: the
// WS write only queues the frame, and the handler's disk save
// (saveContributorProfile) runs on the connection's own read-loop goroutine
// sometime after that. A fixed time.Sleep between writing task_complete and
// reading the profile back off disk is exactly the flake #5037 reported —
// under load the save can simply lose the race against the sleep, and the
// test then drives selectTask with a stale TasksWithPR, reusing an issue
// number that is actually already in its just-booked cooldown (selectTask
// then legitimately returns task_unavailable/no_matching_work for it). Poll
// the real on-disk condition instead of guessing a fixed delay.
const (
	contributorPollTimeout  = 2 * time.Second
	contributorPollInterval = 5 * time.Millisecond
)

// waitForContributorTasksCompleted polls findContributor until its persisted
// TasksCompleted reaches want (or the deadline expires), so the caller
// observes the task_complete handler's saveContributorProfile write rather
// than a fixed sleep guessing when it landed.
func waitForContributorTasksCompleted(t *testing.T, contributorID string, want int) *ContributorProfile {
	t.Helper()
	deadline := time.Now().Add(contributorPollTimeout)
	var p *ContributorProfile
	for time.Now().Before(deadline) {
		p = findContributor(contributorID)
		if p != nil && p.TasksCompleted >= want {
			return p
		}
		time.Sleep(contributorPollInterval)
	}
	if p == nil {
		t.Fatalf("contributor %s not found after waiting for TasksCompleted=%d", contributorID, want)
	}
	t.Fatalf("timed out waiting for TasksCompleted=%d, got %d", want, p.TasksCompleted)
	return nil
}

// waitForContributorTasksWithPR is waitForContributorTasksCompleted's sibling
// for the verified-PR counter driving auto-promotion.
func waitForContributorTasksWithPR(t *testing.T, contributorID string, want int) *ContributorProfile {
	t.Helper()
	deadline := time.Now().Add(contributorPollTimeout)
	var p *ContributorProfile
	for time.Now().Before(deadline) {
		p = findContributor(contributorID)
		if p != nil && p.TasksWithPR >= want {
			return p
		}
		time.Sleep(contributorPollInterval)
	}
	if p == nil {
		t.Fatalf("contributor %s not found after waiting for TasksWithPR=%d", contributorID, want)
	}
	t.Fatalf("timed out waiting for TasksWithPR=%d, got %d", want, p.TasksWithPR)
	return nil
}

// TestIntegration_SelectTask_PriorityOrdering builds a queue mixing:
//   - an issue authored by the connecting contributor ("own work", #2409)
//   - an issue authored by someone else, never failed
//   - an issue authored by someone else that has ONE recent (non-quarantining)
//     failure on record, admissible again but deprioritized (#2435 remedy 3)
//
// and asserts selectTask's stable sort delivers them in exactly the order the
// combined PRs specify: own work first, then fewer-recent-failures first,
// then scan order — regardless of the failed issue's earlier position in the
// scan.
func TestIntegration_SelectTask_PriorityOrdering(t *testing.T) {
	hub, s := covK2Hub(t)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// Scan order: #1 (someone else, one recent failure, AHEAD in scan order),
	// #2 (someone else, never failed), #3 (alice's own work, LAST in scan
	// order). Pre-#2409 first-eligible behavior would pick #1. Post-#2409
	// alone (ignoring #2435) would pick #3 (own work) correctly on the first
	// call but, once #3 is taken, would fall back to plain scan order and
	// hand out #1 (recently failed) ahead of #2 (clean) — the exact ordering
	// bug #2435 fixes. This test asserts the FULL combined order across two
	// consecutive selections.
	setStatusIssues(s,
		intgIssue(1, "Someone else's issue, will fail once", "bob", nil),
		intgIssue(2, "Someone else's clean issue", "carol", nil),
		intgIssue(3, "Alice's own work", "alice", nil),
	)

	// Give #1 one recorded (non-quarantining) failure, then age it past the
	// SHORT cooldown so it is admissible again but still deprioritized by the
	// #2435 remedy-3 backstop.
	hub.recordTaskFailure("myorg/repo1", 1, false)
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#1"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if hub.isTaskInFailureCooldown("myorg/repo1", 1) {
		t.Fatalf("setup: #1's short failure cooldown should have elapsed for this ordering check")
	}

	// First selection: own work (#3) must win despite being last in scan
	// order and despite #1/#2 being fully admissible (#2409, preserved by
	// #2435's sort which checks isOwn before recentFailures).
	first := hub.selectTask(conn)
	if first == nil || first.Type != "task_assign" {
		t.Fatalf("expected a task_assign, got %+v", first)
	}
	if first.Number != 3 {
		t.Fatalf("expected own work (#3) prioritized first (#2409), got #%d", first.Number)
	}

	// Release #3 (simulate it becoming active elsewhere is not needed — clear
	// currentTask and re-seed so #3 is no longer offered a second time via the
	// activeIssues guard, keeping focus on the #1 vs #2 ordering).
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()
	setStatusIssues(s,
		intgIssue(1, "Someone else's issue, will fail once", "bob", nil),
		intgIssue(2, "Someone else's clean issue", "carol", nil),
	)

	// Second selection: with own work gone, #2 (never failed) must be
	// preferred over #1 (one recent failure) even though #1 is ahead in scan
	// order — the #2435 remedy-3 backstop that prevents a flaky head-of-scan
	// issue from monopolizing the queue.
	second := hub.selectTask(conn)
	if second == nil {
		t.Fatalf("expected a second assignment, got nil")
	}
	if second.Number == 1 {
		t.Fatalf("failure-aware backstop broken: recently-failed #1 was offered ahead of clean #2")
	}
	if second.Number != 2 {
		t.Fatalf("expected clean #2 preferred over recently-failed #1, got #%d", second.Number)
	}
}

// TestIntegration_SelectTask_CooldownTiming validates #2408 in the same
// combined-queue shape used elsewhere in this file: a completion WITH a
// pr_url excludes the issue for the long (168h) window, while a completion
// WITHOUT one excludes it only for the short (4h) window — verified via the
// same isTaskInCooldown/selectTask exclusion path exercised end to end.
func TestIntegration_SelectTask_CooldownTiming(t *testing.T) {
	hub, s := covK2Hub(t)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// #10 completed WITH a PR: full 168h cooldown.
	hub.markTaskCompleted("myorg/repo1", 10, "https://github.com/myorg/repo1/pull/1")
	// #20 completed WITHOUT a PR: short 4h cooldown.
	hub.markTaskCompleted("myorg/repo1", 20, "")

	setStatusIssues(s,
		intgIssue(10, "Shipped a PR", "someone", nil),
		intgIssue(20, "Went idle, no PR", "someone", nil),
		intgIssue(30, "Untouched, always eligible", "someone", nil),
	)

	// Immediately after completion both #10 and #20 are excluded; only #30 is
	// admissible.
	msg := hub.selectTask(conn)
	if msg == nil || msg.Number != 30 {
		t.Fatalf("expected only #30 admissible right after both completions, got %+v", msg)
	}

	// Age both completions past the SHORT (4h) window but still well inside
	// the LONG (168h) window: #20 (no-PR) must now be admissible again, #10
	// (with-PR) must still be excluded.
	hub.completedMu.Lock()
	hub.completedTasks["myorg/repo1#10"] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedTasks["myorg/repo1#20"] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()

	if !hub.isTaskInCooldown("myorg/repo1", 10) {
		t.Fatalf("with-PR completion (#10) must still be in cooldown after only %dh (full cooldown is %dh)",
			completedNoPRCooldownHours+1, completedTaskCooldownHours)
	}
	if hub.isTaskInCooldown("myorg/repo1", 20) {
		t.Fatalf("no-PR completion (#20) must be released after %dh, not held for the full %dh",
			completedNoPRCooldownHours, completedTaskCooldownHours)
	}

	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()
	setStatusIssues(s,
		intgIssue(10, "Shipped a PR", "someone", nil),
		intgIssue(20, "Went idle, no PR", "someone", nil),
	)
	msg2 := hub.selectTask(conn)
	if msg2 == nil {
		t.Fatalf("expected #20 to be admissible again, got nil")
	}
	if msg2.Number != 20 {
		t.Fatalf("expected released no-PR issue #20 to be selected, got #%d", msg2.Number)
	}
}

// TestIntegration_SelectTask_TrustControls exercises #2436's two selectTask-
// level gates (disabled_tiers refusal and per-identity MaxConcurrent) inside
// the same mixed-queue shape as the other cases, confirming they compose with
// the #2409 own-work ordering and #2408 cooldown filtering rather than only
// working in isolation.
func TestIntegration_SelectTask_TrustControls(t *testing.T) {
	hub, s := covK2Hub(t)

	setStatusIssues(s, intgIssue(1, "Admissible issue", "someone", nil))

	// Disabled tier: refused outright with an explicit negative-ack, never
	// silently handed the admissible issue that exists in the queue.
	disabled := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "erin", ContributorID: "c-erin", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}
	s.deps.Config.Hub.DisabledTiers = []string{"newcomer"}
	msg := hub.selectTask(disabled)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableTierDisabled {
		t.Fatalf("expected task_unavailable/%s for a disabled tier, got %+v", taskUnavailableTierDisabled, msg)
	}
	s.deps.Config.Hub.DisabledTiers = nil

	// MaxConcurrent: a second live connection for the SAME identity, already
	// holding one task at the configured limit of 1, is refused even though
	// the queue has an admissible issue.
	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"contributor": {MaxConcurrent: 1},
	}
	held := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "frank", ContributorID: "c-frank", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: "t-held", Repo: "myorg/other", Number: 99},
		lastPong:    time.Now(),
	}
	second := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "frank", ContributorID: "c-frank", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-held"] = held
	hub.connections["conn-second"] = second
	hub.mu.Unlock()

	setStatusIssues(s, intgIssue(1, "Admissible issue", "someone", nil))
	msg2 := hub.selectTask(second)
	if msg2 == nil || msg2.Type != "task_unavailable" || msg2.Reason != taskUnavailableConcurrencyLimit {
		t.Fatalf("expected task_unavailable/%s at MaxConcurrent=1, got %+v", taskUnavailableConcurrencyLimit, msg2)
	}

	// Raising the limit admits the second connection to the same queue.
	s.deps.Config.Hub.TierLimits["contributor"] = config.TierRate{MaxConcurrent: 2}
	setStatusIssues(s, intgIssue(1, "Admissible issue", "someone", nil))
	msg3 := hub.selectTask(second)
	if msg3 == nil || msg3.Type != "task_assign" || msg3.Number != 1 {
		t.Fatalf("expected task_assign for #1 once MaxConcurrent raised to 2, got %+v", msg3)
	}
}

// TestIntegration_SelectTask_PromotionRequiresPR exercises the #2436/#2437
// promotion gate through the real WebSocket task_complete handler (not just
// the hub method), confirming that a task_complete with an empty PRURL does
// NOT advance a newcomer toward auto-promotion, while one that reports a
// PRURL does.
func TestIntegration_SelectTask_PromotionRequiresPR(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// #2565: TasksWithPR / promotion now require the reported PR to be VERIFIED
	// server-side (exists, base repo == assignment repo, author == contributor).
	// Install a GitHub mock that returns, for any pulls lookup, a PR in
	// myorg/repo1 authored by "promotion-user" so the PR-shipping loop below can
	// legitimately advance TasksWithPR. Without this the reported URL stays
	// unverified and (correctly) earns no trust credit — which is what the
	// wrong-author/wrong-repo/error tests assert instead.
	s.deps = verifyDepsFor(t, "myorg", "repo1", "promotion-user")

	body := `{"github_username":"promotion-user"}`
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

	// Drive contributorAutoPromoteAt PR-less completions. Each requires its
	// own task_assign first (task_complete for an unassigned task is a no-op).
	for i := 0; i < contributorAutoPromoteAt; i++ {
		s.statusMu.Lock()
		s.status = &StatusPayload{
			Repos: []FrontendRepo{
				{
					Name: "repo1",
					Full: "myorg/repo1",
					ActionableIssues: []any{
						intgIssue(100+i, "No-PR completion round", "someone", nil),
					},
				},
			},
		}
		s.statusMu.Unlock()

		conn.WriteJSON(WSMessage{Type: "ready", Seq: i + 2})
		assign := readMsg(t, conn)
		if assign.Type != "task_assign" {
			t.Fatalf("round %d: expected task_assign, got %+v", i, assign)
		}
		conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: assign.TaskID, Result: "no_pr", PRURL: ""})
		waitForContributorTasksCompleted(t, reg["contributor_id"], i+1)
	}

	p := findContributor(reg["contributor_id"])
	if p == nil {
		t.Fatalf("contributor not found")
	}
	if p.TasksCompleted != contributorAutoPromoteAt {
		t.Fatalf("expected %d completions recorded, got %d", contributorAutoPromoteAt, p.TasksCompleted)
	}
	if p.TrustTier != "newcomer" {
		t.Fatalf("PR-less completions must NOT auto-promote (#2436/#2437): expected still newcomer, got %q", p.TrustTier)
	}

	// One completion that DOES report a PR must advance TasksWithPR and, once
	// it reaches contributorAutoPromoteAt, promote the contributor.
	for p.TasksWithPR < contributorAutoPromoteAt {
		s.statusMu.Lock()
		s.status = &StatusPayload{
			Repos: []FrontendRepo{
				{
					Name: "repo1",
					Full: "myorg/repo1",
					ActionableIssues: []any{
						intgIssue(200+p.TasksWithPR, "PR-shipping round", "someone", nil),
					},
				},
			},
		}
		s.statusMu.Unlock()

		conn.WriteJSON(WSMessage{Type: "ready", Seq: 1000 + p.TasksWithPR})
		assign := readMsg(t, conn)
		if assign.Type != "task_assign" {
			t.Fatalf("expected task_assign while driving TasksWithPR, got %+v", assign)
		}
		prURL := "https://github.com/myorg/repo1/pull/" + itoa(500+p.TasksWithPR)
		conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: assign.TaskID, Result: "pr_created", PRURL: prURL})
		wantTasksWithPR := p.TasksWithPR + 1
		p = waitForContributorTasksWithPR(t, reg["contributor_id"], wantTasksWithPR)
	}
	if p.TrustTier != "contributor" {
		t.Fatalf("expected auto-promotion to contributor once TasksWithPR reached %d, got tier %q (TasksWithPR=%d)",
			contributorAutoPromoteAt, p.TrustTier, p.TasksWithPR)
	}
}

// TestIntegration_SelectTask_FailureQuarantineDoesNotStarveQueue is the
// end-to-end livelock regression (#2435) run inside a queue that ALSO carries
// own work and a cooldown issue (#2408/#2409), confirming quarantine composes
// with the rest of selectTask rather than only being provable in isolation.
// A repeatedly-failing head-of-scan issue must not starve a second admissible
// issue, and once it crosses the quarantine threshold it is parked for the
// long window while the rest of the queue keeps moving.
func TestIntegration_SelectTask_FailureQuarantineDoesNotStarveQueue(t *testing.T) {
	hub, s := covK2Hub(t)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "grace", ContributorID: "c-grace", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// #999 is completed (in a long cooldown) and must never be offered
	// regardless of the failure/own-work behavior under test.
	hub.markTaskCompleted("myorg/repo1", 999, "https://github.com/myorg/repo1/pull/1")

	poisonRepo, poisonNum := "myorg/repo1", 1
	goodNum := 2

	// Round 0: with no failure history yet, the poison issue (head of scan,
	// #1) is the expected pick — proving it is genuinely admissible and
	// genuinely ahead in scan order before any #2435 defense engages.
	setStatusIssues(s,
		intgIssue(poisonNum, "Reliably failing issue (head of scan)", "someone", nil),
		intgIssue(goodNum, "Healthy second issue", "someone", nil),
		intgIssue(999, "Completed, in cooldown", "someone", nil),
	)
	first := hub.selectTask(conn)
	if first == nil || first.Number != poisonNum {
		t.Fatalf("expected poison issue #%d first (no failures yet), got %+v", poisonNum, first)
	}
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()

	// Drive the poison issue's failure count directly to
	// consecutiveFailureQuarantineThreshold (mirroring the real client flow:
	// each round the contributor is handed it, fails it, and the ledger
	// advances) rather than relying on selectTask to keep re-offering it —
	// once its own short cooldown is active it is legitimately NOT re-offered
	// (that is remedy 1 working), so the queue naturally serves #2 in between.
	// What we're proving here is the CONSEQUENCE: after enough failures the
	// issue crosses into quarantine and the queue keeps moving throughout.
	for i := 0; i < consecutiveFailureQuarantineThreshold; i++ {
		hub.recordTaskFailure(poisonRepo, poisonNum, false)
	}

	// Livelock check at the first failure: with the poison issue in its short
	// cooldown, the very next selection must serve the healthy issue instead
	// of starving (this is remedy 1 in isolation — the head-of-scan issue no
	// longer monopolizes the queue after a single failure).
	setStatusIssues(s,
		intgIssue(poisonNum, "Reliably failing issue (short cooldown)", "someone", nil),
		intgIssue(goodNum, "Healthy second issue", "someone", nil),
	)
	afterFirstFailure := hub.selectTask(conn)
	if afterFirstFailure == nil {
		t.Fatalf("expected the healthy issue to be offered right after the first failure, got nil (queue starved)")
	}
	if afterFirstFailure.Number != goodNum {
		t.Fatalf("expected healthy issue #%d offered while #%d is in its short failure cooldown, got #%d",
			goodNum, poisonNum, afterFirstFailure.Number)
	}
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()

	// After consecutiveFailureQuarantineThreshold failures the poison issue
	// must be quarantined for the LONG window, not just short-cooldown.
	if !hub.isTaskInFailureCooldown(poisonRepo, poisonNum) {
		t.Fatalf("expected poison issue quarantined after %d consecutive failures", consecutiveFailureQuarantineThreshold)
	}

	// The queue must still be able to make forward progress: the healthy
	// second issue is offered instead of starving behind the poison one.
	setStatusIssues(s,
		intgIssue(poisonNum, "Reliably failing issue (quarantined)", "someone", nil),
		intgIssue(goodNum, "Healthy second issue", "someone", nil),
	)
	final := hub.selectTask(conn)
	if final == nil {
		t.Fatalf("expected the healthy issue to be offered, got nil (queue starved by quarantined poison issue)")
	}
	if final.Number != goodNum {
		t.Fatalf("expected healthy issue #%d offered while #%d is quarantined, got #%d", goodNum, poisonNum, final.Number)
	}
}
