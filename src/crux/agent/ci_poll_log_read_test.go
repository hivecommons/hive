package agent

import (
	"strings"
	"testing"
	"time"
)

func TestCIPollCommandsLogReads(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    int
	}{
		{"gh run view 123 --log", 0},
		{"hive-gh run view 123 --job 456 --log-failed", 0},
		{"gh run view --log=true 123", 0},
		{"gh run view 123 --log-failed | tail -100", 0},
		{"gh run view 123", 1},
		{"gh run view 123 --json status,conclusion", 1},
		{"gh run view 123 --log=false", 1},
		{"gh run view 123 --log-failed=false", 1},
		{"gh pr checks 123", 1},
		{"gh run view 123; gh run view 456 --log", 1},
	} {
		t.Run(tt.command, func(t *testing.T) {
			if got := countCIPollCommands(tt.command); got != tt.want {
				t.Fatalf("count = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNudgeIfPollingCILogDiagnosis(t *testing.T) {
	m, a := ciPollManager(t, ciPollIdlePane, 11*time.Minute)
	tr := recordConditions(t, m)
	m.nudgeIfPollingCI(a, "", ciPollIdlePane)
	logs := strings.Repeat("gh run view 123 --log-failed\n", ciPollNudgeThreshold+1)
	m.nudgeIfPollingCI(a, logs, ciPollIdlePane)
	if a.ciPollNudgeSent || a.AwaitingCI || a.CIPollNudges != 0 {
		t.Fatalf("log diagnosis treated as polling: nudged=%v awaiting=%v count=%d", a.ciPollNudgeSent, a.AwaitingCI, a.CIPollNudges)
	}
	if ev := ciPollEvidence(tr); len(ev) != 0 {
		t.Fatalf("log diagnosis produced fleet evidence: %#v", ev)
	}
	// Log reads must not hide actual repeated status polling in the same kick.
	m.nudgeIfPollingCI(a, logs+strings.Repeat("gh pr checks 123\n", ciPollNudgeThreshold+1), ciPollIdlePane)
	if !a.ciPollNudgeSent || len(ciPollEvidence(tr)) != 1 {
		t.Fatal("status polling mixed with log reads must still nudge and report")
	}
}
