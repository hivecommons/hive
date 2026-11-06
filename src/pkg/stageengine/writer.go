package stageengine

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageWriter is the ONLY code that adds or removes a stage/* label.
// Implementations wrap stagelabels.Enforce (fresh read, removes before adds,
// kind clamp). The admission test allows exactly one implementation.
type StageWriter interface {
	Apply(ctx context.Context, ref Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error)
}

const stageFlapWindow = 15 * time.Minute

type stageWriteTransition struct {
	from, to stagelabels.Stage
	head     string
	reason   string
	at       time.Time
}

// suppressStageFlap is the final guard before observing or writing a stage.
// It runs under the reconciler's per-Ref lock, keeping the Store and forge on
// the same side of a suppressed reversal. Only successful label writes arm
// it; no-op sweeps and suppressed attempts never slide the window forward.
// PR heads are required: issue activity cannot be identified by a head SHA.
func (r *reconciler) suppressStageFlap(f Facts, d Decision) (stageWriteTransition, bool) {
	key := f.Ref.Key()
	v, ok := r.writes.Load(key)
	if !ok {
		return stageWriteTransition{}, false
	}
	last := v.(stageWriteTransition)
	if f.PR == nil || f.PR.HeadSHA == "" || f.PR.HeadSHA != last.head || r.opts.Now().Sub(last.at) >= stageFlapWindow {
		r.writes.Delete(key)
		return stageWriteTransition{}, false
	}
	// Closing/merging must never be delayed by a display-label guard.
	if d.Terminal {
		r.writes.Delete(key)
		return stageWriteTransition{}, false
	}
	if d.Stage != last.from || last.to == d.Stage {
		return stageWriteTransition{}, false
	}
	r.opts.Logger.Warn("stage flap suppressed", "ref", f.Ref.String(), "head", last.head,
		"from", last.to.String(), "to", d.Stage.String(), "reason", d.Reason)
	return last, true
}

func (r *reconciler) rememberStageWrite(f Facts, from, to stagelabels.Stage, reason string) {
	if f.PR == nil || f.PR.HeadSHA == "" || from == stagelabels.None || to == stagelabels.None {
		r.writes.Delete(f.Ref.Key())
		return
	}
	r.writes.Store(f.Ref.Key(), stageWriteTransition{from: from, to: to, head: f.PR.HeadSHA, reason: reason, at: r.opts.Now()})
}
