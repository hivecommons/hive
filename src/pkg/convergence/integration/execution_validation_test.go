package integration

import (
	"strings"
	"testing"
)

func TestExecutionTypedPlanAndRuntimeObservationValidation(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	admitted, err := store.PlanExecution(cfg, in)
	if err != nil || admitted.Plan == nil {
		t.Fatalf("plan: %+v %v", admitted, err)
	}
	for _, test := range []struct {
		name   string
		change func(*ExecutionPlan)
	}{
		{"recipe", func(p *ExecutionPlan) { p.Recipe.BaseSHA = "short" }},
		{"member", func(p *ExecutionPlan) { p.Recipe.Members[0].PR = 0 }},
		{"duplicate-head", func(p *ExecutionPlan) { p.Recipe.Members[1].Head = p.Recipe.Members[0].Head }},
		{"generation", func(p *ExecutionPlan) { p.Generation = 0 }},
		{"work-count", func(p *ExecutionPlan) { p.WorkKeys = nil }},
		{"work-key", func(p *ExecutionPlan) { p.WorkKeys[0] = "" }},
		{"duplicate-work-key", func(p *ExecutionPlan) { p.WorkKeys[1] = p.WorkKeys[0] }},
		{"binding", func(p *ExecutionPlan) { p.WorkBindings[0].IntentGeneration = 0 }},
		{"negative-plan-revision", func(p *ExecutionPlan) { p.WorkBindings[0].PlanRevision = -1 }},
		{"request", func(p *ExecutionPlan) { p.RequestDigest = "short" }},
		{"unsigned-prepared", func(p *ExecutionPlan) { p.Prepared = append(p.Prepared, executionProofFixture().Subject) }},
		{"reused-generation", func(p *ExecutionPlan) { p.Reason = "different payload same generation" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := cloneExecution(*admitted.Plan)
			test.change(&plan)
			if err := store.AppendExecution("invalid-"+test.name, in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}); err == nil {
				t.Fatal("accepted invalid plan")
			}
		})
	}
	status := ExecutionStatus{Repo: "o/r", Base: "v5", ObservedAt: in.Now, DesiredMode: "enforce", EffectiveWriter: "observer", Reason: "actual provider not qualified"}
	if err := store.AppendExecution("status", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Status: &status}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExecution("two-kinds", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Status: &status, Plan: admitted.Plan}); err == nil {
		t.Fatal("accepted ambiguous typed event")
	}
	invalidStatus := status
	invalidStatus.Reason = ""
	if err := store.AppendExecution("bad-status", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Status: &invalidStatus}); err == nil {
		t.Fatal("accepted undocumented writer state")
	}
	bad := ExecutionInvalidation{Repo: "o/r", Base: "v5", Revision: 0, Reason: "policy changed"}
	if err := store.AppendExecution("bad-invalidation", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Invalidate: &bad}); err == nil {
		t.Fatal("accepted unversioned invalidation")
	}
	promotion := ExecutionPromotion{Repo: "o/r", Base: "v5", SubjectID: strings.Repeat("d", 64), MutationKey: "key", State: "green-ish"}
	if err := store.AppendExecution("bad-promotion", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Promotion: &promotion}); err == nil {
		t.Fatal("accepted invalid promotion")
	}
}
