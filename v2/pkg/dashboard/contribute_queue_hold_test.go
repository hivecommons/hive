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

// ── Operator HOLD / Resume (#queue-hold) + Move-to-bottom + spinner fix ─────────
// A hold is a MANUAL, PERSISTENT operator decision — distinct from the time-based
// cooldown — that parks an issue so it is never offered until Resumed. These tests
// pin the server behaviour (excluded from ReadyQueue's offerable set AND selectTask,
// surfaced as held for display, round-trips through persistence, auth-gated) and the
// dashboard structure (menu items + on-hold badge/class).

// TestReadyQueueExcludesHeldFromOffer proves a held issue is NOT in ReadyQueue's
// offerable set: it is surfaced with Held=true (so the operator can see/Resume it)
// but never counts as offerable work, and it trails the offerable items.
func TestReadyQueueExcludesHeldFromOffer(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3) // issues 1..3
	s.statusMu.Unlock()

	// Hold #2.
	s.deps.Config.Hub.ContributeQueueHold = []string{"acme/repo#2"}

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 3 {
		t.Fatalf("expected all 3 items surfaced (held one included but tagged), got %d: %+v", len(q), q)
	}

	// The offerable items (Held=false) must be #1 and #3, in scan order, ahead of any
	// held item. #2 must be present but tagged Held, and it must trail the offerable.
	if q[0].Number != 1 || q[0].Held {
		t.Fatalf("first offerable item should be #1 (not held), got %+v", q[0])
	}
	if q[1].Number != 3 || q[1].Held {
		t.Fatalf("second offerable item should be #3 (not held), got %+v", q[1])
	}
	if q[2].Number != 2 || !q[2].Held {
		t.Fatalf("held #2 should trail, tagged Held=true, got %+v", q[2])
	}
}

// TestReadyQueueResumeReappears proves clearing the hold puts the issue back into
// the offerable set (untagged), in its normal scan position.
func TestReadyQueueResumeReappears(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 3)
	s.statusMu.Unlock()

	// Held, then resumed (empty hold set).
	s.deps.Config.Hub.ContributeQueueHold = nil
	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 3 {
		t.Fatalf("expected 3 offerable items after resume, got %d", len(q))
	}
	for _, it := range q {
		if it.Held {
			t.Fatalf("no item should be tagged Held after resume: %+v", it)
		}
	}
	if q[0].Number != 1 || q[1].Number != 2 || q[2].Number != 3 {
		t.Fatalf("resumed queue lost scan order: %d,%d,%d", q[0].Number, q[1].Number, q[2].Number)
	}
}

// TestSelectTaskExcludesHeld proves selectTask never offers a held issue — the
// operator's parked issue is skipped and the clean one is offered instead, and once
// resumed the held issue becomes selectable again.
func TestSelectTaskExcludesHeld(t *testing.T) {
	hub, s := covK2Hub(t)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	setStatusIssues(s,
		intgIssue(1, "held issue", "bob", nil),
		intgIssue(2, "clean issue", "bob", nil),
	)

	// Hold #1 (canonical key myorg/repo1#1). It must never be offered.
	s.deps.Config.Hub.ContributeQueueHold = []string{"myorg/repo1#1"}
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected a task_assign for the non-held issue, got %+v", msg)
	}
	if msg.Number == 1 {
		t.Fatalf("held issue #1 was offered; it must be excluded")
	}
	if msg.Number != 2 {
		t.Fatalf("expected the clean #2, got #%d", msg.Number)
	}

	// Resume #1, clear the assignment, and prove #1 is now selectable again.
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()
	s.deps.Config.Hub.ContributeQueueHold = nil
	// Also hold #2 so only #1 is admissible, forcing the selector to pick #1.
	s.deps.Config.Hub.ContributeQueueHold = []string{"myorg/repo1#2"}
	msg = hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 1 {
		t.Fatalf("after resume, #1 should be selectable, got %+v", msg)
	}
}

// TestQueueHoldEndpointPersistsRoundTrip proves the hold set survives through its
// persistence field: a POST hold=true stores the canonical key, a follow-up
// hold=false removes it, and the endpoint sanitises malformed prior state.
func TestQueueHoldEndpointPersistsRoundTrip(t *testing.T) {
	s, deps := apiServer(t)

	// Pre-seed with a malformed/duplicate straggler to prove sanitisation.
	deps.Config.Hub.ContributeQueueHold = []string{"not a key", "acme/repo#9", "acme/repo#9"}

	// Hold acme/repo#4 (owner role — mutation gate fails closed on missing role, C5).
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
		strings.NewReader(`{"key":"acme/repo#4","held":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("hold: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	got := deps.Config.Hub.ContributeQueueHold
	// The malformed key is dropped; the valid pre-existing #9 is kept; #4 is added.
	want := map[string]bool{"acme/repo#9": true, "acme/repo#4": true}
	if len(got) != len(want) {
		t.Fatalf("after hold, persisted set = %v, want keys %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("unexpected persisted key %q (full: %v)", k, got)
		}
	}

	// Resume acme/repo#4 → removed; #9 remains.
	req = httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
		strings.NewReader(`{"key":"acme/repo#4","held":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	got = deps.Config.Hub.ContributeQueueHold
	if len(got) != 1 || got[0] != "acme/repo#9" {
		t.Fatalf("after resume, expected only [acme/repo#9], got %v", got)
	}

	// Response echoes the held flag and the cleaned set.
	var resp struct {
		OK   bool     `json:"ok"`
		Held bool     `json:"held"`
		Hold []string `json:"hold"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.Held {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestQueueHoldEndpointRoleGate proves the hold endpoint is owner/read-write ONLY,
// enforced server-side exactly like the queue-order endpoint: a "read" caller 403s.
func TestQueueHoldEndpointRoleGate(t *testing.T) {
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
			s, _ := apiServer(t)
			req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
				strings.NewReader(`{"key":"acme/repo#1","held":true}`))
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

// TestQueueHoldEndpointRejectsBadKey proves a malformed issue key is rejected so the
// stored hold set can never carry a non-canonical entry that would silently miss the
// selectTask exclusion (the #2648 class of bug).
func TestQueueHoldEndpointRejectsBadKey(t *testing.T) {
	s, _ := apiServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold",
		strings.NewReader(`{"key":"not a canonical key","held":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner") // pass the write gate to reach key validation (C5)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed key: got %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}

// TestQueueMoveToBottomAndHoldControlsPresent pins the dashboard structure for the
// three menu additions: Move-to-bottom, Hold/Resume, and the on-hold badge/class.
func TestQueueMoveToBottomAndHoldControlsPresent(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`data-act="bottom"`,                    // Move-to-bottom menu item
		`Move to bottom`,                       // its label
		`function ccMoveToBottom(key){`,        // wired mover
		`ccMoveToPosition(key,ccQueue.length)`, // sends the item to the last slot
		`data-act="hold"`,                      // Hold/Resume menu item
		`function ccToggleHold(key,held){`,     // wired toggle
		`/api/contribute/queue/hold`,           // calls the hold endpoint
		`cc-q-held`,                            // dimmed held-row class
		`cc-q-held-tag`,                        // on-hold badge class
		`on hold`,                              // badge text
		`color-scheme:light dark`,              // theme-aware spinner-arrow fix (visible in both light + dark; #2612)
		`.cc-q-moverow input[type=number]`,     // spinner fix targets the number input
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue hold/bottom controls missing marker %q", want)
		}
	}

	// The Hold/Resume action must be gated on adminEnabled (owner/read-write) — it
	// travels through ccQueueMenuHTML, which is only emitted when adminEnabled.
	if !strings.Contains(body, "var menu=adminEnabled?ccQueueMenuHTML(") {
		t.Error("per-row hold/resume action is not gated on adminEnabled (owner/read-write only)")
	}
}
