package hub

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/nous"
)

// SetSelfExperimentFlag is the background runner's assignment/revert seam. It
// does not expose an unauthenticated management endpoint or change upgrade or
// governor settings. The runner owns persistence and guardrail evaluation;
// losing its assignment on hub restart safely withdraws the flag next beat.
// nil withdraws the assignment; Enabled=false delivers an explicit revert.
func (s *HubServer) SetSelfExperimentFlag(hiveID string, assignment *nous.SelfFlagAssignment) error {
	if hiveID == "" {
		return fmt.Errorf("self-experiment assignment requires a hive ID")
	}
	if assignment == nil {
		s.selfExperimentFlags.Delete(hiveID)
		return nil
	}
	if err := assignment.Validate(); err != nil {
		return err
	}
	s.selfExperimentFlags.Store(hiveID, *assignment)
	return nil
}

func (s *HubServer) selfExperimentFlagForHeartbeat(hiveID string, optedIn bool) *nous.SelfFlagAssignment {
	if !optedIn {
		return nil
	}
	value, ok := s.selfExperimentFlags.Load(hiveID)
	if !ok {
		return nil
	}
	assignment := value.(nous.SelfFlagAssignment)
	return &assignment
}
