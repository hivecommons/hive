package spektacular

import "github.com/hivecommons/hive/pkg/planengine"

// The engine-neutral types live in pkg/planengine (ADR-0022). These aliases
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

// The stage observer and its lease plumbing live in pkg/planengine too
// (ADR-0022); these aliases keep the boot wiring and the dashboard's
// integration spelled the way they always were.
type (
	Stage         = planengine.Stage
	Registry      = planengine.Registry
	LeaseRegistry = planengine.LeaseRegistry
	TickResult    = planengine.TickResult
)

const (
	DocumentDraft      = planengine.DocumentDraft
	DocumentFinal      = planengine.DocumentFinal
	DocumentStale      = planengine.DocumentStale
	DocumentSuperseded = planengine.DocumentSuperseded
	DocumentArchived   = planengine.DocumentArchived
)

// Lease stage names.
const (
	StageSpec      = planengine.StageSpec
	StagePlan      = planengine.StagePlan
	StageImplement = planengine.StageImplement
)

// Refusal reasons recorded through Registry.Refuse.
const (
	RefuseStalePlan        = planengine.RefuseStalePlan
	RefuseReplacedDocument = planengine.RefuseReplacedDocument
	RefuseArchivedDocument = planengine.RefuseArchivedDocument
	RefusePlanImportFailed = planengine.RefusePlanImportFailed
	RefuseMissingWorkDir   = planengine.RefuseMissingWorkDir
)

// Attribute keys exchanged with the lease registry. pkg/dashboard asserts
// that its own spellings match these.
const (
	AttrRunKey         = planengine.AttrRunKey
	AttrStage          = planengine.AttrStage
	AttrGen            = planengine.AttrGen
	AttrReceipt        = planengine.AttrReceipt
	AttrArtifact       = planengine.AttrArtifact
	AttrArtifactBody   = planengine.AttrArtifactBody
	AttrDocumentStatus = planengine.AttrDocumentStatus
	AttrCurrentStep    = planengine.AttrCurrentStep
	AttrReason         = planengine.AttrReason
)

// RenderTaskList turns an exported plan into the task-list text Hive's
// planner admits. The rendering is Hive-owned (ADR-0022); this is the
// spelling existing callers of this package use.
func RenderTaskList(plan Plan) string { return planengine.RenderTaskList(plan) }
