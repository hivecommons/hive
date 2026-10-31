package automerge

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
)

func TestIntegrationEligibilityIsolatesObservationsWithoutWeakeningPolicy(t *testing.T) {
	stages := NewStageTracker()
	preparation := NewBasePreparationStore("")
	intent, review := &IntentGate{}, &ReviewGate{}
	engine := &Engine{integrationOptions: Options{
		StageTracker: stages, BasePreparation: preparation, IntentGate: intent, ReviewGate: review,
		RequiredChecks: map[string]bool{"security": true}, IntegrationSelfAllowed: func() bool { return false },
		RequireCurrentBase: func(string) bool { return true },
	}}
	probe := engine.integrationReadOnlyOptions()
	if probe.StageTracker == stages || probe.BasePreparation == preparation {
		t.Fatal("probe overwrites real tracker/preparation authority observations")
	}
	if probe.IntentGate != intent || probe.ReviewGate != review || !probe.RequiredChecks["security"] || probe.IntegrationSelfAllowed() || !probe.RequireCurrentBase("o/r") {
		t.Fatal("read-only isolation weakened original intent/review/check/self/base policy")
	}
	called := false
	_, err := probe.MutationBoundary.Execute(context.Background(), effects.Claim{Kind: effects.KindPullRequestMerge}, func(context.Context) (effects.Result, error) { called = true; return effects.Result{}, nil })
	if err == nil || called {
		t.Fatal("private evaluation executed a mutation")
	}
}
