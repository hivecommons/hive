package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

// resetLifecycleStore clears the package-level lazy singleton so tests are
// order-independent. The store is process-wide by design; tests reset it.
func resetLifecycleStore() {
	lifecycleTimelineOnce.Do(func() {}) // burn the Once so the assignment below sticks
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

	// journeys must be [] not null
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if string(raw["journeys"]) != "[]" {
		t.Fatalf("journeys = %s, want []", raw["journeys"])
	}
	if _, ok := raw["fleet"]; !ok {
		t.Fatal("fleet key missing")
	}
}

func TestHandleLifecycleTimelineWithJourneys(t *testing.T) {
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
	// 4 events, but only 2 distinct work items — journeys, not raw rows.
	if len(dto.Journeys) != 2 {
		t.Fatalf("journeys = %d, want 2", len(dto.Journeys))
	}
	var one *timeline.Journey
	for i := range dto.Journeys {
		if dto.Journeys[i].Ref == "org/repo#1" {
			one = &dto.Journeys[i]
		}
	}
	if one == nil {
		t.Fatalf("journey org/repo#1 missing: %+v", dto.Journeys)
	}
	if one.Current != timeline.KindMerged {
		t.Fatalf("journey #1 current = %s, want merged", one.Current)
	}
	if one.Stages[timeline.KindKicked] == nil || one.Stages[timeline.KindKicked].Agent != "bob" {
		t.Fatalf("journey #1 stage history lost: %+v", one.Stages)
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
	if len(dto.Journeys) != 1 || dto.Journeys[0].Agent != "scanner" {
		t.Fatalf("journeys = %+v, want only the scanner journey", dto.Journeys)
	}
	if dto.Fleet.Events != 1 || dto.Fleet.InFlight != 1 {
		t.Fatalf("fleet = %+v, want one in-flight scanner journey", dto.Fleet)
	}
}

func TestHandleLifecycleTimelineLimitAndWindowParams(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	store := s.LifecycleTimeline()
	for i := 0; i < 10; i++ {
		store.Record(timeline.Event{IssueRef: "org/repo#" + string(rune('a'+i)), Kind: timeline.KindKicked})
	}

	req := httptest.NewRequest(http.MethodGet, "/api/lifecycle-timeline?limit=3&window=30", nil)
	rr := httptest.NewRecorder()
	s.handleLifecycleTimeline(rr, req)

	var dto timeline.TimelineDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dto.Journeys) != 3 {
		t.Fatalf("limit=3 gave %d journeys", len(dto.Journeys))
	}
	wantWindowMs := int64(30 * 60 * 1000)
	if dto.Fleet.WindowMs != wantWindowMs {
		t.Fatalf("WindowMs = %d, want %d", dto.Fleet.WindowMs, wantWindowMs)
	}
	// Honest coverage: the data spans ~0s, so CoveredMs must not claim 30m.
	if dto.Fleet.CoveredMs > wantWindowMs/2 {
		t.Fatalf("CoveredMs = %d claims more history than exists", dto.Fleet.CoveredMs)
	}
}

func TestHandleLifecycleTimelineIgnoresBadParams(t *testing.T) {
	resetLifecycleStore()
	s := newTestServer()
	s.LifecycleTimeline().Record(timeline.Event{IssueRef: "x#1", Kind: timeline.KindKicked})

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
	// bad limit → default; still returns the one journey.
	if len(dto.Journeys) != 1 {
		t.Fatalf("journeys = %d, want 1", len(dto.Journeys))
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

// TestEnableLifecyclePersistenceRoundTrip: journeys recorded before a
// "restart" (fresh store, same path) survive it — the #5656 requirement that
// pod rolls stop zeroing the panel.
func TestEnableLifecyclePersistenceRoundTrip(t *testing.T) {
	resetLifecycleStore()
	path := filepath.Join(t.TempDir(), "lifecycle-timeline.json")
	s := newTestServer()
	s.EnableLifecyclePersistence(path)
	s.LifecycleTimeline().Record(timeline.Event{IssueRef: "o/r#1", Kind: timeline.KindMerged, Agent: "quality"})

	resetLifecycleStore() // simulate the process restart
	s2 := newTestServer()
	s2.EnableLifecyclePersistence(path)
	j, ok := s2.LifecycleTimeline().Journey("o/r#1")
	if !ok || j.Current != timeline.KindMerged {
		t.Fatalf("journey did not survive restart: %+v ok=%v", j, ok)
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
	if dto.Journeys == nil {
		t.Fatal("journeys nil over the wire")
	}
}
