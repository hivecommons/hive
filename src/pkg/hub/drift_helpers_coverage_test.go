package hub

import (
	"testing"
	"time"
)

// Tests for pure helper functions in drift.go (and the shared roundedDuration
// helper in alerts.go) that have no dedicated coverage elsewhere:
// parseRFC3339OrTime, roundedDuration, and driftHumanDuration edge-cases not
// reached by the existing table-driven test in drift_test.go. Coverage for
// parseRFC3339, pluralize, and driftEligibleForNorm already lives in
// drift_pure_functions_test.go — duplicating those names here would fail to
// compile, so this file sticks to what is not already exercised.

func TestParseRFC3339OrTime(t *testing.T) {
	ref := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)

	t.Run("non-zero time wins", func(t *testing.T) {
		got, ok := parseRFC3339OrTime(ref, "")
		if !ok {
			t.Fatal("expected ok=true when a non-zero time is passed")
		}
		if !got.Equal(ref) {
			t.Errorf("got %v, want %v", got, ref)
		}
	})

	t.Run("zero time falls back to string", func(t *testing.T) {
		got, ok := parseRFC3339OrTime(time.Time{}, "2025-07-01T12:00:00Z")
		if !ok {
			t.Fatal("expected ok=true for valid RFC3339 fallback")
		}
		if !got.Equal(ref) {
			t.Errorf("got %v, want %v", got, ref)
		}
	})

	t.Run("zero time and empty string", func(t *testing.T) {
		_, ok := parseRFC3339OrTime(time.Time{}, "")
		if ok {
			t.Fatal("expected ok=false when both inputs are empty/zero")
		}
	})

	t.Run("zero time and bad string", func(t *testing.T) {
		_, ok := parseRFC3339OrTime(time.Time{}, "bogus")
		if ok {
			t.Fatal("expected ok=false for unparsable fallback string")
		}
	})
}

func TestRoundedDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "1m0s"}, // rounds to nearest minute → 1m
		{90 * time.Second, "2m0s"}, // rounds to 2 min
		{5 * time.Minute, "5m0s"},  // exact
		{2*time.Hour + 30*time.Minute, "2h30m0s"},
	}
	for _, c := range cases {
		if got := roundedDuration(c.d); got != c.want {
			t.Errorf("roundedDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestDriftHumanDurationSubMinute(t *testing.T) {
	// Sub-minute durations should round up to "1 min".
	if got := driftHumanDuration(0); got != "1 min" {
		t.Errorf("driftHumanDuration(0) = %q, want %q", got, "1 min")
	}
	if got := driftHumanDuration(30 * time.Second); got != "1 min" {
		t.Errorf("driftHumanDuration(30s) = %q, want %q", got, "1 min")
	}
}

func TestDriftHumanDurationMultiDay(t *testing.T) {
	if got := driftHumanDuration(48 * time.Hour); got != "2 days" {
		t.Errorf("driftHumanDuration(48h) = %q, want %q", got, "2 days")
	}
	if got := driftHumanDuration(24 * time.Hour); got != "1 day" {
		t.Errorf("driftHumanDuration(24h) = %q, want %q", got, "1 day")
	}
}
