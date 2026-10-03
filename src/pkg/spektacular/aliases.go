package spektacular

import "github.com/hivecommons/hive/pkg/planengine"

// The engine-neutral types live in pkg/planengine (ADR-0021). These aliases
// keep every existing caller of pkg/spektacular compiling unchanged.
type (
	DocumentStatus  = planengine.DocumentStatus
	ArtifactStatus  = planengine.ArtifactStatus
	PlanTask        = planengine.PlanTask
	Plan            = planengine.Plan
	NotFoundError   = planengine.NotFoundError
	VerbError       = planengine.VerbError
	ContractError   = planengine.ContractError
	WorkDirError    = planengine.WorkDirError
	PlanImportError = planengine.PlanImportError
)

const (
	DocumentDraft      = planengine.DocumentDraft
	DocumentFinal      = planengine.DocumentFinal
	DocumentStale      = planengine.DocumentStale
	DocumentSuperseded = planengine.DocumentSuperseded
	DocumentArchived   = planengine.DocumentArchived
)
