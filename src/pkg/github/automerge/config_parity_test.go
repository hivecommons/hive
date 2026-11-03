package automerge

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSelfMergeMinACMMLevelMatchesConfig(t *testing.T) {
	if SelfMergeMinACMMLevel != config.SelfMergeMinACMMLevel {
		t.Fatalf("SelfMergeMinACMMLevel = %d, config.SelfMergeMinACMMLevel = %d — "+
			"the sweep logs this as the authority for self-authored auto-merge; "+
			"a mismatch would misreport the gate an operator is being held to",
			SelfMergeMinACMMLevel, config.SelfMergeMinACMMLevel)
	}
}

func TestReviewGateModesMatchConfig(t *testing.T) {
	for _, pair := range [][2]string{
		{ReviewGateModeOff, config.ReviewGateModeOff},
		{ReviewGateModeShadow, config.ReviewGateModeShadow},
		{ReviewGateModeEnforce, config.ReviewGateModeEnforce},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("automerge review gate mode %q != config mode %q", pair[0], pair[1])
		}
	}
	if got := NormalizeReviewGateMode((config.ReviewGateConfig{}).ModeOrDefault()); got != ReviewGateModeShadow {
		t.Fatalf("default review gate mode = %q, want shadow", got)
	}
}
