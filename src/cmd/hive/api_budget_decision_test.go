package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestDecideEvalBudgetWorkSkipsOptionalByModeAndCadence(t *testing.T) {
	if d := decideEvalBudgetWork(github.APIBudgetNormal, 3, 2); !d.SkipOptional || len(d.SkippedSteps) == 0 {
		t.Fatalf("normal off-cadence should skip optional: %+v", d)
	}
	if d := decideEvalBudgetWork(github.APIBudgetNormal, 3, 3); d.SkipOptional {
		t.Fatalf("normal on-cadence should run optional: %+v", d)
	}
	if d := decideEvalBudgetWork(github.APIBudgetConserve, 1, 1); !d.SkipOptional {
		t.Fatalf("conserve should skip optional: %+v", d)
	}
}

func TestAPIBudgetEffectiveEvalInterval(t *testing.T) {
	if got := apiBudgetEffectiveEvalInterval(300, 1800, 2, github.APIBudgetNormal); got != 300*time.Second {
		t.Fatalf("normal interval = %v", got)
	}
	if got := apiBudgetEffectiveEvalInterval(300, 1800, 2, github.APIBudgetConserve); got != 600*time.Second {
		t.Fatalf("conserve interval = %v", got)
	}
	if got := apiBudgetEffectiveEvalInterval(600, 1800, 2, github.APIBudgetCritical); got != 1800*time.Second {
		t.Fatalf("critical interval cap = %v", got)
	}
}
