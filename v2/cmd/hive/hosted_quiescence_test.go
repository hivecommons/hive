package main

import (
	"reflect"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
)

func TestFinalizeLifecycleQuiescenceAcceptsCompletedState(t *testing.T) {
	status := map[string]any{
		"production_ready": true,
		"setup_baseline": map[string]any{
			"phase":         integrated.SetupBaselineProductionVerified,
			"pending_audit": false,
		},
		"repair_branch_retirements": map[string]any{
			"pending":             0,
			"migration_completed": true,
		},
		"lifecycle": map[string]any{"pending_outbox": 0},
		"repairs": map[string]any{"attempts": []map[string]any{
			{"stage": repair.StageNoChange},
			{"stage": repair.StagePROpen},
			{"stage": repair.StageFailed},
			{"stage": repair.StageCancelled},
		}},
	}

	finalizeLifecycleQuiescence(status)

	if quiescent, _ := status["quiescent"].(bool); !quiescent {
		t.Fatalf("expected completed state to be quiescent: %#v", status)
	}
	if blockers, ok := status["quiescence_blockers"].([]string); !ok || len(blockers) != 0 {
		t.Fatalf("expected no quiescence blockers, got %#v", status["quiescence_blockers"])
	}
	if ready, _ := status["production_ready"].(bool); !ready {
		t.Fatal("quiescence must not clear an existing production-ready result")
	}
}

func TestFinalizeLifecycleQuiescenceReportsSortedInflightBlockers(t *testing.T) {
	status := map[string]any{
		"production_ready":           true,
		"workflow_dispatch_recovery": map[string]any{},
		"merge_intent":               map[string]any{},
		"repair_state_error":         "invalid state",
		"setup_baseline": map[string]any{
			"phase":         "auditing",
			"pending_audit": true,
		},
		"repair_branch_retirements": map[string]any{
			"pending":             1,
			"migration_completed": false,
		},
		"lifecycle": map[string]any{"pending_outbox": 2},
		"repairs": map[string]any{"attempts": []map[string]any{
			{"stage": repair.StageModelRunning},
		}},
	}

	finalizeLifecycleQuiescence(status)

	want := []string{
		"lifecycle_pending_outbox",
		"merge_intent",
		"nonterminal_repair_attempt",
		"repair_branch_retirements",
		"repair_state_error",
		"setup_baseline",
		"workflow_dispatch_recovery",
	}
	if got, _ := status["quiescence_blockers"].([]string); !reflect.DeepEqual(got, want) {
		t.Fatalf("quiescence blockers = %#v, want %#v", got, want)
	}
	if quiescent, _ := status["quiescent"].(bool); quiescent {
		t.Fatal("expected inflight state to be non-quiescent")
	}
	if ready, _ := status["production_ready"].(bool); ready {
		t.Fatal("non-quiescent state must not remain production-ready")
	}
}

func TestAddLifecycleQuiescenceBlockerIsSortedAndIdempotent(t *testing.T) {
	status := map[string]any{
		"production_ready":    true,
		"quiescent":           true,
		"quiescence_blockers": []string{"zeta"},
	}

	addLifecycleQuiescenceBlocker(status, "alpha")
	addLifecycleQuiescenceBlocker(status, "alpha")

	want := []string{"alpha", "zeta"}
	if got, _ := status["quiescence_blockers"].([]string); !reflect.DeepEqual(got, want) {
		t.Fatalf("quiescence blockers = %#v, want %#v", got, want)
	}
	if quiescent, _ := status["quiescent"].(bool); quiescent {
		t.Fatal("explicit blocker must make the lifecycle non-quiescent")
	}
	if ready, _ := status["production_ready"].(bool); ready {
		t.Fatal("explicit blocker must make the lifecycle not production-ready")
	}
}
