package stageengine

import (
	"context"
	"encoding/json"
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
	evidence string
	at       time.Time
}

// stageWriteEvidence snapshots inputs that can legitimately change a stage on
// the same head. Exclude previous stage/display outputs and clock-driven soak
// progress: those are not new evidence and must not disarm an idle-flap guard.
// Serialize a copy so later mutations of caller-owned facts cannot change the
// evidence remembered for a successful write.
func stageWriteEvidence(f Facts) string {
	if f.PR == nil {
		return ""
	}
	pr := *f.PR
	pr.SoakElapsed = 0
	pr.Lanes = append([]LaneFact(nil), pr.Lanes...)
	for i := range pr.Lanes {
		pr.Lanes[i].Reason = ""
	}
	if pr.Pass != nil {
		pass := *pr.Pass
		pass.Prior = nil
		pass.Route = nil
		pass.Stalled = false
		pr.Pass = &pass
	}
	b, _ := json.Marshal(pr) // PRFacts contains only JSON-serializable values.
	return string(b)
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
	if f.PR == nil || f.PR.HeadSHA == "" || f.PR.HeadSHA != last.head || stageWriteEvidence(f) != last.evidence || r.opts.Now().Sub(last.at) >= stageFlapWindow {
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
	r.writes.Store(f.Ref.Key(), stageWriteTransition{from: from, to: to, head: f.PR.HeadSHA, reason: reason, evidence: stageWriteEvidence(f), at: r.opts.Now()})
}
