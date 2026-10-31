package integration

import (
	"github.com/hivecommons/hive/pkg/config"
	"testing"
	"time"
)

func TestExecutionReservationReleaseRetainsAmbiguityAndScopedAudit(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	admitted, err := store.PlanExecution(cfg, in)
	if err != nil || admitted.Plan == nil {
		t.Fatalf("plan: %+v %v", admitted, err)
	}
	plan := *admitted.Plan
	plan.State, plan.DispatchOutcome, plan.RequestDigest = Unknown, "unknown", cfg.Scopes[0].Policy
	plan.Generation++
	if err := store.AppendExecution("ambiguous", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseExecutionReservations([]config.IntegrationExecutionScope{{Repo: "other/repo", Base: "v5"}}, in.Now); err != nil {
		t.Fatal(err)
	}
	current, _ := store.ExecutionPlan(plan.Recipe.ID())
	if current.ReservationsReleased {
		t.Fatal("released another scope")
	}
	if err := store.ReleaseExecutionReservations(cfg.Scopes, in.Now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExecution(store.path)
	if err != nil {
		t.Fatal(err)
	}
	current, _ = reopened.ExecutionPlan(plan.Recipe.ID())
	if !current.ReservationsReleased || current.State != Unknown || current.DispatchOutcome != "unknown" || current.RequestDigest != plan.RequestDigest {
		t.Fatalf("release erased ambiguity/evidence: %+v", current)
	}
	if _, err := reopened.ResumeExecution(cfg, plan.Recipe.ID(), in.Cost, in.Now.Add(24*time.Hour)); err == nil {
		t.Fatal("TTL/cooldown stole an ambiguous request")
	}
}

func TestExecutionSafeResumeOnlyKnownNotAppliedAndBounded(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	cfg.MaxAttempts = 2
	admitted, err := store.PlanExecution(cfg, in)
	if err != nil || admitted.Plan == nil {
		t.Fatalf("plan: %+v %v", admitted, err)
	}

	plan, err := store.ResumeExecution(cfg, admitted.Plan.Recipe.ID(), in.Cost, in.Now)
	if err != nil || plan.Attempt != 1 {
		t.Fatalf("unissued resume: %+v %v", plan, err)
	}
	plan.State, plan.DispatchOutcome = Rejected, "not-applied"
	plan.Generation++
	if err := store.AppendExecution("refused", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResumeExecution(cfg, plan.Recipe.ID(), in.Cost, in.Now); err == nil {
		t.Fatal("ignored cooldown")
	}
	unknown := in.Cost
	unknown.Complete = false
	if _, err := store.ResumeExecution(cfg, plan.Recipe.ID(), unknown, in.Now.Add(time.Minute)); err == nil {
		t.Fatal("resumed without actual accounting")
	}
	changed := cfg
	changed.Scopes = append([]config.IntegrationExecutionScope(nil), cfg.Scopes...)
	changed.Scopes[0].ExecutorID = cfg.Scopes[0].Policy
	if _, err := store.ResumeExecution(changed, plan.Recipe.ID(), in.Cost, in.Now.Add(time.Minute)); err == nil {
		t.Fatal("resumed across executor handover")
	}
	resumed, err := store.ResumeExecution(cfg, plan.Recipe.ID(), in.Cost, in.Now.Add(time.Minute))
	if err != nil || resumed.Attempt != 2 || resumed.RequestDigest != "" || resumed.ReservationsReleased {
		t.Fatalf("bounded resume: %+v %v", resumed, err)
	}
	resumed.State, resumed.DispatchOutcome = Rejected, "not-applied"
	resumed.Generation++
	if err := store.AppendExecution("refused-again", resumed.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &resumed}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResumeExecution(cfg, plan.Recipe.ID(), in.Cost, in.Now.Add(time.Hour)); err == nil {
		t.Fatal("ignored attempt bound")
	}
}

func TestExecutionResumeRefusesMissingPlanDisabledProfileAndSpentBudget(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	if _, err := store.ResumeExecution(cfg, "missing", in.Cost, in.Now); err == nil {
		t.Fatal("resumed nonexistent recipe")
	}
	admitted, err := store.PlanExecution(cfg, in)
	if err != nil || admitted.Plan == nil {
		t.Fatalf("plan: %+v %v", admitted, err)
	}
	disabled := cfg
	disabled.Mode = config.IntegrationAdvisory
	if _, err := store.ResumeExecution(disabled, admitted.Plan.Recipe.ID(), in.Cost, in.Now); err == nil {
		t.Fatal("advisory resumed dispatch")
	}
	spent := cfg.MaxRunnerMinutes
	cost := in.Cost
	cost.RunnerMinutes = &spent
	if _, err := store.ResumeExecution(cfg, admitted.Plan.Recipe.ID(), cost, in.Now); err == nil {
		t.Fatal("resumed past global minute budget")
	}
}
