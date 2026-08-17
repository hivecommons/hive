package tracing

// Tests for the component reach counters (#3993, phase 2a of #3973).
//
// The load-bearing claims verified here:
//   - counters increment WITHOUT any exporter configured (design D2);
//   - concurrent increments are exact under -race;
//   - the MaxReachComponents cap counts overflow and LOGS the truncation once
//     per component, never silently;
//   - persistence round-trips through the reach-state file, and a different
//     running commit starts fresh keys;
//   - the rolling 1h bucket resets after expiry.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// testCommit is the fake running-binary SHA reach tests key their counters by.
const testCommit = "abc1234"

// collectHandler is a minimal slog.Handler that records every message it
// receives, so tests can assert the overflow truncation is actually logged.
type collectHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *collectHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *collectHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *collectHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(string) slog.Handler      { return h }

func (h *collectHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

// resetReach gives each test a clean registry and restores the previous global
// tracer provider afterward. Installing the explicit no-op provider is the
// point of most of these tests: NO exporter, NO SDK — and the counters must
// increment anyway (D2).
func resetReach(t *testing.T) *collectHandler {
	t.Helper()
	h := &collectHandler{}
	resetReachForTest(testCommit, slog.New(h))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		resetReachForTest("", nil)
	})
	return h
}

// findEntry returns the entry for component, failing the test when absent.
func findEntry(t *testing.T, report *ReachReport, component string) ReachEntry {
	t.Helper()
	if report == nil {
		t.Fatalf("reach report is nil, want an entry for %q", component)
	}
	for _, e := range report.Entries {
		if e.Component == component {
			return e
		}
	}
	t.Fatalf("no reach entry for %q in %+v", component, report.Entries)
	return ReachEntry{}
}

// TestReach_CountsWithoutExporter is design D2 verbatim: no tracing.Init, the
// global provider is the no-op, and spans still count — total, error, first
// and last seen, commit key, and the 1h window.
func TestReach_CountsWithoutExporter(t *testing.T) {
	resetReach(t)
	ctx := context.Background()

	_, ok := StartSpan(ctx, "governor.eval_cycle")
	ok.End()

	_, errSpan := StartSpan(ctx, "governor.enumerate")
	errSpan.SetStatus(codes.Error, "boom")
	errSpan.End()

	_, other := StartSpan(ctx, "agent.kick")
	other.End()

	report := ReachSnapshot()
	gov := findEntry(t, report, "governor")
	if gov.SpansTotal != 2 || gov.SpansError != 1 {
		t.Errorf("governor = total %d / error %d, want 2 / 1", gov.SpansTotal, gov.SpansError)
	}
	if gov.Commit != testCommit {
		t.Errorf("governor commit = %q, want %q", gov.Commit, testCommit)
	}
	if gov.FirstSeen == "" || gov.LastSeen == "" {
		t.Errorf("governor first/last seen empty: %+v", gov)
	}
	if gov.Window1h == nil || gov.Window1h.SpansTotal != 2 || gov.Window1h.SpansError != 1 {
		t.Errorf("governor window = %+v, want total 2 / error 1", gov.Window1h)
	}
	agent := findEntry(t, report, "agent")
	if agent.SpansTotal != 1 || agent.SpansError != 0 {
		t.Errorf("agent = total %d / error %d, want 1 / 0", agent.SpansTotal, agent.SpansError)
	}
}

// TestReach_OkStatusOverridesError follows the OTel status precedence: a span
// whose status ends up Ok must not count as errored, and double-End must not
// double-count.
func TestReach_OkStatusOverridesError(t *testing.T) {
	resetReach(t)

	_, span := StartSpan(context.Background(), "pr.merged")
	span.SetStatus(codes.Error, "transient")
	span.SetStatus(codes.Ok, "")
	span.SetStatus(codes.Error, "must not downgrade Ok")
	span.End()
	span.End() // second End is a no-op for counting

	e := findEntry(t, ReachSnapshot(), "pr")
	if e.SpansTotal != 1 || e.SpansError != 0 {
		t.Errorf("pr = total %d / error %d, want 1 / 0", e.SpansTotal, e.SpansError)
	}
}

// TestReach_ConcurrentIncrements hammers the counters from many goroutines
// (run under -race) and requires EXACT totals — atomic counters may not lose
// increments.
func TestReach_ConcurrentIncrements(t *testing.T) {
	resetReach(t)

	const goroutines = 16
	const spansPerGoroutine = 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < spansPerGoroutine; i++ {
				_, span := StartSpan(context.Background(), "governor.eval_cycle")
				if i%2 == 0 {
					span.SetStatus(codes.Error, "boom")
				}
				span.End()
			}
		}(g)
	}
	wg.Wait()

	e := findEntry(t, ReachSnapshot(), "governor")
	wantTotal := int64(goroutines * spansPerGoroutine)
	wantErr := wantTotal / 2
	if e.SpansTotal != wantTotal || e.SpansError != wantErr {
		t.Errorf("governor = total %d / error %d, want %d / %d",
			e.SpansTotal, e.SpansError, wantTotal, wantErr)
	}
}

// TestReach_ComponentCapOverflowLogged fills the registry past
// MaxReachComponents and verifies: exactly the cap survives, the excess spans
// are counted in overflow_spans, the distinct refused names are counted, and
// the truncation is logged ONCE per refused component — never silent, never
// once per span.
func TestReach_ComponentCapOverflowLogged(t *testing.T) {
	h := resetReach(t)

	for i := 0; i < MaxReachComponents; i++ {
		_, span := StartSpan(context.Background(), fmt.Sprintf("comp%03d.op", i))
		span.End()
	}
	logsBefore := h.count()

	// Three refused components, two spans each: 6 overflow spans, 3 names,
	// and exactly 3 new log lines (one per name, not one per span).
	const refusedComponents = 3
	const spansPerRefused = 2
	for i := 0; i < refusedComponents; i++ {
		for j := 0; j < spansPerRefused; j++ {
			_, span := StartSpan(context.Background(), fmt.Sprintf("overflow%d.op", i))
			span.SetStatus(codes.Error, "must not panic or count against a capped component")
			span.End()
		}
	}

	report := ReachSnapshot()
	if len(report.Entries) != MaxReachComponents {
		t.Errorf("entries = %d, want exactly %d", len(report.Entries), MaxReachComponents)
	}
	if want := int64(refusedComponents * spansPerRefused); report.OverflowSpans != want {
		t.Errorf("overflow_spans = %d, want %d", report.OverflowSpans, want)
	}
	if report.OverflowComponents != refusedComponents {
		t.Errorf("overflow_components = %d, want %d", report.OverflowComponents, refusedComponents)
	}
	if got := h.count() - logsBefore; got != refusedComponents {
		t.Errorf("truncation log lines = %d, want %d (once per refused component)", got, refusedComponents)
	}
}

// TestReach_PersistenceRoundTrip saves counters, wipes the registry, reloads
// for the SAME commit, and requires the counts back intact — then reloads for
// a DIFFERENT commit and requires a fresh start (commit-keyed entries are the
// mechanism by which a new binary never inherits reach it did not earn).
func TestReach_PersistenceRoundTrip(t *testing.T) {
	resetReach(t)
	path := filepath.Join(t.TempDir(), "reach-state.json")

	for i := 0; i < 5; i++ {
		_, span := StartSpan(context.Background(), "governor.eval_cycle")
		if i < 2 {
			span.SetStatus(codes.Error, "boom")
		}
		span.End()
	}
	before := findEntry(t, ReachSnapshot(), "governor")

	if err := SaveReachState(path); err != nil {
		t.Fatalf("SaveReachState: %v", err)
	}

	// Same commit: counters resume.
	resetReachForTest("", nil)
	if err := LoadReachState(path, testCommit, slog.New(&collectHandler{})); err != nil {
		t.Fatalf("LoadReachState (same commit): %v", err)
	}
	after := findEntry(t, ReachSnapshot(), "governor")
	if after.SpansTotal != before.SpansTotal || after.SpansError != before.SpansError {
		t.Errorf("resumed = total %d / error %d, want %d / %d",
			after.SpansTotal, after.SpansError, before.SpansTotal, before.SpansError)
	}
	if after.FirstSeen != before.FirstSeen {
		t.Errorf("resumed first_seen = %q, want %q", after.FirstSeen, before.FirstSeen)
	}
	if after.Window1h == nil || after.Window1h.SpansTotal != before.Window1h.SpansTotal {
		t.Errorf("resumed window = %+v, want %+v", after.Window1h, before.Window1h)
	}

	// Different commit: fresh keys, nothing resumed.
	resetReachForTest("", nil)
	if err := LoadReachState(path, "def5678", slog.New(&collectHandler{})); err != nil {
		t.Fatalf("LoadReachState (new commit): %v", err)
	}
	if report := ReachSnapshot(); report != nil {
		t.Errorf("new commit resumed old counters: %+v", report)
	}
}

// TestReach_LoadMissingFileIsFirstBoot verifies a missing state file is a
// normal first boot, not an error.
func TestReach_LoadMissingFileIsFirstBoot(t *testing.T) {
	resetReach(t)
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if err := LoadReachState(path, testCommit, nil); err != nil {
		t.Fatalf("LoadReachState on a missing file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("load must not create the file")
	}
}

// TestReach_SaveNothingWritesNothing verifies an empty registry writes no file
// — a spoke that emitted no spans has no reach state worth persisting.
func TestReach_SaveNothingWritesNothing(t *testing.T) {
	resetReach(t)
	path := filepath.Join(t.TempDir(), "reach-state.json")
	if err := SaveReachState(path); err != nil {
		t.Fatalf("SaveReachState (empty): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty registry must not write a state file")
	}
}

// TestReach_WindowRollover exercises rollWindow directly with fabricated
// clocks: within the hour the bucket persists, past it the bucket resets.
func TestReach_WindowRollover(t *testing.T) {
	c := &reachCounters{}
	const start = int64(1_000_000)

	c.rollWindow(start)
	c.winTotal.Add(3)
	c.winErrors.Add(1)

	// Still inside the window: nothing resets.
	c.rollWindow(start + reachWindowSeconds - 1)
	if c.winTotal.Load() != 3 || c.winErrors.Load() != 1 || c.winStart.Load() != start {
		t.Errorf("window reset early: start=%d total=%d err=%d",
			c.winStart.Load(), c.winTotal.Load(), c.winErrors.Load())
	}

	// Past the window: bucket restarts at the new time with zeroed counts.
	c.rollWindow(start + reachWindowSeconds)
	if c.winTotal.Load() != 0 || c.winErrors.Load() != 0 || c.winStart.Load() != start+reachWindowSeconds {
		t.Errorf("window did not reset: start=%d total=%d err=%d",
			c.winStart.Load(), c.winTotal.Load(), c.winErrors.Load())
	}
}
