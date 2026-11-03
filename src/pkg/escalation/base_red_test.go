package escalation

import (
	"path/filepath"
	"strings"
	"testing"
)

// baseRedObs is a hive PR red only on checks that are red on the base
// branch's tip as well (hivecommons/hive 2026-10-20: v5's Docker build was
// red, so every rebased PR carried the same red checks).
func baseRedObs(number int, sha string, checks ...string) Observation {
	o := redObs(number, sha, checks...)
	o.BaseRedChecks = append([]string(nil), checks...)
	return o
}

// A red base branch is evidence on its own: one PR whose every failing
// check is red on the base is shared without a fleet-wide consensus.
func TestMarkSharedFailures_BaseRedChecksAreSharedWithoutControl(t *testing.T) {
	obs := []Observation{baseRedObs(1, "a", "docker (hive)", "docker (hub)")}
	MarkSharedFailures(obs)
	if !obs[0].Shared || len(obs[0].SharedChecks) != 2 {
		t.Fatalf("base-red PR was not held: %+v", obs[0])
	}

	// A PR-local failure on top of the base's keeps its countable check.
	mixed := []Observation{redObs(2, "b", "docker (hive)", "changelog-fragment-guard")}
	mixed[0].BaseRedChecks = []string{"docker (hive)"}
	MarkSharedFailures(mixed)
	if mixed[0].Shared || strings.Join(mixed[0].SharedChecks, ",") != "docker (hive)" {
		t.Fatalf("PR-local failure suppressed by a partial base match: %+v", mixed[0])
	}
}

// #13258: three fix attempts re-pushed against a red v5 and escalated to
// humans. A head red only on the base's checks counts no attempt, never
// escalates, and gets one hold notice per head; counting resumes once the
// base recovers and the head is still red.
func TestSweep_BaseRedHoldsWithoutSpendingBudgetAndResumes(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	for i, sha := range []string{"a", "b", "c", "d"} {
		obs := []Observation{baseRedObs(7, sha, "docker (hive)")}
		MarkSharedFailures(obs)
		r := s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
		if r.NewlyEscala || r.Escalated || r.Attempts != 0 {
			t.Fatalf("pass %d: base-red head spent budget: %+v", i+1, r)
		}
		if !r.BaseRedHold {
			t.Fatalf("pass %d: first sight of head %q must ask for the hold notice: %+v", i+1, sha, r)
		}
		s.MarkBaseRedHeld("org/repo", 7, sha)
		again := s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
		if again.BaseRedHold || !again.Shared {
			t.Fatalf("pass %d: hold notice must post once per head: %+v", i+1, again)
		}
	}

	// Base recovered; the same check is now red on this PR alone.
	for i, sha := range []string{"e", "f", "g"} {
		obs := []Observation{redObs(7, sha, "docker (hive)")}
		MarkSharedFailures(obs)
		r := s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
		if r.BaseRedHold || r.Shared {
			t.Fatalf("recovered pass %d: still held: %+v", i+1, r)
		}
		if r.Attempts != i+1 {
			t.Fatalf("recovered pass %d: attempts=%d, want %d (budget untouched by the hold)", i+1, r.Attempts, i+1)
		}
		if (r.NewlyEscala) != (i+1 == DefaultThreshold) {
			t.Fatalf("recovered pass %d: escalation %+v", i+1, r)
		}
	}
}

func TestBaseRedHoldCommentBody_CarriesMarkerChecksAndIncident(t *testing.T) {
	body := BaseRedHoldCommentBody([]string{"docker (hive)", "overlayfs-exec-guard"}, 13208)
	for _, want := range []string{BaseRedHoldMarker, "docker (hive), overlayfs-exec-guard", "#13208", "not a failure in this PR"} {
		if !strings.Contains(body, want) {
			t.Fatalf("hold comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(BaseRedHoldCommentBody([]string{"x"}, 0), "incident #") {
		t.Fatal("hold comment names an incident when there is none")
	}
}

// Retroactive #13258: an escalation the hive raised (label confirmed in the
// ledger) over a head now red only on the base's checks is un-parked with
// the hold wording, and the hold notice is not posted a second time.
func TestSweep_HiveEscalationOverBaseRedIsUnparked(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	for _, sha := range []string{"a", "b", "c"} {
		s.Sweep([]Observation{redObs(7, sha, "docker (hive)")}, DefaultThreshold)
	}
	s.MarkEscalated("org/repo", 7)
	s.MarkLabelApplied("org/repo", 7)

	obs := []Observation{baseRedObs(7, "d", "docker (hive)")}
	obs[0].Labeled = true
	obs[0].SharedCIIncident = 13208
	MarkSharedFailures(obs)
	r := s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
	if !r.Unparked || r.Escalated || r.BaseRedHold {
		t.Fatalf("hive escalation over a red base must un-park: %+v", r)
	}
	for _, want := range []string{"docker (hive)", "red baseline", "#13208"} {
		if !strings.Contains(r.UnparkReason, want) {
			t.Fatalf("un-park reason missing %q: %q", want, r.UnparkReason)
		}
	}

	// Label removal landed: the same head is a plain hold with no second
	// notice (the un-park comment was it).
	obs[0].Labeled = false
	r = s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
	if r.Unparked || r.Escalated || r.BaseRedHold || r.Attempts != 0 {
		t.Fatalf("after un-park: %+v", r)
	}
}

// A needs-human label the ledger never confirmed (no LabelApplied — a human
// applied it, or the ledger was lost) is left alone on the first pass.
func TestSweep_UnconfirmedLabelOverBaseRedStaysParked(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	obs := []Observation{baseRedObs(7, "a", "docker (hive)")}
	obs[0].Labeled = true
	MarkSharedFailures(obs)
	r := s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
	if r.Unparked || !r.Escalated {
		t.Fatalf("unconfirmed label must stay parked: %+v", r)
	}
}
