package timeline

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustEvent(ref string, kind Kind, atMs int64) Event {
	return Event{IssueRef: ref, Kind: kind, At: atMs}
}

func TestNewStoreDefaults(t *testing.T) {
	s := NewStore()
	if s.cap != MaxEvents {
		t.Fatalf("cap = %d, want %d", s.cap, MaxEvents)
	}
	if got := s.Recent(0); len(got) != 0 {
		t.Fatalf("fresh store Recent = %d events, want 0", len(got))
	}
	if got := s.Recent(0); got == nil {
		t.Fatal("Recent must return non-nil slice on empty store")
	}
}

func TestNewStoreWithCapNonPositiveFallsBack(t *testing.T) {
	for _, c := range []int{0, -1, -100} {
		s := NewStoreWithCap(c)
		if s.cap != MaxEvents {
			t.Fatalf("NewStoreWithCap(%d) cap = %d, want %d", c, s.cap, MaxEvents)
		}
	}
}

func TestRecordAndRecentOrder(t *testing.T) {
	s := NewStoreWithCap(10)
	base := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		s.Record(mustEvent(fmt.Sprintf("r#%d", i), KindEnumerated, base+int64(i)))
	}
	recent := s.Recent(0) // all, newest first
	if len(recent) != 5 {
		t.Fatalf("Recent = %d, want 5", len(recent))
	}
	if recent[0].IssueRef != "r#4" {
		t.Fatalf("newest first: got %q, want r#4", recent[0].IssueRef)
	}
	if recent[4].IssueRef != "r#0" {
		t.Fatalf("oldest last: got %q, want r#0", recent[4].IssueRef)
	}

	// limit
	top := s.Recent(2)
	if len(top) != 2 || top[0].IssueRef != "r#4" || top[1].IssueRef != "r#3" {
		t.Fatalf("Recent(2) = %+v", top)
	}
}

func TestRingEvictionAtCap(t *testing.T) {
	const cap = 4
	s := NewStoreWithCap(cap)
	base := time.Now().UnixMilli()
	// Record 10 events into a cap-4 ring.
	for i := 0; i < 10; i++ {
		s.Record(mustEvent(fmt.Sprintf("e#%d", i), KindKicked, base+int64(i)))
	}
	all := s.Recent(0)
	if len(all) != cap {
		t.Fatalf("after overflow, retained = %d, want %d", len(all), cap)
	}
	// Newest four should be e#9..e#6.
	wantNewest := []string{"e#9", "e#8", "e#7", "e#6"}
	for i, w := range wantNewest {
		if all[i].IssueRef != w {
			t.Fatalf("all[%d] = %q, want %q", i, all[i].IssueRef, w)
		}
	}
}

func TestRecordStampsMissingTimestamp(t *testing.T) {
	s := NewStoreWithCap(2)
	before := time.Now().UnixMilli()
	s.Record(Event{IssueRef: "x#1", Kind: KindEnumerated}) // At == 0
	after := time.Now().UnixMilli()
	got := s.Recent(1)
	if len(got) != 1 {
		t.Fatalf("Recent(1) = %d", len(got))
	}
	if got[0].At < before || got[0].At > after {
		t.Fatalf("At = %d, want within [%d,%d]", got[0].At, before, after)
	}
}

func TestByIssueFiltering(t *testing.T) {
	s := NewStoreWithCap(20)
	base := time.Now().UnixMilli()
	s.Record(mustEvent("a#1", KindEnumerated, base+1))
	s.Record(mustEvent("b#2", KindEnumerated, base+2))
	s.Record(mustEvent("a#1", KindKicked, base+3))
	s.Record(mustEvent("a#1", KindMerged, base+4))

	a := s.ByIssue("a#1")
	if len(a) != 3 {
		t.Fatalf("ByIssue(a#1) = %d, want 3", len(a))
	}
	// newest first
	if a[0].Kind != KindMerged || a[2].Kind != KindEnumerated {
		t.Fatalf("ByIssue order wrong: %+v", a)
	}
	if got := s.ByIssue("b#2"); len(got) != 1 {
		t.Fatalf("ByIssue(b#2) = %d, want 1", len(got))
	}
	// unknown / empty ref → empty non-nil
	if got := s.ByIssue("zzz"); got == nil || len(got) != 0 {
		t.Fatalf("ByIssue(unknown) = %+v", got)
	}
	if got := s.ByIssue(""); got == nil || len(got) != 0 {
		t.Fatalf("ByIssue(empty) = %+v", got)
	}
}

func TestFleetHealthDerivation(t *testing.T) {
	s := NewStoreWithCap(50)
	now := time.Now().UnixMilli()
	// issue A: enumerated + kicked + merged  → Merged
	s.Record(mustEvent("A", KindEnumerated, now-1000))
	s.Record(mustEvent("A", KindKicked, now-900))
	s.Record(mustEvent("A", KindMerged, now-800))
	// issue B: kicked, blocked → Blocked
	s.Record(mustEvent("B", KindKicked, now-700))
	s.Record(mustEvent("B", KindBlocked, now-600))
	// issue C: enumerated only → InFlight
	s.Record(mustEvent("C", KindEnumerated, now-500))
	// issue D: kicked only → InFlight
	s.Record(mustEvent("D", KindKicked, now-400))

	fh := s.FleetHealth(time.Hour)
	if fh.Merged != 1 {
		t.Errorf("Merged = %d, want 1", fh.Merged)
	}
	if fh.Blocked != 1 {
		t.Errorf("Blocked = %d, want 1", fh.Blocked)
	}
	if fh.InFlight != 2 {
		t.Errorf("InFlight = %d, want 2 (C,D)", fh.InFlight)
	}
	if fh.Events != 7 {
		t.Errorf("Events = %d, want 7", fh.Events)
	}
	if fh.WindowMs != time.Hour.Milliseconds() {
		t.Errorf("WindowMs = %d, want %d", fh.WindowMs, time.Hour.Milliseconds())
	}
}

func TestFleetHealthMergedWinsOverInFlightAndBlocked(t *testing.T) {
	s := NewStoreWithCap(50)
	now := time.Now().UnixMilli()
	// Same issue: kicked, blocked, then merged → counts only Merged.
	s.Record(mustEvent("X", KindKicked, now-300))
	s.Record(mustEvent("X", KindBlocked, now-200))
	s.Record(mustEvent("X", KindMerged, now-100))
	fh := s.FleetHealth(time.Hour)
	if fh.Merged != 1 || fh.Blocked != 0 || fh.InFlight != 0 {
		t.Fatalf("terminal precedence wrong: %+v", fh)
	}
}

func TestFleetHealthWindowExcludesOld(t *testing.T) {
	s := NewStoreWithCap(50)
	now := time.Now().UnixMilli()
	old := now - (2 * time.Hour).Milliseconds()
	s.Record(mustEvent("old", KindMerged, old))
	s.Record(mustEvent("new", KindKicked, now-1000))

	fh := s.FleetHealth(time.Hour)
	if fh.Events != 1 {
		t.Fatalf("Events = %d, want 1 (old excluded)", fh.Events)
	}
	if fh.Merged != 0 {
		t.Fatalf("Merged = %d, want 0 (old merge outside window)", fh.Merged)
	}
	if fh.InFlight != 1 {
		t.Fatalf("InFlight = %d, want 1", fh.InFlight)
	}
}

func TestFleetHealthDefaultWindow(t *testing.T) {
	s := NewStoreWithCap(10)
	s.Record(mustEvent("A", KindKicked, time.Now().UnixMilli()))
	fh := s.FleetHealth(0) // non-positive → DefaultFleetWindow
	if fh.WindowMs != DefaultFleetWindow.Milliseconds() {
		t.Fatalf("WindowMs = %d, want %d", fh.WindowMs, DefaultFleetWindow.Milliseconds())
	}
}

func TestFleetHealthIgnoresEmptyIssueRef(t *testing.T) {
	s := NewStoreWithCap(10)
	now := time.Now().UnixMilli()
	s.Record(Event{Kind: KindMerged, At: now}) // no IssueRef
	fh := s.FleetHealth(time.Hour)
	if fh.Events != 1 {
		t.Fatalf("Events = %d, want 1", fh.Events)
	}
	if fh.Merged != 0 || fh.InFlight != 0 || fh.Blocked != 0 {
		t.Fatalf("empty-ref event must not be counted per-issue: %+v", fh)
	}
}

func TestSnapshotShapeAndLimits(t *testing.T) {
	s := NewStoreWithCap(20)
	base := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		s.Record(mustEvent(fmt.Sprintf("s#%d", i), KindKicked, base+int64(i)))
	}
	dto := s.Snapshot(3, time.Hour)
	if len(dto.Events) != 3 {
		t.Fatalf("Snapshot(3) events = %d, want 3", len(dto.Events))
	}
	if dto.Events[0].IssueRef != "s#4" {
		t.Fatalf("newest first: %q", dto.Events[0].IssueRef)
	}
	if dto.Fleet.InFlight != 5 {
		t.Fatalf("Fleet.InFlight = %d, want 5", dto.Fleet.InFlight)
	}
}

func TestSnapshotEmptyStoreJSON(t *testing.T) {
	s := NewStore()
	dto := s.Snapshot(0, 0)
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// events must serialize as [] not null
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["events"]) != "[]" {
		t.Fatalf("events = %s, want []", raw["events"])
	}
	if _, ok := raw["fleet"]; !ok {
		t.Fatal("fleet key missing")
	}
}

func TestJSONShapeOfEvent(t *testing.T) {
	e := Event{
		ID:       "id1",
		IssueRef: "org/repo#7",
		Kind:     KindPROpened,
		Agent:    "bob",
		At:       1700000000000,
		Attrs:    map[string]string{"pr": "42"},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "issueRef", "kind", "agent", "at", "attrs"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing json field %q in %s", k, b)
		}
	}
	if got["kind"] != string(KindPROpened) {
		t.Errorf("kind = %v", got["kind"])
	}
}

func TestFromSpanKnown(t *testing.T) {
	e, ok := FromSpan("governor.kick", map[string]string{
		attrIssueRef: "org/repo#9",
		attrAgent:    "alice",
		attrEventID:  "evt-1",
		"extra":      "keepme",
	})
	if !ok {
		t.Fatal("FromSpan(governor.kick) ok = false, want true")
	}
	if e.Kind != KindKicked {
		t.Errorf("Kind = %q, want kicked", e.Kind)
	}
	if e.IssueRef != "org/repo#9" || e.Agent != "alice" || e.ID != "evt-1" {
		t.Errorf("typed fields not populated: %+v", e)
	}
	if e.Attrs["extra"] != "keepme" {
		t.Errorf("attrs not preserved: %+v", e.Attrs)
	}
}

func TestFromSpanAllKnownNames(t *testing.T) {
	for name, wantKind := range spanKindMap {
		e, ok := FromSpan(name, nil)
		if !ok {
			t.Errorf("FromSpan(%q) ok = false", name)
			continue
		}
		if e.Kind != wantKind {
			t.Errorf("FromSpan(%q) kind = %q, want %q", name, e.Kind, wantKind)
		}
	}
}

func TestFromSpanUnknown(t *testing.T) {
	e, ok := FromSpan("something.else", map[string]string{"a": "b"})
	if ok {
		t.Fatal("FromSpan(unknown) ok = true, want false")
	}
	if e.Kind != "" {
		t.Fatalf("unknown span produced kind %q", e.Kind)
	}
}

func TestFromSpanNilAttrs(t *testing.T) {
	e, ok := FromSpan("pr.merged", nil)
	if !ok || e.Kind != KindMerged {
		t.Fatalf("FromSpan(pr.merged, nil) = %+v, %v", e, ok)
	}
	if e.Attrs != nil {
		t.Fatalf("nil attrs should stay nil, got %+v", e.Attrs)
	}
}

func TestRecordViaRecorderInterface(t *testing.T) {
	var rec Recorder = NewStore()
	rec.Record(Event{IssueRef: "iface#1", Kind: KindEnumerated})
	// type assert back to read.
	s := rec.(*Store)
	if got := s.Recent(0); len(got) != 1 {
		t.Fatalf("recorded via interface, Recent = %d", len(got))
	}
}

func TestKnownKinds(t *testing.T) {
	kinds := KnownKinds()
	if len(kinds) != 6 {
		t.Fatalf("KnownKinds len = %d, want 6", len(kinds))
	}
	if kinds[0] != KindEnumerated || kinds[len(kinds)-1] != KindBlocked {
		t.Fatalf("KnownKinds order wrong: %+v", kinds)
	}
}

func TestSortEventsChrono(t *testing.T) {
	events := []Event{
		{ID: "b", At: 20},
		{ID: "a", At: 10},
		{ID: "c", At: 10}, // tie broken by ID
	}
	sortEventsChrono(events)
	if events[0].ID != "a" || events[1].ID != "c" || events[2].ID != "b" {
		t.Fatalf("sort order = %+v", events)
	}
}

func TestNilStoreSafety(t *testing.T) {
	var s *Store
	s.Record(Event{IssueRef: "x"}) // must not panic
	if got := s.Recent(5); got == nil || len(got) != 0 {
		t.Fatalf("nil Recent = %+v", got)
	}
	if got := s.ByIssue("x"); got == nil || len(got) != 0 {
		t.Fatalf("nil ByIssue = %+v", got)
	}
	fh := s.FleetHealth(time.Hour)
	if fh.InFlight != 0 || fh.Merged != 0 || fh.Blocked != 0 || fh.Events != 0 {
		t.Fatalf("nil FleetHealth = %+v", fh)
	}
}

func TestConcurrentRecordRace(t *testing.T) {
	s := NewStoreWithCap(256)
	const goroutines = 16
	const perG = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ref := fmt.Sprintf("g%d#%d", g, i)
				s.Record(Event{IssueRef: ref, Kind: KindKicked})
				if i%10 == 0 {
					_ = s.Recent(5)
					_ = s.ByIssue(ref)
					_ = s.FleetHealth(time.Minute)
				}
			}
		}(g)
	}
	wg.Wait()

	// Ring is capped: retained count must equal capacity (we wrote far more).
	if got := len(s.Recent(0)); got != 256 {
		t.Fatalf("after concurrent overflow, retained = %d, want 256", got)
	}
}

func TestChronologicalPartialFill(t *testing.T) {
	// Exercise size() < cap path and ordering with a partially-filled ring.
	s := NewStoreWithCap(100)
	base := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		s.Record(mustEvent(fmt.Sprintf("p#%d", i), KindEnumerated, base+int64(i)))
	}
	s.mu.RLock()
	chrono := s.chronological()
	s.mu.RUnlock()
	if len(chrono) != 3 {
		t.Fatalf("chronological = %d, want 3", len(chrono))
	}
	if chrono[0].IssueRef != "p#0" || chrono[2].IssueRef != "p#2" {
		t.Fatalf("chronological order wrong: %+v", chrono)
	}
}
