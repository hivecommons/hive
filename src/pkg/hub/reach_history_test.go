package hub

// Tests for the fleet-level error-rate history retention (#3995, phase 2c of
// #3973). The store's input is spoke-authored (sanitized upstream but still
// hostile in spirit), so the load-bearing assertions are the bounds: the ring
// evicts oldest-first at its window cap, over-cap components are clipped AND
// logged (never silently absorbed), a hive re-reporting the same rolling
// window never double-counts, out-of-range timestamps are refused, and every
// cap is re-applied on load so a hand-edited file cannot bypass them.
//
// Fixtures are production-possible states only: multi-hive mixed-commit
// fleets, never one hive reporting two commits at once (2a's loader drops
// other-commit entries on upgrade, so that state cannot exist).

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tracing"
)

// newTestHistoryStore builds a store persisting under t.TempDir, returning
// the store, its path, and a buffer capturing its log output.
func newTestHistoryStore(t *testing.T) (*reachHistoryStore, string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	path := filepath.Join(t.TempDir(), "reach-history.json")
	return newReachHistoryStore(path, logger), path, &buf
}

// histReport builds a single-entry sanitized-shape report whose rolling
// window starts at start.
func histReport(component, commit string, start time.Time, winTotal, winErr int64) *tracing.ReachReport {
	return &tracing.ReachReport{Entries: []tracing.ReachEntry{{
		Component:  component,
		Commit:     commit,
		SpansTotal: winTotal,
		SpansError: winErr,
		FirstSeen:  start.UTC().Format(time.RFC3339),
		LastSeen:   start.UTC().Format(time.RFC3339),
		Window1h: &tracing.ReachWindow{
			Start:      start.UTC().Format(time.RFC3339),
			SpansTotal: winTotal,
			SpansError: winErr,
		},
	}}}
}

// TestReachHistorySumsAcrossHives: a mixed fleet reporting into the same hour
// bucket sums per (bucket, commit) — two hives on the same commit fold into
// one sample; a hive on a different (older) commit lands its own sample.
// histNow is a fixed clock for the store tests: mid-hour, so minute-offset
// window starts below stay inside one bucket deterministically (a real
// time.Now() near an hour boundary would make bucket membership flaky).
var histNow = time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)

func TestReachHistorySumsAcrossHives(t *testing.T) {
	store, _, _ := newTestHistoryStore(t)
	now := histNow
	start := now.Add(-25 * time.Minute) // 12:05 — same bucket through +8m offsets

	store.Append("hive-a", histReport("hub", "beef456", start, 100, 10), now)
	store.Append("hive-b", histReport("hub", "beef456", start.Add(2*time.Minute), 50, 5), now)
	store.Append("hive-c", histReport("hub", "01dc0de", start.Add(time.Minute), 30, 3), now)

	windows := store.ComponentWindows("hub")
	if len(windows) != 2 {
		t.Fatalf("samples = %d, want 2 (one per commit in the bucket): %+v", len(windows), windows)
	}
	byCommit := map[string][2]int64{}
	for _, w := range windows {
		byCommit[w.Commit] = [2]int64{w.SpansTotal, w.SpansError}
	}
	if got := byCommit["beef456"]; got != [2]int64{150, 15} {
		t.Errorf("beef456 sum = %v, want {150 15}", got)
	}
	if got := byCommit["01dc0de"]; got != [2]int64{30, 3} {
		t.Errorf("01dc0de sum = %v, want {30 3}", got)
	}
}

// TestReachHistoryDedupesReReportedWindow is the dedupe guard: the same hive
// re-reporting the SAME rolling window (heartbeats beat far more often than
// windows roll) must contribute only its increment, never a second full copy.
// Removing the cursor increment branch in Append makes this fail with a
// doubled total (verified during development).
func TestReachHistoryDedupesReReportedWindow(t *testing.T) {
	store, _, _ := newTestHistoryStore(t)
	now := histNow
	start := now.Add(-25 * time.Minute)

	// Same window reported three times as it fills: 40 → 40 (idle beat) → 100.
	store.Append("hive-a", histReport("hub", "beef456", start, 40, 4), now)
	store.Append("hive-a", histReport("hub", "beef456", start, 40, 4), now)
	store.Append("hive-a", histReport("hub", "beef456", start, 100, 10), now)

	windows := store.ComponentWindows("hub")
	if len(windows) != 1 {
		t.Fatalf("samples = %d, want 1: %+v", len(windows), windows)
	}
	if windows[0].SpansTotal != 100 || windows[0].SpansError != 10 {
		t.Errorf("sample = {%d %d}, want {100 10} — re-reports must not double-count",
			windows[0].SpansTotal, windows[0].SpansError)
	}

	// A mid-window spoke restart resumes lower: the negative increment is
	// clamped to zero (a small undercount), never subtracted or re-added.
	store.Append("hive-a", histReport("hub", "beef456", start, 60, 6), now)
	windows = store.ComponentWindows("hub")
	if windows[0].SpansTotal != 100 || windows[0].SpansError != 10 {
		t.Errorf("after lower re-report: sample = {%d %d}, want unchanged {100 10}",
			windows[0].SpansTotal, windows[0].SpansError)
	}
}

// TestReachHistoryRingBound: the per-component ring holds at most
// reachHistoryWindows distinct hour buckets, evicting oldest-first.
func TestReachHistoryRingBound(t *testing.T) {
	store, _, _ := newTestHistoryStore(t)
	base := histNow.Truncate(time.Hour)

	// Fill the ring exactly: reachHistoryWindows buckets, all valid at histNow.
	for i := reachHistoryWindows - 1; i >= 0; i-- {
		start := base.Add(-time.Duration(i) * time.Hour).Add(5 * time.Minute)
		store.Append("hive-a", histReport("hub", "beef456", start, int64(i+1), 0), histNow)
	}
	if got := len(store.ComponentWindows("hub")); got != reachHistoryWindows {
		t.Fatalf("precondition: ring holds %d buckets, want %d", got, reachHistoryWindows)
	}

	// An hour later a new window arrives: the ring is over-full and must
	// evict its OLDEST bucket, keeping exactly reachHistoryWindows.
	later := histNow.Add(time.Hour)
	store.Append("hive-a", histReport("hub", "beef456", base.Add(time.Hour).Add(5*time.Minute), 7, 0), later)

	windows := store.ComponentWindows("hub")
	if len(windows) != reachHistoryWindows {
		t.Fatalf("ring holds %d buckets after overflow, want exactly %d", len(windows), reachHistoryWindows)
	}
	oldestKept := windows[0].WindowStart
	wantOldest := base.Add(-time.Duration(reachHistoryWindows-2) * time.Hour)
	if !oldestKept.Equal(wantOldest) {
		t.Errorf("oldest kept bucket = %v, want %v (eviction must be oldest-first)", oldestKept, wantOldest)
	}
	newest := windows[len(windows)-1].WindowStart
	if !newest.Equal(base.Add(time.Hour)) {
		t.Errorf("newest bucket = %v, want %v (the new window must have landed)", newest, base.Add(time.Hour))
	}
}

// TestReachHistoryComponentCapClippedAndLogged: a new component past the cap
// is refused, the refusal is counted, and it is LOGGED — never silent.
// Removing the cap check in Append makes this fail (verified during
// development).
func TestReachHistoryComponentCapClippedAndLogged(t *testing.T) {
	store, _, logBuf := newTestHistoryStore(t)
	now := histNow
	start := now.Add(-25 * time.Minute)

	for i := 0; i < maxReachHistoryComponents; i++ {
		store.Append("hive-a", histReport(fmt.Sprintf("comp%04d", i), "beef456", start, 1, 0), now)
	}
	store.Append("hive-a", histReport("one-too-many", "beef456", start, 1, 0), now)

	if got := store.ComponentWindows("one-too-many"); got != nil {
		t.Errorf("over-cap component was tracked: %+v", got)
	}
	store.mu.Lock()
	clipped := store.clippedComponents
	tracked := len(store.components)
	store.mu.Unlock()
	if tracked != maxReachHistoryComponents {
		t.Errorf("tracked components = %d, want capped at %d", tracked, maxReachHistoryComponents)
	}
	if clipped == 0 {
		t.Error("clippedComponents = 0, want > 0 (the clip must be counted)")
	}
	if !strings.Contains(logBuf.String(), "component cap") {
		t.Error("over-cap clip was not logged — clips must be visible, never silent")
	}
	// Components already inside the cap keep accumulating normally (a second
	// hive reporting an earlier hour — no cursor, so no ordering guard).
	store.Append("hive-b", histReport("comp0000", "beef456", start.Add(-time.Hour), 5, 1), now)
	if got := store.ComponentWindows("comp0000"); len(got) != 2 {
		t.Errorf("in-cap component stopped accumulating: %+v", got)
	}
}

// TestReachHistoryRefusesHostileWindows: unparseable, far-future, and
// ancient window starts are dropped and counted; entries without a commit or
// rolling window are skipped as non-input.
func TestReachHistoryRefusesHostileWindows(t *testing.T) {
	store, _, logBuf := newTestHistoryStore(t)
	now := time.Now().UTC()

	hostile := &tracing.ReachReport{Entries: []tracing.ReachEntry{
		// Unparseable start (sanitizeComponentReach blanks invalid times).
		{Component: "hub", Commit: "beef456", Window1h: &tracing.ReachWindow{Start: "", SpansTotal: 10}},
		// Future window: a broken or lying clock.
		{Component: "hub", Commit: "beef456", Window1h: &tracing.ReachWindow{
			Start: now.Add(2 * time.Hour).Format(time.RFC3339), SpansTotal: 10}},
		// Older than the ring span: would poison the "before" side.
		{Component: "hub", Commit: "beef456", Window1h: &tracing.ReachWindow{
			Start: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339), SpansTotal: 10}},
		// No commit: unattributable to a binary, not history input.
		{Component: "hub", Commit: "", Window1h: &tracing.ReachWindow{
			Start: now.Add(-time.Minute).Format(time.RFC3339), SpansTotal: 10}},
		// No rolling window at all.
		{Component: "hub", Commit: "beef456"},
	}}
	store.Append("hive-a", hostile, now)

	if got := store.ComponentWindows("hub"); got != nil {
		t.Errorf("hostile input landed in the ring: %+v", got)
	}
	store.mu.Lock()
	dropped := store.droppedSamples
	store.mu.Unlock()
	// The three timestamp offenders count as drops; the commit-less and
	// window-less entries are non-input, not hostile.
	if dropped != 3 {
		t.Errorf("droppedSamples = %d, want 3", dropped)
	}
	if !strings.Contains(logBuf.String(), "refused out-of-bounds input") {
		t.Error("hostile drop was not logged")
	}
}

// TestReachHistoryOutOfOrderWindowDropped: a window OLDER than the hive's
// last folded bucket is a replay and must not be re-summed.
func TestReachHistoryOutOfOrderWindowDropped(t *testing.T) {
	store, _, _ := newTestHistoryStore(t)
	now := time.Now().UTC()
	newer := now.Add(-30 * time.Minute)
	older := now.Add(-3 * time.Hour)

	store.Append("hive-a", histReport("hub", "beef456", newer, 100, 10), now)
	store.Append("hive-a", histReport("hub", "beef456", older, 40, 4), now)

	windows := store.ComponentWindows("hub")
	if len(windows) != 1 || !windows[0].WindowStart.Equal(newer.Truncate(time.Hour)) {
		t.Errorf("out-of-order window was folded: %+v", windows)
	}
}

// TestReachHistoryCommitsPerBucketCap: distinct commits per (component, hour
// bucket) are capped; the over-cap commit is refused and counted.
func TestReachHistoryCommitsPerBucketCap(t *testing.T) {
	store, _, _ := newTestHistoryStore(t)
	now := histNow
	start := now.Add(-25 * time.Minute) // 12:05 — +8m offsets stay in-bucket

	// One hive per commit: a hive reports ONE running commit at a time (2a's
	// loader guarantees it), so commit fan-out in a bucket comes from many
	// hives on different builds.
	for i := 0; i < maxReachHistoryCommitsPerBucket+1; i++ {
		hive := fmt.Sprintf("hive-%02d", i)
		commit := fmt.Sprintf("c0mm1t%02d", i)
		store.Append(hive, histReport("hub", commit, start.Add(time.Duration(i)*time.Minute), 10, 1), now)
	}
	windows := store.ComponentWindows("hub")
	if len(windows) != maxReachHistoryCommitsPerBucket {
		t.Errorf("bucket holds %d commits, want capped at %d", len(windows), maxReachHistoryCommitsPerBucket)
	}
	store.mu.Lock()
	clipped := store.clippedComponents
	store.mu.Unlock()
	if clipped == 0 {
		t.Error("over-cap commit refusal was not counted")
	}
}

// TestReachHistoryPersistRoundTrip: SaveNow → fresh store → identical
// windows, and the dedupe cursor survives the restart — the production case
// where a hub rolls mid-window and the next heartbeat re-reports a window
// the previous process already summed.
func TestReachHistoryPersistRoundTrip(t *testing.T) {
	store, path, _ := newTestHistoryStore(t)
	now := time.Now().UTC()
	start := now.Add(-30 * time.Minute)

	store.Append("hive-a", histReport("hub", "beef456", start, 40, 4), now)
	store.SaveNow()

	reloaded := newReachHistoryStore(path, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	windows := reloaded.ComponentWindows("hub")
	if len(windows) != 1 || windows[0].SpansTotal != 40 || windows[0].SpansError != 4 {
		t.Fatalf("round-trip windows = %+v, want one {40 4} sample", windows)
	}

	// The same window re-reported (grown) after the "restart": only the
	// increment lands. Without persisted cursors this would re-add 100.
	reloaded.Append("hive-a", histReport("hub", "beef456", start, 100, 10), now)
	windows = reloaded.ComponentWindows("hub")
	if len(windows) != 1 || windows[0].SpansTotal != 100 || windows[0].SpansError != 10 {
		t.Errorf("post-restart re-report = %+v, want one {100 10} sample (cursor must survive restarts)", windows)
	}
}

// TestReachHistoryLoadReboundsCorruptFile: every cap and clamp re-applies at
// load — negative counts, empty commits, and over-bound windows in a
// hand-edited file must not enter memory.
func TestReachHistoryLoadReboundsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reach-history.json")

	base := time.Now().UTC().Truncate(time.Hour)
	var samples []string
	// More distinct buckets than the ring allows…
	for i := 0; i < reachHistoryWindows+10; i++ {
		samples = append(samples, fmt.Sprintf(
			`{"window_start":%q,"commit":"beef456","spans_total":1,"spans_error":0}`,
			base.Add(-time.Duration(i)*time.Hour).Format(time.RFC3339)))
	}
	// …plus corrupt entries that must be dropped.
	samples = append(samples,
		`{"window_start":"2026-08-17T10:00:00Z","commit":"beef456","spans_total":-5,"spans_error":0}`,
		`{"window_start":"2026-08-17T10:00:00Z","commit":"","spans_total":5,"spans_error":0}`)
	content := fmt.Sprintf(`{"components":{"hub":{"samples":[%s],"cursors":{}}}}`, strings.Join(samples, ","))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	store := newReachHistoryStore(path, slog.New(slog.NewTextHandler(&buf, nil)))
	windows := store.ComponentWindows("hub")
	if len(windows) > reachHistoryWindows {
		t.Errorf("loaded %d buckets, want re-bounded to %d", len(windows), reachHistoryWindows)
	}
	for _, w := range windows {
		if w.SpansTotal < 0 || w.Commit == "" {
			t.Errorf("corrupt sample survived load: %+v", w)
		}
	}
	if !strings.Contains(buf.String(), "re-bounded") {
		t.Error("load re-bounding was not logged")
	}
}

// TestReachHistoryConcurrentAccess exercises Append vs ComponentWindows under
// the race detector (run with -race): heartbeats and /api/reach requests are
// concurrent in production.
func TestReachHistoryConcurrentAccess(t *testing.T) {
	store, _, _ := newTestHistoryStore(t)
	now := time.Now().UTC()
	start := now.Add(-30 * time.Minute)

	var wg sync.WaitGroup
	const writers = 4
	const iterations = 50
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			hive := fmt.Sprintf("hive-%d", w)
			for i := 0; i < iterations; i++ {
				store.Append(hive, histReport("hub", "beef456", start, int64(i), 0), now)
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writers*iterations; i++ {
			store.ComponentWindows("hub")
		}
	}()
	wg.Wait()
}

// TestHeartbeatFoldsReachHistory: the end-to-end receive path — a heartbeat
// carrying component_reach with a rolling window lands in the history store
// via the SANITIZED report, a repeat beat dedupes, and a second hive sums.
func TestHeartbeatFoldsReachHistory(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	var buf bytes.Buffer
	s.reachHistory = newReachHistoryStore(filepath.Join(t.TempDir(), "rh.json"), slog.New(slog.NewTextHandler(&buf, nil)))

	start := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	body := func(hive string, winTotal, winErr int64) string {
		return fmt.Sprintf(`{"hive_id":%q,"org":"kubestellar","component_reach":{"entries":[
			{"component":"hub","commit":"abc1234","spans_total":%d,"spans_error":%d,
			 "first_seen":%q,"last_seen":%q,
			 "window_1h":{"start":%q,"spans_total":%d,"spans_error":%d}}]}}`,
			hive, winTotal, winErr, start, start, start, winTotal, winErr)
	}

	if rec := postHeartbeat(t, s, body("h1", 40, 4)); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	// The same beat again (same window, same counts): dedupe, no growth.
	if rec := postHeartbeat(t, s, body("h1", 40, 4)); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d", rec.Code)
	}
	// A second hive in the same hour: sums.
	if rec := postHeartbeat(t, s, body("h2", 10, 1)); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d", rec.Code)
	}

	windows := s.reachHistory.ComponentWindows("hub")
	if len(windows) != 1 {
		t.Fatalf("windows = %+v, want one summed bucket", windows)
	}
	if windows[0].SpansTotal != 50 || windows[0].SpansError != 5 {
		t.Errorf("bucket = {%d %d}, want {50 5} (h1 deduped + h2 summed)",
			windows[0].SpansTotal, windows[0].SpansError)
	}
	// A beat WITHOUT component_reach must not touch history (nil report —
	// the carry-forward path keeps the registry copy, not the ring).
	if rec := postHeartbeat(t, s, `{"hive_id":"h1","org":"kubestellar"}`); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d", rec.Code)
	}
	windows = s.reachHistory.ComponentWindows("hub")
	if len(windows) != 1 || windows[0].SpansTotal != 50 {
		t.Errorf("report-less beat mutated history: %+v", windows)
	}
}
