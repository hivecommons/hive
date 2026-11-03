package scheduler

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/nous"
)

// Executor durably applies Evaluate decisions for Hive-self experiments.
//
// Design: reverts are latched in two places. The experiment ledger
// (nous.ExperimentStore) is the durable record — a persisted Result is final
// and later decisions never re-enable the treatment. Locally, flag subjects are
// disabled through a per-experiment-ID revert deny-list on nous.SelfFlagGate
// that Apply preserves, so the next heartbeat cannot undo the revert. The
// deny-list lives in memory; Restore rebuilds it from the ledger at startup,
// before the first heartbeat is applied. This is additive and leaves the
// replace-on-heartbeat semantics of the gate unchanged for other experiments.
//
// Channel subjects revert through RevertChannel, the release-channel upgrade
// protocol hook supplied by the caller. PR subjects have no local revert; only
// the gate deny-list entry is recorded for them.
type Executor struct {
	Store *nous.ExperimentStore
	Gate  *nous.SelfFlagGate
	// RevertChannel restores the baseline release channel for a channel subject.
	// It must be idempotent; a nil hook makes channel reverts fail.
	RevertChannel func(nous.Experiment) error
	// Now defaults to time.Now and stamps recorded results.
	Now func() time.Time

	// mu serializes ledger read-modify-write transactions, as ExperimentStore requires.
	mu sync.Mutex
}

// ErrNoChannelRevert is returned when a channel subject must be reverted but no
// RevertChannel hook is configured.
var ErrNoChannelRevert = errors.New("nous scheduler: no release-channel revert hook configured")

// Execute applies d to experiment id and returns the persisted result, if any.
// measurements are the evidence stored with a fast-fail or judged result; they
// must satisfy nous.Result validation or the result is not recorded.
//
//   - An experiment that already has a result is latched: the revert is
//     re-applied (idempotently) and the stored result is returned unchanged.
//   - FastFail persists a fast_failed result before reverting, so any revert
//     that is reported is durable. If the write fails, the flag is still turned
//     off in-process as a fail-safe, but no result is reported.
//   - Revert is applied before a completed judgement is recorded; a failed
//     revert records nothing so the caller retries.
//   - At Complete, Judged&&Passed records improved, Judged&&!Passed records
//     regressed (the pre-registered threshold was not met), and an unjudged
//     decision (missing or invalid evidence) records inconclusive.
func (x *Executor) Execute(id string, d Decision, measurements []nous.Measurement) (*nous.Result, error) {
	if x.Store == nil || x.Gate == nil {
		return nil, fmt.Errorf("nous scheduler: executor requires a store and a flag gate")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	ledger, err := x.Store.Load()
	if err != nil {
		return nil, err
	}
	i := -1
	for j := range ledger {
		if ledger[j].ID == id {
			i = j
			break
		}
	}
	if i < 0 {
		return nil, fmt.Errorf("nous scheduler: experiment %q not found", id)
	}
	e := ledger[i]
	if e.Result != nil {
		if err := x.revert(e); err != nil {
			return nil, err
		}
		r := *e.Result
		return &r, nil
	}
	if d.FastFail {
		res, err := x.record(ledger, i, nous.VerdictFastFailed, measurements, d.Reason)
		if err != nil {
			_ = x.revert(e)
			return nil, err
		}
		if err := x.revert(e); err != nil {
			return nil, err
		}
		return res, nil
	}
	if d.Revert {
		if err := x.revert(e); err != nil {
			return nil, err
		}
	}
	if d.Phase != Complete {
		return nil, nil
	}
	verdict := nous.VerdictInconclusive
	if d.Judged && d.Passed {
		verdict = nous.VerdictImproved
	} else if d.Judged {
		verdict = nous.VerdictRegressed
	}
	return x.record(ledger, i, verdict, measurements, d.Reason)
}

// Restore re-applies the local revert for every experiment with a recorded
// result. Call it at startup before applying the first heartbeat assignment.
func (x *Executor) Restore() error {
	if x.Store == nil || x.Gate == nil {
		return fmt.Errorf("nous scheduler: executor requires a store and a flag gate")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	ledger, err := x.Store.Load()
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ledger {
		if e.Result != nil {
			errs = append(errs, x.revert(e))
		}
	}
	return errors.Join(errs...)
}

func (x *Executor) record(ledger []nous.Experiment, i int, verdict string, measurements []nous.Measurement, summary string) (*nous.Result, error) {
	now := time.Now
	if x.Now != nil {
		now = x.Now
	}
	res := nous.Result{RecordedAt: now().UTC(), Verdict: verdict, Measurements: measurements, Summary: summary}
	next := append([]nous.Experiment(nil), ledger...)
	next[i].Result = &res
	if err := x.Store.Save(next); err != nil {
		return nil, fmt.Errorf("nous scheduler: record %s result for %q: %w", verdict, next[i].ID, err)
	}
	return &res, nil
}

func (x *Executor) revert(e nous.Experiment) error {
	x.Gate.Revert(e.ID)
	if e.Subject.Kind != nous.SubjectChannel {
		return nil
	}
	if x.RevertChannel == nil {
		return ErrNoChannelRevert
	}
	if err := x.RevertChannel(e); err != nil {
		return fmt.Errorf("nous scheduler: revert channel for %q: %w", e.ID, err)
	}
	return nil
}
