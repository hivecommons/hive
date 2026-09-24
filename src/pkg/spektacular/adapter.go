package spektacular

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalate"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// Attribute keys the adapter passes to the lease registry. pkg/dashboard
// reads the same spellings without importing this package (the dashboard
// import ratchet keeps it free of spektacular), so the two lists must match;
// pkg/dashboard's tests assert that they do.
const (
	AttrRunKey         = "run_key"
	AttrStage          = "stage"
	AttrGen            = "gen"
	AttrReceipt        = "receipt"
	AttrArtifact       = "artifact"
	AttrDocumentStatus = "document_status"
	AttrReason         = "reason"
	AttrSeverity       = "severity"
	AttrAttempts       = "attempts"
)

// LeaseRegistry is the primitives-only surface the runner needs from the
// dashboard's lease registry. *dashboard.Server satisfies it; the dashboard
// never imports this package (its internal-import ratchet), so the interface
// lives here and the concrete registry is passed in at boot.
type LeaseRegistry interface {
	// VisitActiveStageLeases calls visit for every lease that carries a
	// stage. Every argument is a primitive so the registry side needs no type
	// from this package (an unnamed func type, so a method spelled the same
	// way on the dashboard satisfies the interface).
	VisitActiveStageLeases(visit func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time)) error
	// AdvanceStageLease persists the receipt for the lease's current
	// generation and advances the lease to stage `to`. attrs carry the
	// Attr* keys (run key, receipt digest, artifact, document status).
	AdvanceStageLease(identity, taskID, to string, now time.Time, receipt []byte, attrs map[string]string) error
	// RetryStageLease mints a new generation of the lease's current stage.
	RetryStageLease(identity, taskID string, now time.Time) error
	// RefuseStageLease records that the runner will not advance the lease.
	RefuseStageLease(taskID string, attrs map[string]string)
	// ResolveRunStageWorkDir returns the repo checkout or per-stage worktree that
	// owns the Spektacular project for this stage. An empty result parks the lease.
	ResolveRunStageWorkDir(runKey, stage, identity, repo string, gen uint64) (string, error)
	// EscalateStageLease records a decision escalation for the run.
	EscalateStageLease(runKey string, at time.Time, attrs map[string]string)
	// ImportRunPlan admits taskList (the planner's task-list text) as the
	// run's DRAFT plan.
	ImportRunPlan(runKey, repo, taskList string) error
}

// leaseAdapter implements Registry over a LeaseRegistry.
type leaseAdapter struct {
	reg LeaseRegistry
}

// NewLeaseRegistryAdapter wraps the dashboard-side registry as the Registry
// the poll loop drives.
func NewLeaseRegistryAdapter(reg LeaseRegistry) Registry {
	return &leaseAdapter{reg: reg}
}

func (a *leaseAdapter) ActiveStages(time.Time) ([]Stage, error) {
	out := []Stage{}
	err := a.reg.VisitActiveStageLeases(func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time) {
		workDir, _ := a.reg.ResolveRunStageWorkDir(runKey, stage, identity, repo, gen)
		// The registry's run key may still be spelled as a file address
		// (`<name>.md`, `<name>/plan.md`); the artifact Spektacular is asked
		// about is always the bare name.
		out = append(out, Stage{
			RunKey: runKey, Artifact: ArtifactKey(runKey), Stage: stage, Key: key,
			Identity: identity, TaskID: taskID, Repo: repo, WorkDir: workDir, Gen: gen, ExpiresAt: expiresAt,
		})
	})
	if err != nil {
		return nil, err
	}
	// The registry iterates a map; give the runner a stable order so ticks
	// are reproducible.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		if out[i].Stage != out[j].Stage {
			return out[i].Stage < out[j].Stage
		}
		return out[i].Gen < out[j].Gen
	})
	return out, nil
}

func (a *leaseAdapter) Advance(_ context.Context, st Stage, status ArtifactStatus, receipt outputschema.StageReceipt, plan *Plan, now time.Time) error {
	if plan != nil {
		if err := a.reg.ImportRunPlan(st.RunKey, st.Repo, RenderTaskList(*plan)); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding receipt: %w", err)
	}
	return a.reg.AdvanceStageLease(st.Identity, st.TaskID, nextStage(st.Stage), now, data, map[string]string{
		AttrRunKey:         st.RunKey,
		AttrStage:          st.Stage,
		AttrGen:            strconv.FormatUint(st.Gen, 10),
		AttrReceipt:        receipt.OutputDigest,
		AttrArtifact:       status.JoinKey(),
		AttrDocumentStatus: string(status.DocumentStatus),
	})
}

func (a *leaseAdapter) Retry(_ context.Context, st Stage, now time.Time) error {
	return a.reg.RetryStageLease(st.Identity, st.TaskID, now)
}

func (a *leaseAdapter) Refuse(st Stage, reason string, status *ArtifactStatus) {
	attrs := map[string]string{
		AttrRunKey: st.RunKey,
		AttrStage:  st.Stage,
		AttrGen:    strconv.FormatUint(st.Gen, 10),
		AttrReason: reason,
	}
	if status != nil {
		attrs[AttrArtifact] = status.JoinKey()
		attrs[AttrDocumentStatus] = string(status.DocumentStatus)
	}
	a.reg.RefuseStageLease(st.TaskID, attrs)
}

// EscalationSink returns the Escalate callback that records decision events
// on the registry.
func EscalationSink(reg LeaseRegistry) func(escalate.Event) {
	return func(ev escalate.Event) {
		reg.EscalateStageLease(ev.RunKey, ev.At, map[string]string{
			AttrStage:    ev.Stage,
			AttrGen:      strconv.FormatUint(ev.Gen, 10),
			AttrAttempts: strconv.Itoa(ev.Attempts),
			AttrSeverity: string(ev.Severity),
			AttrReason:   ev.Reason,
		})
	}
}

// HubRunner is the boot-time shape of the runner: it satisfies the
// dashboard's StageRunner interface (Tick without a result) so the contribute
// hub's cleanup loop can drive it without knowing this package.
type HubRunner struct {
	runner *Runner
}

// NewHubRunner builds the production runner from config against the
// dashboard's lease registry.
func NewHubRunner(cfg config.RunsConfig, reg LeaseRegistry, logger *slog.Logger) *HubRunner {
	return &HubRunner{runner: &Runner{
		Exec:       BinaryExec(cfg.Spektacular.BinaryOrDefault()),
		Poll:       cfg.Spektacular.PollInterval(),
		MaxRetries: cfg.MaxStageRetriesOrDefault(),
		Registry:   NewLeaseRegistryAdapter(reg),
		Escalate:   EscalationSink(reg),
		Logger:     logger,
	}}
}

// Tick runs one poll; the result is logged by the runner itself.
func (h *HubRunner) Tick(ctx context.Context, now time.Time) {
	if h == nil || h.runner == nil {
		return
	}
	h.runner.Tick(ctx, now)
}

// Runner exposes the underlying poll loop (tests and diagnostics).
func (h *HubRunner) Runner() *Runner {
	if h == nil {
		return nil
	}
	return h.runner
}
