// Package spektacular is the Hive side of the Spektacular stage runner
// (hivecommons/hive#8303). It shells out to the `spektacular` CLI, parses the
// status verb defined by hivecommons/hive#8301, and advances a run's lease
// stage when the artifact reaches `document_status: final`.
//
// Contract boundary: Hive never opens a Spektacular file. Every fact about an
// artifact arrives through Exec, which is the only seam to the outside world,
// so tests drive the runner with a fake and production wires BinaryExec.
package spektacular

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DocumentStatus is the #8301 `document_status` value.
type DocumentStatus string

const (
	// DocumentDraft: the artifact is still being authored.
	DocumentDraft DocumentStatus = "draft"
	// DocumentFinal: the artifact is complete and the stage may advance.
	DocumentFinal DocumentStatus = "final"
)

// Artifact kinds accepted by the status verb.
const (
	KindSpec = "spec"
	KindPlan = "plan"
)

// Verb names and flags of the Spektacular CLI the runner invokes. The status
// verb is the #8301 contract; the export verb is an assumption recorded in
// docs/spektacular.md until the Spektacular side confirms it.
const (
	verbStatus = "status"
	verbExport = "export"
	flagJSON   = "--json"
)

// Error codes the runner recognises in a JSON error object printed by a
// non-zero exit. Anything else is surfaced as a VerbError.
const errorCodeNotFound = "not_found"

// ArtifactStatus is the parsed #8301 status document.
type ArtifactStatus struct {
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	DocumentStatus DocumentStatus `json:"document_status"`
	CurrentStep    string         `json:"current_step"`
	CompletedSteps []string       `json:"completed_steps"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	ClosedAt       time.Time      `json:"closed_at"`
}

// Final reports whether the artifact has reached document_status final.
func (s ArtifactStatus) Final() bool { return s.DocumentStatus == DocumentFinal }

// PlanTask is one task of a final plan as exported by Spektacular. Ref is the
// plan-local id (T1, T2, ...), DependsOn references other refs, Execution is
// agent_suitable or human_required (empty means agent_suitable).
type PlanTask struct {
	Ref       string   `json:"ref"`
	Title     string   `json:"title"`
	DependsOn []string `json:"depends_on,omitempty"`
	Execution string   `json:"execution,omitempty"`
}

// Plan is the structured plan export the runner hands to Hive's planner so no
// LLM redecomposition happens.
type Plan struct {
	Kind  string     `json:"kind"`
	Name  string     `json:"name"`
	Tasks []PlanTask `json:"tasks"`
}

// ExecFunc runs the spektacular CLI with args and returns its stdout. A
// non-zero exit must be returned as a non-nil error while stdout is still
// returned, so the caller can read the JSON error object the contract promises.
type ExecFunc func(ctx context.Context, args []string) ([]byte, error)

// BinaryExec returns an ExecFunc that runs binary with the given args.
func BinaryExec(binary string) ExecFunc {
	return func(ctx context.Context, args []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			return stdout.Bytes(), fmt.Errorf("spektacular %s: %w (stderr: %s)",
				strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), nil
	}
}

// NotFoundError is returned when the status verb reports that the artifact
// does not exist. Callers distinguish it from transport failures because it
// is a signal about the document (it may have been replaced by a new one),
// not about the CLI.
type NotFoundError struct {
	Kind    string
	Name    string
	Message string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("spektacular %s %q not found: %s", e.Kind, e.Name, e.Message)
}

// VerbError is a JSON error object the CLI printed for a reason other than a
// missing artifact.
type VerbError struct {
	Kind    string
	Name    string
	Code    string
	Message string
}

func (e *VerbError) Error() string {
	return fmt.Sprintf("spektacular %s %q: %s (%s)", e.Kind, e.Name, e.Message, e.Code)
}

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

// errorObject is the JSON error body printed on stdout with a non-zero exit.
type errorObject struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
}

// Status invokes `spektacular <kind> status <name> --json` and parses the
// result. kind must be KindSpec or KindPlan.
func (r *Runner) Status(ctx context.Context, kind, name string) (ArtifactStatus, error) {
	if err := validateKind(kind); err != nil {
		return ArtifactStatus{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ArtifactStatus{}, &ContractError{Kind: kind, Reason: "empty artifact name"}
	}
	out, execErr := r.exec(ctx, []string{kind, verbStatus, name, flagJSON})
	if execErr != nil {
		return ArtifactStatus{}, classifyExecError(kind, name, out, execErr)
	}
	var st ArtifactStatus
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: "status is not valid JSON", Err: err}
	}
	if st.Kind != kind {
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: fmt.Sprintf("kind %q does not match requested %q", st.Kind, kind)}
	}
	if st.Name != name {
		// A status answered under a different name is exactly the "new
		// document replaced the old one" case #8227 left open; the lease is
		// bound to the name it was minted with and must never silently rebind.
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: fmt.Sprintf("name %q does not match requested %q", st.Name, name)}
	}
	switch st.DocumentStatus {
	case DocumentDraft, DocumentFinal:
	default:
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: fmt.Sprintf("unknown document_status %q", st.DocumentStatus)}
	}
	return st, nil
}

// ExportPlan invokes `spektacular plan export <name> --json` and returns the
// structured task list of a final plan. It is the one verb beyond #8301 the
// runner needs; the fixture encodes its assumed shape.
func (r *Runner) ExportPlan(ctx context.Context, name string) (Plan, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Plan{}, &ContractError{Kind: KindPlan, Reason: "empty artifact name"}
	}
	out, execErr := r.exec(ctx, []string{KindPlan, verbExport, name, flagJSON})
	if execErr != nil {
		return Plan{}, classifyExecError(KindPlan, name, out, execErr)
	}
	var plan Plan
	if err := json.Unmarshal(bytes.TrimSpace(out), &plan); err != nil {
		return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: "plan export is not valid JSON", Err: err}
	}
	if plan.Name != name {
		return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: fmt.Sprintf("export name %q does not match requested %q", plan.Name, name)}
	}
	if len(plan.Tasks) == 0 {
		return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: "plan export carries no tasks"}
	}
	return plan, nil
}

// RenderTaskList turns an exported plan into the ordered task-list text that
// planning.DecomposeFromOutput parses, so Spektacular's structure is admitted
// verbatim and no model is asked to redecompose it.
func RenderTaskList(plan Plan) string {
	var b strings.Builder
	for i, task := range plan.Tasks {
		ref := strings.TrimSpace(task.Ref)
		if ref == "" {
			ref = fmt.Sprintf("T%d", i+1)
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, ref, strings.TrimSpace(task.Title))
		if len(task.DependsOn) > 0 {
			fmt.Fprintf(&b, " (depends: %s)", strings.Join(task.DependsOn, ", "))
		}
		execution := strings.TrimSpace(task.Execution)
		if execution == "" {
			execution = "agent_suitable"
		}
		fmt.Fprintf(&b, " [%s]\n", execution)
	}
	return b.String()
}

func (r *Runner) exec(ctx context.Context, args []string) ([]byte, error) {
	if r == nil || r.Exec == nil {
		return nil, errors.New("spektacular: runner has no Exec")
	}
	return r.Exec(ctx, args)
}

func validateKind(kind string) error {
	switch kind {
	case KindSpec, KindPlan:
		return nil
	}
	return &ContractError{Kind: kind, Reason: "unsupported artifact kind"}
}

// classifyExecError turns a non-zero exit into the typed error the contract
// implies: a JSON error object with code not_found (or a message saying so)
// is a NotFoundError, any other JSON error object is a VerbError, and a
// non-JSON stdout is a ContractError wrapping the exec failure.
func classifyExecError(kind, name string, out []byte, execErr error) error {
	trimmed := bytes.TrimSpace(out)
	var eo errorObject
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &eo) != nil || eo.Error == "" {
		return &ContractError{Kind: kind, Name: name, Reason: "non-zero exit without a JSON error object", Err: execErr}
	}
	if eo.Code == errorCodeNotFound || strings.Contains(strings.ToLower(eo.Error), "not found") {
		return &NotFoundError{Kind: kind, Name: name, Message: eo.Error}
	}
	return &VerbError{Kind: kind, Name: name, Code: eo.Code, Message: eo.Error}
}
