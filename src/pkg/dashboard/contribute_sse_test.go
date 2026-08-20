package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Operations command center: SSE stream + ready-work queue ─────────────────
//
// These tests pin the READ-ONLY command-center backend:
//   1. /api/contribute/events is PUBLIC (isPublicPath) and streams an appended
//      ActivityEntry to a subscriber, then cleans the subscriber up on disconnect
//      (no leaked subscriber/goroutine).
//   2. ReadyQueue reflects the ActionableIssues set.
//   3. The rendered /contribute page ships the command-center markup.
//   4. The public-page / gated-controls invariant holds: an anonymous (read)
//      viewer gets the stream + queue but 403 on the control endpoints.

// TestSSEEventsIsPublic asserts the stream path is in isPublicPath so anonymous
// viewers may subscribe — the whole clanker page is public, and this stream is
// read-only info.
func TestSSEEventsIsPublic(t *testing.T) {
	for _, p := range []string{"/api/contribute/events", "/api/contribute/queue"} {
		if !isPublicPath(p) {
			t.Errorf("%s must be public (read-only viewers subscribe with no auth)", p)
		}
	}
}

// syncRecorder wraps httptest.ResponseRecorder with a mutex so the test goroutine
// can poll the body while the SSE handler goroutine is still writing to it.
// httptest.ResponseRecorder is NOT safe for concurrent use — reading rec.Body
// directly while handleContributeEvents streams into it is a data race (#2581).
// It implements http.Flusher (delegating to the recorder) so the handler streams.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{rec: httptest.NewRecorder()}
}

func (s *syncRecorder) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header()
}

func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(b)
}

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

// BodyString returns the body accumulated so far, synchronized with the writer.
func (s *syncRecorder) BodyString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

// TestSSEStreamsAppendedActivity connects a subscriber, appends an activity event,
// and asserts the event is streamed as an SSE "data:" frame. Then it cancels the
// request context and asserts the subscriber is unregistered (leak-safe cleanup).
func TestSSEStreamsAppendedActivity(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	hub := s.contributeHub

	if got := hub.sse.count(); got != 0 {
		t.Fatalf("expected 0 subscribers at start, got %d", got)
	}

	// syncRecorder implements http.Flusher, so the handler streams; its mutex makes
	// concurrent handler writes + test-side body polls race-free.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newSyncRecorder()

	done := make(chan struct{})
	go func() {
		s.handleContributeEvents(rec, req)
		close(done)
	}()

	// Wait for the handler to register its subscriber (it does so before blocking).
	waitFor(t, func() bool { return hub.sse.count() == 1 }, "subscriber to register")

	// Append a real activity event — this is the exact fan-in point join/leave/
	// pick-up/complete all funnel through.
	hub.addActivity("kylerankin", "picked up", "contributor", "claude", "sonnet", "", "projectbluefin/common#796")

	// The event must reach the stream as an SSE data frame carrying the username.
	waitFor(t, func() bool {
		b := rec.BodyString()
		return strings.Contains(b, "kylerankin") && strings.Contains(b, "data:")
	}, "activity event to stream to subscriber")

	// Disconnect: cancelling the context must tear the handler down AND unregister
	// the subscriber — no leak.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel (possible goroutine leak)")
	}
	if got := hub.sse.count(); got != 0 {
		t.Fatalf("subscriber not cleaned up after disconnect: %d still registered (leak)", got)
	}
}

// TestSSEHelloReplaysRecentActivity asserts a fresh subscriber gets a "hello" frame
// so the page is not blank on load — recent activity is replayed.
func TestSSEHelloReplaysRecentActivity(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	hub := s.contributeHub
	hub.addActivity("castrojo", "completed", "trusted", "claude", "opus", "", "projectbluefin/common#707")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newSyncRecorder()
	go s.handleContributeEvents(rec, req)

	waitFor(t, func() bool {
		b := rec.BodyString()
		return strings.Contains(b, `"type":"hello"`) && strings.Contains(b, "castrojo")
	}, "hello frame with replayed activity")
}

// TestReadyQueueReflectsActionableIssues seeds a status with ActionableIssues and
// asserts ReadyQueue surfaces them (repo/number/title), i.e. the queue read maps to
// the real candidate set.
func TestReadyQueueReflectsActionableIssues(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "common", Full: "projectbluefin/common",
				ActionableIssues: []any{
					map[string]any{"number": float64(796), "title": "Fix the widget", "labels": []any{"bug"}},
					map[string]any{"number": float64(707), "title": "Add docs", "labels": []any{}},
				},
			},
		},
	}
	s.statusMu.Unlock()

	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 2 {
		t.Fatalf("expected 2 ready-queue items, got %d: %+v", len(q), q)
	}
	sortReadyQueue(q)
	if q[0].Repo != "projectbluefin/common" || q[0].Number != 707 {
		t.Errorf("unexpected first item: %+v", q[0])
	}
	if q[1].Number != 796 || q[1].Title != "Fix the widget" {
		t.Errorf("unexpected second item: %+v", q[1])
	}

	// The public JSON queue endpoint returns the same set.
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue endpoint: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "projectbluefin/common") {
		t.Errorf("queue endpoint missing seeded repo: %s", rec.Body.String())
	}
}

// TestReadyQueueExcludesInFlight asserts an issue already being worked is NOT shown
// as "ready" (it is not waiting to be picked off) — mirroring selectTask.
func TestReadyQueueExcludesInFlight(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	hub := s.contributeHub

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "common", Full: "projectbluefin/common",
			ActionableIssues: []any{
				map[string]any{"number": float64(796), "title": "Fix the widget"},
			},
		}},
	}
	s.statusMu.Unlock()

	// Simulate the issue being in flight on a connected clanker.
	hub.mu.Lock()
	hub.connections["c1"] = &ContributorConnection{
		currentTask: &WSTaskAssign{Repo: "projectbluefin/common", Number: 796},
	}
	hub.mu.Unlock()

	q := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 0 {
		t.Fatalf("in-flight issue should not be in the ready queue, got %+v", q)
	}
}

// TestCommandCenterMarkupRendered asserts the /contribute page ships the
// command-center markup (queue, dev-log, army framing, live pill, SSE wiring).
func TestCommandCenterMarkupRendered(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="cc-queue"`,          // ready-work queue
		`id="cc-log"`,            // dev-log narration
		`id="cc-army"`,           // army framing
		`id="cc-live"`,           // live/poll status pill
		`id="cc-ach-wrap"`,       // achievement pops
		`/api/contribute/events`, // SSE wiring
		`EventSource`,            // stream client
		`/api/contribute/queue`,  // graceful poll fallback
		`function ccTravel`,      // task-assign travel animation
		`Ready-work queue`,       // queue heading
		`Live Activity`,          // rail heading (renamed, consistent with Onboarding)
		`Your army`,              // army heading
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing command-center marker %q", want)
		}
	}
	// The gated controls invariant: the per-clanker admin actions + role gate must
	// still be present (command center did not weaken them).
	for _, want := range []string{`initAdmin`, `/api/role`, `role!=='owner'&&role!=='read-write'`} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page lost the role gate marker %q (controls must stay gated)", want)
		}
	}
}

// TestCommandCenterPublicButControlsGated proves the invariant end-to-end through
// the real Handler middleware chain: an anonymous "read" viewer can GET the stream
// and the queue (public, read-only) but is 403'd on the control endpoints.
func TestCommandCenterPublicButControlsGated(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	h := s.Handler()

	// Read viewer CAN read the public queue endpoint (200).
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	req.Header.Set("X-Hive-Role", "read")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read viewer denied the public queue: got %d, want 200", rec.Code)
	}

	// Read viewer CANNOT drive a control endpoint (403).
	req2 := httptest.NewRequest(http.MethodPost, "/api/contributors/c-alice/revoke", nil)
	req2.Header.Set("X-Hive-Role", "read")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("read viewer was allowed a control endpoint: got %d, want 403", rec2.Code)
	}
	if p := findContributor("c-alice"); p == nil || p.TrustTier != "newcomer" {
		t.Errorf("read viewer mutated a profile through a control endpoint: %+v", p)
	}
}

// TestOpsFleetHydrationIsResilient pins the regression fix for #2574: the command
// center must NOT be able to leave the pre-existing fleet panels (Connected
// clankers / Pipeline & policy / My work) stuck on their "Loading…" placeholders.
//
// The failure mode was JS coupling introduced by #2574:
//   - renderClankers updated the army roster only AFTER the row map, so a throw in
//     a row builder left BOTH the list on "Loading fleet…" AND the roster at 0/0/0;
//   - opsPoll rendered all three panels in one try block with an empty catch, so a
//     single throw stuck all three with no diagnostics;
//   - activateTab called opsPoll();ccStart(); on one line, so a ccStart() throw
//     could abort fleet hydration.
//
// We can't execute the inline JS in a Go test, so we assert the RESILIENCE WIRING
// is present in the served page: the roster hydrates independently of the rows,
// each panel renders in isolation (safeRender), errors are logged not swallowed,
// and opsPoll()/ccStart() are guarded independently. A separate node --check in CI
// (and the PR's manual harness) covers runtime correctness.
func TestOpsFleetHydrationIsResilient(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	// The three fleet panels + their placeholders must still exist (the panels we
	// are keeping alive).
	for _, want := range []string{
		`id="clanker-list"`, `Loading fleet`,
		`id="policy-body"`, `Loading policy`,
		`id="work-list"`, `Loading work`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served page missing fleet panel marker %q", want)
		}
	}

	// Resilience wiring introduced by the fix:
	for _, want := range []string{
		// opsPoll renders each panel in isolation so one throw cannot stick all three.
		`function safeRender`,
		`safeRender('clankers'`,
		`safeRender('work'`,
		`safeRender('policy'`,
		// Errors are logged, never silently swallowed (the old empty catch{}).
		`opsPoll fetch failed`,
		// opsPoll() and ccStart() are started independently so one throwing cannot
		// prevent the other.
		`try{opsPoll();}catch`,
		`try{ccStart();}catch`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served page missing resilience wiring %q (regression #2574 could recur)", want)
		}
	}

	// The army roster must be updated BEFORE the row map in renderClankers, so a
	// row-build throw cannot leave the roster at 0/0/0. Assert ccUpdateArmy(list)
	// appears ahead of the row map (el.innerHTML=list.map) within renderClankers.
	rc := body[strings.Index(body, "function renderClankers"):]
	rc = rc[:strings.Index(rc, "function ccUpdateArmy")]
	army := strings.Index(rc, "ccUpdateArmy(list)")
	rowMap := strings.Index(rc, "el.innerHTML=list.map")
	if army < 0 || rowMap < 0 {
		t.Fatalf("renderClankers missing expected structure (army=%d rowMap=%d)", army, rowMap)
	}
	if army > rowMap {
		t.Errorf("renderClankers updates the army roster AFTER the row map; a row throw would leave the roster at 0/0/0 (regression #2574)")
	}

	// The command center must stay intact alongside the fix.
	for _, want := range []string{`function ccStart`, `id="cc-queue"`, `id="cc-log"`, `id="cc-army"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fix regressed the command center: missing %q", want)
		}
	}
}

// waitFor polls cond up to ~2s. Keeps the SSE tests free of fixed sleeps that would
// be flaky under load while still bounding the wait.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
