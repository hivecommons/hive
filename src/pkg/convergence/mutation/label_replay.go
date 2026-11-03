package mutation

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// reopenLabel supersedes a cached success only after the boundary observes
// that the label state no longer holds, while holding its mutation claim.
// Keep the same entry and attempt history: retries after an ambiguous new
// write must still reconcile. Generic Reconcile retains its terminal invariant.
func (j *Journal) reopenLabel(observed Operation, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	unlock, err := j.lockAndRefreshLocked()
	if err != nil {
		return err
	}
	defer unlock()
	op, ok := j.ops[observed.LogicalID]
	if !ok || op.Effect.Kind != effects.KindLabelMutation || op.Status != StatusApplied ||
		!op.UpdatedAt.Equal(observed.UpdatedAt) || len(op.Attempts) != len(observed.Attempts) {
		return fmt.Errorf("%s changed before label replay: %w", observed.LogicalID, ErrResultRegression)
	}
	work := op.clone()
	work.Status = StatusNotApplied
	work.Result = ""
	work.UpdatedAt = now
	_, err = j.commit(work.LogicalID, work)
	return err
}
