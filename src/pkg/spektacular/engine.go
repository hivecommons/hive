package spektacular

import (
	"context"

	"github.com/hivecommons/hive/pkg/planengine"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Receipt constants. They are the engine's own spellings; the stage observer
// (pkg/planengine) stamps them on every receipt it builds for this engine.
const (
	// ContractRevision names the status contract the receipt was produced under.
	ContractRevision = "spektacular-status/v1"
	// EngineName identifies Spektacular as the producing engine on the receipt.
	EngineName = "spektacular"
	// engineVersion is the CLI version the runner assumes; the status verb does
	// not report one, so it names the Spektacular PR that shipped the contract.
	engineVersion = "spektacular-pr-45"
)

// Engine answers the stage observer's questions about Spektacular documents
// through the CLI. It never sees a lease: identity, task id and generation
// stay on the Hive side of the boundary (ADR-0021).
type Engine struct {
	binary string
	runner *Runner
}

// NewEngine returns the Spektacular engine driving exec. binary is the CLI
// name Probe reports; an empty one probes the default binary.
func NewEngine(binary string, exec ExecFunc) *Engine {
	return &Engine{binary: binary, runner: &Runner{Exec: exec}}
}

// NewBinaryEngine returns the production engine shelling out to binary.
func NewBinaryEngine(binary string) *Engine { return NewEngine(binary, BinaryExec(binary)) }

// Name is the registry name and the receipt's Engine.Name.
func (e *Engine) Name() string { return EngineName }

// ContractRevision is the status contract every receipt is stamped with.
func (e *Engine) ContractRevision() string { return ContractRevision }

// EngineVersion is the receipt's Engine.Version.
func (e *Engine) EngineVersion() string { return engineVersion }

// Probe reports presence and version for the dashboard status card.
func (e *Engine) Probe(ctx context.Context) (planengine.ProbeResult, error) {
	result, err := Probe(ctx, e.binary)
	return planengine.ProbeResult(result), err
}

// Status runs `spektacular <kind> status <artifact>` in dir.
func (e *Engine) Status(ctx context.Context, dir, kind, artifact string) (ArtifactStatus, error) {
	return e.runner.statusInDir(ctx, dir, kind, artifact)
}

// ExportPlan exports a final plan, including the on-disk fallbacks.
func (e *Engine) ExportPlan(ctx context.Context, dir, artifact string) (Plan, error) {
	return e.runner.exportPlanWithFallbackInDir(ctx, dir, artifact)
}

// ReadSpec reads the body of a final spec for the checkpoint summary.
func (e *Engine) ReadSpec(ctx context.Context, dir, artifact string) (string, error) {
	return e.runner.readSpecInDir(ctx, dir, artifact)
}

// ResolveArtifact maps Hive's run slug to the artifact name the store holds.
func (e *Engine) ResolveArtifact(ctx context.Context, dir, kind, slug string) (string, error) {
	return e.runner.ResolveArtifact(ctx, dir, kind, slug)
}

// ArtifactName is the Spektacular artifact a run key is polled under: a
// worksource run key becomes the CLI-safe slug, every other spelling reduces
// to the bare name.
func (e *Engine) ArtifactName(runKey string) string {
	if ref, ok := worksource.ParseKey(runKey); ok && ref.Repo != "" {
		return RunArtifactName(runKey)
	}
	return ArtifactKey(runKey)
}

var (
	_ planengine.Engine          = (*Engine)(nil)
	_ planengine.ArtifactNamer   = (*Engine)(nil)
	_ planengine.VersionedEngine = (*Engine)(nil)
)
