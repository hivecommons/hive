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

	requests, err := NewObservationLedger(path).Reconcile(nil, t0.Add(7*24*time.Hour))
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

	closedAt := t0.Add(7*24*time.Hour + time.Hour)
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
	series := []ObservationSample{
		{At: t0.Add(6 * 24 * time.Hour), Active: false},
		{At: t0, Active: true},
		{At: t0.Add(7 * 24 * time.Hour), Active: false},
		{At: t0.Add(8 * 24 * time.Hour), Active: false},
	}
	since, recovered := RecoverySince(series, t0.Add(8*24*time.Hour))
	if !since.Equal(t0.Add(7*24*time.Hour)) || !recovered {
		t.Fatalf("recovery series = since %v recovered %v", since, recovered)
	}

	series = append(series, ObservationSample{At: t0.Add(9 * 24 * time.Hour), Active: true})
	since, recovered = RecoverySince(series, t0.Add(9*24*time.Hour))
	if !since.IsZero() || recovered {
		t.Fatalf("regressing series = since %v recovered %v", since, recovered)
	}
}
