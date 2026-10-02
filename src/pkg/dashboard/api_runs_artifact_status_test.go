package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/timeline"
)

// An implement-stage run has no Spektacular document, so it must not inherit
// the plan document's artifact/status/step (#10121).
func TestApplyRunArtifactStatusImplementSkipsPlanFallback(t *testing.T) {
	events := []timeline.Event{{
		IssueRef: "smokeorg/app#11", Kind: timeline.Kind("stage_completed"), At: 1000,
		Attrs: map[string]string{
			stageAttrStage: StagePlan, stageAttrArtifact: "smokeorg-app-11",
			stageAttrDocumentStatus: "final", stageAttrCurrentStep: "finished",
		},
	}}
	run := Run{Stage: StageImplement}
	applyRunArtifactStatus(&run, events)
	if run.ArtifactID != "" || run.ArtifactName != "" || run.DocumentStatus != "" || run.CurrentStep != "" {
		t.Fatalf("implement run inherited plan fields: artifact=%q name=%q status=%q step=%q",
			run.ArtifactID, run.ArtifactName, run.DocumentStatus, run.CurrentStep)
	}
}

// Spec/plan runs keep the any-stage fallback for fields their own stage
// events do not carry.
func TestApplyRunArtifactStatusPlanKeepsFallback(t *testing.T) {
	events := []timeline.Event{{
		IssueRef: "smokeorg/app#11", Kind: timeline.Kind("stage_completed"), At: 1000,
		Attrs: map[string]string{
			stageAttrStage: StageSpec, stageAttrArtifact: "smokeorg-app-11",
			stageAttrDocumentStatus: "final", stageAttrCurrentStep: "finished",
		},
	}}
	run := Run{Stage: StagePlan}
	applyRunArtifactStatus(&run, events)
	if run.ArtifactID != "smokeorg-app-11" || run.DocumentStatus != "final" || run.CurrentStep != "finished" {
		t.Fatalf("plan run lost fallback fields: %+v", run)
	}
}
