package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// projectbluefin/finpilot#624 (the hive's own `pr=projectbluefin/actions#697`
// comment marker) and #390 (a maintainer's) still read "labelled blocked —
// the label records no reason". The row's enumeration reason names the
// blocker; the band signal now says it.
func TestBlockedRowSignalNamesMarker(t *testing.T) {
	now := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	cfg := config.DashboardIssueBandsConfig{}
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{reason: "blocked on projectbluefin/actions#697", want: "blocked on projectbluefin/actions#697 — lifted when it merges or closes"},
		{reason: "blocked until 2026-11-07T00:00:00Z", want: "blocked until 2026-11-07T00:00:00Z — re-checked then"},
		{reason: "blocked", want: blockedNoReasonSignal},
		{reason: "", want: blockedNoReasonSignal},
	} {
		row := github.RepoWorkIssue{Repo: "projectbluefin/finpilot", Number: 624, Labels: []string{"blocked", "triage/accepted"}, Bucket: "filtered", Reason: tc.reason}
		info := nonActionableIssueInfo(row, "finpilot", cfg, now, nil)
		got := blockedSignals11716(info)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("reason %q: blocked signals = %q, want [%q]", tc.reason, got, tc.want)
		}
	}
}

func TestBlockedReasonSignal(t *testing.T) {
	for reason, want := range map[string]string{
		"blocked on o/r#5":       "blocked on o/r#5 — lifted when it merges or closes",
		"blocked":                "",
		"hold label":             "",
		"blocked until ":         "",
		"blocked on o/r#notanum": "",
		"  blocked on o/r#12  ":  "blocked on o/r#12 — lifted when it merges or closes",
		"blocked until tomorrow": "blocked until tomorrow — re-checked then",
	} {
		if got := blockedReasonSignal(reason); got != want {
			t.Errorf("blockedReasonSignal(%q) = %q, want %q", reason, got, want)
		}
	}
}
