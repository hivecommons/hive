package stagelabels

import (
	"strings"
	"testing"
)

// projectbluefin/utah#1097 and projectbluefin/finpilot#627: a hold the
// human-merge-path refusal set was described with the reporter-trust step
// ("the lane merges on its next pass … until a maintainer has reviewed the
// issue's reporter"), contradicting the refusal's own "a person must merge
// it". The step is now worded for the gate that set the hold.
func TestSelfImposedHoldStepMatchesItsGate(t *testing.T) {
	route := func(h *HoldProvenance) Route {
		return Route{Stage: NeedsDecision, Head: "bb9de1e6b0000000000000000000000000000000", Holds: []string{"hold"}, Hold: h}
	}
	for _, tc := range []struct {
		name  string
		hold  *HoldProvenance
		want  []string
		never []string
	}{
		{
			name:  "reporter trust keeps its release step",
			hold:  &HoldProvenance{By: HoldByHive, Source: HoldSourceReporterTrust, Reason: "reporter trust (issue #15017 by @Karibhou, contributor)"},
			want:  []string{"reporter trust (issue #15017", "the lane merges on its next pass", "reviewed the issue's reporter"},
			never: []string{"merges it by hand"},
		},
		{
			name:  "human-merge path is a person's merge",
			hold:  &HoldProvenance{By: HoldByHive, Source: HoldSourceHumanMergePath, Reason: "it changes " + HumanMergePathReason},
			want:  []string{"`auto_merge.human_merge_paths`", "the hive never merges it", "merges it by hand", "does not hand it back to the lane"},
			never: []string{"the lane merges on its next pass", "reporter"},
		},
		{
			name:  "a gate known only from the label event",
			hold:  &HoldProvenance{By: HoldByHive, Source: HoldSourceLabel},
			want:  []string{"its notice on this PR names the gate", "remove the `hold` label"},
			never: []string{"the lane merges on its next pass", "reporter", "its own gate"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecisionActions(route(tc.hold))
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q:\n%s", w, got)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(got, n) {
					t.Errorf("must not say %q:\n%s", n, got)
				}
			}
		})
	}
}
