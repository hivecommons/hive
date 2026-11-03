package gfi

import (
	"testing"
	"time"
)

func TestOpsTrackingConstants(t *testing.T) {
	if SkipOpsTracking != "ops-tracking" || SkipTimeGated != "time-gated" {
		t.Fatalf("skip reasons changed: %q %q", SkipOpsTracking, SkipTimeGated)
	}
	if !HasOpsTrackingMarker(OpsTrackingMarker) {
		t.Fatalf("OpsTrackingMarker must match its own detector")
	}
	if !withdrawLabels["blocked"] || !withdrawLabels["hold"] || len(withdrawLabels) != 2 {
		t.Fatalf("withdrawLabels = %v", withdrawLabels)
	}
}

func TestHasOpsTrackingMarker(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"exact", OpsTrackingMarker, true},
		{"embedded", "intro\n" + OpsTrackingMarker + "\n- [ ] check", true},
		{"extra whitespace", "<!--   hive:ops-tracking   -->", true},
		{"no whitespace", "<!--hive:ops-tracking-->", true},
		{"different marker", "<!-- hive:other -->", false},
		{"not a comment", "hive:ops-tracking", false},
		{"recheck marker only", "<!-- hive:recheck-after 2026-10-21T21:20:00Z -->", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasOpsTrackingMarker(tc.text); got != tc.want {
				t.Errorf("HasOpsTrackingMarker(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestParseRecheckAfter(t *testing.T) {
	want := time.Date(2026, 10, 21, 21, 20, 0, 0, time.UTC)
	tests := []struct {
		name   string
		text   string
		want   time.Time
		wantOK bool
	}{
		{"empty", "", time.Time{}, false},
		{"missing marker", "no deadline here", time.Time{}, false},
		{"utc seconds", "<!-- hive:recheck-after 2026-10-21T21:20:00Z -->", want, true},
		{"utc no seconds", "<!-- hive:recheck-after 2026-10-21T21:20Z -->", want, true},
		{"positive offset normalised", "<!-- hive:recheck-after 2026-10-21T23:20:00+02:00 -->", want, true},
		{"negative offset no seconds normalised", "<!-- hive:recheck-after 2026-10-21T16:20-05:00 -->", want, true},
		{"extra whitespace", "<!--   hive:recheck-after   2026-10-21T21:20:00Z   -->", want, true},
		{"embedded in body", "text\n<!-- hive:recheck-after 2026-10-21T21:20:00Z -->\nmore", want, true},
		{"first of multiple wins", "<!-- hive:recheck-after 2026-10-21T21:20:00Z -->\n<!-- hive:recheck-after 2030-01-01T00:00:00Z -->", want, true},
		{"missing zone", "<!-- hive:recheck-after 2026-10-21T21:20:00 -->", time.Time{}, false},
		{"date only", "<!-- hive:recheck-after 2026-10-21 -->", time.Time{}, false},
		{"garbage timestamp", "<!-- hive:recheck-after soon -->", time.Time{}, false},
		{"impossible month", "<!-- hive:recheck-after 2026-13-21T21:20:00Z -->", time.Time{}, false},
		{"wrong marker name", "<!-- hive:recheck 2026-10-21T21:20:00Z -->", time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRecheckAfter(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tc.wantOK, got)
			}
			if !ok {
				if !got.IsZero() {
					t.Errorf("got %v, want zero time", got)
				}
				return
			}
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Errorf("got %v (%v), want %v UTC", got, got.Location(), tc.want)
			}
		})
	}
}

func TestOpsTrackingPhrases(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"24h after promotion", true},
		{"24 H AFTER PROMOTION", true},
		{"6 hours after the release", true},
		{"2 hrs after roll-out", true},
		{"3h after rolling out", true},
		{"4 hours after merge", true},
		{"10h after deployment", true},
		{"Post-Hoc Soak", true},
		{"soak evidence", true},
		{"Promote-Stable", true},
		{"stable promotion", true},
		{"hours after promotion", false},
		{"24h before promotion", false},
		{"unstable promotions", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := opsTrackingPhrases.MatchString(tc.text); got != tc.want {
			t.Errorf("opsTrackingPhrases(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
