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
