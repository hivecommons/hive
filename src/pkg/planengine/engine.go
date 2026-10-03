// Package planengine is the engine-neutral planning-engine boundary for
// Project Inception runs (ADR-0021, hivecommons/hive#10293). It holds the
// Engine interface, the name-keyed engine registry, and the document types an
// engine reports. It must not import pkg/dashboard or pkg/spektacular.
package planengine

import "context"

// Artifact kinds a stage's document can have. They are the neutral kind the
// observer asks an engine about, not a CLI spelling.
const (
	KindSpec = "spec"
	KindPlan = "plan"
)

// Engine observes planning documents. It never sees lease identity, task IDs,
// or generations, and it has no method that changes Hive state.
type Engine interface {
	// Name is the registry name and the receipt Engine.Name ("spektacular").
	Name() string
	// ContractRevision is stamped on every receipt ("spektacular-status/v1").
	ContractRevision() string
	// Probe reports presence/version for the dashboard status card.
	Probe(ctx context.Context) (ProbeResult, error)
	// Status returns the document status of artifact (kind = "spec"|"plan")
	// in dir, the stage's resolved WorkDir. Missing artifact → *NotFoundError.
	Status(ctx context.Context, dir, kind, artifact string) (ArtifactStatus, error)
	// ResolveArtifact maps Hive's run slug to the engine's artifact name.
	// Missing → *NotFoundError.
	ResolveArtifact(ctx context.Context, dir, kind, slug string) (string, error)
	// ExportPlan returns the structured task list of a FINAL plan, including
	// any engine-internal fallbacks.
	ExportPlan(ctx context.Context, dir, artifact string) (Plan, error)
	// ReadSpec returns the body of a FINAL spec for the checkpoint summary.
	ReadSpec(ctx context.Context, dir, artifact string) (string, error)
}

// ProbeResult is what Engine.Probe reports for the dashboard status card.
type ProbeResult struct {
	Present bool
	Version string
	Binary  string
}

// ArtifactNamer is implemented by an engine whose artifacts are not named by
// the run key itself. The observer polls the run key verbatim otherwise.
type ArtifactNamer interface {
	ArtifactName(runKey string) string
}

// VersionedEngine is implemented by an engine that reports the version
// stamped on the receipt's Engine.Version. Without it the receipt records the
// engine name.
type VersionedEngine interface {
	EngineVersion() string
}

// artifactName is the name the observer polls runKey under.
func artifactName(engine Engine, runKey string) string {
	if namer, ok := engine.(ArtifactNamer); ok && namer != nil {
		return namer.ArtifactName(runKey)
	}
	return runKey
}

// engineVersion is the version the receipt stamps for engine.
func engineVersion(engine Engine) string {
	if versioned, ok := engine.(VersionedEngine); ok && versioned != nil {
		if v := versioned.EngineVersion(); v != "" {
			return v
		}
	}
	return engine.Name()
}
