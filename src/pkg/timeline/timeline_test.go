package timeline

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mustEvent(ref string, kind Kind, atMs int64) Event {
	return Event{IssueRef: ref, Kind: kind, At: atMs}
}

func TestNewStoreDefaults(t *testing.T) {
	s := NewStore()
	if s.cap != MaxJourneys {
		t.Fatalf("cap = %d, want %d", s.cap, MaxJourneys)
	}
	if got := s.Journeys(0); got == nil || len(got) != 0 {
		t.Fatalf("fresh store Journeys = %v, want empty non-nil", got)
	}
	if got := s.Recent(0); got == nil || len(got) != 0 {
		t.Fatalf("fresh store Recent = %v, want empty non-nil", got)
	}
}

func TestNewStoreWithCapNonPositiveFallsBack(t *testing.T) {
	for _, c := range []int{0, -1, -100} {
		s := NewStoreWithCap(c)
		if s.cap != MaxJourneys {
			t.Fatalf("NewStoreWithCap(%d) cap = %d, want %d", c, s.cap, MaxJourneys)
		}
	}
}

// TestDedupeByRefAndKind is the #5656 core invariant: re-recording the same
// (ref, kind) — the every-cycle enumeration sweep — refreshes the existing
// stage instead of appending, so the store never floods.
func TestDedupeByRefAndKind(t *testing.T) {
	s := NewStoreWithCap(10)
	base := time.Now().UnixMilli()
	for i := 0; i < 50; i++ {
		s.Record(mustEvent("r#1", KindEnumerated, base+int64(i)))
	}
	journeys := s.Journeys(0)
	if len(journeys) != 1 {
		t.Fatalf("50 re-enumerations made %d journeys, want 1", len(journeys))
	}
	st := journeys[0].Stages[KindEnumerated]
	if st == nil {
		t.Fatal("enumerated stage missing")
	}
	if st.Count != 50 {
		t.Fatalf("Count = %d, want 50", st.Count)
	}
	if st.FirstAt != base || st.LastAt != base+49 {
		t.Fatalf("stage span = [%d,%d], want [%d,%d]", st.FirstAt, st.LastAt, base, base+49)
	}
}

func TestStageTransitionsDriveCurrent(t *testing.T) {
	base := time.Now().UnixMilli()
	steps := []struct {
		kind Kind
		want Kind
	}{
		{KindEnumerated, KindEnumerated},
		{KindClassified, KindClassified},
		{KindKicked, KindKicked},
		{KindEnumerated, KindKicked}, // re-enumeration must not regress the stage
		{KindPROpened, KindPROpened},
		{KindMerged, KindMerged},
	}
	s := NewStoreWithCap(10)
	for i, step := range steps {
		s.Record(mustEvent("r#7", step.kind, base+int64(i)))
		j, ok := s.Journey("r#7")
		if !ok {
			t.Fatalf("step %d: journey missing", i)
		}
		if j.Current != step.want {
			t.Fatalf("step %d (%s): Current = %s, want %s", i, step.kind, j.Current, step.want)
		}
	}
}

func TestBlockedHoldsUntilLaterProgress(t *testing.T) {
	base := time.Now().UnixMilli()
	s := NewStoreWithCap(10)
	s.Record(mustEvent("r#9", KindKicked, base))
	s.Record(mustEvent("r#9", KindBlocked, base+10))
	if j, _ := s.Journey("r#9"); j.Current != KindBlocked {
		t.Fatalf("Current = %s, want blocked after block", j.Current)
	}
	// Progress after the block clears it.
	s.Record(mustEvent("r#9", KindPROpened, base+20))
	if j, _ := s.Journey("r#9"); j.Current != KindPROpened {
		t.Fatalf("Current = %s, want pr_opened after post-block progress", j.Current)
	}
	// Merged is sticky-terminal even if a block lands later.
	s.Record(mustEvent("r#9", KindMerged, base+30))
	s.Record(mustEvent("r#9", KindBlocked, base+40))
	if j, _ := s.Journey("r#9"); j.Current != KindMerged {
		t.Fatalf("Current = %s, want merged to stay terminal", j.Current)
	}
}

func TestJourneyEvictionAtCap(t *testing.T) {
	const cap = 4
	s := NewStoreWithCap(cap)
	base := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		s.Record(mustEvent(fmt.Sprintf("e#%d", i), KindKicked, base+int64(i)))
	}
	all := s.Journeys(0)
	if len(all) != cap {
		t.Fatalf("after overflow, retained = %d, want %d", len(all), cap)
	}
	wantNewest := []string{"e#9", "e#8", "e#7", "e#6"}
	for i, w := range wantNewest {
		if all[i].Ref != w {
			t.Fatalf("all[%d] = %q, want %q", i, all[i].Ref, w)
		}
	}
	// A refresh of a retained journey protects it from eviction (LRU).
	s.Record(mustEvent("e#6", KindEnumerated, base+100))
	s.Record(mustEvent("e#10", KindKicked, base+101))
	if _, ok := s.Journey("e#6"); !ok {
		t.Fatal("recently refreshed journey e#6 was evicted")
	}
	if _, ok := s.Journey("e#7"); ok {
		t.Fatal("oldest journey e#7 should have been evicted")
	}
}

func TestRecordStampsMissingTimestamp(t *testing.T) {
	s := NewStoreWithCap(2)
	before := time.Now().UnixMilli()
	s.Record(Event{IssueRef: "x#1", Kind: KindEnumerated}) // At == 0
	after := time.Now().UnixMilli()
	j, ok := s.Journey("x#1")
	if !ok {
		t.Fatal("journey missing")
	}
	if j.LastAt < before || j.LastAt > after {
		t.Fatalf("LastAt = %d, want within [%d,%d]", j.LastAt, before, after)
	}
}

func TestRecordDropsEmptyRefAndKind(t *testing.T) {
	s := NewStoreWithCap(4)
	s.Record(Event{Kind: KindKicked, Agent: "scanner"}) // agent-scoped, no ref
	s.Record(Event{IssueRef: "r#1"})                    // no kind
	if got := s.Journeys(0); len(got) != 0 {
		t.Fatalf("journeys = %d, want 0 (no identity, no journey)", len(got))
	}
}

func TestAgentAndAttrsAggregation(t *testing.T) {
	s := NewStoreWithCap(4)
	base := time.Now().UnixMilli()
	s.Record(Event{IssueRef: "r#1", Kind: KindKicked, Agent: "scanner", At: base,
		Attrs: map[string]string{"trigger": "governor-eval", "keep": "old"}})
	s.Record(Event{IssueRef: "r#1", Kind: KindKicked, Agent: "quality", At: base + 1,
		Attrs: map[string]string{"trigger": "manual"}})
	j, _ := s.Journey("r#1")
	st := j.Stages[KindKicked]
	if st.Agent != "quality" || j.Agent != "quality" {
		t.Fatalf("agent = stage %q journey %q, want quality/quality", st.Agent, j.Agent)
	}
	if st.Attrs["trigger"] != "manual" || st.Attrs["keep"] != "old" {
		t.Fatalf("attrs merge lost data: %v", st.Attrs)
	}
	if st.Count != 2 {
		t.Fatalf("Count = %d, want 2", st.Count)
	}
}

func TestByIssueAndRecentSynthesizedViews(t *testing.T) {
	s := NewStoreWithCap(20)
	base := time.Now().UnixMilli()
	s.Record(mustEvent("a#1", KindEnumerated, base+1))
	s.Record(mustEvent("a#1", KindKicked, base+2))
	s.Record(mustEvent("a#1", KindKicked, base+3))
	s.Record(mustEvent("b#2", KindEnumerated, base+4))

	got := s.ByIssue("a#1")
	if len(got) != 2 {
		t.Fatalf("ByIssue = %d events, want 2 (stages, deduped)", len(got))
	}
	if got[0].Kind != KindKicked || got[0].At != base+3 {
		t.Fatalf("newest first: %+v", got[0])
	}
	if got[0].Attrs["count"] != "2" {
		t.Fatalf("collapsed cardinality must ride in attrs, got %v", got[0].Attrs)
	}
	if len(s.ByIssue("")) != 0 || len(s.ByIssue("zzz#9")) != 0 {
		t.Fatal("empty/unknown ref must yield empty slice")
	}
	recent := s.Recent(0)
	if len(recent) != 3 {
		t.Fatalf("Recent = %d, want 3 stage events", len(recent))
	}
	if recent[0].IssueRef != "b#2" {
		t.Fatalf("Recent newest first: %+v", recent[0])
	}
}

func TestFleetHealthDerivation(t *testing.T) {
	s := NewStoreWithCap(20)
	now := time.Now().UnixMilli()
	// r#1: kicked then merged → merged
	s.Record(mustEvent("r#1", KindKicked, now-1000))
	s.Record(mustEvent("r#1", KindMerged, now-500))
	// r#2: blocked
	s.Record(mustEvent("r#2", KindBlocked, now-400))
	// r#3: in flight
	s.Record(mustEvent("r#3", KindKicked, now-300))

	fh := s.FleetHealth(time.Hour)
	if fh.Merged != 1 || fh.Blocked != 1 || fh.InFlight != 1 {
		t.Fatalf("fleet = %+v, want 1/1/1", fh)
	}
	if fh.Events != 3 {
		t.Fatalf("Events = %d, want 3 journeys in window", fh.Events)
	}
	if fh.WindowMs != time.Hour.Milliseconds() {
		t.Fatalf("WindowMs = %d", fh.WindowMs)
	}
}

// TestFleetHealthHonestCoverage: the roll-up must label how much history
// actually backs it. One second of data inside a 6h window reports ~1s of
// coverage, not 6h (#5656: "6h window" over a 14-minute ring).
func TestFleetHealthHonestCoverage(t *testing.T) {
	s := NewStoreWithCap(20)
	now := time.Now().UnixMilli()
	s.Record(mustEvent("r#1", KindKicked, now-1000))
	fh := s.FleetHealth(6 * time.Hour)
	if fh.WindowMs != (6 * time.Hour).Milliseconds() {
		t.Fatalf("WindowMs = %d", fh.WindowMs)
	}
	if fh.CoveredMs < 1000 || fh.CoveredMs > 60_000 {
		t.Fatalf("CoveredMs = %d, want ≈1s (the real span), not the window", fh.CoveredMs)
	}

	// With history deeper than the window, coverage saturates at the window.
	s.Record(mustEvent("r#2", KindKicked, now-(12*time.Hour).Milliseconds()))
	fh = s.FleetHealth(6 * time.Hour)
	if fh.CoveredMs != fh.WindowMs {
		t.Fatalf("CoveredMs = %d, want saturated at WindowMs %d", fh.CoveredMs, fh.WindowMs)
	}
}

func TestFleetHealthWindowExcludesOld(t *testing.T) {
	s := NewStoreWithCap(20)
	now := time.Now().UnixMilli()
	s.Record(mustEvent("old#1", KindMerged, now-(2*time.Hour).Milliseconds()))
	s.Record(mustEvent("new#1", KindKicked, now-1000))
	fh := s.FleetHealth(time.Hour)
	if fh.Merged != 0 {
		t.Fatalf("merged outside window counted: %+v", fh)
	}
	if fh.InFlight != 1 || fh.Events != 1 {
		t.Fatalf("fleet = %+v, want only the fresh journey", fh)
	}
}

func TestFleetHealthDefaultWindow(t *testing.T) {
	s := NewStoreWithCap(4)
	fh := s.FleetHealth(0)
	if fh.WindowMs != DefaultFleetWindow.Milliseconds() {
		t.Fatalf("WindowMs = %d, want default", fh.WindowMs)
	}
	var nilStore *Store
	if got := nilStore.FleetHealth(-1); got.WindowMs != DefaultFleetWindow.Milliseconds() {
		t.Fatalf("nil store WindowMs = %d", got.WindowMs)
	}
}

func TestSnapshotShapeAndLimits(t *testing.T) {
	s := NewStoreWithCap(20)
	base := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		s.Record(mustEvent(fmt.Sprintf("r#%d", i), KindKicked, base+int64(i)))
	}
	dto := s.Snapshot(2, time.Hour)
	if len(dto.Journeys) != 2 {
		t.Fatalf("Journeys = %d, want limited to 2", len(dto.Journeys))
	}
	if dto.Journeys[0].Ref != "r#4" {
		t.Fatalf("most recently active first, got %q", dto.Journeys[0].Ref)
	}
}

func TestSnapshotEmptyStoreJSON(t *testing.T) {
	s := NewStoreWithCap(4)
	data, err := json.Marshal(s.Snapshot(10, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["journeys"]) != "[]" {
		t.Fatalf("journeys = %s, want []", raw["journeys"])
	}
	if _, ok := raw["fleet"]; !ok {
		t.Fatal("fleet key missing")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.json")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	s := NewStoreWithCap(10)
	if err := s.EnablePersistence(path, logger); err != nil {
		t.Fatalf("enable: %v", err)
	}
	base := time.Now().UnixMilli()
	s.Record(Event{IssueRef: "r#1", Kind: KindKicked, Agent: "scanner", At: base,
		Attrs: map[string]string{"trigger": "governor-eval"}})
	s.Record(mustEvent("r#1", KindMerged, base+10)) // terminal → persists immediately

	// A fresh store on the same path restores the journeys.
	s2 := NewStoreWithCap(10)
	if err := s2.EnablePersistence(path, logger); err != nil {
		t.Fatalf("reload: %v", err)
	}
	j, ok := s2.Journey("r#1")
	if !ok {
		t.Fatal("journey lost across restart")
	}
	if j.Current != KindMerged {
		t.Fatalf("Current = %s, want merged (re-derived on load)", j.Current)
	}
	st := j.Stages[KindKicked]
	if st == nil || st.Agent != "scanner" || st.Attrs["trigger"] != "governor-eval" {
		t.Fatalf("kicked stage lost detail: %+v", st)
	}
	// And keeps recording + persisting.
	s2.Record(mustEvent("r#2", KindBlocked, base+20))
	s3 := NewStoreWithCap(10)
	if err := s3.EnablePersistence(path, logger); err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	if _, ok := s3.Journey("r#2"); !ok {
		t.Fatal("post-restore journey not persisted")
	}
	s3.Record(Event{IssueRef: "run#1", Kind: KindStageCompleted, At: base + 30,
		Attrs: map[string]string{"stage_from": "implement", "stage_to": "completed"}})
	s4 := NewStoreWithCap(10)
	if err := s4.EnablePersistence(path, logger); err != nil {
		t.Fatalf("reload 3: %v", err)
	}
	if _, ok := s4.Journey("run#1"); !ok {
		t.Fatal("stage_completed journey not persisted")
	}
}

func TestPersistenceCorruptFileStartsEmptyAndMovesAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o660); err != nil {
		t.Fatal(err)
	}
	s := NewStoreWithCap(4)
	if err := s.EnablePersistence(path, nil); err != nil {
		t.Fatalf("corrupt telemetry must not refuse startup: %v", err)
	}
	if got := s.Journeys(0); len(got) != 0 {
		t.Fatalf("journeys = %d, want 0", len(got))
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt bytes must be kept for inspection: %v", err)
	}
}

func TestPersistenceThrottleCoalescesNonTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.json")
	s := NewStoreWithCap(10)
	if err := s.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixMilli()
	s.Record(mustEvent("r#1", KindEnumerated, base)) // first write (lastPersist zero)
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("first record should persist: %v", err)
	}
	// A flood of non-terminal refreshes within the throttle window coalesces.
	for i := 0; i < 20; i++ {
		s.Record(mustEvent("r#1", KindEnumerated, base+int64(i)))
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info2.ModTime().Equal(info1.ModTime()) {
		t.Fatal("non-terminal flood should not rewrite the file inside the throttle window")
	}
	// A terminal stage forces the write through the throttle.
	s.Record(mustEvent("r#1", KindMerged, base+100))
	s2 := NewStoreWithCap(10)
	if err := s2.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	if j, ok := s2.Journey("r#1"); !ok || j.Current != KindMerged {
		t.Fatalf("terminal stage must persist immediately, got %+v", j)
	}
}

func TestJSONShapeOfJourney(t *testing.T) {
	s := NewStoreWithCap(4)
	s.Record(Event{IssueRef: "o/r#1", Kind: KindKicked, Agent: "bob", At: 123,
		Attrs: map[string]string{"k": "v"}})
	j, _ := s.Journey("o/r#1")
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ref", "current", "firstAt", "lastAt", "stages"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("journey JSON missing %q: %s", key, data)
		}
	}
	var back Journey
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Stages[KindKicked] == nil || back.Stages[KindKicked].Attrs["k"] != "v" {
		t.Fatalf("journey did not round-trip: %+v", back)
	}
}

func TestFromSpanKnown(t *testing.T) {
	e, ok := FromSpan("pr.merged", map[string]string{
		"issue.ref": "o/r#5",
		"agent":     "quality",
		"event.id":  "evt-1",
		"extra":     "kept",
	})
	if !ok {
		t.Fatal("pr.merged should map")
	}
	if e.Kind != KindMerged || e.IssueRef != "o/r#5" || e.Agent != "quality" || e.ID != "evt-1" {
		t.Fatalf("typed fields wrong: %+v", e)
	}
	if e.Attrs["extra"] != "kept" {
		t.Fatalf("attrs not preserved: %+v", e.Attrs)
	}
}

func TestFromSpanAllKnownNames(t *testing.T) {
	for name, want := range spanKindMap {
		e, ok := FromSpan(name, nil)
		if !ok || e.Kind != want {
			t.Fatalf("FromSpan(%q) = (%v,%v), want kind %s", name, e.Kind, ok, want)
		}
	}
}

func TestFromSpanUnknown(t *testing.T) {
	if _, ok := FromSpan("http.request", map[string]string{"a": "b"}); ok {
		t.Fatal("non-lifecycle span must not map")
	}
}

func TestRecordViaRecorderInterface(t *testing.T) {
	var r Recorder = NewStoreWithCap(4)
	r.Record(Event{IssueRef: "i#1", Kind: KindEnumerated})
	s := r.(*Store)
	if len(s.Journeys(0)) != 1 {
		t.Fatal("Record via interface did not land")
	}
}

func TestKnownKinds(t *testing.T) {
	kinds := KnownKinds()
	want := []Kind{KindEnumerated, KindClassified, KindKicked, KindPROpened, KindMerged, KindBlocked}
	if len(kinds) != len(want) {
		t.Fatalf("KnownKinds = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("KnownKinds[%d] = %s, want %s", i, kinds[i], want[i])
		}
	}
}

func TestNilStoreSafety(t *testing.T) {
	var s *Store
	s.Record(Event{IssueRef: "x#1", Kind: KindKicked}) // must not panic
	if got := s.Recent(5); got == nil || len(got) != 0 {
		t.Fatal("nil store Recent must be empty non-nil")
	}
	if got := s.ByIssue("x#1"); got == nil || len(got) != 0 {
		t.Fatal("nil store ByIssue must be empty non-nil")
	}
	if got := s.Journeys(5); got == nil || len(got) != 0 {
		t.Fatal("nil store Journeys must be empty non-nil")
	}
	if _, ok := s.Journey("x#1"); ok {
		t.Fatal("nil store Journey must miss")
	}
	if err := s.EnablePersistence("/tmp/never", nil); err != nil {
		t.Fatal("nil store EnablePersistence must be a no-op")
	}
	fh := s.FleetHealth(time.Hour)
	if fh.InFlight != 0 || fh.Merged != 0 || fh.Blocked != 0 {
		t.Fatalf("nil store FleetHealth = %+v", fh)
	}
}

func TestConcurrentRecordRace(t *testing.T) {
	s := NewStoreWithCap(64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Record(Event{
					IssueRef: fmt.Sprintf("c#%d", i%16),
					Kind:     KnownKinds()[i%6],
					Agent:    fmt.Sprintf("agent-%d", g),
				})
				_ = s.Journeys(4)
				_, _ = s.Journey("c#1")
				_ = s.FleetHealth(time.Hour)
			}
		}(g)
	}
	wg.Wait()
	if got := len(s.Journeys(0)); got == 0 || got > 64 {
		t.Fatalf("journeys after race = %d", got)
	}
}

// TestRecordBackfillsEarlierTimestamps: producers can report out of order (the
// audit sink vs the typed hook); an older At must extend the stage/journey
// span backwards, never shrink or reorder it.
func TestRecordBackfillsEarlierTimestamps(t *testing.T) {
	s := NewStoreWithCap(4)
	base := time.Now().UnixMilli()
	s.Record(mustEvent("r#1", KindKicked, base))
	s.Record(mustEvent("r#1", KindKicked, base-500)) // late-arriving older event
	j, _ := s.Journey("r#1")
	st := j.Stages[KindKicked]
	if st.FirstAt != base-500 || st.LastAt != base {
		t.Fatalf("stage span = [%d,%d], want [%d,%d]", st.FirstAt, st.LastAt, base-500, base)
	}
	if j.FirstAt != base-500 || j.LastAt != base {
		t.Fatalf("journey span = [%d,%d], want [%d,%d]", j.FirstAt, j.LastAt, base-500, base)
	}
}

// TestDeriveCurrentUnknownKindsFallBackToMostRecent: the Kind set is closed
// for producers, but the store tolerates unknown kinds (a future stage, a
// hand-rolled span translation). With no known stage present, the most
// recently touched stage is the honest "current", ties broken by kind name.
func TestDeriveCurrentUnknownKindsFallBackToMostRecent(t *testing.T) {
	s := NewStoreWithCap(4)
	base := time.Now().UnixMilli()
	s.Record(mustEvent("r#1", Kind("triaged"), base))
	s.Record(mustEvent("r#1", Kind("audited"), base+10))
	if j, _ := s.Journey("r#1"); j.Current != Kind("audited") {
		t.Fatalf("current = %s, want the most recent unknown stage", j.Current)
	}
	// Equal LastAt: lexicographically smaller kind wins, deterministically.
	s2 := NewStoreWithCap(4)
	s2.Record(mustEvent("r#2", Kind("zeta"), base))
	s2.Record(mustEvent("r#2", Kind("alpha"), base))
	if j, _ := s2.Journey("r#2"); j.Current != Kind("alpha") {
		t.Fatalf("tie current = %s, want deterministic alpha", j.Current)
	}
	// Only KNOWN progress stages clear a block: a later unknown kind leaves
	// the journey blocked, so an attention flag can't be dismissed by a stage
	// the lifecycle model doesn't understand.
	s3 := NewStoreWithCap(4)
	s3.Record(mustEvent("r#3", KindBlocked, base))
	s3.Record(mustEvent("r#3", Kind("reopened"), base+10))
	if j, _ := s3.Journey("r#3"); j.Current != KindBlocked {
		t.Fatalf("post-unknown current = %s, want blocked to hold", j.Current)
	}
}

// TestRecentLimitTruncates: the ?limit contract on the synthesized event view.
func TestRecentLimitTruncates(t *testing.T) {
	s := NewStoreWithCap(8)
	base := time.Now().UnixMilli()
	for i := 0; i < 4; i++ {
		s.Record(mustEvent(fmt.Sprintf("r#%d", i), KindKicked, base+int64(i)))
	}
	top := s.Recent(2)
	if len(top) != 2 || top[0].IssueRef != "r#3" || top[1].IssueRef != "r#2" {
		t.Fatalf("Recent(2) = %+v", top)
	}
}

// TestFleetHealthClampsFutureTimestamps: a producer with a skewed clock must
// not yield negative coverage.
func TestFleetHealthClampsFutureTimestamps(t *testing.T) {
	s := NewStoreWithCap(4)
	s.Record(mustEvent("r#1", KindKicked, time.Now().Add(time.Hour).UnixMilli()))
	fh := s.FleetHealth(6 * time.Hour)
	if fh.CoveredMs < 0 {
		t.Fatalf("CoveredMs = %d, want clamped >= 0", fh.CoveredMs)
	}
	if fh.InFlight != 1 {
		t.Fatalf("fleet = %+v, want the journey counted", fh)
	}
}

// TestEnablePersistenceLoadEnforcesCapAndSkipsInvalid: a file written by a
// larger (or older) store loads into a smaller one without exceeding its cap
// — oldest journeys evicted — and entries with no identity or no stages are
// skipped rather than resurrected as empty rows.
func TestEnablePersistenceLoadEnforcesCapAndSkipsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.json")
	base := time.Now().UnixMilli()

	big := NewStoreWithCap(10)
	if err := big.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		big.Record(mustEvent(fmt.Sprintf("r#%d", i), KindMerged, base+int64(i))) // terminal → persisted
	}

	// Append an invalid journey by hand: no ref, no stages.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedTimeline
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	persisted.Journeys = append(persisted.Journeys,
		Journey{Ref: "", Stages: map[Kind]*Stage{KindKicked: {FirstAt: base, LastAt: base, Count: 1}}},
		Journey{Ref: "empty#1"},
	)
	data, err = json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o660); err != nil {
		t.Fatal(err)
	}

	small := NewStoreWithCap(4)
	if err := small.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	journeys := small.Journeys(0)
	if len(journeys) != 4 {
		t.Fatalf("loaded %d journeys into a cap-4 store, want 4", len(journeys))
	}
	for _, j := range journeys {
		if j.Ref == "" || j.Ref == "empty#1" {
			t.Fatalf("invalid persisted journey resurrected: %+v", j)
		}
	}
	// LRU on load: the two oldest (r#0, r#1) are the ones evicted.
	if _, ok := small.Journey("r#0"); ok {
		t.Fatal("oldest journey should be evicted on over-cap load")
	}
	if _, ok := small.Journey("r#5"); !ok {
		t.Fatal("newest journey lost on load")
	}
}

// TestEnablePersistenceUnreadableFileReturnsError: an existing file we cannot
// read is a real error the caller must hear about (unlike ENOENT or corrupt
// bytes) — persistence stays enabled and the next save overwrites.
func TestEnablePersistenceUnreadableFileReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are advisory for root")
	}
	path := filepath.Join(t.TempDir(), "lifecycle.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"journeys":[]}`), 0o000); err != nil {
		t.Fatal(err)
	}
	s := NewStoreWithCap(4)
	if err := s.EnablePersistence(path, nil); err == nil {
		t.Fatal("unreadable existing file must surface an error, not silently start empty")
	}
	// Recording still works and re-persists over the unreadable file.
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	s.Record(mustEvent("r#1", KindMerged, time.Now().UnixMilli()))
	s2 := NewStoreWithCap(4)
	if err := s2.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Journey("r#1"); !ok {
		t.Fatal("store did not recover persistence after the unreadable-load error")
	}
}

// TestPersistFailureKeepsJourneysAndThrottlesLogging: a persist that cannot
// land (missing directory — the dev-machine /data case) must never lose the
// in-memory journeys or panic, and repeated failures within the log-throttle
// window must not re-log. Verified behaviorally: journeys stay queryable and
// the dirty state is retried on the next terminal record.
func TestPersistFailureKeepsJourneysAndThrottlesLogging(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist", "lifecycle.json")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	s := NewStoreWithCap(4)
	if err := s.EnablePersistence(missing, logger); err != nil {
		t.Fatalf("ENOENT on load must not error: %v", err)
	}
	base := time.Now().UnixMilli()
	s.Record(mustEvent("r#1", KindMerged, base))  // forced persist → fails, logs
	s.Record(mustEvent("r#2", KindBlocked, base)) // second failure inside the log-throttle window
	if got := len(s.Journeys(0)); got != 2 {
		t.Fatalf("failed persistence dropped journeys: %d, want 2", got)
	}
	if j, ok := s.Journey("r#1"); !ok || j.Current != KindMerged {
		t.Fatalf("journey degraded after persist failure: %+v ok=%v", j, ok)
	}

	// Once the directory exists, the next terminal record lands everything.
	if err := os.Mkdir(filepath.Dir(missing), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Record(mustEvent("r#3", KindMerged, base+1))
	s2 := NewStoreWithCap(4)
	if err := s2.EnablePersistence(missing, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(s2.Journeys(0)); got != 3 {
		t.Fatalf("recovered persist wrote %d journeys, want all 3 (dirty state retried)", got)
	}
}
