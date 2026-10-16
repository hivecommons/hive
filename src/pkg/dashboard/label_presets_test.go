package dashboard

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestNeedsHumanPresetLabelsDefaultOrder(t *testing.T) {
	got := needsHumanPresetLabels(nil, "")
	want := append([]string{"needs-direction", "needs-decision", "needs-spec", "needs-signal", "needs-human"}, github.HoldLabels...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default needs-human preset = %v, want %v", got, want)
	}
}

func TestNeedsHumanPresetLabelsHonoursConfigAndHiveHold(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.IssueFilter.HardSuppressLabels = config.HardSuppressLabelsConfig{
		NeedsDirection: []string{"triage/direction"},
		NeedsHuman:     []string{"Needs-Human", " human-please "},
	}
	got := needsHumanPresetLabels(cfg, "acme")
	if got[0] != "triage/direction" {
		t.Fatalf("configured needs-direction bucket must sort first, got %v", got)
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
