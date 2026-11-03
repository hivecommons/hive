package stagelabels

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
)

func hiveSupersederEvidence() Evidence {
	e := readyEvidence()
	e.SelfAuthored = true
	e.AuthorKind = authorkind.Hive
	e.HiveSuperseder = true
	e.Verdict = VerdictRequiresHuman
	e.DecisionFinding = "the security review withheld approval"
	return e
}

// TestRouteHumanHiveSupersederRequiresHumanGoesToFixer: the owner's rule —
// a reviewer requires_human on the hive's superseding PR is the fixer's
// (070), never an 085 decision, and the caption says so.
func TestRouteHumanHiveSupersederRequiresHumanGoesToFixer(t *testing.T) {
	r := RouteHuman(hiveSupersederEvidence())
	if r.Stage != Fixing || r.LaneReason != LaneReasonHiveSupersederFix {
		t.Fatalf("route = %+v, want 070 fixer route", r)
	}
	if len(r.Reasons) != 1 || !strings.HasPrefix(r.Reasons[0], HiveSupersederAutoMergeCaption) {
		t.Fatalf("reasons = %v, want the auto-merge caption", r.Reasons)
	}
}

// TestRouteHumanHiveSupersederHardStopsPark: each hard-stop token parks at
// 085 with a lane reason naming the stop and a caption that names it.
func TestRouteHumanHiveSupersederHardStopsPark(t *testing.T) {
	for _, token := range []string{
		HiveSupersederHardStopSentinel,
		HiveSupersederHardStopHold,
		HiveSupersederHardStopFixExhausted,
		HiveSupersederHardStopCIRed,
		HiveSupersederHardStopConflict,
	} {
		t.Run(token, func(t *testing.T) {
			e := hiveSupersederEvidence()
			e.HiveSupersederHardStop = token
			r := RouteHuman(e)
			if r.Stage != NeedsDecision || r.LaneReason != LaneReasonHiveSupersederHardStopPrefix+token {
				t.Fatalf("route = %+v, want 085 with hard-stop lane reason", r)
			}
			prose := HiveSupersederHardStopProse(token)
			if prose == "" || !strings.Contains(r.Reasons[0], prose) {
				t.Fatalf("reasons = %v, want prose %q", r.Reasons, prose)
			}
			if !strings.Contains(strings.Join(r.Reasons, "\n"), "requires_human") || !strings.Contains(strings.Join(r.Reasons, "\n"), e.DecisionFinding) {
				t.Fatalf("reasons = %v, want the reviewer's finding named", r.Reasons)
			}
			if HiveSupersederHardStopReason(r.LaneReason) != token {
				t.Fatalf("HiveSupersederHardStopReason(%q) = %q", r.LaneReason, HiveSupersederHardStopReason(r.LaneReason))
			}
		})
	}
}

// TestRouteHumanNonSupersederRequiresHumanStillParks: without the
// superseder flag the same evidence still owes a decision.
func TestRouteHumanNonSupersederRequiresHumanStillParks(t *testing.T) {
	e := hiveSupersederEvidence()
	e.HiveSuperseder = false
	if r := RouteHuman(e); r.Stage != NeedsDecision {
		t.Fatalf("route = %+v, want 085", r)
	}
}

func TestHiveSupersederHardStopReasonRejectsOtherReasons(t *testing.T) {
	for _, reason := range []string{"", LaneReasonChangesRequested, LaneReasonHiveSupersederFix, "hive-superseder-hard-stop"} {
		if got := HiveSupersederHardStopReason(reason); got != "" {
			t.Fatalf("HiveSupersederHardStopReason(%q) = %q, want empty", reason, got)
		}
	}
	if HiveSupersederHardStopProse("unknown") != "" {
		t.Fatal("unknown token must have no prose")
	}
}
