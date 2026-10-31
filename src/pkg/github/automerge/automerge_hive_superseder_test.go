package automerge

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestHiveSupersederHardStop: the owner's exception list, one branch each —
// and the ordinary cases (a reviewer finding, a pending check, a first
// conflict, an acknowledged sentinel) that are NOT stops.
func TestHiveSupersederHardStop(t *testing.T) {
	cap := ContributorLaneVerdict{FixCycleCapReached: true}
	cases := []struct {
		name    string
		e       stagelabels.Evidence
		verdict ContributorLaneVerdict
		ackBy   string
		want    string
	}{
		{"sentinel unacknowledged", stagelabels.Evidence{Sentinel: true}, ContributorLaneVerdict{}, "", stagelabels.HiveSupersederHardStopSentinel},
		{"sentinel acknowledged", stagelabels.Evidence{Sentinel: true}, ContributorLaneVerdict{}, "clubanderson", ""},
		{"hold label", stagelabels.Evidence{Holds: []string{"hold"}}, ContributorLaneVerdict{}, "", stagelabels.HiveSupersederHardStopHold},
		{"hold provenance", stagelabels.Evidence{Hold: &stagelabels.HoldProvenance{By: "hive"}}, ContributorLaneVerdict{}, "", stagelabels.HiveSupersederHardStopHold},
		{"fix exhausted, CI red", stagelabels.Evidence{Checks: stagelabels.CheckSummary{Known: true, Red: []string{"unit"}}}, cap, "", stagelabels.HiveSupersederHardStopCIRed},
		{"fix exhausted, conflict", stagelabels.Evidence{Conflict: true}, cap, "", stagelabels.HiveSupersederHardStopConflict},
		{"fix exhausted", stagelabels.Evidence{Verdict: stagelabels.VerdictRequiresHuman}, cap, "", stagelabels.HiveSupersederHardStopFixExhausted},
		{"rebase failed twice", stagelabels.Evidence{Conflict: true, LaneReason: stagelabels.LaneReasonRebaseFailedTwice}, ContributorLaneVerdict{}, "", stagelabels.HiveSupersederHardStopConflict},
		{"adoption-stalled rebase failed twice", stagelabels.Evidence{Conflict: true, CauseReason: stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonRebaseFailedTwice}, ContributorLaneVerdict{}, "", stagelabels.HiveSupersederHardStopConflict},
		{"first conflict is the fixer's", stagelabels.Evidence{Conflict: true}, ContributorLaneVerdict{}, "", ""},
		{"reviewer requires_human is the fixer's", stagelabels.Evidence{Verdict: stagelabels.VerdictRequiresHuman, DecisionFinding: "security"}, ContributorLaneVerdict{HiveAuthored: true}, "", ""},
		{"red CI before fixes is the fixer's", stagelabels.Evidence{Checks: stagelabels.CheckSummary{Known: true, Red: []string{"unit"}}}, ContributorLaneVerdict{}, "", ""},
		{"sentinel beats hold", stagelabels.Evidence{Sentinel: true, Holds: []string{"hold"}}, cap, "", stagelabels.HiveSupersederHardStopSentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := hiveSupersederHardStop(tc.e, tc.verdict, tc.ackBy)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("hiveSupersederHardStop = %q,%v want %q", got, ok, tc.want)
			}
		})
	}
}

// TestEngineHiveSupersedingPR: hive-authored + `Supersedes #N` under the
// (default-on) knob; off when the knob is off, a contributor wrote it, or
// the body does not declare a supersession.
func TestEngineHiveSupersedingPR(t *testing.T) {
	body := "Supersedes #15646\n\nRe-authored from the contributor's branch."
	off := false
	cases := []struct {
		name string
		c    *Engine
		obs  stageObservation
		want bool
	}{
		{"hive author kind", &Engine{}, stageObservation{AuthorKind: authorkind.Hive, Body: body}, true},
		{"self lane", &Engine{}, stageObservation{Lane: stageLaneSelf, Body: body}, true},
		{"nil engine defaults on", nil, stageObservation{AuthorKind: authorkind.Hive, Body: body}, true},
		{"contributor", &Engine{}, stageObservation{AuthorKind: authorkind.Clanker, Body: body}, false},
		{"no marker", &Engine{}, stageObservation{AuthorKind: authorkind.Hive, Body: "Fixes #1"}, false},
		{"knob off", &Engine{hiveSupersedingAutoMerge: func() bool { return off }}, stageObservation{AuthorKind: authorkind.Hive, Body: body}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.hiveSupersedingPR(tc.obs); got != tc.want {
				t.Fatalf("hiveSupersedingPR = %v want %v", got, tc.want)
			}
		})
	}
}

func TestEngineSentinelAcknowledgedForUsesHook(t *testing.T) {
	c := &Engine{sentinelAcknowledgedBy: func(repo string, number int, head string) string {
		if repo == "hivecommons/hive" && number == 15650 && head == "abc" {
			return " clubanderson "
		}
		return ""
	}}
	if got := c.sentinelAcknowledgedFor("hivecommons/hive", 15650, "abc"); got != "clubanderson" {
		t.Fatalf("ack = %q", got)
	}
	if got := c.sentinelAcknowledgedFor("hivecommons/hive", 15650, "def"); got != "" {
		t.Fatalf("ack = %q, want none", got)
	}
	if got := (&Engine{}).sentinelAcknowledgedFor("hivecommons/hive", 15650, ""); got != "" {
		t.Fatalf("empty head ack = %q", got)
	}
}
