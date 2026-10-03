package planengine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Attribute keys the adapter passes to the lease registry. pkg/dashboard
// reads the same spellings without importing this package (the dashboard
// import ratchet keeps it free of the engine packages), so the two lists must
// match; pkg/dashboard's tests assert that they do.
const (
	AttrRunKey         = "run_key"
	AttrStage          = "stage"
	AttrGen            = "gen"
	AttrReceipt        = "receipt"
	AttrArtifact       = "artifact"
	AttrArtifactBody   = "artifact_body"
	AttrDocumentStatus = "document_status"
	AttrCurrentStep    = "current_step"
	AttrReason         = "reason"
)

// LeaseRegistry is the primitives-only surface the observer needs from the
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
	// RefuseStageLease records that the runner will not advance the lease.
	RefuseStageLease(taskID string, attrs map[string]string)
	// RecordStageProgress records non-terminal activity for the run timeline.
	RecordStageProgress(runKey, taskID string, attrs map[string]string, at time.Time)
	// ResolveRunStageWorkDir returns the repo checkout or per-stage worktree that
	// owns the engine's project for this stage. An empty result parks the lease.
	ResolveRunStageWorkDir(runKey, stage, identity, repo string, gen uint64) (string, error)
	// ImportRunPlan admits taskList (the planner's task-list text) as the
	// run's DRAFT plan.
	ImportRunPlan(runKey, repo, taskList string) error
}

// leaseAdapter implements Registry over a LeaseRegistry.
type leaseAdapter struct {
	reg    LeaseRegistry
	engine Engine
}

type pendingStageIdentityRegistry interface {
	IsPendingStageIdentity(identity string) bool
}

// NewLeaseRegistryAdapter wraps the dashboard-side registry as the Registry
// the poll loop drives. engine supplies the artifact name a run key is polled
// under.
func NewLeaseRegistryAdapter(reg LeaseRegistry, engine Engine) Registry {
	return &leaseAdapter{reg: reg, engine: engine}
}

func (a *leaseAdapter) ActiveStages(time.Time) ([]Stage, error) {
	out := []Stage{}
	var workDirErr error
	err := a.reg.VisitActiveStageLeases(func(runKey, key, stage, identity, taskID, repo string, gen uint64, _ time.Time) {
		workDir, wdErr := a.reg.ResolveRunStageWorkDir(runKey, stage, identity, repo, gen)
		if wdErr != nil && workDirErr == nil {
			workDirErr = fmt.Errorf("resolving workdir for run %s stage %s: %w", runKey, stage, wdErr)
		}
		artifact := artifactName(a.engine, runKey)
		relayHeld := false
		if checker, ok := a.reg.(pendingStageIdentityRegistry); ok {
			relayHeld = !checker.IsPendingStageIdentity(identity)
		}
		out = append(out, Stage{
			RunKey: runKey, Artifact: artifact, Stage: stage, Key: key,
			Identity: identity, TaskID: taskID, Repo: repo, WorkDir: workDir, Gen: gen,
			Unclaimed: identity == worksource.RunAdmissionIdentity,
			RelayHeld: relayHeld,
		})
	})
	if err != nil {
		return nil, err
	}
	if workDirErr != nil {
		return nil, workDirErr
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
			return &PlanImportError{RunKey: st.RunKey, Artifact: status.JoinKey(), Err: err}
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
		AttrArtifactBody:   status.Body,
		AttrDocumentStatus: string(status.DocumentStatus),
	})
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

func (a *leaseAdapter) RecordProgress(st Stage, attrs map[string]string, now time.Time) {
	eventAttrs := map[string]string{
		AttrRunKey: st.RunKey,
		AttrStage:  st.Stage,
		AttrGen:    strconv.FormatUint(st.Gen, 10),
	}
	for k, v := range attrs {
		if v != "" {
			eventAttrs[k] = v
		}
	}
	a.reg.RecordStageProgress(st.RunKey, st.TaskID, eventAttrs, now)
}
