package classify

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const spekChildMarker = "<!-- spek:publish name=billing artifact_id=spk_1 target=github task=t2 rfc=https://github.com/acme/app/issues/9 -->"

func TestTriageRules(t *testing.T) {
	longBody := "This issue has enough detail to explain the requested behaviour, constraints, and expected outcome."
	cases := []struct {
		name   string
		issue  github.Issue
		class  Classification
		cfg    config.TriageConfig
		want   TriageVerdict
		signal string
	}{
		{"spec override wins", github.Issue{Labels: []string{"run/spec"}, Body: "short"}, Classification{Tier: TierSimple}, config.TriageConfig{}, TriageSpec, "label:run/spec"},
		{"fix override wins", github.Issue{Labels: []string{"run/fix", "kind/feature"}, Body: "short"}, Classification{Tier: TierComplex}, config.TriageConfig{}, TriageFix, "label:run/fix"},
		{"short body clarifies", github.Issue{Body: "too short"}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageClarify, "body:short"},
		{"option list clarifies", github.Issue{Body: longBody + "\nSeverity: high/medium/low"}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageClarify, "body:unchosen_option:high/medium/low"},
		{"placeholder clarifies", github.Issue{Body: longBody + "\n<analysis>"}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageClarify, "body:placeholder:<analysis>"},
		{"feature label specs", github.Issue{Labels: []string{"kind/feature"}, Body: longBody}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageSpec, "label:kind/feature"},
		{"complex specs", github.Issue{Body: longBody}, Classification{Tier: TierComplex}, config.TriageConfig{}, TriageSpec, "tier:Complex"},
		{"bug label fixes", github.Issue{Labels: []string{"kind/bug"}, Body: longBody}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageFix, "label:kind/bug"},
		{"simple fixes", github.Issue{Body: longBody}, Classification{Tier: TierSimple}, config.TriageConfig{}, TriageFix, "tier:Simple"},
		{"medium fixes", github.Issue{Body: longBody}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageFix, "tier:Medium"},
		{"custom spec label", github.Issue{Labels: []string{"needs-design"}, Body: longBody}, Classification{Tier: TierMedium}, config.TriageConfig{SpecLabels: []string{"needs-design"}}, TriageSpec, "label:needs-design"},
		// spektacular#114: a Spek-published child is already specified, so
		// neither a spec label nor a Complex tier re-routes it into a spec run.
		{"spek child beats spec label", github.Issue{Labels: []string{"kind/feature"}, Body: longBody + "\n" + spekChildMarker}, Classification{Tier: TierComplex}, config.TriageConfig{}, TriageFix, SpekPublishedSignal},
		{"spek child beats short body", github.Issue{Body: spekChildMarker}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageFix, SpekPublishedSignal},
		{"spek umbrella never specs", github.Issue{Labels: []string{"Epic"}, Body: longBody + "\n<!-- spek:publish name=n artifact_id=a target=github -->"}, Classification{Tier: TierComplex}, config.TriageConfig{}, TriageFix, SpekPublishedSignal},
		{"run/spec still overrides spek marker", github.Issue{Labels: []string{"run/spec"}, Body: spekChildMarker}, Classification{Tier: TierSimple}, config.TriageConfig{}, TriageSpec, "label:run/spec"},
		{"malformed spek marker is no marker", github.Issue{Labels: []string{"kind/feature"}, Body: longBody + "\n<!-- spek:publish name=n -->"}, Classification{Tier: TierMedium}, config.TriageConfig{}, TriageSpec, "label:kind/feature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Triage(context.Background(), tc.issue, tc.class, tc.cfg)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (%+v)", got.Verdict, tc.want, got)
			}
			if len(got.Signals) == 0 || got.Signals[0] != tc.signal {
				t.Fatalf("signals = %v, want first %q", got.Signals, tc.signal)
			}
		})
	}
}
