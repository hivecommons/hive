package retro

import (
	"bytes"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/timeline"
)

func armBeads(arms []ArmSummary) map[string]int {
	out := map[string]int{}
	for _, a := range arms {
		out[a.Arm] = a.Beads
	}
	return out
}

// TestAdvisorAttributionEveryClosePath closes beads through each path a bead
// can close by (bd close, a status update to done, the governor releasing a
// topic claim when its PR merges, and an operator CloseAll) and asserts that
// each lands in an arm, including holdout with nothing delivered
// (hivecommons/hive#19577).
func TestAdvisorAttributionEveryClosePath(t *testing.T) {
	decisions := NewAdvisorDecisionStore("")
	rec := func(kick, ref, mode, outcome string) {
		decisions.Record(AdvisorDecisionRecord{Agent: "dev", Kick: kick, Refs: []string{ref}, Mode: mode, Outcome: outcome})
	}
	// Kicks record qualified refs; beads carry whatever the agent typed.
	rec("dev#1", "hivecommons/hive#101", "rank", "held-out")
	rec("dev#2", "hivecommons/hive#101", "rank", "held-out")
	rec("dev#3", "hivecommons/hive#102", "rank", "error")
	rec("dev#4", "hivecommons/hive#103", "rank", "held-out")
	rec("dev#5", "hivecommons/hive#103", "rank", "delivered")
	rec("dev#6", "hivecommons/hive#105", "shadow", "gated")

	source := newStore(t, "attr-source")
	retroStore := newStore(t, "attr-retro")
	create := func(title, ref string, typ beads.BeadType) *beads.Bead {
		t.Helper()
		b, err := source.Create(title, typ, beads.PriorityMedium, "dev", ref)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// bd close: an owner-less ref against the kick's qualified ref.
	if err := source.Close(create("holdout via bd close", "hive#101", beads.TypeTask).ID); err != nil {
		t.Fatal(err)
	}
	// bd update --status done: the documented "gh-N" external ref.
	done := create("deliver via status done", "gh-102", beads.TypeBug)
	if err := source.Update(done.ID, func(b *beads.Bead) { b.Status = beads.StatusDone }); err != nil {
		t.Fatal(err)
	}
	// PR merge: the governor releases the topic claim held through the PR.
	if _, err := source.ClaimTopic("dev", beads.TopicClaim{Key: "bump-deps", Repo: "hivecommons/hive", PR: 103, Branch: "bump"}); err != nil {
		t.Fatal(err)
	}
	if n, err := source.ReleaseTopicsForPR("hivecommons/hive", 103, beads.TopicStateMerged); err != nil || n != 1 {
		t.Fatalf("release topic = %d, %v", n, err)
	}
	// A closed advisory bead is not work and is never classed.
	if err := source.Close(create("advisory", "hivecommons/hive#101", beads.TypeAdvisory).ID); err != nil {
		t.Fatal(err)
	}
	// Manual close-all: a planning-epic ref with no decision, a qualified ref
	// whose journey the timeline keeps under the short key, and a bead with
	// no work ref at all.
	create("no decision", "gh-hivecommons/hive#104", beads.TypeEpic)
	create("shadow via close-all", "hivecommons/hive#105", beads.TypeTask)
	create("no ref", "sla-breach-2026-11-20", beads.TypeTask)
	if n, err := source.CloseAll("operator"); err != nil || n != 3 {
		t.Fatalf("close-all = %d, %v", n, err)
	}

	tl := timeline.NewStore()
	tl.Record(timeline.Event{IssueRef: "hive#105", Kind: timeline.KindKicked, Agent: "dev"})
	tl.Record(timeline.Event{IssueRef: "hive#105", Kind: timeline.KindMerged, Attrs: map[string]string{timeline.AttrPRRef: "hive#205"}})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	stores := map[string]*beads.Store{"dev": source, Actor: retroStore}
	attribution := NewAdvisorAttribution(stores, retroStore, tl, nil, logger)
	attribution.SetAdvisorDecisions(decisions)
	got := attribution.Report()

	wantArms := map[string]int{"none": 2, "shadow": 1, "deliver": 1, "holdout": 1, "mixed": 1}
	if !reflect.DeepEqual(armBeads(got.Arms), wantArms) {
		t.Fatalf("arms = %+v, want %v", got.Arms, wantArms)
	}
	wantCov := AdvisorArmCoverage{ClosedBeads: 6, WithClosedPR: 2, NoRef: 1, NoDecision: 1}
	if got.Coverage != wantCov {
		t.Fatalf("coverage = %+v, want %+v", got.Coverage, wantCov)
	}
	for _, a := range got.Arms {
		if a.Arm == AdvisorArmShadow && a.MeanKicksReceived != 1 {
			t.Fatalf("shadow bead did not pick up its short-key journey: %+v", a)
		}
	}
	if !strings.Contains(logs.String(), "without a matching knowledge-advisor decision") || !strings.Contains(logs.String(), "no_decision=1") {
		t.Fatalf("missing coverage log:\n%s", logs.String())
	}
	logs.Reset()
	attribution.Report()
	if logs.Len() != 0 {
		t.Fatalf("unchanged coverage logged again:\n%s", logs.String())
	}

	// The lane serves the same report; it no longer needs a closed PR.
	lane := NewLane(stores, retroStore, tl, nil, Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane.SetAdvisorDecisions(decisions)
	if lr := lane.AdvisorArmReport(); !reflect.DeepEqual(lr, got) {
		t.Fatalf("lane report = %+v, want %+v", lr, got)
	}
}

// TestAdvisorAttributionNoBeadsClosed keeps "no beads closed" apart from
// "beads closed, none matched".
func TestAdvisorAttributionNoBeadsClosed(t *testing.T) {
	source := newStore(t, "empty-source")
	if _, err := source.Create("still open", beads.TypeTask, beads.PriorityMedium, "dev", "hive#1"); err != nil {
		t.Fatal(err)
	}
	a := NewAdvisorAttribution(map[string]*beads.Store{"dev": source}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.SetAdvisorDecisions(NewAdvisorDecisionStore(""))
	if got := a.Report(); len(got.Arms) != 0 || got.Coverage != (AdvisorArmCoverage{}) {
		t.Fatalf("report = %+v, want empty", got)
	}
	var nilAttr *AdvisorAttribution
	if got := nilAttr.Report(); got.Arms != nil {
		t.Fatalf("nil attribution = %+v", got)
	}
}

func TestParseWorkRef(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"hivecommons/hive#12", "hivecommons/hive#12", true},
		{"Hive#12", "hive#12", true},
		{"gh-12", "#12", true},
		{"#12", "#12", true},
		{"gh-hivecommons/hive#12", "hivecommons/hive#12", true},
		{"https://github.com/hivecommons/hive/issues/12", "hivecommons/hive#12", true},
		{"https://github.com/hivecommons/hive/pull/12#issuecomment-1", "hivecommons/hive#12", true},
		{"topic:bump-deps", "", false},
		{"sla-breach-2026-11-20", "", false},
		{"o/r#2-pr", "", false},
		{"hive#0", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		w, ok := parseWorkRef(tc.in)
		if ok != tc.ok || (ok && w.String() != tc.want) {
			t.Errorf("parseWorkRef(%q) = %q, %v; want %q, %v", tc.in, w.String(), ok, tc.want, tc.ok)
		}
	}
}

func TestAdvisorDecisionsForRefsMatching(t *testing.T) {
	s := NewAdvisorDecisionStore("")
	s.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "k1", Refs: []string{"hivecommons/hive#7", "hivecommons/hive#70"}, Mode: "rank", Outcome: "held-out"})
	s.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "k2", Refs: []string{"hivecommons/docs#8"}, Mode: "rank", Outcome: "held-out"})
	s.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "k3", Refs: []string{"hivecommons/hive#8"}, Mode: "rank", Outcome: "delivered"})
	s.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "k4", Refs: []string{"other/hive#7"}, Mode: "rank", Outcome: "delivered"})

	kicks := func(ds []AdvisorDecision) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Kick)
		}
		return out
	}
	cases := []struct {
		name string
		refs []string
		want []string
	}{
		{"qualified stored, short bead ref", []string{"hive#7"}, []string{"k1", "k4"}},
		{"owner disambiguates", []string{"hivecommons/hive#7"}, []string{"k1"}},
		{"issue and PR ref count a decision once", []string{"hivecommons/hive#7", "hivecommons/hive#70"}, []string{"k1"}},
		{"bare number, one repo", []string{"gh-70"}, []string{"k1"}},
		{"bare number, two repos is ambiguous", []string{"gh-8"}, nil},
		{"url", []string{"https://github.com/hivecommons/docs/issues/8"}, []string{"k2"}},
		{"empty", []string{""}, nil},
	}
	for _, tc := range cases {
		if got := kicks(s.AdvisorDecisionsForRefs(tc.refs...)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: kicks = %v, want %v", tc.name, got, tc.want)
		}
	}
}
