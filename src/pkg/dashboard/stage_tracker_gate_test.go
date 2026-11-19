package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// gateStateCase is one row of testdata/gate_states.json: a tracker item as
// the payload carries it and what its human gate must show.
type gateStateCase struct {
	Name string           `json:"name"`
	Item StageTrackerItem `json:"item"`
	Want struct {
		Reasons    []string `json:"reasons"`
		Actions    []string `json:"actions"`
		Answered   bool     `json:"answered"`
		AnsweredBy string   `json:"answered_by"`
		Wait       string   `json:"wait"`
		Inert      bool     `json:"inert"`
	} `json:"want"`
}

func loadGateStateCases(t *testing.T) []gateStateCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "gate_states.json"))
	if err != nil {
		t.Fatalf("reading the gate table: %v", err)
	}
	var table struct {
		Cases []gateStateCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("parsing the gate table: %v", err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("the gate table is empty")
	}
	return table.Cases
}

// TestStageTrackerHumanGateTable runs the shared gate table through the
// server rule (hivecommons/hive#18769): the reasons on the pill, the verbs
// offered and the blocker named when no decision is pending. The invariant
// the bug broke is asserted on every row: a verb is offered if and only if
// the action precondition accepts it, so no state offers a button that
// answers "nothing to act on".
func TestStageTrackerHumanGateTable(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range loadGateStateCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			g := stageTrackerHumanGateFor(tc.Item, now)
			if tc.Want.Inert {
				if g != nil {
					t.Fatalf("an inert row carries a gate: %+v", g)
				}
				return
			}
			if g == nil {
				t.Fatal("no gate computed")
			}
			if !slices.Equal(g.Reasons, tc.Want.Reasons) {
				t.Errorf("reasons = %v, want %v", g.Reasons, tc.Want.Reasons)
			}
			if !slices.Equal(g.Actions, tc.Want.Actions) {
				t.Errorf("actions = %v, want %v", g.Actions, tc.Want.Actions)
			}
			if g.Answered != tc.Want.Answered || g.AnsweredBy != tc.Want.AnsweredBy {
				t.Errorf("answered = %v by %q, want %v by %q", g.Answered, g.AnsweredBy, tc.Want.Answered, tc.Want.AnsweredBy)
			}
			wait := ""
			if g.WaitingOn != nil {
				wait = g.WaitingOn.Kind
				if strings.TrimSpace(g.WaitingOn.Text) == "" {
					t.Errorf("waiting_on %q has no text", wait)
				}
			}
			if wait != tc.Want.Wait {
				t.Errorf("waiting on %q, want %q (%+v)", wait, tc.Want.Wait, g.WaitingOn)
			}
			// Offered ⇔ accepted, over every verb any reason could offer.
			for _, key := range stageTrackerHumanGateOrder {
				for _, verb := range stageTrackerHumanGateVerbs[key] {
					offered := slices.Contains(g.Actions, verb)
					if offered && !stageTrackerHumanGateAccepts(tc.Item, verb) {
						t.Errorf("%s is offered but the action refuses it", verb)
					}
					if offered && !slices.Contains(g.Reasons, key) {
						continue
					}
					if !offered && slices.Contains(g.Reasons, key) && stageTrackerHumanGateAccepts(tc.Item, verb) &&
						(tc.Item.Kind == "pr" || !slices.Contains(stageTrackerHumanGatePROnly, verb)) {
						t.Errorf("%s would act on a %s gate but is not offered", verb, key)
					}
				}
			}
			if len(g.Reasons) > 0 && len(g.Actions) == 0 {
				t.Errorf("a gate with reasons %v offers no verb", g.Reasons)
			}
			if len(g.Reasons) > 0 && g.WaitingOn != nil {
				t.Errorf("a row with a pending decision also names a blocker: %+v", g.WaitingOn)
			}
		})
	}
}

// TestStageTrackerHumanGate18667 is the reported row end to end: a person
// approved the head the reviewer asked about, so the PR left 085 and waits
// on required checks, two of which have sat in progress for hours. The CI
// track must name them as stuck rather than running, and the gate must
// offer no decision verb and name them as the blocker.
func TestStageTrackerHumanGate18667(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	started := now.Add(-3 * time.Hour)
	recent := now.Add(-4 * time.Minute)
	const head = "ac459c3c3"
	pr := github.PullRequest{HeadSHA: head, CIActivity: &github.PRCIActivity{
		Head: head, Source: "github-check-runs", ObservedAt: &now, RequiredKnown: true, StatusesKnown: true, RequiredSource: "protection",
		Required:       []string{"build", "PR Type", "PR Issue"},
		RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "PR Type", AppKnown: true}, {Name: "PR Issue", AppKnown: true}},
		Checks: []github.PRActivityCheck{
			{Name: "build", State: "completed", Conclusion: "success", Since: &recent},
			{Name: "PR Type", State: "in_progress", Since: &started},
			{Name: "PR Issue", State: "in_progress", Since: &started},
		},
	}}
	ci := stageTrackerCIActivity(pr, now)
	if !slices.Equal(ci.ChecksStuck, []string{"PR Type", "PR Issue"}) || ci.StuckSince == nil || !ci.StuckSince.Equal(started) {
		t.Fatalf("stuck checks = %v since %v, want PR Type and PR Issue since %v", ci.ChecksStuck, ci.StuckSince, started)
	}
	if ci.Active || !strings.HasPrefix(ci.Caption, "CI stuck · ") || !strings.Contains(ci.Caption, "'PR Type'") || !strings.Contains(ci.Caption, "'PR Issue'") {
		t.Errorf("a head whose only pending checks are stuck must not read as running: active=%v caption=%q", ci.Active, ci.Caption)
	}
	if !strings.Contains(ci.Detail, "far longer than a check normally runs") {
		t.Errorf("detail does not explain the stuck checks: %q", ci.Detail)
	}

	it := StageTrackerItem{Repo: "hivecommons/hive", Number: 18667, Kind: "pr", Stage: trackerNumberReviewed, Head: head, BaseRef: "v6",
		HumanApprover: "clubanderson", Reason: "verdict-requires-human:human-approved@clubanderson", CIActivity: ci,
		ReviewActivity: &StageTrackerActivity{Head: head, State: "human_decision", Caption: "Review findings · needs owner sign-off"}}
	g := stageTrackerHumanGateFor(it, now)
	if g == nil || len(g.Reasons) != 0 || len(g.Actions) != 0 {
		t.Fatalf("an answered sign-off still shows a gate: %+v", g)
	}
	if !g.Answered || g.AnsweredBy != "clubanderson" {
		t.Errorf("answered = %v by %q, want the approver named", g.Answered, g.AnsweredBy)
	}
	if g.WaitingOn == nil || g.WaitingOn.Kind != gateWaitChecksStuck || !slices.Equal(g.WaitingOn.Checks, []string{"PR Type", "PR Issue"}) {
		t.Fatalf("blocker = %+v, want the two stuck required checks", g.WaitingOn)
	}
	if want := "required checks stuck: PR Type, PR Issue (in progress 3h, far longer than a normal run)"; g.WaitingOn.Text != want {
		t.Errorf("blocker text = %q, want %q", g.WaitingOn.Text, want)
	}
	for _, verb := range []string{decisionActionApprove, decisionActionDelegate} {
		if stageTrackerHumanGateAccepts(it, verb) {
			t.Errorf("%s is accepted on a PR no decision is pending on", verb)
		}
	}

	// The same head before the approval: parked at 085, the ask is live.
	parked := it
	parked.Stage, parked.HumanApprover, parked.Reason = trackerNumberNeedsDecision, "", ""
	g = stageTrackerHumanGateFor(parked, now)
	if g == nil || !slices.Equal(g.Reasons, []string{gateReasonReview}) || !slices.Equal(g.Actions, []string{decisionActionApprove, gateActionRequestChanges, decisionActionDelegate}) || g.Answered || g.WaitingOn != nil {
		t.Errorf("the parked head must carry the review gate and its verbs: %+v", g)
	}
}

// TestStageTrackerStuckChecksThreshold: a check within the threshold is
// running; past it, stuck; a head with a stuck check beside real motion
// stays active and counts the stuck one; the threshold is configurable.
func TestStageTrackerStuckChecksThreshold(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	activity := func(since map[string]time.Duration) *StageTrackerActivity {
		out := &StageTrackerActivity{State: "running", Caption: "CI running · 2 checks pending", Active: true}
		pending := map[string]time.Time{}
		for name, ago := range since {
			out.ChecksPending = append(out.ChecksPending, name)
			pending[name] = now.Add(-ago)
		}
		slices.Sort(out.ChecksPending)
		stageTrackerMarkStuckChecks(out, pending, now)
		return out
	}
	if a := activity(map[string]time.Duration{"build": trackerCheckStuckAfterDefault - time.Minute}); len(a.ChecksStuck) != 0 || !a.Active {
		t.Errorf("a check inside the threshold was called stuck: %+v", a)
	}
	a := activity(map[string]time.Duration{"build": time.Minute, "PR Type": trackerCheckStuckAfterDefault + time.Minute})
	if !slices.Equal(a.ChecksStuck, []string{"PR Type"}) || !a.Active || !strings.HasSuffix(a.Caption, " · 1 stuck") {
		t.Errorf("one stuck check beside a running one: %+v", a)
	}
	t.Setenv(trackerCheckStuckAfterEnv, "10m")
	if a := activity(map[string]time.Duration{"build": 11 * time.Minute}); !slices.Equal(a.ChecksStuck, []string{"build"}) || a.Active {
		t.Errorf("%s=10m did not lower the threshold: %+v", trackerCheckStuckAfterEnv, a)
	}
	t.Setenv(trackerCheckStuckAfterEnv, "nonsense")
	if got := trackerCheckStuckAfter(); got != trackerCheckStuckAfterDefault {
		t.Errorf("an unparsable threshold must fall back to the default, got %v", got)
	}
}

// TestStageTrackerResolveHumanGates: every row gets its gate and a nested
// PR link mirrors the listed PR's, so the issue row that draws the PR
// shows the same state.
func TestStageTrackerResolveHumanGates(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	items := []StageTrackerItem{
		{Repo: "acme/api", Number: 2, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "abc"},
		{Repo: "acme/api", Number: 1, Kind: "issue", Stage: trackerNumberCoveredByPR, Mirrored: true,
			LinkedPRs: []StageTrackerLinkedPR{{Number: 2, Head: "abc", Stage: trackerNumberNeedsDecision}, {Number: 3, Head: "old", State: "closed"}}},
	}
	stageTrackerResolveHumanGates(items, now)
	if items[0].HumanGate == nil || !slices.Equal(items[0].HumanGate.Reasons, []string{gateReasonDecision}) {
		t.Fatalf("PR gate = %+v", items[0].HumanGate)
	}
	if items[1].LinkedPRs[0].HumanGate != items[0].HumanGate {
		t.Errorf("the link does not mirror its PR's gate: %+v", items[1].LinkedPRs[0].HumanGate)
	}
	if items[1].LinkedPRs[1].HumanGate != nil {
		t.Errorf("a closed link carries a gate: %+v", items[1].LinkedPRs[1].HumanGate)
	}
	// The issue row decides its one parked PR: delegate is accepted there.
	if !stageTrackerHumanGateAccepts(items[1], decisionActionDelegate) {
		t.Error("an issue with one parked nested PR must accept Let hive decide")
	}
}
