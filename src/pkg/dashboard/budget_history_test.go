package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
)

// ── Per-budget-window history (#4298) ───────────────────────────────────────

func windowStatus(startsAt, endsAt time.Time, limit, used int64, exhausted bool) *StatusPayload {
	b := FrontendBudget{WeeklyBudget: limit, Used: used, Exhausted: exhausted}
	if !endsAt.IsZero() {
		b.WindowEndsAt = endsAt.UTC().Format(time.RFC3339)
		b.WindowStartsAt = startsAt.UTC().Format(time.RFC3339)
	}
	return &StatusPayload{Budget: b}
}

// TestWindowRollRecordsPeakNotPostResetReading is the property the whole
// feature turns on. The roll is only ever observed on the status build AFTER it
// happened, by which point the live spend has already reset toward zero. If the
// tracker recorded the reading it sees at that moment, every window in the
// report would say "barely used" — the exact opposite of the answer the
// operator needs.
func TestWindowRollRecordsPeakNotPostResetReading(t *testing.T) {
	s := &Server{}
	w1Start := time.Now().Add(-14 * 24 * time.Hour)
	w1End := w1Start.Add(7 * 24 * time.Hour)

	s.ObserveBudgetWindow(windowStatus(w1Start, w1End, 1_000_000, 10_000, false))
	s.ObserveBudgetWindow(windowStatus(w1Start, w1End, 1_000_000, 940_000, false))
	// The window peaked here, then rolled.
	s.ObserveBudgetWindow(windowStatus(w1Start, w1End, 1_000_000, 1_000_000, true))

	// Next window opens; live spend is near zero again.
	w2Start := w1End
	w2End := w2Start.Add(7 * 24 * time.Hour)
	s.ObserveBudgetWindow(windowStatus(w2Start, w2End, 1_000_000, 12, false))

	hist := s.BudgetWindowHistory()
	if len(hist) != 1 {
		t.Fatalf("expected one closed window, got %d: %+v", len(hist), hist)
	}
	if hist[0].Used != 1_000_000 {
		t.Errorf("Used = %d, want the peak 1000000 — a post-reset reading would record ~12", hist[0].Used)
	}
	if !hist[0].Exhausted {
		t.Error("the closed window hit its limit and must be recorded as exhausted")
	}
	if hist[0].PctUsed < 99.9 {
		t.Errorf("PctUsed = %v, want ~100", hist[0].PctUsed)
	}
	// The status payload carries RFC3339, which is whole seconds — so the
	// recorded boundary is the second, not the sub-second instant the test
	// happened to construct. Compare against what the wire format can actually
	// preserve rather than pretending it round-trips milliseconds.
	wantEnd := w1End.UTC().Truncate(time.Second).UnixMilli()
	if hist[0].WindowEnd != wantEnd {
		t.Errorf("WindowEnd = %d, want %d", hist[0].WindowEnd, wantEnd)
	}
}

// TestHistoryAnswersTheVacationQuestion is the use case verbatim: over several
// closed windows, which ones hit the budget?
func TestHistoryAnswersTheVacationQuestion(t *testing.T) {
	s := &Server{}
	start := time.Now().Add(-28 * 24 * time.Hour)
	const limit = int64(1_000_000)
	week := 7 * 24 * time.Hour

	// Four windows: quiet, exhausted, exhausted, quiet.
	peaks := []struct {
		used      int64
		exhausted bool
	}{
		{200_000, false},
		{limit, true},
		{limit, true},
		{350_000, false},
	}
	for i, p := range peaks {
		ws := start.Add(time.Duration(i) * week)
		we := ws.Add(week)
		s.ObserveBudgetWindow(windowStatus(ws, we, limit, p.used, p.exhausted))
	}
	// One more observation to roll the last one closed.
	last := start.Add(time.Duration(len(peaks)) * week)
	s.ObserveBudgetWindow(windowStatus(last, last.Add(week), limit, 0, false))

	hist := s.BudgetWindowHistory()
	if len(hist) != 4 {
		t.Fatalf("expected 4 closed windows, got %d", len(hist))
	}
	// Newest first.
	if hist[0].Used != 350_000 {
		t.Errorf("newest window Used = %d, want 350000 — history must be newest first", hist[0].Used)
	}
	exhaustedCount := 0
	for _, e := range hist {
		if e.Exhausted {
			exhaustedCount++
		}
	}
	if exhaustedCount != 2 {
		t.Errorf("exhausted windows = %d, want 2 — this is the answer to 'did the budget limit activity?'", exhaustedCount)
	}
}

// TestLimitIsRecordedPerWindow pins that raising the limit later cannot rewrite
// history and make a window that genuinely ran out look comfortable.
func TestLimitIsRecordedPerWindow(t *testing.T) {
	s := &Server{}
	ws := time.Now().Add(-14 * 24 * time.Hour)
	we := ws.Add(7 * 24 * time.Hour)

	s.ObserveBudgetWindow(windowStatus(ws, we, 100_000, 100_000, true))
	// Operator raises the limit; a new window opens under the new limit.
	s.ObserveBudgetWindow(windowStatus(we, we.Add(7*24*time.Hour), 5_000_000, 1_000, false))

	hist := s.BudgetWindowHistory()
	if len(hist) != 1 {
		t.Fatalf("expected one closed window, got %d", len(hist))
	}
	if hist[0].Limit != 100_000 {
		t.Errorf("Limit = %d, want the 100000 in force at the time, not the raised 5000000", hist[0].Limit)
	}
	if !hist[0].Exhausted {
		t.Error("raising the limit afterwards must not un-exhaust a past window")
	}
}

// TestNoBudgetConfiguredRecordsNothing pins the compatibility requirement:
// budgeting off must not produce a stream of empty rows.
func TestNoBudgetConfiguredRecordsNothing(t *testing.T) {
	s := &Server{}
	for i := 0; i < 5; i++ {
		s.ObserveBudgetWindow(&StatusPayload{Budget: FrontendBudget{}})
	}
	if hist := s.BudgetWindowHistory(); len(hist) != 0 {
		t.Fatalf("no configured budget must record nothing, got %+v", hist)
	}
}

// TestZeroValueServerAndNilStatusAreSafe is the "must not crash when installed
// into an existing environment" requirement, taken literally.
func TestZeroValueServerAndNilStatusAreSafe(t *testing.T) {
	s := &Server{}
	s.ObserveBudgetWindow(nil)
	if hist := s.BudgetWindowHistory(); len(hist) != 0 {
		t.Fatalf("a zero-value Server with no observations must report no history, got %+v", hist)
	}
	// A window whose timestamps do not parse must be ignored, not panic.
	s.ObserveBudgetWindow(&StatusPayload{Budget: FrontendBudget{
		WeeklyBudget: 10, Used: 5, WindowEndsAt: "not-a-timestamp", WindowStartsAt: "also-not",
	}})
	if hist := s.BudgetWindowHistory(); len(hist) != 0 {
		t.Fatalf("unparseable window bounds must record nothing, got %+v", hist)
	}
}

// TestRetentionKeepsNewest pins the ring bound and that it drops the OLDEST.
func TestRetentionKeepsNewest(t *testing.T) {
	s := &Server{}
	start := time.Now().Add(-time.Duration(collect.BudgetWindowMaxEntries+10) * 7 * 24 * time.Hour)
	week := 7 * 24 * time.Hour
	total := collect.BudgetWindowMaxEntries + 5
	for i := 0; i < total+1; i++ {
		ws := start.Add(time.Duration(i) * week)
		s.ObserveBudgetWindow(windowStatus(ws, ws.Add(week), 1000, int64(i), false))
	}

	hist := s.BudgetWindowHistory()
	if len(hist) != collect.BudgetWindowMaxEntries {
		t.Fatalf("history = %d entries, want the cap %d", len(hist), collect.BudgetWindowMaxEntries)
	}
	// Newest first, and the newest closed window is the one before the open one.
	if hist[0].Used != int64(total-1) {
		t.Errorf("newest entry Used = %d, want %d — retention must drop the OLDEST", hist[0].Used, total-1)
	}
}

// TestSeedRoundTripsThroughJSON pins that what is persisted reloads in the same
// order, which is what makes the history survive a restart.
func TestSeedRoundTripsThroughJSON(t *testing.T) {
	s := &Server{}
	start := time.Now().Add(-21 * 24 * time.Hour)
	week := 7 * 24 * time.Hour
	for i := 0; i < 3; i++ {
		ws := start.Add(time.Duration(i) * week)
		s.ObserveBudgetWindow(windowStatus(ws, ws.Add(week), 1000, int64(100+i), false))
	}
	last := start.Add(3 * week)
	s.ObserveBudgetWindow(windowStatus(last, last.Add(week), 1000, 0, false))

	saved := s.BudgetWindowHistory()
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var reloaded []collect.BudgetWindowEntry
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	restarted := &Server{}
	restarted.SeedBudgetWindowHistory(reloaded)
	got := restarted.BudgetWindowHistory()

	if len(got) != len(saved) {
		t.Fatalf("after restart: %d entries, want %d", len(got), len(saved))
	}
	for i := range saved {
		if got[i] != saved[i] {
			t.Errorf("entry %d changed across the round trip: %+v vs %+v", i, got[i], saved[i])
		}
	}
}

// TestBudgetHistoryEndpointAlwaysReturnsAnArray is the compatibility guard at
// the wire. A hive with no history must serve `[]`, never `null` — a client
// that iterates the field would break on nil, which is exactly what #4298 says
// must not happen when the feature lands in an environment that kept no
// history.
func TestBudgetHistoryEndpointAlwaysReturnsAnArray(t *testing.T) {
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	req := httptest.NewRequest("GET", "/api/budget/history", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Windows []collect.BudgetWindowEntry `json:"windows"`
	}
	raw := rec.Body.String()
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, raw)
	}
	if body.Windows == nil {
		t.Errorf("windows must serialize as [] on a fresh hive, not null: %s", raw)
	}
	if !containsSub(raw, `"windows":[]`) {
		t.Errorf("expected an empty array on the wire, got: %s", raw)
	}
}

func containsSub(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
