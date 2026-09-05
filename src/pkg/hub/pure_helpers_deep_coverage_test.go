package hub

import (
	"math"
	"testing"
	"time"
)

// ============================================================
// imageTagIsMutable (self_upgrade.go)
// ============================================================

func TestImageTagIsMutableEdgeCases(t *testing.T) {
	cases := []struct {
		image string
		want  bool
	}{
		// Mutable: branch-channel tags ending in "-latest"
		{"ghcr.io/hivecommons/hive:v2-latest", true},
		{"ghcr.io/hivecommons/hive:v3-latest", true},
		{"ghcr.io/hivecommons/hive:mk-latest", true},
		{"ghcr.io/hivecommons/hive-hub:feat-vanity-url-latest", true},

		// Immutable: SHA-pinned tags
		{"ghcr.io/hivecommons/hive:f45d2e9", false},
		{"ghcr.io/hivecommons/hive:438a9f2b1c3a4e5f6789012345678901234567ab", false},

		// Immutable: digest-pinned (contains @)
		{"ghcr.io/hivecommons/hive@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", false},

		// No tag at all (implicitly :latest, but function returns false)
		{"ghcr.io/hivecommons/hive", false},

		// Empty string
		{"", false},

		// Registry port must not confuse the parser (colon before slash)
		{"registry.local:5000/hivecommons/hive", false},
		{"registry.local:5000/hivecommons/hive:v2-latest", true},

		// Tag that is literally "latest" (not "-latest" suffix)
		{"ghcr.io/hivecommons/hive:latest", false},

		// Tag ending in "-latest" but with digest present
		{"ghcr.io/hivecommons/hive:v2-latest@sha256:abc123", false},
	}
	for _, tc := range cases {
		got := imageTagIsMutable(tc.image)
		if got != tc.want {
			t.Errorf("imageTagIsMutable(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
}

// ============================================================
// recentlyStarted (alerts.go)
// ============================================================

func TestRecentlyStarted(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		startedAt string
		want      bool
	}{
		{"5 minutes ago", now.Add(-5 * time.Minute).Format(time.RFC3339), true},
		{"14 minutes ago", now.Add(-14 * time.Minute).Format(time.RFC3339), true},
		{"exactly 15 minutes ago (at ceiling)", now.Add(-15 * time.Minute).Format(time.RFC3339), false},
		{"1 hour ago", now.Add(-1 * time.Hour).Format(time.RFC3339), false},
		{"unparseable", "not-a-timestamp", false},
		{"empty", "", false},
		{"future time", now.Add(10 * time.Minute).Format(time.RFC3339), false},
	}
	for _, tc := range cases {
		got := recentlyStarted(tc.startedAt, now)
		if got != tc.want {
			t.Errorf("%s: recentlyStarted(%q) = %v, want %v", tc.name, tc.startedAt, got, tc.want)
		}
	}
}

// ============================================================
// heartbeatAge (alerts.go)
// ============================================================

func TestHeartbeatAge(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		input    string
		wantOK   bool
		wantAge  time.Duration
		ageDelta time.Duration // allowed tolerance
	}{
		{"10 minutes ago", now.Add(-10 * time.Minute).Format(time.RFC3339), true, 10 * time.Minute, time.Second},
		{"1 hour ago", now.Add(-1 * time.Hour).Format(time.RFC3339), true, 1 * time.Hour, time.Second},
		{"future", now.Add(5 * time.Minute).Format(time.RFC3339), false, 0, 0},
		{"empty", "", false, 0, 0},
		{"garbage", "not-a-time", false, 0, 0},
	}
	for _, tc := range cases {
		age, ok := heartbeatAge(tc.input, now)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if ok {
			diff := age - tc.wantAge
			if diff < 0 {
				diff = -diff
			}
			if diff > tc.ageDelta {
				t.Errorf("%s: age = %v, want ~%v (delta %v)", tc.name, age, tc.wantAge, diff)
			}
		}
	}
}

// ============================================================
// registeredLongerThan (alerts.go)
// ============================================================

func TestRegisteredLongerThan(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		registeredAt string
		d            time.Duration
		want         bool
	}{
		{"registered 2 hours ago, threshold 1 hour", now.Add(-2 * time.Hour).Format(time.RFC3339), 1 * time.Hour, true},
		{"registered 30 min ago, threshold 1 hour", now.Add(-30 * time.Minute).Format(time.RFC3339), 1 * time.Hour, false},
		{"exactly at threshold", now.Add(-1 * time.Hour).Format(time.RFC3339), 1 * time.Hour, false},
		{"unparseable", "bogus", 1 * time.Hour, false},
		{"empty", "", 1 * time.Hour, false},
	}
	for _, tc := range cases {
		got := registeredLongerThan(tc.registeredAt, tc.d, now)
		if got != tc.want {
			t.Errorf("%s: registeredLongerThan(%q, %v) = %v, want %v", tc.name, tc.registeredAt, tc.d, got, tc.want)
		}
	}
}

// ============================================================
// roundedDuration (alerts.go)
// ============================================================

func TestRoundedDurationAlertPaths(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{90 * time.Second, "2m0s"},
		{2*time.Hour + 29*time.Second, "2h0m0s"},
		{15 * time.Minute, "15m0s"},
	}
	for _, tc := range cases {
		got := roundedDuration(tc.d)
		if got != tc.want {
			t.Errorf("roundedDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ============================================================
// itoa / itoa64 (alerts.go)
// ============================================================

func TestItoa(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{-1, "-1"},
		{42, "42"},
		{-999, "-999"},
		{2147483647, "2147483647"},
	}
	for _, tc := range cases {
		got := itoa(tc.n)
		if got != tc.want {
			t.Errorf("itoa(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestItoa64Extremes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{-1, "-1"},
		{math.MaxInt64, "9223372036854775807"},
		{math.MinInt64, "-9223372036854775808"},
	}
	for _, tc := range cases {
		got := itoa64(tc.n)
		if got != tc.want {
			t.Errorf("itoa64(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
