package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runstate"
)

// Saved-run-results routes beyond the record itself (S7): an explicit,
// audited restart, the caller's listing and the record's status history.

// Typed errors the serve routes map onto api.Code* values.
var (
	// ErrLive: a restart was asked for while the saved state is accepted,
	// running or waiting. The runner may still be alive, so two drivers
	// cannot both resume the same task; cancel first, or wait.
	ErrLive = errors.New("runs: the run is still live; cancel it or wait for it to finish before restarting")
	// ErrNoRequest: the record predates saved requests and holds nothing a
	// restart could re-run; create the run again instead.
	ErrNoRequest = errors.New("runs: the record does not carry the request it was created from; start a new run instead")
)

// DefaultListLimit caps GET /v1/runs when the caller names no limit.
const DefaultListLimit = 100

// ListFilter narrows List. Owner is enforced by the caller of List (the
// serve routes pass the principal's name unless it is the operator); an
// empty Owner lists every record. Status is one of the closed set or empty.
type ListFilter struct {
	Owner  string
	Status string
	Limit  int
}

// Restart re-runs finished run id as a new record owned by the original
// owner and linked both ways (RestartedFrom on the new record, RestartedBy
// on the old). The request the run was created from is validated again
// against the current configuration and checkouts, so a workspace that no
// longer exists or a checkout that was deleted is refused rather than
// guessed around. A run whose saved state is not terminal is ErrLive;
// created is false when req.IdempotencyKey names an earlier restart.
func (s *Service) Restart(launchCtx context.Context, caller Caller, id string, req api.RunRestartRequest) (api.ContainedRun, bool, error) {
	if !s.Configured() {
		return api.ContainedRun{}, false, ErrNotConfigured
	}
	rec, err := s.Store.Get(id)
	if err != nil {
		return api.ContainedRun{}, false, err
	}
	if !runstate.Terminal(rec.Status) {
		return api.ContainedRun{}, false, fmt.Errorf("%w (run %s is %s)", ErrLive, id, rec.Status)
	}
	var old detail
	_ = json.Unmarshal(rec.Detail, &old)
	if old.Request == nil {
		return api.ContainedRun{}, false, ErrNoRequest
	}
	// The new run is validated as its owner: the operator restarting a
	// project's run does not widen what the run may touch.
	p, err := s.validate(Caller{Name: rec.Owner, Operator: caller.Operator}, *old.Request)
	if err != nil {
		return api.ContainedRun{}, false, err
	}
	d := s.detailFor(p, *old.Request)
	d.RestartedFrom, d.RestartReason = id, req.Reason
	run, created, err := s.accept(launchCtx, rec.Owner, req.IdempotencyKey, p, d)
	if err != nil || !created {
		return run, created, err
	}
	// Link back. The old record is terminal; its status does not change,
	// so the store permits the detail update.
	_, _ = s.Store.Update(id, func(r *runstate.Record) error {
		var od detail
		_ = json.Unmarshal(r.Detail, &od)
		od.RestartedBy = run.ID
		raw, err := json.Marshal(od)
		r.Detail = raw
		return err
	})
	run.RestartedFrom = id
	return run, true, nil
}

// List is the saved records matching f, newest first, without the command
// output (the record route carries it).
func (s *Service) List(f ListFilter) (api.ContainedRunList, error) {
	if f.Status != "" && !runstate.Valid(f.Status) {
		return api.ContainedRunList{}, &RequestError{fmt.Sprintf("status %q is not a run status", f.Status)}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	recs, err := s.Store.List()
	if err != nil {
		return api.ContainedRunList{}, err
	}
	out := api.ContainedRunList{Runs: []api.ContainedRun{}}
	for i := len(recs) - 1; i >= 0 && len(out.Runs) < limit; i-- {
		rec := recs[i]
		if (f.Owner != "" && rec.Owner != f.Owner) || (f.Status != "" && rec.Status != f.Status) {
			continue
		}
		run := toWire(rec)
		run.Stdout, run.Stderr, run.StdoutTruncatedAt, run.StderrTruncatedAt = "", "", nil, nil
		out.Runs = append(out.Runs, run)
	}
	return out, nil
}

// History is every status run id passed through, oldest first.
func (s *Service) History(id string) (api.RunHistory, error) {
	entries, err := s.Store.History(id)
	if err != nil {
		return api.RunHistory{}, err
	}
	out := api.RunHistory{Run: id, Transitions: make([]api.RunTransition, 0, len(entries))}
	for _, e := range entries {
		out.Transitions = append(out.Transitions, api.RunTransition{At: e.At, Status: e.Status, Reason: e.Reason})
	}
	return out, nil
}
