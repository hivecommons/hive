package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Queue hold follow-ups: on-hold header count + resume-all + hold reason ───────
// These pin the three enhancements layered on the operator HOLD feature (#queue-hold):
//   (1) held_count — the fleet payload tally the header surfaces as "N on hold";
//   (2) resume-all — POST /api/contribute/queue/hold/clear drops the whole hold set;
//   (3) hold reason — an OPTIONAL note stored with a hold and surfaced on the badge.

// TestHeldCount_ReflectsActionableHoldSet proves HeldCount() counts only held issues
// that are STILL in the actionable universe (would be offerable but for the hold),
// mirroring what ReadyQueue surfaces as Held — a hold key for a no-longer-actionable
// issue does not inflate the count.
func TestHeldCount_ReflectsActionableHoldSet(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3) // issues 1..3
	s.statusMu.Unlock()

	// Hold #2 (actionable) and a stale key for a non-actionable #99.
	s.deps.Config.Hub.ContributeQueueHold = []string{"acme/repo#2", "acme/repo#99"}

	if got := s.contributeHub.HeldCount(); got != 1 {
		t.Fatalf("HeldCount = %d, want 1 (only the actionable #2 counts; stale #99 does not)", got)
	}

	// No holds → 0.
	s.deps.Config.Hub.ContributeQueueHold = nil
	if got := s.contributeHub.HeldCount(); got != 0 {
		t.Fatalf("HeldCount = %d, want 0 with an empty hold set", got)
	}
}

// TestContributeFleet_IncludesHeldCount asserts the /api/contribute/fleet JSON carries
// the held_count field reflecting the hold set, so the queue header can render "N on hold".
func TestContributeFleet_IncludesHeldCount(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3)
	s.statusMu.Unlock()
	s.deps.Config.Hub.ContributeQueueHold = []string{"acme/repo#1", "acme/repo#3"}

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		HeldCount *int `json:"held_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.HeldCount == nil {
		t.Fatalf("fleet payload missing held_count: %s", w.Body.String())
	}
	if *resp.HeldCount != 2 {
		t.Fatalf("held_count = %d, want 2", *resp.HeldCount)
	}
}

// TestQueueHoldClearEndpoint_ClearsAll proves resume-all drops the ENTIRE hold set
// (and its reason map) and round-trips through persistence.
func TestQueueHoldClearEndpoint_ClearsAll(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeQueueHold = []string{"acme/repo#1", "acme/repo#2"}
	deps.Config.Hub.ContributeQueueHoldReasons = map[string]string{"acme/repo#1": "flaky"}

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold/clear", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(deps.Config.Hub.ContributeQueueHold) != 0 {
		t.Fatalf("hold set not cleared: %v", deps.Config.Hub.ContributeQueueHold)
	}
	if len(deps.Config.Hub.ContributeQueueHoldReasons) != 0 {
		t.Fatalf("hold reasons not cleared: %v", deps.Config.Hub.ContributeQueueHoldReasons)
	}
	var resp struct {
		OK      bool `json:"ok"`
		Cleared int  `json:"cleared"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.Cleared != 2 {
		t.Fatalf("unexpected clear response: %+v", resp)
	}
}

// TestQueueHoldClearEndpoint_RoleGate proves resume-all is owner/read-write ONLY:
// a "read" caller 403s, exactly like the single-issue hold endpoint.
func TestQueueHoldClearEndpoint_RoleGate(t *testing.T) {
	cases := []struct {
		role string
		want int
	}{
		{"read", http.StatusForbidden},
		{"owner", http.StatusOK},
		{"read-write", http.StatusOK},
		{"", http.StatusOK}, // absent header = local/dev owner
	}
	for _, tc := range cases {
		t.Run("role_"+tc.role, func(t *testing.T) {
			s, _ := apiServer(t)
			req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold/clear", nil)
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

// TestQueueHoldReason_StoreSurfaceClear proves the OPTIONAL hold reason round-trips:
// a hold=true with a reason stores it under the canonical key and ReadyQueue surfaces
// it on the held item; resuming (hold=false) drops the reason.
func TestQueueHoldReason_StoreSurfaceClear(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3)
	s.statusMu.Unlock()

	// Hold #2 WITH a reason.
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
		strings.NewReader(`{"key":"acme/repo#2","held":true,"reason":"waiting on upstream"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("hold-with-reason: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if r := s.deps.Config.Hub.ContributeQueueHoldReasons["acme/repo#2"]; r != "waiting on upstream" {
		t.Fatalf("stored reason = %q, want %q", r, "waiting on upstream")
	}

	// The held ReadyQueueItem must carry the reason.
	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	var found bool
	for _, it := range q {
		if it.Number == 2 && it.Held {
			found = true
			if it.HeldReason != "waiting on upstream" {
				t.Fatalf("held item HeldReason = %q, want %q", it.HeldReason, "waiting on upstream")
			}
		}
	}
	if !found {
		t.Fatalf("held #2 not surfaced in ReadyQueue: %+v", q)
	}

	// Resume #2 → the reason is dropped.
	req = httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
		strings.NewReader(`{"key":"acme/repo#2","held":false}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if _, ok := s.deps.Config.Hub.ContributeQueueHoldReasons["acme/repo#2"]; ok {
		t.Fatalf("reason for #2 should be dropped on resume, still present: %v", s.deps.Config.Hub.ContributeQueueHoldReasons)
	}
}

// TestQueueHoldReason_OptionalNoNote proves holding WITHOUT a reason works exactly as
// before: the hold is stored and no reason entry is created.
func TestQueueHoldReason_OptionalNoNote(t *testing.T) {
	s, deps := apiServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
		strings.NewReader(`{"key":"acme/repo#5","held":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("hold-no-reason: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(deps.Config.Hub.ContributeQueueHold) != 1 || deps.Config.Hub.ContributeQueueHold[0] != "acme/repo#5" {
		t.Fatalf("hold set = %v, want [acme/repo#5]", deps.Config.Hub.ContributeQueueHold)
	}
	if len(deps.Config.Hub.ContributeQueueHoldReasons) != 0 {
		t.Fatalf("no reason expected, got %v", deps.Config.Hub.ContributeQueueHoldReasons)
	}
}

// TestQueueHoldFollowupControlsPresent pins the dashboard structure for the three
// enhancements: the "on hold" header segment, the resume-all affordance, and the
// reason input + tooltip wiring.
func TestQueueHoldFollowupControlsPresent(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// (1) on-hold header segment sourced from the fleet payload.
		`ccHeldCount+' on hold'`,
		`data.held_count`,
		// (2) resume-all affordance: button, wired handler, bulk-clear endpoint.
		`queue-resume-all-btn`,
		`function ccResumeAll(`,
		`/api/contribute/queue/hold/clear`,
		`function ccRenderResumeAll(`,
		// resume-all is gated on adminEnabled (owner/read-write only).
		`if(adminEnabled&&ccHeldCount>0)`,
		// (3) hold reason: inline input, passed to the toggle, surfaced in the tooltip.
		`cc-q-holdreason-input`,
		`function ccToggleHold(key,held,reason)`,
		`q.held_reason`,
		`'On hold — '+heldReason`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue hold follow-up controls missing marker %q", want)
		}
	}
}
