package dashboard

import (
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// TestStageTrackerSentinelJudgesCurrentHead (hivecommons/hive#19262): the
// tracker's sentinel state belongs to the PR's current head. A finding the
// sweep recorded for an earlier head stays in the /api/sentinel ring for
// audit, but once the sweep has removed its own label for a clean new head
// the row no longer reads "Needs human · sentinel" and names no findings; a
// finding on the new head, or a label a person put on, still flags it.
func TestStageTrackerSentinelJudgesCurrentHead(t *testing.T) {
	const (
		repo    = "acme/console"
		number  = 19192
		oldHead = "0bee4e4aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newHead = "f718e15bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	s := newTestServer()
	s.RecordSentinelFinding(github.SentinelSweepEvent{Repo: repo, Number: number, HeadSHA: oldHead, Blocked: true, Findings: []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "changes 1 sensitive path(s)", Paths: []string{"Makefile"}},
	}})

	// Old head, label on: flagged, with the old head's details.
	alert, _, _, details := s.stageTrackerSentinel(nil, repo, number, oldHead, []string{config.DefaultSentinelLabel})
	if !alert || len(details) != 1 {
		t.Fatalf("old head: alert=%v details=%+v", alert, details)
	}

	// Clean new head after the sweep removed its label: not flagged, and the
	// old head's finding is not shown as this head's.
	alert, n, reason, details := s.stageTrackerSentinel(nil, repo, number, newHead, []string{"lgtm"})
	if alert || n != 0 || reason != "" || details != nil {
		t.Fatalf("clean new head must not flag: alert=%v n=%d reason=%q details=%+v", alert, n, reason, details)
	}
	if slices.Contains(stageTrackerHumanGateReasons(StageTrackerItem{Kind: "pr", Head: newHead, SentinelAlert: alert}), gateReasonSentinel) {
		t.Fatal("clean new head must not carry the sentinel human gate")
	}
	if !slices.Contains(stageTrackerHumanGateReasons(StageTrackerItem{Kind: "pr", Head: oldHead, SentinelAlert: true}), gateReasonSentinel) {
		t.Fatal("positive control: a flagged head carries the sentinel human gate")
	}

	// A label still on the new head (a person's, which the sweep leaves
	// alone) keeps the gate.
	if alert, _, _, _ := s.stageTrackerSentinel(nil, repo, number, newHead, []string{config.DefaultSentinelLabel}); !alert {
		t.Fatal("a sentinel label on the current head must still flag")
	}

	// A finding recorded for the new head is the new head's.
	s.RecordSentinelFinding(github.SentinelSweepEvent{Repo: repo, Number: number, HeadSHA: newHead, Blocked: true, Findings: []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "changes 1 sensitive path(s)", Paths: []string{"Makefile"}},
	}})
	if _, _, _, details := s.stageTrackerSentinel(nil, repo, number, newHead, []string{config.DefaultSentinelLabel}); len(details) != 1 {
		t.Fatalf("new head finding details=%+v", details)
	}

	// History is kept: both heads stay in the ring.
	heads := map[string]bool{}
	for _, f := range s.sentinelFindings.list() {
		if f.Number == number {
			heads[f.HeadSHA] = true
		}
	}
	if !heads[oldHead] || !heads[newHead] {
		t.Fatalf("ring must keep every head's finding for audit: %v", heads)
	}
}
