package agent

import (
	"fmt"
	"testing"
	"time"
)

// ci_poll_sweep_test.go pins #14016: the scanner's merge sweep and CI-repair
// pass read `gh pr checks` once for each of many PRs. That is a sweep, not a
// wait on one PR, so only command lines run more than once count toward the
// nudge threshold and the fleet self-report.

func TestCountRepeatedCIPollCommands(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want int
	}{
		{
			name: "distinct PRs read once each",
			pane: "gh pr checks 1\ngh pr checks 2\ngh pr checks 3\ngh pr checks 4",
			want: 0,
		},
		{
			name: "same PR polled counts every read",
			pane: "gh pr checks 7\ngh pr checks 8\ngh pr checks 7\ngh pr checks 7",
			want: 3,
		},
		{
			name: "pane padding and box rule do not split one target",
			pane: "● gh run watch 42   ┃\n  gh run watch  42\n",
			want: 2,
		},
		{
			name: "quoted prohibition is not a read",
			pane: "no `gh pr checks` loops\ngh pr checks 5",
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countRepeatedCIPollCommands(tt.pane); got != tt.want {
				t.Errorf("countRepeatedCIPollCommands(%q) = %d, want %d", tt.pane, got, tt.want)
			}
		})
	}
}

func TestNudgeIfPollingCISweepIsNotPolling(t *testing.T) {
	sweep := ""
	for pr := range ciPollNudgeThreshold + 3 {
		sweep += fmt.Sprintf("gh pr checks %d --repo acme/widgets\n", 100+pr)
	}
	for _, tt := range []struct {
		name       string
		kickAge    time.Duration
		wantNudged bool
	}{
		{name: "sweep, kick young: no nudge", kickAge: 2 * time.Minute, wantNudged: false},
		{name: "sweep past wall clock: nudged, not reported", kickAge: 11 * time.Minute, wantNudged: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, a := ciPollManager(t, ciPollIdlePane, tt.kickAge)
			tr := recordConditions(t, m)

			m.nudgeIfPollingCI(a, "", ciPollIdlePane)
			m.nudgeIfPollingCI(a, sweep, ciPollIdlePane)

			if a.ciPollNudgeSent != tt.wantNudged {
				t.Errorf("ciPollNudgeSent = %v, want %v", a.ciPollNudgeSent, tt.wantNudged)
			}
			if ev := ciPollEvidence(tr); len(ev) != 0 {
				t.Errorf("ci_poll_nudge evidence = %d for a sweep, want 0: %#v", len(ev), ev)
			}
		})
	}
}
