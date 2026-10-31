package integration

import (
	"fmt"
	"github.com/hivecommons/hive/pkg/config"
	"time"
)

// ReleaseExecutionReservations changes only admission reservations, retaining
// unknown transport outcomes, exact evidence, issuer history and mutation audit.
func (s *Store) ReleaseExecutionReservations(scopes []config.IntegrationExecutionScope, now time.Time) error {
	selected := map[string]bool{}
	for _, scope := range scopes {
		selected[scopeKey(scope.Repo, scope.Base)] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	for _, current := range s.executionData().plans {
		if !selected[scopeKey(current.Recipe.Repo, current.Recipe.Base)] || current.ReservationsReleased {
			continue
		}
		plan := cloneExecution(current)
		plan.ReservationsReleased, plan.ObservedAt = true, now
		plan.Generation++
		plan.Reason = "operator released only execution admission reservation; outcome/evidence audit retained"
		if err := s.appendLocked(Event{Version: SchemaVersion,
			ID: fmt.Sprintf("execution/release/%s/%d", plan.Recipe.ID(), plan.Generation), At: now,
			Execution: &ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}}); err != nil {
			return err
		}
	}
	return nil
}
