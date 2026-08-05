package hub

import (
	"testing"
	"time"
)

// Tests for the pure helper functions in drift.go that have no dedicated
// coverage: parseRFC3339, parseRFC3339OrTime, pluralize, driftEligibleForNorm,
// roundedDuration, and driftHumanDuration edge-cases not reached by the
// existing table-driven test.

func TestParseRFC3339(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		desc string
	}{
		{"", false, "empty string"},
		{"   ", false, "whitespace only"},
		{"not-a-timestamp", false, "garbage input"},
		{"2025-07-01", false, "date without time"},
		{"2025-07-01T12:00:00Z", true, "valid UTC timestamp"},
		{"2025-07-01T12:00:00+05:30", true, "valid offset timestamp"},
		{"  2025-07-01T12:00:00Z  ", true, "whitespace-padded valid"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got, ok := parseRFC3339(c.in)
			if ok != c.ok {
				t.Fatalf("parseRFC3339(%q) ok = %v, want %v", c.in, ok, c.ok)
			}
			if c.ok && got.IsZero() {
				t.Fatalf("parseRFC3339(%q) returned zero time with ok=true", c.in)
			}
		})
	}
}

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

func TestPluralize(t *testing.T) {
	cases := []struct {
		n    int
		one  string
		many string
		want string
	}{
		{0, "min", "min", "0 min"},
		{1, "min", "min", "1 min"},
		{2, "min", "min", "2 min"},
		{1, "hour", "hours", "1 hour"},
		{5, "hour", "hours", "5 hours"},
		{1, "day", "days", "1 day"},
		{30, "day", "days", "30 days"},
	}
	for _, c := range cases {
		if got := pluralize(c.n, c.one, c.many); got != c.want {
			t.Errorf("pluralize(%d, %q, %q) = %q, want %q", c.n, c.one, c.many, got, c.want)
		}
	}
}

func TestDriftEligibleForNorm(t *testing.T) {
	t.Run("online claimed hive is eligible", func(t *testing.T) {
		h := MyHiveEntry{RegistryEntry: RegistryEntry{Online: true, Org: "acme"}}
		if !driftEligibleForNorm(h) {
			t.Error("online claimed hive should be eligible")
		}
	})

	t.Run("offline hive is not eligible", func(t *testing.T) {
		h := MyHiveEntry{RegistryEntry: RegistryEntry{Online: false, Org: "acme"}}
		if driftEligibleForNorm(h) {
			t.Error("offline hive should not be eligible")
		}
	})

	t.Run("placeholder by provStatus is not eligible", func(t *testing.T) {
		h := MyHiveEntry{RegistryEntry: RegistryEntry{Online: true}, ProvStatus: statusAvailable}
		if driftEligibleForNorm(h) {
			t.Error("placeholder hive should not be eligible")
		}
	})

	t.Run("placeholder by org prefix is not eligible", func(t *testing.T) {
		h := MyHiveEntry{RegistryEntry: RegistryEntry{Online: true, Org: placeholderOrgPrefix + "pool-1"}}
		if driftEligibleForNorm(h) {
			t.Error("placeholder hive (org prefix) should not be eligible")
		}
	})
}

func TestRoundedDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "1m0s"},     // rounds to nearest minute → 1m
		{90 * time.Second, "2m0s"},     // rounds to 2 min
		{5 * time.Minute, "5m0s"},      // exact
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
