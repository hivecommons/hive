// Package spektacular is the Hive side of the Spektacular stage runner
// (hivecommons/hive#8303). It shells out to the `spektacular` CLI, parses the
// per-artifact status verb (hivecommons/hive#8301, shipped by
// jumppad-labs/spektacular#45), and advances a run's lease stage when the
// artifact reaches `document_status: final`.
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
	"regexp"
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

// Verb names of the Spektacular CLI the runner invokes. The status verb is
// the #8301 contract as confirmed on jumppad-labs/spektacular#45; the export
// verb is still an open ask recorded in docs/spektacular.md.
//
// The CLI has no `--json` flag anywhere (its only global flag is `--fields`):
// every verb already prints JSON, and an unknown flag is a usage error. The
// runner therefore never passes one.
const (
	verbStatus = "status"
	verbExport = "export"
	verbFile   = "file"
	verbRead   = "read"
)

// Error codes the runner recognises in the JSON error envelope printed with
// a non-zero exit. `artifact_not_found` is what the status verb emits
// (spektacular#45); `not_found` is what the `file` verbs emit for the same
// condition and is kept so a future list-based poll classifies the same way.
// Anything else is surfaced as a VerbError.
const (
	errorCodeArtifactNotFound = "artifact_not_found"
	errorCodeNotFound         = "not_found"
)

// Artifact name normalisation. Spektacular addresses a spec as
// `<name>.md` and a plan as `<name>/plan.md` on the `file` verbs, while the
// status verbs (and the workflow's `data.name`) use the bare name. The bare
// name is the only stable join key across spec, plan and implement
// (spektacular#45, #46), so every name that reaches the CLI is reduced to it.
const (
	artifactPathSeparator   = "/"
	artifactExtMarkdown     = ".md"
	artifactExtMarkdownLong = ".markdown"
)

// ArtifactKey reduces any spelling of an artifact address to the bare name
// Spektacular's status verbs accept and the workflow state records:
// `000057_git-commit.md`, `000057_git-commit/plan.md` and `000057_git-commit`
// all resolve to `000057_git-commit`. Only the markdown document extensions
// are stripped, so a name that legitimately carries a dot is left alone.
func ArtifactKey(name string) string {
	key := strings.TrimSpace(name)
	// `<name>/plan.md`: drop the document segment, but only when it is a
	// markdown document, so an issue-style key such as `org/repo#42` is
	// left alone.
	if i := strings.LastIndex(key, artifactPathSeparator); i >= 0 && hasMarkdownExt(key[i+1:]) {
		key = key[:i]
	}
	if ext := markdownExt(key); ext != "" {
		key = key[:len(key)-len(ext)]
	}
	return strings.TrimSpace(key)
}

// markdownExt returns the markdown extension name carries, or "".
func markdownExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{artifactExtMarkdown, artifactExtMarkdownLong} {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}
	return ""
}

func hasMarkdownExt(name string) bool { return markdownExt(name) != "" }

// ArtifactStatus is the parsed per-artifact status document
// (spektacular#45):
//
//	{"error":false,"kind","name","document_status","current_step",
//	 "completed_steps":[],"created_at","updated_at","closed_at","spec","plan"}
//
// Progress is decided by DocumentStatus, CurrentStep and CompletedSteps
// only. UpdatedAt is informational: it is workflow activity only while the
// in-progress workflow state matches this artifact, and a file mtime
// otherwise (moved by a checkout, a reformat or a touch), and Spektacular
// may omit it entirely. Nothing in this package reads it to decide progress
// or staleness; a stale lease is decided by Hive's own lease clock
// (Stage.ExpiresAt). Spec and Plan are the frontmatter cross-references,
// which are almost never populated; they are surfaced for diagnostics and
// are never a join key.
type ArtifactStatus struct {
	Kind           string
	Name           string
	DocumentStatus DocumentStatus
	CurrentStep    string
	CompletedSteps []string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClosedAt       time.Time
	Spec           string
	Plan           string
}

// artifactStatusWire is the on-the-wire shape. Timestamps are RFC3339
// strings that Spektacular emits as "" when unknown (the frontmatter dates
// are optional) and may omit or null altogether, so they are decoded through
// flexTime rather than time.Time, whose decoder rejects "".
type artifactStatusWire struct {
	Error          json.RawMessage `json:"error"`
	Kind           string          `json:"kind"`
	Name           string          `json:"name"`
	DocumentStatus DocumentStatus  `json:"document_status"`
	CurrentStep    string          `json:"current_step"`
	CompletedSteps []string        `json:"completed_steps"`
	CreatedAt      flexTime        `json:"created_at"`
	UpdatedAt      flexTime        `json:"updated_at"`
	ClosedAt       flexTime        `json:"closed_at"`
	Spec           string          `json:"spec"`
	Plan           string          `json:"plan"`
}

// flexTime decodes an RFC3339 timestamp that may also be absent, null or the
// empty string, all of which mean "unknown" (the zero time).
type flexTime struct{ time.Time }

func (t *flexTime) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// UnmarshalJSON decodes the wire shape into ArtifactStatus.
func (s *ArtifactStatus) UnmarshalJSON(data []byte) error {
	var w artifactStatusWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*s = ArtifactStatus{
		Kind:           w.Kind,
		Name:           w.Name,
		DocumentStatus: w.DocumentStatus,
		CurrentStep:    w.CurrentStep,
		CompletedSteps: w.CompletedSteps,
		CreatedAt:      w.CreatedAt.Time,
		UpdatedAt:      w.UpdatedAt.Time,
		ClosedAt:       w.ClosedAt.Time,
		Spec:           w.Spec,
		Plan:           w.Plan,
	}
	return nil
}

// Final reports whether the artifact has reached document_status final.
func (s ArtifactStatus) Final() bool { return s.DocumentStatus == DocumentFinal }

// PlanTask is one task of a final plan as exported by Spektacular. Ref is the
// plan-local id (T1, T2, ...), DependsOn references other refs, Execution is
// agent_suitable or human_required (empty means agent_suitable).
type PlanTask struct {
	ID        string   `json:"id,omitempty"`
	Ref       string   `json:"ref"`
	Repo      string   `json:"repo,omitempty"`
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
// returned, so the caller can read the JSON error envelope the contract
// promises.
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

// errorEnvelope is the JSON error shape every Spektacular failure is
// expressed in (internal/output.ErrorResponse):
//
//	{"error":true,"code":"artifact_not_found","message":"...",
//	 "resource":"<name>","next_action":"..."}
//
// Successful results carry `"error":false` instead. Error may be a bool (the
// current CLI) or, defensively, a string message (the pre-#45 assumption).
type errorEnvelope struct {
	Error      json.RawMessage `json:"error"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Resource   string          `json:"resource"`
	NextAction string          `json:"next_action"`
}

// parseErrorEnvelope decodes data as an error envelope. ok is false when the
// bytes are not an envelope that says something went wrong: not JSON, no
// `error` member, or `error:false`. message is the human text, taken from
// `message` or, for a string-valued `error`, from that string.
func parseErrorEnvelope(data []byte) (env errorEnvelope, message string, ok bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &env) != nil || len(env.Error) == 0 {
		return env, "", false
	}
	var isErr bool
	if json.Unmarshal(env.Error, &isErr) == nil {
		if !isErr {
			return env, "", false
		}
		message = env.Message
		if message == "" {
			message = env.Code
		}
		return env, message, message != "" || env.Code != ""
	}
	var text string
	if json.Unmarshal(env.Error, &text) == nil && text != "" {
		message = env.Message
		if message == "" {
			message = text
		}
		return env, message, true
	}
	return env, "", false
}

// Status invokes `spektacular <kind> status <name>` and parses the result.
// kind must be KindSpec or KindPlan; name is reduced to its bare artifact
// name with ArtifactKey before it reaches the CLI.
func (r *Runner) Status(ctx context.Context, kind, name string) (ArtifactStatus, error) {
	if err := validateKind(kind); err != nil {
		return ArtifactStatus{}, err
	}
	name = ArtifactKey(name)
	if name == "" {
		return ArtifactStatus{}, &ContractError{Kind: kind, Reason: "empty artifact name"}
	}
	out, execErr := r.exec(ctx, []string{kind, verbStatus, name})
	if execErr != nil {
		return ArtifactStatus{}, classifyExecError(kind, name, out, execErr)
	}
	trimmed := bytes.TrimSpace(out)
	if env, message, isErr := parseErrorEnvelope(trimmed); isErr {
		// A zero exit that still carries error:true is an envelope, not a
		// status; classify it the same way a non-zero exit would be.
		return ArtifactStatus{}, classifyEnvelope(kind, name, env, message)
	}
	var st ArtifactStatus
	if err := json.Unmarshal(trimmed, &st); err != nil {
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: "status is not valid JSON", Err: err}
	}
	if st.Kind != kind {
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: fmt.Sprintf("kind %q does not match requested %q", st.Kind, kind)}
	}
	if ArtifactKey(st.Name) != name {
		// A status answered under a different name is exactly the "new
		// document replaced the old one" case #8227 left open; the lease is
		// bound to the name it was minted with and must never silently rebind.
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: fmt.Sprintf("name %q does not match requested %q", st.Name, name)}
	}
	st.Name = name
	switch st.DocumentStatus {
	case DocumentDraft, DocumentFinal:
	default:
		return ArtifactStatus{}, &ContractError{Kind: kind, Name: name, Reason: fmt.Sprintf("unknown document_status %q", st.DocumentStatus)}
	}
	return st, nil
}

// ExportPlan invokes `spektacular plan export <name> --format json` and returns the
// structured task list of a final plan. It is the one verb beyond #8301 the
// runner needs and is still an open ask on the Spektacular side; the fixture
// encodes its assumed shape.
func (r *Runner) ExportPlan(ctx context.Context, name string) (Plan, error) {
	name = ArtifactKey(name)
	if name == "" {
		return Plan{}, &ContractError{Kind: KindPlan, Reason: "empty artifact name"}
	}
	out, execErr := r.exec(ctx, []string{KindPlan, verbExport, name, "--format", "json"})
	if execErr != nil {
		return Plan{}, classifyExecError(KindPlan, name, out, execErr)
	}
	trimmed := bytes.TrimSpace(out)
	if env, message, isErr := parseErrorEnvelope(trimmed); isErr {
		return Plan{}, classifyEnvelope(KindPlan, name, env, message)
	}
	plan, err := parsePlanJSON(name, trimmed, "plan export")
	if err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// ExportPlanWithFallback preserves `spektacular plan export <name> --format json` as the
// primary task-list source. Until upstream ships that verb, an
// unknown-subcommand response falls back to Hive's documented on-disk plan
// artifact convention: `<name>/tasks.json` first, then `<name>/plan.md`, both
// read through `spektacular plan file read` so the CLI still owns store
// access.
func (r *Runner) ExportPlanWithFallback(ctx context.Context, name string) (Plan, error) {
	plan, err := r.ExportPlan(ctx, name)
	if err == nil || !planExportUnavailable(err) {
		return plan, err
	}
	fallback, fallbackErr := r.ExportPlanFallback(ctx, name)
	if fallbackErr == nil {
		return fallback, nil
	}
	return Plan{}, fmt.Errorf("%w; fallback failed: %v", err, fallbackErr)
}

// ExportPlanFallback reads the Hive-side task-list convention from a
// Spektacular plan artifact. `<name>/tasks.json` uses the same task graph JSON
// as the requested export verb. `<name>/plan.md` may carry lines like
// `- [T1] Title (repo: owner/repo) (depends: T0) [agent_suitable]`.
func (r *Runner) ExportPlanFallback(ctx context.Context, name string) (Plan, error) {
	name = ArtifactKey(name)
	if name == "" {
		return Plan{}, &ContractError{Kind: KindPlan, Reason: "empty artifact name"}
	}
	tasksPath := name + "/tasks.json"
	out, err := r.readPlanFile(ctx, name, tasksPath)
	if err == nil {
		return parsePlanJSON(name, bytes.TrimSpace(out), "tasks.json")
	}
	if !isNotFound(err) {
		return Plan{}, err
	}
	planPath := name + "/plan.md"
	out, err = r.readPlanFile(ctx, name, planPath)
	if err != nil {
		return Plan{}, err
	}
	return ParsePlanMarkdown(name, out)
}

func (r *Runner) readPlanFile(ctx context.Context, name, path string) ([]byte, error) {
	out, execErr := r.exec(ctx, []string{KindPlan, verbFile, verbRead, path})
	if execErr != nil {
		return nil, classifyExecError(KindPlan, name, out, execErr)
	}
	trimmed := bytes.TrimSpace(out)
	if env, message, isErr := parseErrorEnvelope(trimmed); isErr {
		return nil, classifyEnvelope(KindPlan, name, env, message)
	}
	return out, nil
}

func parsePlanJSON(name string, data []byte, source string) (Plan, error) {
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: source + " is not valid JSON", Err: err}
	}
	if ArtifactKey(plan.Name) != name {
		if strings.TrimSpace(plan.Name) != "" {
			return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: fmt.Sprintf("%s name %q does not match requested %q", source, plan.Name, name)}
		}
		plan.Name = name
	}
	plan.Name = name
	if len(plan.Tasks) == 0 {
		return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: source + " carries no tasks"}
	}
	return plan, nil
}

var markdownTaskLine = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s+(?:\[[ xX]\]\s+)?(?:\[([^\]]+)\]\s+)?(.+?)\s*$`)

// ParsePlanMarkdown parses Hive's plan.md fallback convention into the same
// structured plan shape as `plan export`. It intentionally accepts only a
// small task-list subset so accidental prose does not become implement work.
func ParsePlanMarkdown(name string, data []byte) (Plan, error) {
	name = ArtifactKey(name)
	if name == "" {
		return Plan{}, &ContractError{Kind: KindPlan, Reason: "empty artifact name"}
	}
	body := stripFrontmatter(data)
	var tasks []PlanTask
	for _, line := range strings.Split(string(body), "\n") {
		m := markdownTaskLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		title := strings.TrimSpace(m[2])
		execution := trailingBracketValue(&title)
		depends := parenListValue(&title, "depends")
		repo := parenValue(&title, "repo")
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		ref := strings.TrimSpace(m[1])
		if ref == "" {
			ref = fmt.Sprintf("T%d", len(tasks)+1)
		}
		tasks = append(tasks, PlanTask{Ref: ref, ID: ref, Title: title, Repo: repo, DependsOn: depends, Execution: execution})
	}
	if len(tasks) == 0 {
		return Plan{}, &ContractError{Kind: KindPlan, Name: name, Reason: "plan.md carries no task-list entries"}
	}
	return Plan{Kind: KindPlan, Name: name, Tasks: tasks}, nil
}

func stripFrontmatter(data []byte) []byte {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return data
	}
	lines := strings.SplitAfter(text, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return []byte(strings.Join(lines[i+1:], ""))
		}
	}
	return data
}

func trailingBracketValue(title *string) string {
	s := strings.TrimSpace(*title)
	if !strings.HasSuffix(s, "]") {
		return ""
	}
	start := strings.LastIndex(s, "[")
	if start < 0 {
		return ""
	}
	value := strings.TrimSpace(s[start+1 : len(s)-1])
	switch value {
	case "agent_suitable", "human_required":
		*title = strings.TrimSpace(s[:start])
		return value
	default:
		return ""
	}
}

func parenValue(title *string, key string) string {
	s := *title
	needle := "(" + key + ":"
	start := strings.LastIndex(strings.ToLower(s), needle)
	if start < 0 {
		return ""
	}
	end := strings.Index(s[start:], ")")
	if end < 0 {
		return ""
	}
	end += start
	value := strings.TrimSpace(s[start+len(needle) : end])
	*title = strings.TrimSpace(s[:start] + s[end+1:])
	return value
}

func parenListValue(title *string, key string) []string {
	value := parenValue(title, key)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// RenderTaskList turns an exported plan into the ordered task-list text that
// planning.DecomposeFromOutput parses, so Spektacular's structure is admitted
// verbatim and no model is asked to redecompose it.
func RenderTaskList(plan Plan) string {
	var b strings.Builder
	for i, task := range plan.Tasks {
		ref := strings.TrimSpace(task.Ref)
		if ref == "" {
			ref = strings.TrimSpace(task.ID)
		}
		if ref == "" {
			ref = fmt.Sprintf("T%d", i+1)
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, ref, strings.TrimSpace(task.Title))
		if repo := strings.TrimSpace(task.Repo); repo != "" {
			fmt.Fprintf(&b, " [repo:%s]", repo)
		}
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
// implies: a JSON error envelope with code artifact_not_found or not_found
// (or a message saying so) is a NotFoundError, any other envelope is a
// VerbError, and a non-JSON stdout is a ContractError wrapping the exec
// failure.
func classifyExecError(kind, name string, out []byte, execErr error) error {
	env, message, ok := parseErrorEnvelope(out)
	if !ok {
		return &ContractError{Kind: kind, Name: name, Reason: "non-zero exit without a JSON error envelope", Err: execErr}
	}
	return classifyEnvelope(kind, name, env, message)
}

func classifyEnvelope(kind, name string, env errorEnvelope, message string) error {
	switch {
	case env.Code == errorCodeArtifactNotFound, env.Code == errorCodeNotFound,
		strings.Contains(strings.ToLower(message), "not found"):
		return &NotFoundError{Kind: kind, Name: name, Message: message}
	}
	return &VerbError{Kind: kind, Name: name, Code: env.Code, Message: message}
}

func isNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}

func planExportUnavailable(err error) bool {
	var ve *VerbError
	if errors.As(err, &ve) {
		code := strings.ToLower(ve.Code)
		msg := strings.ToLower(ve.Message)
		return code == "unknown_subcommand" || code == "unknown_command" ||
			(strings.Contains(msg, "unknown") && strings.Contains(msg, "export"))
	}
	var ce *ContractError
	if errors.As(err, &ce) && ce.Err != nil {
		msg := strings.ToLower(ce.Err.Error())
		return strings.Contains(msg, "unknown") && strings.Contains(msg, "export")
	}
	return false
}
