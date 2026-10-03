package planengine

import (
	"fmt"
	"strings"
)

// The error texts keep their "spektacular" prefix so messages stay
// byte-identical while Spektacular is the only engine.

// NotFoundError is returned when the status verb reports that the artifact
// does not exist (error code artifact_not_found). Callers distinguish it from
// transport failures because it is a signal about the document (it may have
// been replaced by a new one), not about the CLI.
type NotFoundError struct {
	Kind    string
	Name    string
	Message string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("spektacular %s %q not found: %s", e.Kind, e.Name, e.Message)
}

// VerbError is a JSON error envelope the CLI printed for a reason other than
// a missing artifact.
type VerbError struct {
	Kind    string
	Name    string
	Code    string
	Message string
}

func (e *VerbError) Error() string {
	return fmt.Sprintf("spektacular %s %q: %s (%s)", e.Kind, e.Name, e.Message, e.Code)
}

// WorkDirError means the stage runner could not resolve a repository checkout
// for a run. It is typed so callers and tests can distinguish an intentional
// park from a CLI/status failure.
type WorkDirError struct {
	RunKey string
	Stage  string
	Repo   string
}

func (e *WorkDirError) Error() string {
	parts := []string{"spektacular: no repo workdir resolved"}
	if e.RunKey != "" {
		parts = append(parts, "run="+e.RunKey)
	}
	if e.Stage != "" {
		parts = append(parts, "stage="+e.Stage)
	}
	if e.Repo != "" {
		parts = append(parts, "repo="+e.Repo)
	}
	return strings.Join(parts, " ")
}

// PlanImportError means a final plan could not be turned into the run's task
// list: the plan export failed or the registry rejected the import. It is a
// fact about the plan (or the hub's configuration), not a transient poll
// failure, so the runner parks the stage instead of retrying every poll.
type PlanImportError struct {
	RunKey   string
	Artifact string
	Err      error
}

func (e *PlanImportError) Error() string {
	return fmt.Sprintf("spektacular: plan %q for run %q could not be imported: %v", e.Artifact, e.RunKey, e.Err)
}

func (e *PlanImportError) Unwrap() error { return e.Err }

// ContractError means the CLI produced output that does not match the #8301
// shape (unparseable JSON, unknown document_status, wrong kind or name).
type ContractError struct {
	Kind   string
	Name   string
	Reason string
	Err    error
}

func (e *ContractError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("spektacular %s %q: contract violation: %s: %v", e.Kind, e.Name, e.Reason, e.Err)
	}
	return fmt.Sprintf("spektacular %s %q: contract violation: %s", e.Kind, e.Name, e.Reason)
}

func (e *ContractError) Unwrap() error { return e.Err }
