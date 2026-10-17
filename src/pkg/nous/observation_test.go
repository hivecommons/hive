package nous

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservationLedgerRoundTripAndSevenDayRecovery(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "nous", "observations.json")
	ledger := NewObservationLedger(path)
	active := []ActiveObservation{{
		Fingerprint: "h1:queue-growth",
		IssueNumber: 42,
		URL:         "https://github.com/hivecommons/hive/issues/42",
		Before:      "p90=15",
		After:       "p90=30",
		Evidence:    "before 15, after 30",
	}}
	if got, err := ledger.Reconcile(active, t0); err != nil || len(got) != 0 {
		t.Fatalf("initial reconcile = %#v, err=%v", got, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger was not written: %v", err)
	}

	// A fresh instance proves that opened_at, last_seen_at and the issue number
	// are durable rather than process-local.
	reloaded := NewObservationLedger(path)
	if got, err := reloaded.Records(); err != nil || len(got) != 1 || got["h1:queue-growth"].IssueNumber != 42 {
		t.Fatalf("round-trip records = %#v, err=%v", got, err)
	}

	for day := 1; day <= 6; day++ {
		if got, err := reloaded.Reconcile(nil, t0.Add(time.Duration(day)*24*time.Hour)); err != nil || len(got) != 0 {
			t.Fatalf("recovery day %d = %#v, err=%v", day, got, err)
		}
	}
	records, err := reloaded.Records()
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(24 * time.Hour); !records["h1:queue-growth"].RecoveredSince.Equal(want) {
		t.Fatalf("recovered_since = %v, want %v", records["h1:queue-growth"].RecoveredSince, want)
	}

	// Day 7 is only six days into the inactive run that began on day 1.
	if got, err := NewObservationLedger(path).Reconcile(nil, t0.Add(7*24*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("recovery day 7 = %#v, err=%v", got, err)
	}
	requests, err := NewObservationLedger(path).Reconcile(nil, t0.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].IssueNumber != 42 || requests[0].Fingerprint != "h1:queue-growth" {
		t.Fatalf("seven-day recovery requests = %#v", requests)
	}
	for _, want := range []string{"7 consecutive days", "p90=15", "p90=30", "before", "after"} {
		if !strings.Contains(requests[0].Comment, want) {
			t.Fatalf("recovery comment missing %q:\n%s", want, requests[0].Comment)
		}
	}

	closedAt := t0.Add(8*24*time.Hour + time.Hour)
	if err := NewObservationLedger(path).MarkClosed("h1:queue-growth", closedAt); err != nil {
		t.Fatal(err)
	}
	closed, err := NewObservationLedger(path).Records()
	if err != nil {
		t.Fatal(err)
	}
	if !closed["h1:queue-growth"].ClosedAt.Equal(closedAt) {
		t.Fatalf("closed_at = %v, want %v", closed["h1:queue-growth"].ClosedAt, closedAt)
	}
	refireAt := closedAt.Add(time.Hour)
	if _, err := NewObservationLedger(path).Reconcile([]ActiveObservation{{
		Fingerprint: "h1:queue-growth", IssueNumber: 99, Before: "p90=30", After: "p90=12",
	}}, refireAt); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewObservationLedger(path).Records()
	if err != nil {
		t.Fatal(err)
	}
	if fresh["h1:queue-growth"].IssueNumber != 99 || !fresh["h1:queue-growth"].ClosedAt.IsZero() || !fresh["h1:queue-growth"].OpenedAt.Equal(refireAt) {
		t.Fatalf("refired lifecycle = %#v", fresh["h1:queue-growth"])
	}
}

func TestObservationLedgerRecoveryResetsWhenFindingRegresses(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ledger := NewObservationLedger(filepath.Join(t.TempDir(), "observations.json"))
	active := []ActiveObservation{{Fingerprint: "h1:hold-growth", IssueNumber: 7}}
	if _, err := ledger.Reconcile(active, t0); err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 6; day++ {
		if _, err := ledger.Reconcile(nil, t0.Add(time.Duration(day)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// The finding returns before seven inactive days have elapsed. The next
	// inactive run must start at this active observation's successor.
	if _, err := ledger.Reconcile(active, t0.Add(6*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for day := 7; day <= 13; day++ {
		got, err := ledger.Reconcile(nil, t0.Add(time.Duration(day)*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("recovery before seven days on day %d = %#v", day, got)
		}
	}
	got, err := ledger.Reconcile(nil, t0.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].RecoveredSince.Equal(t0.Add(7*24*time.Hour)) {
		t.Fatalf("recovery after reset = %#v", got)
	}
}

func TestRecoverySinceSyntheticSeries(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// Unsorted on purpose: the inactive run starts at the first inactive
	// sample after the last active one, regardless of slice order.
	series := []ObservationSample{
		{At: t0.Add(1 * 24 * time.Hour), Active: false},
		{At: t0, Active: true},
		{At: t0.Add(7 * 24 * time.Hour), Active: false},
		{At: t0.Add(8 * 24 * time.Hour), Active: false},
	}
	since, recovered := RecoverySince(series, t0.Add(7*24*time.Hour))
	if !since.Equal(t0.Add(24*time.Hour)) || recovered {
		t.Fatalf("six-day series = since %v recovered %v", since, recovered)
	}
	since, recovered = RecoverySince(series, t0.Add(8*24*time.Hour))
	if !since.Equal(t0.Add(24*time.Hour)) || !recovered {
		t.Fatalf("recovery series = since %v recovered %v", since, recovered)
	}

	series = append(series, ObservationSample{At: t0.Add(9 * 24 * time.Hour), Active: true})
	since, recovered = RecoverySince(series, t0.Add(9*24*time.Hour))
	if !since.IsZero() || recovered {
		t.Fatalf("regressing series = since %v recovered %v", since, recovered)
	}
}

func TestObservationLedger_NilAndDefaults(t *testing.T) {
	var nilLedger *ObservationLedger
	if got := nilLedger.Path(); got != "" {
		t.Fatalf("nilLedger.Path() = %q, want empty", got)
	}
	if _, err := nilLedger.Records(); err == nil {
		t.Fatal("nilLedger.Records() expected error")
	}
	if _, err := nilLedger.Reconcile(nil, time.Now()); err == nil {
		t.Fatal("nilLedger.Reconcile() expected error")
	}
	if err := nilLedger.RecordIssue("fp", 1, "url", time.Now()); err == nil {
		t.Fatal("nilLedger.RecordIssue() expected error")
	}
	if err := nilLedger.MarkClosed("fp", time.Now()); err == nil {
		t.Fatal("nilLedger.MarkClosed() expected error")
	}

	defaultLedger := NewObservationLedger("   ")
	if got := defaultLedger.Path(); got != DefaultObservationLedgerPath {
		t.Fatalf("defaultLedger.Path() = %q, want %q", got, DefaultObservationLedgerPath)
	}
}

func TestObservationLedger_RecordIssue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nous", "observations.json")
	ledger := NewObservationLedger(path)

	// Validation errors
	if err := ledger.RecordIssue("bad fp with space", 10, "http://example.com", time.Time{}); err == nil {
		t.Fatal("expected error for invalid fingerprint")
	}
	if err := ledger.RecordIssue("good:fp", 0, "http://example.com", time.Time{}); err == nil {
		t.Fatal("expected error for zero issueNumber")
	}
	if err := ledger.RecordIssue("good:fp", -5, "http://example.com", time.Time{}); err == nil {
		t.Fatal("expected error for negative issueNumber")
	}

	// Normal record with zero time (defaults to time.Now())
	if err := ledger.RecordIssue("good:fp", 100, "http://example.com/issue/100", time.Time{}); err != nil {
		t.Fatalf("RecordIssue failed: %v", err)
	}
	records, err := ledger.Records()
	if err != nil {
		t.Fatalf("Records() failed: %v", err)
	}
	rec, ok := records["good:fp"]
	if !ok || rec.IssueNumber != 100 || rec.URL != "http://example.com/issue/100" {
		t.Fatalf("unexpected record: %#v", rec)
	}
	if rec.OpenedAt.IsZero() || rec.LastSeenAt.IsZero() {
		t.Fatalf("expected non-zero timestamps: %#v", rec)
	}

	// Update issue number for existing active record
	t1 := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	if err := ledger.RecordIssue("good:fp", 101, "http://example.com/issue/101", t1); err != nil {
		t.Fatalf("RecordIssue update failed: %v", err)
	}
	records, err = ledger.Records()
	if err != nil {
		t.Fatalf("Records() failed: %v", err)
	}
	rec = records["good:fp"]
	if rec.IssueNumber != 101 || !rec.LastSeenAt.Equal(t1) {
		t.Fatalf("record not updated: %#v", rec)
	}

	// Mark closed, then re-record issue -> resets closed state
	tClosed := t1.Add(time.Hour)
	if err := ledger.MarkClosed("good:fp", tClosed); err != nil {
		t.Fatalf("MarkClosed failed: %v", err)
	}
	records, err = ledger.Records()
	if err != nil {
		t.Fatal(err)
	}
	if !records["good:fp"].ClosedAt.Equal(tClosed) {
		t.Fatalf("record not closed: %#v", records["good:fp"])
	}

	tReopen := tClosed.Add(2 * time.Hour)
	if err := ledger.RecordIssue("good:fp", 200, "http://example.com/issue/200", tReopen); err != nil {
		t.Fatalf("RecordIssue re-open failed: %v", err)
	}
	records, err = ledger.Records()
	if err != nil {
		t.Fatal(err)
	}
	rec = records["good:fp"]
	if rec.IssueNumber != 200 || !rec.ClosedAt.IsZero() || !rec.OpenedAt.Equal(tReopen) {
		t.Fatalf("record not properly reopened: %#v", rec)
	}
}

func TestObservationLedger_MarkClosed_EdgeCases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nous", "observations.json")
	ledger := NewObservationLedger(path)

	// Invalid fingerprint
	if err := ledger.MarkClosed("invalid fp!", time.Time{}); err == nil {
		t.Fatal("expected error on invalid fingerprint")
	}

	// Unknown fingerprint is idempotent no-op
	if err := ledger.MarkClosed("unknown:fingerprint", time.Time{}); err != nil {
		t.Fatalf("MarkClosed for unknown fingerprint failed: %v", err)
	}
}

func TestObservationLedger_Reconcile_ValidationAndEdgeCases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nous", "observations.json")
	ledger := NewObservationLedger(path)

	// Reconcile with invalid fingerprint in active slice
	_, err := ledger.Reconcile([]ActiveObservation{{Fingerprint: "invalid fingerprint with spaces"}}, time.Time{})
	if err == nil {
		t.Fatal("expected error for invalid fingerprint in Reconcile")
	}

	// Reconcile with zero now defaults to time.Now()
	t0 := time.Now().UTC().Truncate(time.Second)
	reqs, err := ledger.Reconcile([]ActiveObservation{{Fingerprint: "h1:valid-fp", IssueNumber: 50, URL: "http://example.com/50"}}, time.Time{})
	if err != nil {
		t.Fatalf("Reconcile with zero time failed: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("unexpected recovery requests: %#v", reqs)
	}
	records, err := ledger.Records()
	if err != nil {
		t.Fatal(err)
	}
	rec := records["h1:valid-fp"]
	if rec.OpenedAt.Before(t0) {
		t.Fatalf("OpenedAt was not set to current time: %v vs %v", rec.OpenedAt, t0)
	}
}

func TestObservationLedger_LoadAndSaveErrors(t *testing.T) {
	dir := t.TempDir()

	// Corrupted JSON
	corruptPath := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptLedger := NewObservationLedger(corruptPath)
	if _, err := corruptLedger.Records(); err == nil || !strings.Contains(err.Error(), "decode observation ledger") {
		t.Fatalf("expected decode error, got %v", err)
	}
	if _, err := corruptLedger.Reconcile(nil, time.Now()); err == nil {
		t.Fatal("expected reconcile error on corrupted ledger")
	}
	if err := corruptLedger.RecordIssue("fp", 1, "", time.Now()); err == nil {
		t.Fatal("expected RecordIssue error on corrupted ledger")
	}
	if err := corruptLedger.MarkClosed("fp", time.Now()); err == nil {
		t.Fatal("expected MarkClosed error on corrupted ledger")
	}

	// Unsupported version
	unsupportedPath := filepath.Join(dir, "version.json")
	if err := os.WriteFile(unsupportedPath, []byte(`{"version": 999, "observations": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unsupportedLedger := NewObservationLedger(unsupportedPath)
	if _, err := unsupportedLedger.Records(); err == nil || !strings.Contains(err.Error(), "unsupported observation ledger version 999") {
		t.Fatalf("expected unsupported version error, got %v", err)
	}

	// Read error (path is a directory)
	dirPath := filepath.Join(dir, "dir-as-file")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	dirLedger := NewObservationLedger(dirPath)
	if _, err := dirLedger.Records(); err == nil {
		t.Fatal("expected read error when path is a directory")
	}

	// Save error: directory cannot be created (a file blocks directory path)
	fileBlockingDir := filepath.Join(dir, "blocked")
	if err := os.WriteFile(fileBlockingDir, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	unwritableLedger := NewObservationLedger(filepath.Join(fileBlockingDir, "child", "ledger.json"))
	if _, err := unwritableLedger.Reconcile([]ActiveObservation{{Fingerprint: "fp", IssueNumber: 1}}, time.Now()); err == nil {
		t.Fatal("expected save error when directory cannot be created")
	}
}

func TestObservationLedger_SanitizeAndFingerprint(t *testing.T) {
	// normalizeObservationFingerprint
	for _, invalid := range []string{
		"",
		"   ",
		"has space",
		"has\ttab",
		"has\nnewline",
		"has\"quote",
		"has'quote",
		"has<bracket",
		"has>bracket",
		strings.Repeat("a", 129),
	} {
		if _, err := normalizeObservationFingerprint(invalid); err == nil {
			t.Errorf("expected error for fingerprint %q", invalid)
		}
	}
	valid := "valid:fingerprint-123.abc"
	if got, err := normalizeObservationFingerprint(valid); err != nil || got != valid {
		t.Errorf("normalizeObservationFingerprint(%q) = %q, %v", valid, got, err)
	}

	// sanitizeObservationField
	withPipes := sanitizeObservationField("col1 | col2 | col3")
	if strings.Contains(withPipes, " | ") {
		t.Errorf("pipe was not escaped: %s", withPipes)
	}
	longStr := strings.Repeat("x", maxObservationFieldBytes+100)
	sanitizedLong := sanitizeObservationField(longStr)
	if len(sanitizedLong) > maxObservationFieldBytes {
		t.Errorf("field length %d > max %d", len(sanitizedLong), maxObservationFieldBytes)
	}

	// observationRecoveryComment with empty fields
	comment := observationRecoveryComment("fp", ObservationRecord{}, time.Now())
	if !strings.Contains(comment, "—") {
		t.Errorf("expected em dash for empty fields, got:\n%s", comment)
	}
}

func TestRecoverySince_EdgeCases(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// Empty samples
	since, recovered := RecoverySince(nil, t0)
	if !since.IsZero() || recovered {
		t.Fatalf("expected zero/false for nil samples, got since=%v recovered=%v", since, recovered)
	}

	// Sample with zero At or future At
	samples := []ObservationSample{
		{At: time.Time{}, Active: false},
		{At: t0.Add(24 * time.Hour), Active: false}, // future
		{At: t0.Add(-24 * time.Hour), Active: false},
	}
	// Zero now defaults to time.Now()
	since, _ = RecoverySince(samples, time.Time{})
	if since.IsZero() {
		t.Fatalf("expected non-zero since with zero now, got %v", since)
	}
}

func TestObservationLedger_AdditionalCoverage(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// 1. File with null observations
	nullObsPath := filepath.Join(dir, "null_obs.json")
	if err := os.WriteFile(nullObsPath, []byte(`{"version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	nullLedger := NewObservationLedger(nullObsPath)
	records, err := nullLedger.Records()
	if err != nil || len(records) != 0 {
		t.Fatalf("expected empty records, got %v, err=%v", records, err)
	}

	// 2. Existing record with zero OpenedAt is backfilled on Reconcile
	zeroOpenedPath := filepath.Join(dir, "zero_opened.json")
	if err := os.WriteFile(zeroOpenedPath, []byte(`{"version": 1, "observations": {"h1:test": {"issue_number": 10}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	zeroLedger := NewObservationLedger(zeroOpenedPath)
	if _, err := zeroLedger.Reconcile([]ActiveObservation{{Fingerprint: "h1:test", IssueNumber: 10}}, t0); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	records, err = zeroLedger.Records()
	if err != nil || !records["h1:test"].OpenedAt.Equal(t0) {
		t.Fatalf("expected OpenedAt backfilled to %v, got %#v", t0, records["h1:test"])
	}

	// 3. Multi-request sorting in Reconcile
	multiPath := filepath.Join(dir, "multi.json")
	multiLedger := NewObservationLedger(multiPath)
	active := []ActiveObservation{
		{Fingerprint: "z:fp", IssueNumber: 1},
		{Fingerprint: "a:fp", IssueNumber: 2},
	}
	if _, err := multiLedger.Reconcile(active, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := multiLedger.Reconcile(nil, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	reqs, err := multiLedger.Reconcile(nil, t0.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].Fingerprint != "a:fp" || reqs[1].Fingerprint != "z:fp" {
		t.Fatalf("expected sorted requests [a:fp, z:fp], got %#v", reqs)
	}

	// 4. SaveLocked failure when directory is read-only
	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0o700); err != nil {
		t.Fatal(err)
	}
	roPath := filepath.Join(roDir, "ledger.json")
	roLedger := NewObservationLedger(roPath)
	if err := roLedger.RecordIssue("h1:ro", 1, "", t0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(roDir, 0o700)
	if err := roLedger.RecordIssue("h1:ro", 2, "", t0); err == nil {
		t.Fatal("expected save error in read-only directory")
	}
}
