package dashboard

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/forgestate"
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

func (s *Server) executionPromotionWork(store *integration.Store, subject forgestate.IntegrationCandidateSubject, now time.Time) error {
	index := len(subject.Members) - 1
	if index < 0 {
		return fmt.Errorf("promotion subject has no members")
	}
	for _, plan := range store.ExecutionSnapshot().Plans {
		for _, prepared := range plan.Prepared {
			if !prepared.SameCore(subject) || len(plan.WorkBindings) <= index || len(plan.Recipe.Members) <= index ||
				plan.Recipe.Members[index] != subject.Members[index] {
				continue
			}
			// Earlier members already have exact documented merged lineage.
			// Only the member being promoted needs a live authoring claim/DAG.
			plan.WorkBindings = []integration.ExecutionWorkBinding{plan.WorkBindings[index]}
			plan.Recipe.Members = []forgestate.IntegrationCandidateMember{subject.Members[index]}
			return s.executionLiveWork(plan, now)
		}
	}
	return fmt.Errorf("current promotion member has no exact canonical work binding")
}
