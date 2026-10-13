package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateStore is an in-memory NousGateStore for the gate protocol tests
// (#11696): the agent "publishes" by setting pending, the dashboard records
// decisions into decided.
type fakeGateStore struct {
	mu        sync.Mutex
	pending   *NousGateRequest
	readErr   error
	writeErr  error
	markErr   error
	decided   []NousGateDecision
	notified  string
	markCalls int
}

func (f *fakeGateStore) PendingGate() (NousGateRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return NousGateRequest{}, false, f.readErr
	}
	if f.pending == nil {
		return NousGateRequest{}, false, nil
	}
	return *f.pending, true, nil
}

func (f *fakeGateStore) RecordGateDecision(d NousGateDecision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.decided = append(f.decided, d)
	f.pending = nil
	return nil
}

func (f *fakeGateStore) LastNotifiedGate() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notified
}

func (f *fakeGateStore) MarkGateNotified(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markCalls++
	if f.markErr != nil {
		return f.markErr
	}
	f.notified = id
	return nil
}

func (f *fakeGateStore) publish(req NousGateRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = &req
}

type recordedPing struct{ title, message string }

var gateTestNow = time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)

func gateTestServer(t *testing.T, store *fakeGateStore) (*Server, *Dependencies, *[]recordedPing) {
	t.Helper()
	s, deps := apiServer(t)
	pings := &[]recordedPing{}
	deps.Nous = &NousState{
		Mode:      "suggest",
		Scope:     "governor",
		Status:    map[string]interface{}{},
		GateStore: store,
		GateNotify: func(title, message string) {
			*pings = append(*pings, recordedPing{title, message})
		},
		Now: func() time.Time { return gateTestNow },
	}
	return s, deps, pings
}

func designGate(id string) NousGateRequest {
	return NousGateRequest{
		ID:          id,
		Gate:        "design",
		Question:    "Run the cache-size experiment with 3 arms?",
		Scope:       "governor",
		Summary:     "3 arms, ~2h",
		Artifact:    json.RawMessage(`{"arms":3}`),
		RequestedAt: gateTestNow.Add(-time.Minute),
	}
}

func lastAudit(t *testing.T, s *Server, action string) AuditEntry {
	t.Helper()
	for _, e := range s.GetAudit().Recent(50) {
		if e.Action == action {
			return e
		}
	}
	t.Fatalf("no %s audit entry", action)
	return AuditEntry{}
}

// The agent-side write path: a published gate shows up on
// GET /api/nous/gate-pending without waiting for the poller, and the owner
// is pinged exactly once for it however often the state is re-synced.
func TestNousGatePendingMirrorsStoreAndPingsOnce(t *testing.T) {
	store := &fakeGateStore{}
	s, deps, pings := gateTestServer(t, store)

	rec := doGet(s, "/api/nous/gate-pending")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("no gate: %d %s", rec.Code, rec.Body.String())
	}
	if len(*pings) != 0 {
		t.Fatalf("pinged with nothing pending: %+v", *pings)
	}

	store.publish(designGate("design-1"))
	rec = doGet(s, "/api/nous/gate-pending")
	got := decodeJSON(t, rec)
	if got["id"] != "design-1" || got["gate"] != "design" || got["scope"] != "governor" || got["summary"] != "3 arms, ~2h" {
		t.Fatalf("pending = %v", got)
	}
	if got["question"] != "Run the cache-size experiment with 3 arms?" {
		t.Fatalf("question = %v", got["question"])
	}
	if art, _ := got["artifact"].(map[string]interface{}); art["arms"] != float64(3) {
		t.Fatalf("artifact passthrough = %v", got["artifact"])
	}
	if got["requested_at"] != gateTestNow.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("requested_at = %v", got["requested_at"])
	}

	for i := 0; i < 3; i++ {
		doGet(s, "/api/nous/gate-pending")
		if err := deps.Nous.SyncGate(); err != nil {
			t.Fatal(err)
		}
	}
	if len(*pings) != 1 {
		t.Fatalf("pings = %d, want exactly one per gate id", len(*pings))
	}
	p := (*pings)[0]
	if p.title != nousGateNotifyTitle || !strings.Contains(p.message, "design gate (governor scope)") || !strings.Contains(p.message, "cache-size") {
		t.Fatalf("ping = %+v", p)
	}
	if store.notified != "design-1" || store.markCalls != 1 {
		t.Fatalf("marker = %q after %d calls", store.notified, store.markCalls)
	}

	// A different gate id is a new question: one more ping.
	store.publish(designGate("findings-1"))
	doGet(s, "/api/nous/gate-pending")
	if len(*pings) != 2 || store.notified != "findings-1" {
		t.Fatalf("second gate: pings=%d marker=%q", len(*pings), store.notified)
	}

	// The agent withdraws the gate: the mirror empties.
	store.pending = nil
	rec = doGet(s, "/api/nous/gate-pending")
	if body := strings.TrimSpace(rec.Body.String()); body != "{}" {
		t.Fatalf("withdrawn gate body = %s", body)
	}
	if len(*pings) != 2 {
		t.Fatalf("withdrawal must not ping: %d", len(*pings))
	}
}

// The dedupe survives a restart: a fresh NousState over a store whose
// marker already names the pending gate does not ping again.
func TestNousGateNotifyDedupesAcrossRestart(t *testing.T) {
	store := &fakeGateStore{notified: "design-1"}
	store.publish(designGate("design-1"))
	_, deps, pings := gateTestServer(t, store)
	if err := deps.Nous.SyncGate(); err != nil {
		t.Fatal(err)
	}
	if len(*pings) != 0 || store.markCalls != 0 {
		t.Fatalf("restart re-pinged: pings=%d marks=%d", len(*pings), store.markCalls)
	}

	// A marker write failure is logged but the owner is still pinged once.
	var logBuf bytes.Buffer
	store = &fakeGateStore{markErr: errors.New("disk full")}
	store.publish(designGate("design-2"))
	_, deps, pings = gateTestServer(t, store)
	deps.Nous.GateLogger = slog.New(slog.NewTextHandler(&logBuf, nil))
	for i := 0; i < 2; i++ {
		if err := deps.Nous.SyncGate(); err != nil {
			t.Fatal(err)
		}
	}
	if len(*pings) != 1 || !strings.Contains(logBuf.String(), "disk full") {
		t.Fatalf("marker failure: pings=%d log=%s", len(*pings), logBuf.String())
	}

	// No notifier wired: nothing is marked, nothing panics.
	store = &fakeGateStore{}
	store.publish(designGate("design-3"))
	_, deps, _ = gateTestServer(t, store)
	deps.Nous.GateNotify = nil
	if err := deps.Nous.SyncGate(); err != nil || store.markCalls != 0 {
		t.Fatalf("no notifier: err=%v marks=%d", err, store.markCalls)
	}
}

// The owner's answer round-trips: recorded in the store with actor and
// clock, mirrored into GateResponse, the pending gate cleared, and audited.
func TestNousGateDecisionRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     map[string]string
		decision string
		reason   string
	}{
		{"approve with reason", map[string]string{"decision": "approve", "reason": "design is sound"}, NousGateApprove, "design is sound"},
		{"reject with note alias", map[string]string{"decision": "reject", "note": "too many arms"}, NousGateReject, "too many arms"},
		{"abort with explicit gate id", map[string]string{"decision": "abort", "gate_id": "design-1"}, NousGateAbort, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGateStore{}
			store.publish(designGate("design-1"))
			s, deps, _ := gateTestServer(t, store)

			rec := doPut(s, "/api/nous/gate-decision", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
			}
			resp := decodeJSON(t, rec)
			if resp["status"] != "decided" || resp["decision"] != tc.decision || resp["gate_id"] != "design-1" {
				t.Fatalf("response = %v", resp)
			}

			if len(store.decided) != 1 {
				t.Fatalf("store decisions = %+v", store.decided)
			}
			d := store.decided[0]
			if d.GateID != "design-1" || d.Decision != tc.decision || d.Reason != tc.reason || !d.DecidedAt.Equal(gateTestNow) {
				t.Fatalf("recorded = %+v", d)
			}
			if d.DecidedBy == "" {
				t.Fatal("decision must name the owner who made it")
			}

			if len(deps.Nous.GatePending) != 0 {
				t.Fatalf("pending not cleared: %v", deps.Nous.GatePending)
			}
			resp = decodeJSON(t, doGet(s, "/api/nous/gate-response"))
			if resp["decision"] != tc.decision || resp["gate_id"] != "design-1" || resp["decided_at"] != gateTestNow.Format(time.RFC3339) || resp["reason"] != tc.reason {
				t.Fatalf("gate-response = %v", resp)
			}
			if body := strings.TrimSpace(doGet(s, "/api/nous/gate-pending").Body.String()); body != "{}" {
				t.Fatalf("gate-pending after decision = %s", body)
			}

			e := lastAudit(t, s, "nous_gate_decision")
			if !strings.Contains(e.Detail, "decision="+tc.decision) || !strings.Contains(e.Detail, "gate_id=design-1") {
				t.Fatalf("audit detail = %q", e.Detail)
			}
		})
	}
}

func TestNousGateDecisionRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*fakeGateStore)
		body    map[string]string
		status  int
		wantErr string
	}{
		{"unknown decision", func(f *fakeGateStore) { f.publish(designGate("design-1")) },
			map[string]string{"decision": "maybe"}, http.StatusBadRequest, "approve, reject or abort"},
		{"nothing pending", func(*fakeGateStore) {},
			map[string]string{"decision": "approve"}, http.StatusConflict, "no gate is pending"},
		{"stale gate id", func(f *fakeGateStore) { f.publish(designGate("design-2")) },
			map[string]string{"decision": "approve", "gate_id": "design-1"}, http.StatusConflict, "no longer pending"},
		{"store read error", func(f *fakeGateStore) { f.readErr = errors.New("pending.json: bad json") },
			map[string]string{"decision": "approve"}, http.StatusInternalServerError, "read pending gate"},
		{"store write error", func(f *fakeGateStore) { f.publish(designGate("design-1")); f.writeErr = errors.New("read-only fs") },
			map[string]string{"decision": "approve"}, http.StatusInternalServerError, "record gate decision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGateStore{}
			tc.setup(store)
			s, deps, _ := gateTestServer(t, store)
			rec := doPut(s, "/api/nous/gate-decision", tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Fatalf("PUT = %d %s, want %d containing %q", rec.Code, rec.Body.String(), tc.status, tc.wantErr)
			}
			if len(store.decided) != 0 {
				t.Fatalf("rejected decision was recorded: %+v", store.decided)
			}
			if deps.Nous.GateResponse != nil {
				t.Fatalf("GateResponse set on failure: %v", deps.Nous.GateResponse)
			}
			if store.pending != nil {
				_ = deps.Nous.SyncGate()
				if deps.Nous.pendingGateID() != store.pending.ID {
					t.Fatalf("pending gate lost on failure: %v", deps.Nous.GatePending)
				}
			}
			e := lastAudit(t, s, "nous_gate_decision_failed")
			if !strings.Contains(e.Detail, "error=") {
				t.Fatalf("failure audit detail = %q", e.Detail)
			}
		})
	}
}

// Without a GateStore the handler keeps its pre-#11696 shape (in-memory
// GateResponse, no pending required) but the decision vocabulary is still
// enforced.
func TestNousGateDecisionWithoutStore(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.GatePending = map[string]interface{}{"id": "legacy-1", "question": "deploy?"}
	rec := doPut(s, "/api/nous/gate-decision", map[string]string{"decision": "reject", "reason": "no"})
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy PUT = %d %s", rec.Code, rec.Body.String())
	}
	if deps.Nous.GateResponse["decision"] != "reject" || deps.Nous.GateResponse["reason"] != "no" || deps.Nous.GateResponse["gate_id"] != "legacy-1" {
		t.Fatalf("legacy GateResponse = %v", deps.Nous.GateResponse)
	}
	if rec := doPut(s, "/api/nous/gate-decision", map[string]string{"decision": "yes"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("legacy invalid decision = %d", rec.Code)
	}
	deps.Nous.GatePending = nil
	if rec := doPut(s, "/api/nous/gate-decision", map[string]string{"decision": "approve"}); rec.Code != http.StatusOK {
		t.Fatalf("legacy nil pending = %d", rec.Code)
	}
	if deps.Nous.GatePending == nil {
		t.Fatal("legacy path must still initialise GatePending")
	}
}

// RunNousGateWatch syncs on every injected tick, logs an unreadable gate
// once per distinct error, and stops with the context — no sleeping.
func TestRunNousGateWatch(t *testing.T) {
	store := &fakeGateStore{}
	_, deps, pings := gateTestServer(t, store)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunNousGateWatch(ctx, deps.Nous, ticks, logger)
		close(done)
	}()

	tick := func() { ticks <- gateTestNow }

	tick()
	if len(*pings) != 0 {
		t.Fatalf("ping with nothing pending")
	}

	store.publish(designGate("design-1"))
	tick()
	tick()
	deps.Nous.Mu.Lock()
	id := deps.Nous.pendingGateID()
	deps.Nous.Mu.Unlock()
	if id != "design-1" || len(*pings) != 1 {
		t.Fatalf("after publish: id=%q pings=%d", id, len(*pings))
	}

	store.mu.Lock()
	store.readErr = errors.New("pending.json: unexpected end of JSON input")
	store.mu.Unlock()
	tick()
	tick()
	tick()
	if n := strings.Count(logBuf.String(), "pending gate unreadable"); n != 1 {
		t.Fatalf("unreadable gate logged %d times, want once: %s", n, logBuf.String())
	}
	deps.Nous.Mu.Lock()
	id = deps.Nous.pendingGateID()
	deps.Nous.Mu.Unlock()
	if id != "design-1" {
		t.Fatalf("read error must keep the last good mirror, got %q", id)
	}

	store.mu.Lock()
	store.readErr = errors.New("pending.json: permission denied")
	store.mu.Unlock()
	tick()
	if n := strings.Count(logBuf.String(), "pending gate unreadable"); n != 2 {
		t.Fatalf("a different error must be logged again (%d)", n)
	}

	store.mu.Lock()
	store.readErr = nil
	store.mu.Unlock()
	tick()
	store.mu.Lock()
	store.readErr = errors.New("pending.json: unexpected end of JSON input")
	store.mu.Unlock()
	tick()
	if n := strings.Count(logBuf.String(), "pending gate unreadable"); n != 3 {
		t.Fatalf("error after recovery must be logged again (%d)", n)
	}

	cancel()
	<-done

	// Guards: nothing to watch returns immediately.
	RunNousGateWatch(context.Background(), nil, ticks, logger)
	RunNousGateWatch(context.Background(), &NousState{}, ticks, logger)
	StartNousGateWatch(context.Background(), &NousState{}, time.Second, logger)
	StartNousGateWatch(context.Background(), deps.Nous, 0, logger)
}

func TestStartNousGateWatchRunsOnTicker(t *testing.T) {
	store := &fakeGateStore{}
	store.publish(designGate("design-1"))
	_, deps, _ := gateTestServer(t, store)
	synced := make(chan struct{}, 1)
	deps.Nous.GateNotify = func(string, string) {
		select {
		case synced <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartNousGateWatch(ctx, deps.Nous, time.Millisecond, nil)
	select {
	case <-synced:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ticker-driven watch never synced")
	}
}

func TestNousGateHelpers(t *testing.T) {
	// SyncGate / RecordGateDecision on a nil or store-less state are safe.
	var nilState *NousState
	if err := nilState.SyncGate(); err != nil {
		t.Fatal(err)
	}
	if (&NousState{}).nousGateNow().IsZero() {
		t.Fatal("default clock must be time.Now")
	}
	if nousGateStatus(errors.New("plain")) != http.StatusInternalServerError {
		t.Fatal("unknown errors map to 500")
	}

	long := strings.Repeat("q", nousGateNotifyMaxQuestion+50)
	msg := nousGateNotifyMessage(NousGateRequest{Question: long})
	if !strings.HasPrefix(msg, "Gate is waiting") || !strings.Contains(msg, nousGateNotifyEllipsis) || strings.Contains(msg, long) {
		t.Fatalf("long question not truncated: %d chars", len(msg))
	}

	// Unparseable raw passthroughs are served as strings, never dropped.
	m := gateRequestMap(NousGateRequest{ID: "g", Question: "q", Reviews: json.RawMessage(`not json`)})
	if m["reviews"] != "not json" {
		t.Fatalf("reviews = %v", m["reviews"])
	}
	if _, has := m["gate"]; has {
		t.Fatalf("empty optional fields must be omitted: %v", m)
	}
	for _, in := range []string{"approve", "reject", "abort"} {
		if !ValidNousGateDecision(in) {
			t.Errorf("%q should be valid", in)
		}
	}
	for _, in := range []string{"", "Approve", "ok"} {
		if ValidNousGateDecision(in) {
			t.Errorf("%q should be invalid", in)
		}
	}
}
