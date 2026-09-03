package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

// resetLifecycleStore clears the package-level lazy singleton so tests are
// order-independent. The store is process-wide by design; tests reset it.
func resetLifecycleStore() {
	lifecycleStore = timeline.NewStore()
}

func TestHandleLifecycleTimelineEmpty(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/lifecycle-timeline", nil)
	rr := httptest.NewRecorder()
	s.handleLifecycleTimeline(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}

	// events must be [] not null
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if string(raw["events"]) != "[]" {
		t.Fatalf("events = %s, want []", raw["events"])
	}
	if _, ok := raw["fleet"]; !ok {
		t.Fatal("fleet key missing")
	}
}

func TestHandleLifecycleTimelineWithEvents(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()

	store := s.LifecycleTimeline()
	store.Record(timeline.Event{IssueRef: "org/repo#1", Kind: timeline.KindEnumerated})
	store.Record(timeline.Event{IssueRef: "org/repo#1", Kind: timeline.KindKicked, Agent: "bob"})
	store.Record(timeline.Event{IssueRef: "org/repo#1", Kind: timeline.KindMerged})
	store.Record(timeline.Event{IssueRef: "org/repo#2", Kind: timeline.KindKicked})

	req := httptest.NewRequest(http.MethodGet, "/api/lifecycle-timeline", nil)
	rr := httptest.NewRecorder()
	s.handleLifecycleTimeline(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var dto timeline.TimelineDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if len(dto.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(dto.Events))
	}
	// newest first
	if dto.Events[0].IssueRef != "org/repo#2" {
		t.Fatalf("events[0] = %q, want org/repo#2", dto.Events[0].IssueRef)
	}
	if dto.Fleet.Merged != 1 {
		t.Fatalf("Fleet.Merged = %d, want 1", dto.Fleet.Merged)
	}
	if dto.Fleet.InFlight != 1 {
		t.Fatalf("Fleet.InFlight = %d, want 1 (issue #2)", dto.Fleet.InFlight)
	}
}

func TestHandleLifecycleTimelineFiltersOperabilityAgentsBelowL5(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	level := 3
	s.deps = &Dependencies{Config: &config.Config{ACMMLevel: &level}}
	store := s.LifecycleTimeline()
	store.Record(timeline.Event{IssueRef: "org/repo#1", Kind: timeline.KindKicked, Agent: "telemetry"})
	store.Record(timeline.Event{IssueRef: "org/repo#2", Kind: timeline.KindKicked, Agent: "operations"})
	store.Record(timeline.Event{IssueRef: "org/repo#3", Kind: timeline.KindKicked, Agent: "scanner"})

	req := httptest.NewRequest(http.MethodGet, "/api/lifecycle-timeline", nil)
	rr := httptest.NewRecorder()
	s.handleLifecycleTimeline(rr, req)

	var dto timeline.TimelineDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dto.Events) != 1 || dto.Events[0].Agent != "scanner" {
		t.Fatalf("events = %+v, want only scanner event", dto.Events)
	}
	if dto.Fleet.Events != 1 || dto.Fleet.InFlight != 1 {
		t.Fatalf("fleet = %+v, want one in-flight scanner event", dto.Fleet)
	}
}

func TestHandleLifecycleTimelineLimitAndWindowParams(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	store := s.LifecycleTimeline()
	for i := 0; i < 10; i++ {
		store.Record(timeline.Event{IssueRef: "org/repo#L", Kind: timeline.KindKicked})
	}

	req := httptest.NewRequest(http.MethodGet, "/api/lifecycle-timeline?limit=3&window=30", nil)
	rr := httptest.NewRecorder()
	s.handleLifecycleTimeline(rr, req)

	var dto timeline.TimelineDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dto.Events) != 3 {
		t.Fatalf("limit=3 gave %d events", len(dto.Events))
	}
	wantWindowMs := int64(30 * 60 * 1000)
	if dto.Fleet.WindowMs != wantWindowMs {
		t.Fatalf("WindowMs = %d, want %d", dto.Fleet.WindowMs, wantWindowMs)
	}
}

func TestHandleLifecycleTimelineIgnoresBadParams(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	s.LifecycleTimeline().Record(timeline.Event{IssueRef: "x", Kind: timeline.KindKicked})

	req := httptest.NewRequest(http.MethodGet, "/api/lifecycle-timeline?limit=abc&window=-5", nil)
	rr := httptest.NewRecorder()
	s.handleLifecycleTimeline(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var dto timeline.TimelineDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// bad limit → default; still returns the one event.
	if len(dto.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(dto.Events))
	}
	// bad window → DefaultFleetWindow
	if dto.Fleet.WindowMs != timeline.DefaultFleetWindow.Milliseconds() {
		t.Fatalf("WindowMs = %d, want default %d", dto.Fleet.WindowMs, timeline.DefaultFleetWindow.Milliseconds())
	}
}

func TestLifecycleTimelineLazyNonNil(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	if s.LifecycleTimeline() == nil {
		t.Fatal("LifecycleTimeline() returned nil")
	}
	// idempotent
	first := s.LifecycleTimeline()
	second := s.LifecycleTimeline()
	if first != second {
		t.Fatal("LifecycleTimeline() not stable")
	}
}

// TestHandleLifecycleTimelineViaMux exercises route registration end-to-end.
func TestHandleLifecycleTimelineViaMux(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/lifecycle-timeline", s.handleLifecycleTimeline)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/lifecycle-timeline")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var dto timeline.TimelineDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Events == nil {
		t.Fatal("events nil over the wire")
	}
}
