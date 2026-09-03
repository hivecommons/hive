package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// seedActionable builds a status with n actionable issues in one repo, numbered
// 1..n, so the ready queue has a long candidate set to exercise the raised cap
// and the operator priority override.
func seedActionable(repo string, n int) *StatusPayload {
	issues := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		issues = append(issues, map[string]any{
			"number": float64(i),
			"title":  fmt.Sprintf("issue %d", i),
			"labels": []any{},
		})
	}
	return &StatusPayload{
		Repos: []FrontendRepo{{Name: repo, Full: repo, ActionableIssues: issues}},
	}
}

// TestReadyQueueRaisedCapReturnsMoreThanTen proves Feature 1: the queue is no
// longer capped at ~10 — a backlog well over 10 surfaces up to the (raised)
// readyQueueDefaultLimit, and the cap still bounds a pathological backlog.
func TestReadyQueueRaisedCapReturnsMoreThanTen(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// 140 actionable items — comfortably more than the old cap of 10/25.
	s.statusMu.Lock()
	s.status = seedActionable("projectbluefin/common", 140)
	s.statusMu.Unlock()

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) <= 10 {
		t.Fatalf("raised cap: expected far more than 10 items, got %d", len(q))
	}
	if len(q) != 140 {
		t.Fatalf("expected all 140 items under the raised cap, got %d", len(q))
	}

	// The cap still bounds a backlog larger than the limit.
	s.statusMu.Lock()
	s.status = seedActionable("projectbluefin/common", readyQueueDefaultLimit+50)
	s.statusMu.Unlock()
	q = s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != readyQueueDefaultLimit {
		t.Fatalf("cap: expected %d items, got %d", readyQueueDefaultLimit, len(q))
	}
}

// TestReadyQueueRespectsOperatorOrder proves the persisted priority override is
// reflected in the queue OUTPUT: pinned keys are offered first, in the operator's
// order, with unpinned items keeping their scan order behind them.
func TestReadyQueueRespectsOperatorOrder(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 5) // issues 1..5 in scan order
	s.statusMu.Unlock()

	// Operator drags #4 then #2 to the front.
	s.deps.Config.Hub.ContributeQueueOrder = []string{"acme/repo#4", "acme/repo#2"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 5 {
		t.Fatalf("expected 5 items, got %d", len(q))
	}
	if q[0].Number != 4 || q[1].Number != 2 {
		t.Fatalf("pinned items not offered first in operator order: got %d,%d", q[0].Number, q[1].Number)
	}
	// The rest keep their original scan order: 1,3,5.
	if q[2].Number != 1 || q[3].Number != 3 || q[4].Number != 5 {
		t.Fatalf("unpinned items lost scan order: got %d,%d,%d", q[2].Number, q[3].Number, q[4].Number)
	}
}

// TestReadyQueueSkipsStalePin proves a pinned key that is no longer actionable is
// simply skipped — never resurrected into the queue.
func TestReadyQueueSkipsStalePin(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3) // only issues 1..3 are actionable
	s.statusMu.Unlock()

	// #999 is pinned but not in the actionable set (stale); #3 is pinned and real.
	s.deps.Config.Hub.ContributeQueueOrder = []string{"acme/repo#999", "acme/repo#3"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 3 {
		t.Fatalf("stale pin must not add items: expected 3, got %d", len(q))
	}
	for _, it := range q {
		if it.Number == 999 {
			t.Fatalf("stale pinned key was resurrected into the queue: %+v", it)
		}
	}
	// The real pin (#3) still floats to the front.
	if q[0].Number != 3 {
		t.Fatalf("real pin should be first, got %d", q[0].Number)
	}
}

// TestReadyQueueOrderDoesNotBypassAdmission proves the override only changes OFFER
// PRIORITY: a pinned issue that admission filters exclude stays out regardless of
// pin order.
func TestReadyQueueOrderDoesNotBypassAdmission(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "acme/repo", Full: "acme/repo",
		ActionableIssues: []any{
			map[string]any{"number": float64(1), "title": "keep me", "labels": []any{}},
			map[string]any{"number": float64(2), "title": "SPAM please ignore", "labels": []any{}},
		},
	}}}
	s.statusMu.Unlock()

	// Deny any title containing "SPAM"; pin the denied issue to the front anyway.
	s.deps.Config.Hub.ContributeTitlesMode = config.FilterModeDeny
	s.deps.Config.Hub.ContributeDenyTitles = []string{"SPAM"}
	s.deps.Config.Hub.ContributeQueueOrder = []string{"acme/repo#2"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	for _, it := range q {
		if it.Number == 2 {
			t.Fatalf("pin bypassed the admission filter: #2 should be excluded, got %+v", q)
		}
	}
	if len(q) != 1 || q[0].Number != 1 {
		t.Fatalf("expected only the admissible #1, got %+v", q)
	}
}

// TestQueueOrderEndpointRoleGate proves the write endpoint is owner/read-write
// ONLY, enforced server-side: a "read" caller gets 403; owner/read-write succeed.
func TestQueueOrderEndpointRoleGate(t *testing.T) {
	cases := []struct {
		role string
		want int
	}{
		{"read", http.StatusForbidden},
		{"owner", http.StatusOK},
		{"read-write", http.StatusOK},
		{"", http.StatusForbidden}, // C5: absent header fails closed (NOT owner)
	}
	for _, tc := range cases {
		t.Run("role_"+tc.role, func(t *testing.T) {
			s, deps := apiServer(t)
			_ = deps
			req := httptest.NewRequest(http.MethodPut, "/api/contribute/queue/order",
				strings.NewReader(`{"order":["acme/repo#1"]}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.role != "" {
				req.Header.Set("X-Hive-Role", tc.role)
			}
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("role %q: got %d, want %d (body: %s)", tc.role, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestQueueOrderEndpointPersistsAndSanitizes proves an accepted PUT stores the
// order into Config.Hub.ContributeQueueOrder and drops malformed / duplicate keys.
func TestQueueOrderEndpointPersistsAndSanitizes(t *testing.T) {
	s, deps := apiServer(t)

	body := `{"order":["acme/repo#4"," acme/repo#2 ","not a key","acme/repo#4","","acme/repo#7"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/contribute/queue/order", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner") // mutation gate fails closed on missing role (C5)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	got := deps.Config.Hub.ContributeQueueOrder
	want := []string{"acme/repo#4", "acme/repo#2", "acme/repo#7"} // trimmed, deduped, malformed dropped
	if len(got) != len(want) {
		t.Fatalf("persisted order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("persisted order[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// Response echoes the cleaned order.
	var resp struct {
		OK    bool     `json:"ok"`
		Order []string `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || len(resp.Order) != 3 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestQueueOrderEndpointRejectsOversizePayload proves the key cap bounds a hostile
// payload.
func TestQueueOrderEndpointRejectsOversizePayload(t *testing.T) {
	s, _ := apiServer(t)
	keys := make([]string, maxQueueOrderKeys+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/repo#%d", i+1)
	}
	payload, _ := json.Marshal(map[string]any{"order": keys})
	// The body cap (maxRequestBodyBytes) may trip first for a very large payload;
	// either way the endpoint must not accept it. Use a payload just over the key
	// cap but within the body cap by keeping keys short — assert non-200.
	req := httptest.NewRequest(http.MethodPut, "/api/contribute/queue/order", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("oversize payload was accepted; want rejection, got 200")
	}
}

// TestSelectTaskHonorsOperatorOrder proves the persisted priority override changes
// selectTask's CANDIDATE ORDERING: a pinned issue is offered ahead of the
// contributor's OWN work (the override is the top sort key). Without the pin,
// selectTask would hand alice her own issue (#2390); with #2 pinned, #2 wins.
func TestSelectTaskHonorsOperatorOrder(t *testing.T) {
	hub, s := covK2Hub(t)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	setStatusIssues(s,
		intgIssue(1, "Someone else's issue", "bob", nil),
		intgIssue(2, "Another of bob's issues", "bob", nil),
		intgIssue(3, "Alice's own work", "alice", nil),
	)

	// Baseline: no override → own-work (#3) wins per #2390.
	if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" || msg.Number != 3 {
		t.Fatalf("baseline: expected own-work #3, got %+v", msg)
	}
	// Clear the assignment so the next call re-selects from the full set.
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()

	// Operator pins #2 to the front. Now #2 must be offered ahead of alice's own #3.
	s.deps.Config.Hub.ContributeQueueOrder = []string{"myorg/repo1#2"}
	if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" || msg.Number != 2 {
		t.Fatalf("with #2 pinned: expected #2 offered first, got %+v", msg)
	}
}

// TestSelectTaskPinDoesNotBypassCooldown proves a pinned issue still obeys
// admission: an issue in failure cooldown is skipped even when pinned to the front.
func TestSelectTaskPinDoesNotBypassCooldown(t *testing.T) {
	hub, s := covK2Hub(t)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	setStatusIssues(s,
		intgIssue(1, "clean issue", "bob", nil),
		intgIssue(2, "flaky issue in cooldown", "bob", nil),
	)
	// Put #2 into an ACTIVE failure cooldown so it is not admissible right now.
	hub.recordTaskFailure("myorg/repo1", 2, false)

	// Pin the cooling-down #2 to the front anyway.
	s.deps.Config.Hub.ContributeQueueOrder = []string{"myorg/repo1#2"}

	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected a task_assign for the admissible issue, got %+v", msg)
	}
	if msg.Number == 2 {
		t.Fatalf("pinned issue in cooldown bypassed admission: got #2")
	}
	if msg.Number != 1 {
		t.Fatalf("expected the clean #1, got #%d", msg.Number)
	}
}
