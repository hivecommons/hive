package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteCounterWeekStart(t *testing.T) {
	cases := map[string]string{
		"2026-10-12T00:00:00Z": "2026-10-12", // Monday
		"2026-10-15T20:00:00Z": "2026-10-12",
		"2026-10-18T23:59:59Z": "2026-10-12", // Sunday
		"2026-10-19T00:00:00Z": "2026-10-19",
		"2026-01-01T10:00:00Z": "2025-12-29", // crosses the year
	}
	for in, want := range cases {
		ts, _ := time.Parse(time.RFC3339, in)
		if got := WriteCounterWeekStart(ts); got != want {
			t.Errorf("WriteCounterWeekStart(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestWriteCountersFlushMergesAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "write-counters.json")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	var first writeCounters
	first.now = func() time.Time { return now }
	first.record("ISSUES_ONLY", "")
	first.record("ISSUES_ONLY", DenyReasonMode)
	if err := first.flush(path, now); err != nil {
		t.Fatal(err)
	}

	var second writeCounters
	second.now = func() time.Time { return now.AddDate(0, 0, 7) }
	second.record("ISSUES_ONLY", DenyReasonMode)
	second.record("ADVISORY", DenyReasonRepoPaused)
	if err := second.flush(path, now); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDurableWriteCounts(path)
	if err != nil {
		t.Fatal(err)
	}
	io := got.Totals["ISSUES_ONLY"]
	if io.Attempted != 3 || io.Blocked != 2 || io.BlockedByReason[DenyReasonMode] != 2 {
		t.Errorf("totals ISSUES_ONLY = %+v", io)
	}
	w1 := got.Weeks["2026-10-12"]
	if w1.Attempted != 2 || w1.Blocked != 1 || w1.Tiers["ISSUES_ONLY"].Blocked != 1 {
		t.Errorf("week 1 = %+v", w1)
	}
	w2 := got.Weeks["2026-10-19"]
	if w2.Attempted != 2 || w2.Blocked != 2 || w2.BlockedByReason[DenyReasonRepoPaused] != 1 || w2.Tiers["ADVISORY"].Attempted != 1 {
		t.Errorf("week 2 = %+v", w2)
	}

	// Nothing pending: flush must not rewrite or fail.
	if err := second.flush(path, now); err != nil {
		t.Fatal(err)
	}
}

func TestWriteCountersFlushFailureKeepsPending(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var c writeCounters
	c.record("ADVISORY", DenyReasonMode)
	if err := c.flush(filepath.Join(blocker, "x.json"), time.Now()); err == nil {
		t.Fatal("expected flush error")
	}
	good := filepath.Join(dir, "ok.json")
	if err := c.flush(good, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadDurableWriteCounts(good)
	if got.Totals["ADVISORY"].Attempted != 1 {
		t.Errorf("pending lost after failed flush: %+v", got.Totals)
	}
}

func TestLoadDurableWriteCountsMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if d, err := LoadDurableWriteCounts(filepath.Join(dir, "none.json")); err != nil || !d.empty() {
		t.Errorf("missing file = %+v, %v", d, err)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := LoadDurableWriteCounts(bad); err == nil {
		t.Error("expected parse error")
	}
	var c writeCounters
	c.record("ADVISORY", "")
	if err := c.flush(bad, time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, _ := LoadDurableWriteCounts(bad); d.Totals["ADVISORY"].Attempted != 1 {
		t.Errorf("corrupt file not replaced: %+v", d)
	}
}
