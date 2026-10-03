package planengine

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// The log messages keep their "[spektacular]" prefix, like the error texts
// in errors.go, so operator-visible output stays byte-identical while
// Spektacular is the only engine.

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
	// RunKey is the stable run identity: the engine's bare artifact name
	// (Spektacular's workflow data.name), as the registry spells it.
	RunKey string
	// Artifact is the bare artifact name to poll (the engine's name for the
	// run key unless the registry knows a different binding).
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
	// WorkDir is the repo checkout or per-stage worktree that owns the
	// engine's project for this lease. The runner refuses to poll when it is
	// empty so the hub process cwd can never influence status.
	WorkDir string
	// Unclaimed marks the admission lease no relay has taken yet. There is
	// no checkout to poll by construction, so the runner leaves it alone
	// (neither polled nor refused) until a contributor claims the stage.
	Unclaimed bool
	// RelayHeld marks leases currently owned by a contributor relay. Relay
	// progress is reported over the contribute websocket; the hub poller must
	// not try to inspect a hub-side checkout for these leases.
	RelayHeld bool
	// Gen is the lease generation; a change means a retry or advance happened.
	Gen uint64
}

// Registry is what the observer needs from the lease registry. The dashboard
// implements it over ContributeWSHub; tests use an in-memory fake.
//
// There is deliberately no Retry: the retry budget (runs.max_stage_retries) is
// owned by the hub executor, the only component that knows when a generation
// has actually been spent (#9143). The runner only observes documents.
type Registry interface {
	// ActiveStages lists every lease that carries a stage.
	ActiveStages(now time.Time) ([]Stage, error)
	// Advance records receipt, imports plan when non-nil (only for a final
	// plan), and advances the lease to the next stage.
	Advance(ctx context.Context, st Stage, status ArtifactStatus, receipt outputschema.StageReceipt, plan *Plan, now time.Time) error
	// Refuse records that the runner will not advance st for reason (stale
	// plan, replaced or archived document, failed plan import) so an operator
	// can see why the run is parked.
	Refuse(st Stage, reason string, status *ArtifactStatus)
}

type progressRegistry interface {
	RecordProgress(st Stage, attrs map[string]string, now time.Time)
}

// Refusal reasons recorded through Registry.Refuse.
const (
	// RefuseStalePlan: document_status is stale, or went from final back to
	// draft under the same name, so a previously final artifact is invalid.
	RefuseStalePlan = "stale_plan"
	// RefuseReplacedDocument: the artifact the lease is bound to no longer
	// exists after having been observed, so a new document with a new name has
	// most likely replaced it. The lease needs an explicit reset.
	RefuseReplacedDocument = "replaced_document"
	// RefuseArchivedDocument: the artifact was archived, so it will never go
	// final. The lease needs an explicit reset.
	RefuseArchivedDocument = "archived_document"
	// RefusePlanImportFailed: the plan is final but its task list could not be
	// exported or imported. Retrying every poll cannot fix the plan or the
	// hub's configuration, so the lease is parked for an explicit reset.
	RefusePlanImportFailed = "plan_import_failed"
	// RefuseMissingWorkDir: the registry could not resolve a repo checkout or
	// per-stage worktree for the run, so polling would fall back to the hub cwd.
	RefuseMissingWorkDir = "missing_workdir"
)

// Runner polls the planning engine for every active stage lease and drives
// the lease registry. All time comes from the caller (Tick's now), so tests
// never sleep.
//
// The runner advances on final and refuses stale, replaced or archived
// documents and final plans whose task list cannot be imported. It
// never retries or escalates a stage: a non-final generation is settled by the
// hub executor against runs.max_stage_retries (#9143).
type Runner struct {
	// Engine is the only path to the outside world: it answers questions
	// about documents and never sees the lease.
	Engine Engine
	// Poll is the minimum interval between two status calls for one stage.
	Poll time.Duration
	// Registry is the lease registry the runner drives.
	Registry Registry
	// Logger receives operational logs; nil means slog.Default.
	Logger *slog.Logger

	mu     sync.Mutex
	stages map[string]*stageState
}

// stageState is the runner's memory of one run stage across ticks.
type stageState struct {
	gen             uint64
	identity        string
	lastPolled      time.Time
	lastStatus      DocumentStatus
	artifact        string
	seenFinal       bool
	advanced        bool
	refused         bool
	relaySkipLogged bool
	// seeded marks a successor entry created by an advance before the
	// registry has listed that stage. It keeps the poll pacing across the
	// stage change and is exempt from the end-of-tick prune until the stage
	// is first observed live.
	seeded bool
}

// TickResult counts what one Tick did.
type TickResult struct {
	Polled   int
	Advanced int
	Refused  int
	// Unclaimed counts admission leases still waiting for a relay to take
	// them; they are listed but neither polled nor refused.
	Unclaimed int
	RelayHeld int
	Errors    int
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func stageKey(st Stage) string { return st.RunKey + "\x1f" + st.Stage }

// kindForStage maps a lease stage to the artifact kind whose status decides
// it. The implement stage has no planning document: its completion is the
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

// Tick polls every active stage once (respecting Poll), advancing or refusing
// as the status dictates. It is safe to call from a single goroutine at a
// time; Run does so on a ticker.
func (r *Runner) Tick(ctx context.Context, now time.Time) TickResult {
	var res TickResult
	if r == nil || r.Registry == nil || r.Engine == nil {
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
		if ctx.Err() != nil {
			return res
		}
		key := stageKey(st)
		live[key] = true
		state := r.stages[key]
		if state == nil {
			state = &stageState{gen: st.Gen, identity: st.Identity}
			r.stages[key] = state
		}
		if state.gen != st.Gen || state.seeded {
			// A new generation (a retry, or the successor stage of an advance):
			// forget the per-generation observations but keep any terminal
			// decision and the poll pacing. lastPolled is deliberately NOT
			// reset, so a generation change never earns an extra status call
			// and a second tick at the same instant is a no-op. The idempotency
			// key is therefore (runKey, stage, gen): one generation is polled and
			// advanced at most once.
			state.seeded = false
			state.gen = st.Gen
			// The resolved document id is per generation too: a retry may
			// have created a newer timestamped document, so re-resolve it
			// from the lease's artifact rather than polling the old one.
			state.artifact = ""
			state.lastStatus = ""
			state.seenFinal = false
			state.advanced = false
			state.refused = false
			state.relaySkipLogged = false
		}
		if state.identity != st.Identity {
			// The same generation changed hands (a relay claimed the admission
			// lease, or a reclaim moved it to another relay). Whatever was
			// refused about the previous owner's checkout says nothing about
			// the new owner's, so the stage is eligible to be polled again.
			state.identity = st.Identity
			state.refused = false
			state.relaySkipLogged = false
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
	if state.refused || state.advanced {
		return
	}
	kind, ok := kindForStage(st.Stage)
	if !ok {
		return
	}
	if st.Unclaimed {
		// Admission created the stage but no relay has claimed it, so no
		// checkout exists yet. That is the expected shape of a freshly started
		// run, not a misconfiguration: wait for the claim rather than parking
		// the lease with missing_workdir before anyone could act on it.
		res.Unclaimed++
		return
	}
	if st.RelayHeld {
		if !state.relaySkipLogged {
			r.logger().Info("[spektacular] skipping relay-held stage lease",
				"run", st.RunKey, "stage", st.Stage, "repo", st.Repo, "identity", st.Identity)
			state.relaySkipLogged = true
		}
		res.RelayHeld++
		return
	}
	if !state.lastPolled.IsZero() && now.Sub(state.lastPolled) < r.Poll {
		return
	}
	dir := strings.TrimSpace(st.WorkDir)
	if dir == "" {
		state.refused = true
		res.Refused++
		err := &WorkDirError{RunKey: st.RunKey, Stage: st.Stage, Repo: st.Repo}
		r.Registry.Refuse(st, RefuseMissingWorkDir, nil)
		r.logger().Warn("[spektacular] refusing to poll without a repo workdir",
			"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "repo", st.Repo, "error", err)
		return
	}
	state.lastPolled = now
	res.Polled++
	artifact := strings.TrimSpace(state.artifact)
	if artifact == "" {
		artifact = st.Artifact
	}
	status, err := r.Engine.Status(ctx, dir, kind, artifact)
	if err == nil {
		state.artifact = status.JoinKey()
		r.observe(ctx, st, state, status, now, res)
		return
	}
	var nf *NotFoundError
	if errors.As(err, &nf) && state.lastStatus == "" {
		resolved, resolveErr := r.Engine.ResolveArtifact(ctx, dir, kind, st.Artifact)
		var resolveNF *NotFoundError
		if resolveErr != nil && !errors.As(resolveErr, &resolveNF) {
			r.logger().Warn("[spektacular] resolving run artifact id failed",
				"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "error", resolveErr)
		}
		if resolveErr == nil && resolved != "" && resolved != artifact {
			status, err = r.Engine.Status(ctx, dir, kind, resolved)
			if err == nil {
				state.artifact = status.JoinKey()
				r.logger().Info("[spektacular] resolved run artifact id",
					"run", st.RunKey, "stage", st.Stage, "requested", st.Artifact, "resolved", state.artifact)
				r.recordProgress(st, map[string]string{
					AttrArtifact: status.JoinKey(),
					AttrReason:   "artifact_resolved",
				}, now)
				r.observe(ctx, st, state, status, now, res)
				return
			}
		}
	}
	if errors.As(err, &nf) && state.lastStatus != "" {
		// Seen before, gone now: a new document has replaced it. Never
		// rebind the lease to whatever appeared; park it for a reset.
		state.refused = true
		res.Refused++
		r.Registry.Refuse(st, RefuseReplacedDocument, nil)
		r.logger().Warn("[spektacular] artifact vanished after being observed; lease needs reset",
			"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "error", err)
		return
	}
	res.Errors++
	r.logger().Warn("[spektacular] status failed", "run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "error", err)
}

func (r *Runner) observe(ctx context.Context, st Stage, state *stageState, status ArtifactStatus, now time.Time, res *TickResult) {
	prev := state.lastStatus
	state.lastStatus = status.DocumentStatus
	if prev != status.DocumentStatus || status.CurrentStep != "" {
		r.recordProgress(st, map[string]string{
			AttrArtifact:       status.JoinKey(),
			AttrDocumentStatus: string(status.DocumentStatus),
			AttrCurrentStep:    status.CurrentStep,
			"completed_steps":  strconv.Itoa(len(status.CompletedSteps)),
		}, now)
	}
	if status.DocumentStatus == DocumentStale {
		state.refused = true
		res.Refused++
		r.Registry.Refuse(st, RefuseStalePlan, &status)
		r.logger().Warn("[spektacular] plan reported stale; refusing to advance",
			"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact)
		return
	}

	switch status.DocumentStatus {
	case DocumentSuperseded:
		state.refused = true
		res.Refused++
		r.Registry.Refuse(st, RefuseReplacedDocument, &status)
		r.logger().Warn("[spektacular] document superseded; lease needs reset",
			"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact)
		return
	case DocumentArchived:
		state.refused = true
		res.Refused++
		r.Registry.Refuse(st, RefuseArchivedDocument, &status)
		r.logger().Warn("[spektacular] document archived; lease needs reset",
			"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact)
		return
	}

	if !status.Final() {
		if state.seenFinal || prev == DocumentFinal {
			state.refused = true
			res.Refused++
			r.Registry.Refuse(st, RefuseStalePlan, &status)
			r.logger().Warn("[spektacular] document went final -> draft; refusing to advance",
				"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact)
		}
		return
	}
	state.seenFinal = true
	// Read the artifact under the id Spektacular actually reports (it prefixes
	// a timestamp to the requested slug); the slug alone is not_found.
	artifact := strings.TrimSpace(status.JoinKey())
	if artifact == "" {
		artifact = st.Artifact
	}
	var plan *Plan
	if st.Stage == StagePlan {
		exported, err := r.Engine.ExportPlan(ctx, st.WorkDir, artifact)
		if err != nil {
			r.refusePlanImport(st, state, status, res, &PlanImportError{RunKey: st.RunKey, Artifact: artifact, Err: err})
			return
		}
		plan = &exported
	}
	if st.Stage == StageSpec {
		body, err := r.Engine.ReadSpec(ctx, st.WorkDir, artifact)
		if err != nil {
			r.logger().Warn("[spektacular] spec is final but artifact read failed; advancing without postback body",
				"run", st.RunKey, "artifact", artifact, "error", err)
		} else {
			status.Body = body
		}
	}
	receipt := BuildReceipt(r.Engine, st, status, now)
	if err := r.Registry.Advance(ctx, st, status, receipt, plan, now); err != nil {
		var importErr *PlanImportError
		if errors.As(err, &importErr) {
			r.refusePlanImport(st, state, status, res, err)
			return
		}
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

// refusePlanImport parks a final plan whose task list cannot be imported, so
// the run shows why it is not advancing instead of re-exporting every poll.
func (r *Runner) refusePlanImport(st Stage, state *stageState, status ArtifactStatus, res *TickResult, err error) {
	state.refused = true
	res.Refused++
	r.Registry.Refuse(st, RefusePlanImportFailed, &status)
	r.logger().Warn("[spektacular] plan is final but task-list import failed; refusing to advance",
		"run", st.RunKey, "stage", st.Stage, "artifact", st.Artifact, "error", err)
}

func (r *Runner) recordProgress(st Stage, attrs map[string]string, now time.Time) {
	rec, ok := r.Registry.(progressRegistry)
	if !ok || rec == nil {
		return
	}
	rec.RecordProgress(st, attrs, now)
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
