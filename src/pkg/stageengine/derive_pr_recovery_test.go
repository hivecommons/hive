package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDerivePRRecoveryPermissionBlockIsNotSourceFixing(t *testing.T) {
	f := hiveFixingFacts("required-check-failing:gosec")
	f.PR.Pass = &PRPassFacts{
		AuthorIsApp: true, RecoveryUnauthorized: "rerun-shared-ci",
	}
	d := DerivePR(f)
	if d.Stage != stagelabels.NeedsDecision || d.Reason != "unauthorized:rerun-shared-ci" {
		t.Fatalf("permission block looks like source fixing: %+v", d)
	}
	f.PR.Pass.RecoveryUnauthorized = ""
	if d := DerivePR(f); d.Stage != stagelabels.Fixing {
		t.Fatalf("cleared recovery block must preserve actual CI failure: %+v", d)
	}
}
