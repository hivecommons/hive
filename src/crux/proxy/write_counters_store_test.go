package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
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

func TestWriteCountersReadFailureKeepsFileAndPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "counters.json")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var c writeCounters
	c.now = func() time.Time { return now }
	c.record("ADVISORY", "")
	if err := c.flush(path, now); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	// A symlink loop produces a deterministic read error even when CI runs as root.
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		t.Fatal(err)
	}
	c.record("ADVISORY", DenyReasonMode)
	if err := c.flush(path, now); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("flush error = %v, want ELOOP", err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unreadable path was replaced: %v, %v", info, err)
	}
	saved, err := os.ReadFile(path + ".saved")
	if err != nil || string(saved) != string(original) {
		t.Fatalf("durable data changed: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if err := c.flush(path, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDurableWriteCounts(path)
	if err != nil {
		t.Fatal(err)
	}
	if wc := got.Totals["ADVISORY"]; wc.Attempted != 2 || wc.Blocked != 1 {
		t.Errorf("retry totals = %+v", wc)
	}
	if !got.RecordedSince.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("recorded_since changed: %s", got.RecordedSince)
	}
}

func TestLoadDurableWriteCountsMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if d, err := LoadDurableWriteCounts(filepath.Join(dir, "none.json")); err != nil || !d.empty() {
		t.Errorf("missing file = %+v, %v", d, err)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := LoadDurableWriteCounts(bad); !errors.Is(err, errInvalidWriteCounts) {
		t.Errorf("expected parse error, got %v", err)
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

// TestMigrateWriteCountersAndRecordedSince pins the move of the counter file
// under /data (hivecommons/hive#12368): a file left at the legacy
// container-local path is folded into the durable one and removed, and the
// durable file records when it started counting (floored to its earliest
// week) so a reader can say "not recorded before <date>".
func TestMigrateWriteCountersAndRecordedSince(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "run", "write-counters.json")
	durable := filepath.Join(dir, "data", "write-counters.json")
	wk := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) // a Monday
	now := wk.Add(3 * 24 * time.Hour)

	var c writeCounters
	c.now = func() time.Time { return wk.Add(time.Hour) }
	c.record("ISSUES_ONLY", "")
	c.record("ISSUES_ONLY", DenyReasonMode)
	if err := c.flush(legacy, now); err != nil {
		t.Fatal(err)
	}
	if err := MigrateWriteCounters(legacy, durable, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy file should be removed after migration, stat err = %v", err)
	}
	got, err := LoadDurableWriteCounts(durable)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals["ISSUES_ONLY"].Attempted != 2 || got.Totals["ISSUES_ONLY"].Blocked != 1 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if got.EarliestWeek() != "2026-10-12" {
		t.Errorf("earliest week = %q", got.EarliestWeek())
	}
	if !got.RecordedSince.Equal(wk) {
		t.Errorf("recorded_since = %s, want floored to earliest week %s", got.RecordedSince, wk)
	}
	// Idempotent: nothing left to migrate, durable file untouched.
	if err := MigrateWriteCounters(legacy, durable, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadDurableWriteCounts(durable)
	if !again.RecordedSince.Equal(wk) || again.Totals["ISSUES_ONLY"].Attempted != 2 {
		t.Errorf("second migrate changed the file: %+v", again)
	}
	if err := MigrateWriteCounters(durable, durable, now); err != nil {
		t.Errorf("same-path migrate must be a no-op, got %v", err)
	}
}
