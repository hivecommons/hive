package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ready-work queue play/pause control (Operations tab). This is NOT a second,
// independent toggle — it is the SAME contribute_suspended state as the
// Management "Suspend contributions" switch (#admin-suspend-switch), surfaced a
// second place on the queue header. Both read/write through the one
// setContributeSuspended() JS handler, which PUTs the existing
// PUT /api/config/governor/hub endpoint (the same call the Management switch
// always used) and re-renders BOTH surfaces from the single result. These tests
// pin: (1) the markup exists and is wired to the shared handler, not a parallel
// PUT; (2) the queue's status pill reads from the same policy.suspended field
// GET /api/contribute/fleet already exposes; (3) the server boundary — a "read"
// viewer still gets 403 on the governor-hub PUT, matching the Management path.

// TestQueuePlayPauseMarkupPresent asserts the /contribute page ships the queue
// play/pause button + status pill, and that they are wired to the SAME shared
// handler and endpoint as the Management suspend switch — not a separate PUT.
func TestQueuePlayPauseMarkupPresent(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// The control + the read-only status pill live on the queue card header.
		`id="queue-suspend-btn"`,
		`id="queue-suspend-pill"`,
		`Ready-work queue`,
		// One shared handler/state, not a parallel implementation: both the
		// Management switch and the queue button route through this function.
		`function setContributeSuspended(`,
		`function renderQueueSuspendControl(`,
		`onEl('queue-suspend-btn','click'`,
		// The shared handler persists through the SAME endpoint the Management
		// switch (and the Governor Hub dialog) already use — no new endpoint.
		`contribute_suspended:next`,
		`/api/config/governor/hub`,
		// The Management switch's own click routes through the shared handler
		// too (not two independent PUTs for the same field).
		`key==='contribute_suspended'`,
		`setContributeSuspended(next,'management')`,
		// The queue's status is re-derived from the read-only policy snapshot on
		// every poll tick, so it converges even if the change came from elsewhere.
		`renderQueueSuspendControl(adminHub?!!adminHub.contribute_suspended:!!p.suspended)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue play/pause markup/wiring missing %q", want)
		}
	}

	// The button must start hidden (display:none) — it only appears once
	// initAdmin() resolves owner/read-write via /api/role, same as the rest of
	// the admin controls. A read viewer must never get a usable control.
	i := strings.Index(body, `id="queue-suspend-btn"`)
	if i < 0 {
		t.Fatal("queue-suspend-btn not found")
	}
	// Look at the tag surrounding the id to confirm it ships hidden by default.
	tagStart := strings.LastIndex(body[:i], "<button")
	tagEnd := strings.Index(body[tagStart:], ">")
	if tagStart < 0 || tagEnd < 0 || !strings.Contains(body[tagStart:tagStart+tagEnd], `style="display:none"`) {
		t.Error("queue-suspend-btn must start hidden (display:none) until the owner/read-write role check resolves")
	}
}

// TestQueuePlayPauseSharesGovernorHubPUT proves the queue control's declared
// wire path actually round-trips through the real handler: PUTting
// contribute_suspended via /api/config/governor/hub (exactly what
// setContributeSuspended's fetch call sends) flips Config.Hub.ContributeSuspended,
// and GET /api/contribute/fleet's policy.suspended (what renderPolicy uses to
// re-sync the queue pill every tick) reflects it immediately — proving the queue
// and Management surfaces read/write the identical field with no separate state.
func TestQueuePlayPauseSharesGovernorHubPUT(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeSuspended = false

	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub",
		strings.NewReader(`{"contribute_suspended":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorHub(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("governor hub update got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !deps.Config.Hub.ContributeSuspended {
		t.Fatal("contribute_suspended not persisted by handleGovernorHub")
	}

	// The policy the queue re-syncs from (buildContributeAdmissionPolicy, served
	// by /api/contribute/fleet) must reflect the SAME field.
	p := s.buildContributeAdmissionPolicy()
	if !p.Suspended {
		t.Error("ContributeAdmissionPolicy.Suspended did not reflect the governor-hub PUT — queue and Management would show different states")
	}

	// Flip back through the same endpoint and confirm both sides track it.
	req2 := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub",
		strings.NewReader(`{"contribute_suspended":false}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	markOwnerRequest(req2)
	s.handleGovernorHub(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("governor hub resume got %d, want 200 (body: %s)", w2.Code, w2.Body.String())
	}
	if deps.Config.Hub.ContributeSuspended {
		t.Fatal("contribute_suspended still true after resume PUT")
	}
	if s.buildContributeAdmissionPolicy().Suspended {
		t.Error("policy.Suspended still true after resume — queue would show stale paused state")
	}
}

// TestQueuePlayPause_ReadViewerForbidden proves the queue control's PUT path
// (the SAME PUT /api/config/governor/hub the Management switch uses) still 403s
// a read viewer server-side, independent of the UI hiding the button. This is
// the same boundary TestGovernorHubSave_ReadViewerForbidden pins for the
// Management surface; asserting it again here documents that adding a second
// UI placement did not open a second, weaker path to the same mutation.
func TestQueuePlayPause_ReadViewerForbidden(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeSuspended = false

	h := s.Handler()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub",
		strings.NewReader(`{"contribute_suspended":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("read viewer PUT /api/config/governor/hub got %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if deps.Config.Hub.ContributeSuspended {
		t.Error("read viewer mutated Config.Hub.ContributeSuspended via the queue's wire path")
	}
}

// TestQueuePlayPause_FleetPolicyExposesSuspendedForReadViewer proves a read /
// anonymous viewer can still SEE the paused/active status on the queue (it's
// read-only info per the spec) even though they cannot toggle it: GET
// /api/contribute/fleet (the endpoint opsPoll() uses, unauthenticated-safe on
// this public page) must carry policy.suspended.
func TestQueuePlayPause_FleetPolicyExposesSuspendedForReadViewer(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeSuspended = true

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	req.Header.Set("X-Hive-Role", "read")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("read viewer GET /api/contribute/fleet got %d, want 200 (status must be visible to all)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"suspended":true`) {
		t.Errorf("fleet response missing suspended:true for read viewer; body: %s", w.Body.String())
	}
}
