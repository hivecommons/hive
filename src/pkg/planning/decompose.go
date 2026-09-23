// Package planning implements "planning intelligence" for Hive: turning a
// high-level epic bead into an ordered DAG of child sub-task beads.
//
// Phase 1 (this package) covers epic -> bead-DAG decomposition with a MANUAL
// trigger. It asks the architect lane to break an epic's goal into ordered,
// mostly-independent sub-tasks, parses that plan with the shared agentparse
// task-list parser, and materializes the result as child beads linked by
// dependencies and tagged with parent/execution metadata.
//
// Phase 1 deliberately does NOT touch the governor, the main eval loop, or the
// live agent-launch path. The planner's output is provided to Decompose via an
// injected PlannerFunc (or as raw text via DecomposeFromOutput), keeping the
// whole package unit-testable without spawning a real agent.
//
// The readiness gate that hides draft-epic children from Ready() is Phase 2;
// this package only lays the metadata convention (plan_status=draft on the
// epic, parent_epic + execution on each child) that Phase 2 will filter on.
package planning

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/github"
)

// Metadata keys written by Decompose. These form the convention Phase 2's
// Ready()-filtering will read.
const (
	// MetaParentEpic on a child bead holds the ID of the epic it decomposes.
	MetaParentEpic = "parent_epic"
	// MetaExecution on a child bead records whether it is agent-suitable or
	// human-required (agentparse.ExecutionAgentSuitable / ExecutionHumanRequired).
	MetaExecution = "execution"
	// MetaPlanStatus on the epic records decomposition state. Phase 1 only ever
	// sets it to PlanStatusDraft. Phase 2 will transition it (e.g. to
	// "approved") to release the children through Ready().
	MetaPlanStatus = "plan_status"
	// MetaPlanRef on a child records the planner's local task reference (e.g.
	// "T1") so a plan can be re-derived / audited.
	MetaPlanRef = "plan_ref"
	// MetaClaimedBy on a child records who picked the task up (agent name or
	// GitHub login). Set by whoever claims it — e.g.
	// `bd update <id> --claim --set-metadata claimed_by=<name>` — and read by
	// the dashboard plan review so an epic's tasks show who is on them
	// (hivecommons/hive#8011).
	MetaClaimedBy = "claimed_by"
	// MetaPRURL on a child records the pull request that carries its work
	// (`bd update <id> --set-metadata pr_url=<url>`). The plan review renders
	// it as the task's PR link (hivecommons/hive#8011).
	MetaPRURL = "pr_url"
	// MetaPlanRepo records the repository a multi-repo run child belongs to.
	MetaPlanRepo = "plan_repo"
	// MetaPlanRepoRole records that repository's role within a run wave.
	MetaPlanRepoRole = "plan_repo_role"
	// MetaPlanWave records the 1-based wave number for multi-repo run barriers.
	MetaPlanWave = "plan_wave"
)

const (
	// PlanStatusDraft marks an epic whose plan has been drafted but not yet
	// approved. While an epic carries plan_status=draft, beads.Store.Ready()
	// (Phase 2) excludes its children, so no agent can claim the sub-tasks
	// until a human approves the plan.
	PlanStatusDraft = "draft"
	// PlanStatusApproved marks an epic whose plan a human has approved. Only
	// this value releases the epic's children through beads.Store.Ready().
	// It is set by ApprovePlan (or immediately at decomposition time when the
	// active ACMM pack enables plan_auto_approve).
	PlanStatusApproved = "approved"
)

// defaultChildPriority is the priority assigned to generated child beads when
// the epic carries none distinguishable. Children inherit the epic's priority
// (see Options.ChildPriority) but default to Medium.
const defaultChildPriority = beads.PriorityMedium

// PlannerFunc produces the raw planner output for an epic prompt. It is the
// single injection seam that keeps Decompose free of any live agent/tmux
// dependency in Phase 1. Implementations must honor ctx cancellation.
type PlannerFunc func(ctx context.Context, prompt string) (string, error)

// Options tune a decomposition. The zero value is valid.
type Options struct {
	// Actor overrides the child beads' actor/lane. When empty, Decompose
	// derives the lane from the epic title via the classifier, falling back to
	// the architect lane (the planner role for Phase 1).
	Actor string
	// ChildPriority overrides the child beads' priority. When nil, children
	// inherit the epic's priority.
	ChildPriority *beads.Priority
	// AutoApprove, when true, sets the epic's plan_status to approved (instead
	// of draft) immediately at decomposition time, releasing the children
	// through Ready() without a human approval step. It is driven by the active
	// ACMM pack's plan_auto_approve knob — high maturity levels (L5/L6) trust
	// the planner enough to skip the review gate; lower levels leave it false so
	// the plan stays draft pending human approval. Default false.
	AutoApprove bool
}

// Result summarizes a decomposition.
type Result struct {
	// Children are the newly created child beads, in plan order.
	Children []*beads.Bead
	// Tasks are the parsed planner tasks aligned 1:1 with Children.
	Tasks []agentparse.Task
}

// BuildPrompt renders the architect-style decomposition prompt for an epic. It
// reuses the architect lane's framing ([agent:architect]) so the planner role
// is the existing architect, not a new role. The prompt asks for an ordered,
// dependency-annotated, execution-tagged task list in the exact shape
// agentparse.ParseTaskList consumes.
func BuildPrompt(epic *beads.Bead) string {
	var b strings.Builder
	b.WriteString("[agent:architect]\n")
	b.WriteString("Decompose the following EPIC into an ordered plan of sub-tasks.\n\n")

	b.WriteString("EPIC: ")
	b.WriteString(epic.Title)
	b.WriteString("\n")
	b.WriteString("EPIC ID: ")
	b.WriteString(epic.ID)
	b.WriteString("\n")
	if url := epicIssueURL(epic); url != "" {
		// The label trigger mints epics from an enumerated github.Issue, which
		// carries no body. Send the URL so the architect reads the whole issue
		// itself instead of planning a title (hivecommons/hive#8010).
		b.WriteString("ISSUE: ")
		b.WriteString(url)
		b.WriteString("\n")
	}
	if body := epicBody(epic); body != "" {
		b.WriteString("DETAILS:\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("INSTRUCTIONS:\n")
	if epicIssueURL(epic) != "" {
		b.WriteString("  0. Read the full issue first (`gh issue view <ISSUE URL> --comments`); the DETAILS above may be only the title.\n")
	}
	b.WriteString("  1. Break the goal into ordered, independent-where-possible sub-tasks.\n")
	b.WriteString("  2. Give each sub-task a local id in [brackets] (T1, T2, ...).\n")
	b.WriteString("  3. Note dependencies with a trailing \"(depends: T1, T2)\" clause.\n")
	b.WriteString("  4. Mark each sub-task \"[agent_suitable]\" or \"[human_required]\".\n")
	b.WriteString("  5. Do NOT open PRs and do NOT create beads by hand.\n")
	// Closing the loop (hivecommons/hive#8010): planning.DecomposeFromOutput is
	// the only thing that turns a task list into child beads and clears the
	// pending marker, and before this its only caller was the `bd decompose`
	// CLI run by an operator. The architect has `bd` on PATH and BD_DIR set to
	// its own store, so it hands the plan to the same CLI itself.
	b.WriteString("  6. When the plan is final, write it to a file and hand it to Hive with:\n")
	b.WriteString("       bd decompose ")
	b.WriteString(epic.ID)
	b.WriteString(" --plan <file>\n")
	b.WriteString("     That command creates the child tasks and marks this epic as planned. Until it runs, the plan does not exist.\n\n")

	b.WriteString("FORMAT (one task per line, nothing else in the file):\n")
	b.WriteString("  1. [T1] <task> [agent_suitable]\n")
	b.WriteString("  2. [T2] <task> (depends: T1) [human_required]\n")

	return b.String()
}

// epicIssueURL returns the source issue URL for an issue-minted epic, else "".
func epicIssueURL(epic *beads.Bead) string {
	if epic == nil {
		return ""
	}
	return strings.TrimSpace(epic.Meta(MetaIssueURL))
}

// epicBody returns the most descriptive text available for an epic: its Notes,
// else a "body"/"description" metadata value, else "".
func epicBody(epic *beads.Bead) string {
	if strings.TrimSpace(epic.Notes) != "" {
		return strings.TrimSpace(epic.Notes)
	}
	for _, k := range []string{"body", "description"} {
		if v := epic.Meta(k); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Decompose runs a full manual decomposition: it builds the architect prompt,
// invokes planner to get the plan text, then materializes child beads via
// DecomposeFromOutput. planner must not be nil.
func Decompose(ctx context.Context, store *beads.Store, epic *beads.Bead, planner PlannerFunc, opts Options) (*Result, error) {
	if planner == nil {
		return nil, fmt.Errorf("planning: planner func is nil")
	}
	if err := validateEpic(store, epic); err != nil {
		return nil, err
	}

	prompt := BuildPrompt(epic)
	output, err := planner(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("planning: planner failed: %w", err)
	}
	return DecomposeFromOutput(store, epic, output, opts)
}

// DecomposeFromOutput materializes child beads from an already-obtained planner
// output. It is the unit-testable core: no agent, no context needed. Steps:
//
//  1. parse the output into tasks (agentparse.ParseTaskList),
//  2. create one child bead per task,
//  3. link dependencies by resolving each task's local refs to child ids,
//  4. tag children with parent_epic + execution (+ plan_ref),
//  5. mark the epic plan_status=draft (Phase 2 readiness convention).
//
// It returns an error if the epic is invalid or the output yields no tasks.
func DecomposeFromOutput(store *beads.Store, epic *beads.Bead, output string, opts Options) (*Result, error) {
	if err := validateEpic(store, epic); err != nil {
		return nil, err
	}

	tasks := agentparse.ParseTaskList(agentparse.SplitLines(output))
	if len(tasks) == 0 {
		return nil, fmt.Errorf("planning: planner output produced no tasks")
	}

	actor := opts.Actor
	if actor == "" {
		actor = deriveActor(epic)
	}
	priority := epic.Priority
	if opts.ChildPriority != nil {
		priority = *opts.ChildPriority
	} else if priority < beads.PriorityCritical || priority > beads.PriorityMinor {
		priority = defaultChildPriority
	}

	children := make([]*beads.Bead, 0, len(tasks))
	// refToID maps a task's local ref (e.g. "T1") to the created child bead id,
	// so dependency clauses can be resolved after all children exist.
	refToID := make(map[string]string, len(tasks))

	// NOTE: the store.Create / SetMetadata / AddDependency error branches below
	// only fire on underlying persistence (disk I/O) failures — the inputs are
	// always valid (TypeTask, string metadata, known ids). They are covered
	// representatively by TestDecomposeFromOutput_CreateChildError (read-only
	// store dir); the remaining sibling branches are defensive I/O guards.
	childWaves := map[string]int{}
	waveChildren := map[int][]string{}
	for _, task := range tasks {
		title, annotations := planTaskAnnotations(task.Title)
		child, err := store.Create(title, beads.TypeTask, priority, actor, "")
		if err != nil {
			return nil, fmt.Errorf("planning: creating child bead for %q: %w", title, err)
		}
		children = append(children, child)
		if task.Ref != "" {
			refToID[task.Ref] = child.ID
		}

		if err := store.SetMetadata(child.ID, MetaParentEpic, epic.ID); err != nil {
			return nil, fmt.Errorf("planning: tagging parent_epic on %s: %w", child.ID, err)
		}
		execution := task.Execution
		if execution == "" {
			// Default unmarked tasks to agent-suitable; a human can re-tag later.
			execution = agentparse.ExecutionAgentSuitable
		}
		if err := store.SetMetadata(child.ID, MetaExecution, execution); err != nil {
			return nil, fmt.Errorf("planning: tagging execution on %s: %w", child.ID, err)
		}
		if task.Ref != "" {
			if err := store.SetMetadata(child.ID, MetaPlanRef, task.Ref); err != nil {
				return nil, fmt.Errorf("planning: tagging plan_ref on %s: %w", child.ID, err)
			}
		}
		for key, value := range annotations {
			if err := store.SetMetadata(child.ID, key, value); err != nil {
				return nil, fmt.Errorf("planning: tagging %s on %s: %w", key, child.ID, err)
			}
		}
		if wave, ok := planWaveValue(annotations[MetaPlanWave]); ok {
			childWaves[child.ID] = wave
			waveChildren[wave] = append(waveChildren[wave], child.ID)
		}
	}

	// Second pass: wire dependencies now that every ref is known. Unknown refs
	// (a task depending on an id the planner never defined) are skipped rather
	// than erroring, so a partially-malformed plan still produces a usable DAG.
	for i, task := range tasks {
		for _, ref := range task.DependsOn {
			depID, ok := refToID[ref]
			if !ok || depID == children[i].ID {
				// Unknown ref or self-dependency — skip.
				continue
			}
			if err := store.AddDependency(children[i].ID, depID); err != nil {
				return nil, fmt.Errorf("planning: linking %s -> %s: %w", children[i].ID, depID, err)
			}
		}
	}
	if err := addPlanWaveBarriers(store, children, childWaves, waveChildren); err != nil {
		return nil, err
	}

	// Set the epic's plan_status. Normally draft (children stay gated until a
	// human approves via ApprovePlan); when the active pack enables
	// plan_auto_approve, approved (children released immediately).
	planStatus := PlanStatusDraft
	if opts.AutoApprove {
		planStatus = PlanStatusApproved
	}
	if err := store.SetMetadata(epic.ID, MetaPlanStatus, planStatus); err != nil {
		return nil, fmt.Errorf("planning: setting plan_status on epic %s: %w", epic.ID, err)
	}

	// Children now exist: clear any decompose_pending marker set when the epic was
	// minted from an issue (Phase 4), so the queued request no longer shows as
	// "waiting on the architect". Best-effort — the marker is advisory.
	if epic.Meta(MetaDecomposePending) == "true" {
		_ = ClearDecomposePending(store, epic.ID)
	}

	return &Result{Children: children, Tasks: tasks}, nil
}

func planTaskAnnotations(title string) (string, map[string]string) {
	annotations := map[string]string{}
	parts := strings.Fields(title)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if !strings.HasPrefix(token, "[") || !strings.HasSuffix(token, "]") || !strings.Contains(token, ":") {
			out = append(out, part)
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(token, "["), "]")
		key, value, ok := strings.Cut(body, ":")
		if !ok {
			out = append(out, part)
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "repo", "repository":
			annotations[MetaPlanRepo] = value
		case "role":
			annotations[MetaPlanRepoRole] = value
		case "wave":
			if wave, ok := planWaveValue(value); ok {
				annotations[MetaPlanWave] = strconv.Itoa(wave)
			}
		default:
			out = append(out, part)
		}
	}
	if len(annotations) == 0 {
		return title, annotations
	}
	cleaned := strings.Join(out, " ")
	return strings.TrimSpace(cleaned), annotations
}

func planWaveValue(raw string) (int, bool) {
	wave, err := strconv.Atoi(strings.TrimSpace(raw))
	return wave, err == nil && wave > 0
}

func addPlanWaveBarriers(store *beads.Store, children []*beads.Bead, childWaves map[string]int, waveChildren map[int][]string) error {
	for _, child := range children {
		wave, ok := childWaves[child.ID]
		if !ok || wave <= 1 {
			continue
		}
		prevWave := 0
		for candidate := range waveChildren {
			if candidate < wave && candidate > prevWave {
				prevWave = candidate
			}
		}
		if prevWave == 0 {
			continue
		}
		for _, depID := range waveChildren[prevWave] {
			if depID == child.ID {
				continue
			}
			if err := store.AddDependency(child.ID, depID); err != nil {
				return fmt.Errorf("planning: linking wave barrier %s -> %s: %w", child.ID, depID, err)
			}
		}
	}
	return nil
}

// validateEpic ensures epic is non-nil, of type epic, and (when store is
// provided) still exists in the store.
func validateEpic(store *beads.Store, epic *beads.Bead) error {
	if epic == nil {
		return fmt.Errorf("planning: epic is nil")
	}
	if epic.Type != beads.TypeEpic {
		return fmt.Errorf("planning: bead %s is type %q, only %q may be decomposed", epic.ID, epic.Type, beads.TypeEpic)
	}
	if store != nil {
		if _, err := store.Get(epic.ID); err != nil {
			return fmt.Errorf("planning: epic %s not found in store: %w", epic.ID, err)
		}
	}
	return nil
}

// architectActor is the actor/lane for the planner role in Phase 1. It matches
// classify.LaneArchitect and is the fallback when the classifier does not route
// the epic elsewhere.
const architectActor = string(classify.LaneArchitect)

// deriveActor picks the child beads' actor/lane. It runs the epic title through
// the classifier; a non-default lane wins, otherwise it falls back to the
// architect lane (the Phase 1 planner role).
func deriveActor(epic *beads.Bead) string {
	c := classify.Classify(github.Issue{Title: epic.Title, Labels: epicLabels(epic)})
	if lane := string(c.Lane); lane != "" && lane != classify.DefaultLane {
		return lane
	}
	return architectActor
}

// epicLabels extracts comma-separated labels from an epic's "labels" metadata,
// if present, so the classifier can use them.
func epicLabels(epic *beads.Bead) []string {
	raw := epic.Meta("labels")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
