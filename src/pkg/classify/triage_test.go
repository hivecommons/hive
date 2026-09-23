package classify

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Triage(tc.issue, tc.class, tc.cfg)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (%+v)", got.Verdict, tc.want, got)
			}
			if len(got.Signals) == 0 || got.Signals[0] != tc.signal {
				t.Fatalf("signals = %v, want first %q", got.Signals, tc.signal)
			}
		})
	}
}
