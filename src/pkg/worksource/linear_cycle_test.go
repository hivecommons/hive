package worksource

import (
	"testing"
	"time"
)

// Linear's API returns cycle boundaries either as full RFC3339 timestamps or
// as bare dates; parseLinearCycleTime must accept both and, for a bare date
// used as an end boundary, extend it to the end of that day so the cycle's
// final day still counts as inside the cycle.
func TestParseLinearCycleTime(t *testing.T) {
	rfc := "2026-08-25T09:00:00Z"
	rfcWant, _ := time.Parse(time.RFC3339, rfc)
	dayWant, _ := time.Parse("2006-01-02", "2026-08-25")

	cases := []struct {
		name     string
		raw      string
		endOfDay bool
		want     time.Time
		ok       bool
	}{
		{"empty is no answer", "", false, time.Time{}, false},
		{"RFC3339 parses as-is", rfc, false, rfcWant, true},
		{"RFC3339 ignores endOfDay", rfc, true, rfcWant, true},
		{"bare date as start", "2026-08-25", false, dayWant, true},
		{"bare date as end extends to end of day", "2026-08-25", true, dayWant.Add(24 * time.Hour), true},
		{"garbage is no answer", "next tuesday", true, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseLinearCycleTime(tc.raw, tc.endOfDay)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Errorf("parseLinearCycleTime(%q, %v) = (%v, %v), want (%v, %v)",
					tc.raw, tc.endOfDay, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The current-cycle predicate must be closed-open: inside [start, end) is in,
// a missing cycle is out, a malformed start fails closed, and a malformed end
// degrades to "started means current" rather than dropping the issue.
func TestLinearIssueInCurrentCycle(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-08-29T12:00:00Z")
	cycle := func(start, end string) *struct {
		Name     string `json:"name"`
		StartsAt string `json:"startsAt"`
		EndsAt   string `json:"endsAt"`
	} {
		return &struct {
			Name     string `json:"name"`
			StartsAt string `json:"startsAt"`
			EndsAt   string `json:"endsAt"`
		}{StartsAt: start, EndsAt: end}
	}

	cases := []struct {
		name string
		node linearIssueNode
		want bool
	}{
		{"no cycle", linearIssueNode{}, false},
		{"inside cycle", linearIssueNode{Cycle: cycle("2026-08-25", "2026-09-05")}, true},
		{"before cycle starts", linearIssueNode{Cycle: cycle("2026-09-01", "2026-09-14")}, false},
		{"after cycle ends", linearIssueNode{Cycle: cycle("2026-08-01", "2026-08-14")}, false},
		{"last day of a date-only cycle still counts", linearIssueNode{Cycle: cycle("2026-08-25", "2026-08-29")}, true},
		{"unparseable start fails closed", linearIssueNode{Cycle: cycle("not-a-date", "2026-09-05")}, false},
		{"unparseable end means started is enough", linearIssueNode{Cycle: cycle("2026-08-25", "not-a-date")}, true},
		{"empty start fails closed", linearIssueNode{Cycle: cycle("", "2026-09-05")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := linearIssueInCurrentCycle(tc.node, now); got != tc.want {
				t.Errorf("linearIssueInCurrentCycle = %v, want %v", got, tc.want)
			}
		})
	}
}
