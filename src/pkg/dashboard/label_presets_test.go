package dashboard

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestNeedsHumanPresetLabelsDefaultOrder(t *testing.T) {
	got := needsHumanPresetLabels(nil, "")
	want := append([]string{github.NeedsDecisionStageLabel, github.PRNeedsDecisionStageLabel, "needs-decision", "needs-direction", "needs-spec", "needs-signal", "needs-human"}, github.HoldLabels...)
	want = append(want, stagelabels.NeedsAuthor.Label(), stagelabels.UnauthorizedLabel, config.DefaultSentinelLabel, config.DefaultReporterTrustAwaitingLabel)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default needs-human preset = %v, want %v", got, want)
	}
	if got[len(got)-4] != "stage/071-needs-author" || got[len(got)-3] != "hive:unauthorized" || got[len(got)-2] != "sentinel-alert" || got[len(got)-1] != "needs-triage" {
		t.Fatalf("needs-author, hive:unauthorized, sentinel-alert and needs-triage must close the default set, got %v", got)
	}
}

// The sentinel alert label follows sentinel.label: a renamed label is
// offered under its new spelling and the default is not.
func TestNeedsHumanPresetLabelsSentinelLabel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sentinel.Label = "security/sentinel"
	got := needsHumanPresetLabels(cfg, "")
	if !slices.Contains(got, "security/sentinel") || slices.Contains(got, config.DefaultSentinelLabel) {
		t.Fatalf("configured sentinel label must replace the default, got %v", got)
	}
}

// The reporter-trust awaiting label follows config: a renamed label is
// offered under its new spelling, a disabled one ("" ) is dropped.
func TestNeedsHumanPresetLabelsReporterTrustAwaiting(t *testing.T) {
	renamed := "triage/needed"
	cfg := &config.Config{}
	cfg.Project.IssueFilter.ReporterTrust.AwaitingLabel = &renamed
	got := needsHumanPresetLabels(cfg, "")
	if !slices.Contains(got, renamed) || slices.Contains(got, config.DefaultReporterTrustAwaitingLabel) {
		t.Fatalf("renamed awaiting label must replace the default, got %v", got)
	}
	disabled := ""
	cfg.Project.IssueFilter.ReporterTrust.AwaitingLabel = &disabled
	got = needsHumanPresetLabels(cfg, "")
	if slices.Contains(got, "") || slices.Contains(got, config.DefaultReporterTrustAwaitingLabel) {
		t.Fatalf("disabled awaiting label must not be offered, got %v", got)
	}
	if !slices.Contains(got, stagelabels.NeedsAuthor.Label()) {
		t.Fatalf("stage/071-needs-author must always be offered, got %v", got)
	}
}

func TestNeedsHumanPresetLabelsHonoursConfigAndHiveHold(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.IssueFilter.HardSuppressLabels = config.HardSuppressLabelsConfig{
		NeedsDirection: []string{"triage/direction"},
		NeedsHuman:     []string{"Needs-Human", " human-please "},
	}
	got := needsHumanPresetLabels(cfg, "acme")
	if got[0] != github.NeedsDecisionStageLabel {
		t.Fatalf("stage/045 decision park must sort first, got %v", got)
	}
	if !slices.Contains(got, "triage/direction") {
		t.Fatalf("configured legacy needs-direction bucket must be included, got %v", got)
	}
	idx := map[string]int{}
	for i, l := range got {
		if _, dup := idx[l]; dup {
			t.Fatalf("duplicate label %q in %v", l, got)
		}
		idx[l] = i
	}
	for _, want := range []string{"Needs-Human", "human-please", "needs-decision", "needs-spec", "needs-signal", "hold"} {
		if _, ok := idx[want]; !ok {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	if hold := github.CanonicalHiveHoldLabel("acme"); hold != "" {
		if _, ok := idx[hold]; !ok {
			t.Fatalf("missing hive hold label %q in %v", hold, got)
		}
	}
	if idx["needs-decision"] > idx["Needs-Human"] {
		t.Fatalf("decision labels must sort before needs-human: %v", got)
	}
	for _, l := range got {
		if l == "blocked" {
			t.Fatalf("blocked is not a parked-on-a-person label and must not be in the preset: %v", got)
		}
	}
}

func TestLabelPresetsNeedsHumanFirst(t *testing.T) {
	presets := labelPresets(nil, "")
	if len(presets) < 3 || presets[0].Key != labelPresetNeedsHumanKey {
		t.Fatalf("presets = %+v, want Needs human first", presets)
	}
	keys := map[string]bool{}
	for _, p := range presets {
		if keys[p.Key] {
			t.Fatalf("duplicate preset key %q", p.Key)
		}
		keys[p.Key] = true
		if p.Label == "" || (len(p.Labels) == 0 && len(p.States) == 0) {
			t.Fatalf("preset %q must carry a label and at least one label or state facet: %+v", p.Key, p)
		}
	}
}

func TestStatusWithOverviewBandsCarriesLabelPresets(t *testing.T) {
	s := &Server{}
	out := s.statusWithOverviewBands(&StatusPayload{HiveID: "acme"}, time.Now())
	if len(out.LabelPresets) == 0 || out.LabelPresets[0].Key != labelPresetNeedsHumanKey {
		t.Fatalf("status payload must carry labelPresets with Needs human first, got %+v", out.LabelPresets)
	}
}

// The tracker payload carries the same needs-human set the Projects
// preset offers, so the two views cannot drift (one definition in Go).
func TestStageTrackerCarriesNeedsHumanLabels(t *testing.T) {
	srv := &Server{}
	status := &StatusPayload{HiveID: "acme"}
	tr := srv.buildStageTracker(status, nil, time.Now())
	want := needsHumanPresetLabels(nil, "acme")
	if !reflect.DeepEqual(tr.NeedsHumanLabels, want) {
		t.Fatalf("tracker needs_human_labels = %v, want %v", tr.NeedsHumanLabels, want)
	}
	if !slices.Contains(tr.NeedsHumanLabels, "stage/071-needs-author") || !slices.Contains(tr.NeedsHumanLabels, "needs-triage") {
		t.Fatalf("tracker set must include needs-author and needs-triage, got %v", tr.NeedsHumanLabels)
	}
	if tr := srv.buildStageTracker(nil, nil, time.Now()); len(tr.NeedsHumanLabels) != 0 {
		t.Fatalf("no status, no label set: got %v", tr.NeedsHumanLabels)
	}
}
