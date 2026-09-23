package spektacular

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/escalate"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// Lease stage names the runner understands. They mirror the dashboard's
// lease registry (StageSpec / StagePlan / StageImplement) without importing
// it, so this package stays free of the dashboard.
const (
	StageSpec      = "spec"
	StagePlan      = "plan"
	StageImplement = "implement"
)

// Stage is one active lease stage as the registry exposes it to the runner.
type Stage struct {
	// RunKey is the stable run identity: Spektacular's bare artifact name
	// (the workflow's data.name), as the registry spells it.
	RunKey string
	// Artifact is the bare Spektacular artifact name to poll (ArtifactKey of
	// the run key unless the registry knows a different binding).
	Artifact string
	// Stage is the lease stage (spec, plan, implement).
	Stage string
	// Identity and TaskID address the lease in the registry.
	Identity string
	TaskID   string
	// Key is the registry's canonical work-item key for the lease, used to
	// keep receipts on the same timeline journey as stage_completed events.
	Key string
	// Repo is the repository the run targets.
	Repo string
	// Gen is the lease generation; a change means a retry or advance happened.
	Gen uint64
	// ExpiresAt is when the lease lapses without renewal. It is Hive's own
	// lease clock and the only input to the stale-stage decision; the status
	// document's updated_at is never consulted.
	ExpiresAt time.Time
}

// Registry is what the runner needs from the lease registry. The dashboard
// implements it over ContributeWSHub; tests use an in-memory fake.
type Registry interface {
	// ActiveStages lists every lease that carries a stage.
	ActiveStages(now time.Time) ([]Stage, error)
	// Advance records receipt, imports plan when non-nil (only for a final
	// plan), and advances the lease to the next stage.
	Advance(ctx context.Context, st Stage, status ArtifactStatus, receipt outputschema.StageReceipt, plan *Plan, now time.Time) error
	// Retry mints a new generation of the same stage.
	Retry(ctx context.Context, st Stage, now time.Time) error
	// Refuse records that the runner will not advance st for reason (stale
	// plan, replaced document) so an operator can see why the run is parked.
	Refuse(st Stage, reason string)
}

// Refusal reasons recorded through Registry.Refuse.
const (
	// RefuseStalePlan: document_status went from final back to draft under the
	// same name, so a previously final artifact is being revised.
	RefuseStalePlan = "stale_plan"
	// RefuseReplacedDocument: the artifact the lease is bound to no longer
	// exists after having been observed, so a new document with a new name has
	// most likely replaced it. The lease needs an explicit reset.
	RefuseReplacedDocument = "replaced_document"
)

// Runner polls Spektacular for every active stage lease and drives the lease
// registry. All time comes from the caller (Tick's now), so tests never sleep.
type Runner struct {
	// Exec is the only path to the outside world.
	Exec ExecFunc
	// Poll is the minimum interval between two status calls for one stage.
	Poll time.Duration
	// MaxRetries bounds how many generations one stage may burn before an
	// escalation is raised. Zero means the config default.
	MaxRetries int
	// Registry is the lease registry the runner drives.
	Registry Registry
	// Escalate receives the decision event when a stage exhausts its budget.
	Escalate func(escalate.Event)
	// Logger receives operational logs; nil means slog.Default.
	Logger *slog.Logger

	mu     sync.Mutex
	stages map[string]*stageState
}

// stageState is the runner's memory of one run stage across ticks.
type stageState struct {
	gen        uint64
	lastPolled time.Time
	lastStatus DocumentStatus
	seenFinal  bool
	advanced   bool
	expiries   int
	escalated  bool
	refused    bool
	// seeded marks a successor entry created by an advance before the
	// registry has listed that stage. It keeps the poll pacing across the
	// stage change and is exempt from the end-of-tick prune until the stage
	// is first observed live.
	seeded bool
}

// TickResult counts what one Tick did.
type TickResult struct {
	Polled    int
	Advanced  int
	Retried   int
	Escalated int
	Refused   int
	Errors    int
}

// DefaultMaxRetries mirrors config.DefaultMaxStageRetries without importing
// the config package.
const DefaultMaxRetries = 2

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) maxRetries() int {
	if r.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return r.MaxRetries
}

func stageKey(st Stage) string { return st.RunKey + "\x1f" + st.Stage }

// kindForStage maps a lease stage to the artifact kind whose status decides
// it. The implement stage has no Spektacular document: its completion is the
// existing hold-gated PR flow, so the runner leaves it alone.
func kindForStage(stage string) (string, bool) {
	switch stage {
	case StageSpec:
		return KindSpec, true
	case StagePlan:
		return KindPlan, true
	}
	return "", false
}

// nextStage mirrors the registry's ordering; the registry re-validates.
func nextStage(stage string) string {
	switch stage {
	case StageSpec:
		return StagePlan
	case StagePlan:
		return StageImplement
	}
	return ""
}

// Tick polls every active stage once (respecting Poll), advancing, retrying,
// refusing, or escalating as the status dictates. It is safe to call from a
// single goroutine at a time; Run does so on a ticker.
func (r *Runner) Tick(ctx context.Context, now time.Time) TickResult {
	var res TickResult
	if r == nil || r.Registry == nil {
		return res
	}
	stages, err := r.Registry.ActiveStages(now)
	if err != nil {
		r.logger().Warn("[spektacular] listing active stages failed", "error", err)
		res.Errors++
		return res
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stages == nil {
		r.stages = make(map[string]*stageState)
	}
	live := make(map[string]bool, len(stages))
	for _, st := range stages {
		key := stageKey(st)
		live[key] = true
		state := r.stages[key]
		if state == nil {
			state = &stageState{gen: st.Gen}
			r.stages[key] = state
		}
		if state.gen != st.Gen || state.seeded {
			// A new generation (a retry, or the successor stage of an advance):
			// forget the per-generation observations but keep the budget, any
			// terminal decision, and the poll pacing. lastPolled is deliberately
			// NOT reset, so a generation change never earns an extra status call
			// and a second tick at the same instant is a no-op. The idempotency
			// key is therefore (runKey, stage, gen): one generation is polled and
			// advanced at most once.
			state.seeded = false
			state.gen = st.Gen
			state.lastStatus = ""
			state.seenFinal = false
			state.advanced = false
			state.refused = false
		}
		r.tickStage(ctx, st, state, now, &res)
	}
	for key, state := range r.stages {
		// A successor seeded during this tick is not in the registry snapshot
		// taken before the advance; it must survive until it is observed.
		if !live[key] && !state.seeded {
			delete(r.stages, key)
		}
	}
	return res
}

func (r *Runner) tickStage(ctx context.Context, st Stage, state *stageState, now time.Time, res *TickResult) {
	if state.escalated || state.refused || state.advanced {
		return
	}
	kind, ok := kindForStage(st.Stage)
	if !ok {
		return
	}
	// The poll interval only paces the status call. Expiry is checked on
	// every tick so a lapsed lease is retried before the registry prunes it,
	// whatever the poll cadence is.
	if state.lastPolled.IsZero() || now.Sub(state.lastPolled) >= r.Poll {
		state.lastPolled = now
		res.Polled++
		status, err := r.Status(ctx, kind, st.Artifact)
		switch {
		case err == nil:
			r.observe(ctx, st, state, status, now, res)
		default:
			var nf *NotFoundError
			if errors.As(err, &nf) && state.lastStatus != "" {
				// Seen before, gone now: a new document has replaced it. Never
				// rebind the lease to whatever appeared; park it for a reset.
				state.refused = true
				res.Refused++
				r.Registry.Refuse(st, RefuseReplacedDocument)
				r.logger().Warn("[spektacular] artifact vanished after being observed; lease needs reset",
					"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "error", err)
				return
			}
			res.Errors++
			r.logger().Warn("[spektacular] status failed", "run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "error", err)
		}
		if state.advanced || state.refused {
			return
		}
	}
	r.expire(ctx, st, state, now, res)
}

func (r *Runner) observe(ctx context.Context, st Stage, state *stageState, status ArtifactStatus, now time.Time, res *TickResult) {
	prev := state.lastStatus
	state.lastStatus = status.DocumentStatus
	if !status.Final() {
		if state.seenFinal || prev == DocumentFinal {
			state.refused = true
			res.Refused++
			r.Registry.Refuse(st, RefuseStalePlan)
			r.logger().Warn("[spektacular] document went final -> draft; refusing to advance",
				"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact)
		}
		return
	}
	state.seenFinal = true
	var plan *Plan
	if st.Stage == StagePlan {
		exported, err := r.ExportPlanWithFallback(ctx, st.Artifact)
		if err != nil {
			res.Errors++
			r.logger().Warn("[spektacular] plan is final but task-list import failed; not advancing",
				"run", st.RunKey, "artifact", st.Artifact, "error", err)
			return
		}
		plan = &exported
	}
	receipt := BuildReceipt(st, status, now)
	if err := r.Registry.Advance(ctx, st, status, receipt, plan, now); err != nil {
		res.Errors++
		r.logger().Warn("[spektacular] advance failed", "run", st.RunKey, "stage", st.Stage, "error", err)
		return
	}
	state.advanced = true
	res.Advanced++
	// Seed the successor stage's pacing from this poll: it is first polled
	// one Poll after the advance, never in the same tick.
	if next := nextStage(st.Stage); next != "" {
		r.stages[stageKey(Stage{RunKey: st.RunKey, Stage: next})] = &stageState{lastPolled: now, seeded: true}
	}
	r.logger().Info("[spektacular] stage advanced on final",
		"run", st.RunKey, "stage", st.Stage, "next", nextStage(st.Stage), "gen", st.Gen)
}

// expire applies the retry budget when the lease has lapsed without final:
// each expiry but the last mints a retry generation; the last raises a
// decision escalation and the runner stops touching the stage.
func (r *Runner) expire(ctx context.Context, st Stage, state *stageState, now time.Time, res *TickResult) {
	if st.ExpiresAt.IsZero() || !now.After(st.ExpiresAt) {
		return
	}
	state.expiries++
	if state.expiries < r.maxRetries() {
		if err := r.Registry.Retry(ctx, st, now); err != nil {
			res.Errors++
			r.logger().Warn("[spektacular] retry failed", "run", st.RunKey, "stage", st.Stage, "error", err)
			return
		}
		res.Retried++
		r.logger().Info("[spektacular] lease expired without final; retry generation minted",
			"run", st.RunKey, "stage", st.Stage, "attempt", state.expiries+1, "budget", r.maxRetries())
		return
	}
	state.escalated = true
	res.Escalated++
	ev := escalate.Event{
		Severity: escalate.SeverityDecision,
		RunKey:   st.RunKey,
		Stage:    st.Stage,
		Artifact: st.Artifact,
		Gen:      st.Gen,
		Attempts: state.expiries,
		Reason:   fmt.Sprintf("lease expired %d times without document_status final (budget %d)", state.expiries, r.maxRetries()),
		At:       now,
	}
	r.logger().Warn("[spektacular] retry budget exhausted; escalating", "event", ev.String())
	if r.Escalate != nil {
		r.Escalate(ev)
	}
}

// Run ticks on Poll until ctx is done. now supplies the clock so callers can
// inject one; nil means time.Now.
func (r *Runner) Run(ctx context.Context, now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	interval := r.Poll
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Tick(ctx, now())
		}
	}
}
