package dashboard

import (
	"fmt"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"time"
)

func (s *Server) executionLiveWork(plan integration.ExecutionPlan, now time.Time) error {
	intents, err := s.integrationStore()
	if err != nil {
		return err
	}
	current := map[string]integration.Intent{}
	for _, intent := range intents.Intents() {
		current[intent.Key] = intent
	}
	for index, binding := range plan.WorkBindings {
		intent, exists := current[binding.Key]
		if !exists || intent.Generation != binding.IntentGeneration || intent.PlanRevision != binding.PlanRevision ||
			intent.Head != plan.Recipe.Members[index].Head || !intent.ScopeKnown {
			return fmt.Errorf("exact canonical work/head/scope generation changed: %s", binding.Key)
		}
		input := s.integrationInput(binding.Key, plan.Recipe.Repo, plan.Recipe.Base, now)
		if !input.PlanApproved || input.PlanRevision != binding.PlanRevision {
			return fmt.Errorf("approved DAG generation changed: %s", binding.Key)
		}
		for _, prerequisite := range intent.Prerequisites {
			dependency, exists := input.Dependencies[prerequisite.Key]
			if !exists || !dependency.Known || !dependency.Satisfied {
				return fmt.Errorf("validated prerequisite not satisfied: %s", prerequisite.Key)
			}
		}
	}
	return nil
}
