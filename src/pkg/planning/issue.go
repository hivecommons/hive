package planning

// Phase 4 planning intelligence: the GitHub-issue entry point. This file turns
// an actionable GitHub issue into an epic bead so it can be decomposed by the
// same machinery Phases 1-3 use. It is the shared, idempotent core behind both
// the dashboard "Plan this issue" click (Part A) and the `plan`-label auto-trigger
// (Part B): both call EpicFromIssue, then request decomposition.
//
// EpicFromIssue creates NO children and kicks NO agent — it only mints (or finds)
// the epic and stamps its metadata. Decomposition is driven separately (the
// dashboard kicks the architect out-of-band, then the plan-review flow materializes
// the children), keeping this function pure, fast, and safe to call every eval
// cycle. It never touches the agent-manager launch path.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/github"
)

// Issue metadata keys stamped on an epic minted from a GitHub issue. They let
// the dashboard/plan-review flow trace an epic back to its source issue and
// render a link, and let the classifier reuse the issue's labels.
const (
	// MetaIssueRepo records the source issue's "owner/repo".
	MetaIssueRepo = "issue_repo"
	// MetaIssueNumber records the source issue's number (as a string).
	MetaIssueNumber = "issue_number"
	// MetaIssueURL records the source issue's HTML URL.
	MetaIssueURL = "issue_url"
	// MetaIssueLabels records the source issue's labels, comma-separated, so
	// deriveActor/epicLabels can route the epic (and its children) by label.
	MetaIssueLabels = "labels"
	// MetaSource records how an epic was created ("github-issue" here) so the
	// dashboard can distinguish issue-sourced epics from bd-created ones.
	MetaSource = "source"
	// MetaDecomposePending marks an epic that has been ACCEPTED for planning but
	// not yet decomposed by the architect — the request is queued. It is set at
	// mint time (alongside plan_status=draft) so the epic immediately surfaces in
	// the plan-review flow as "waiting to be built", and it makes the label
	// trigger idempotent (no re-kick while a decompose is already pending). The
	// architect clears it when it materializes the children.
	MetaDecomposePending = "decompose_pending"
)

// SourceGitHubIssue is the MetaSource value for epics minted from a GH issue.
const SourceGitHubIssue = "github-issue"

// externalRefPrefix namespaces issue-sourced epics' external refs so they never
// collide with other external-ref conventions.
const externalRefPrefix = "gh-"

// IssueRef returns the stable external ref for a GitHub issue: "gh-<repo>#<num>"
// when a repo and number are known, else the issue URL, else "". The ref is the
// idempotency key EpicFromIssue uses via FindByExternalRef, so it must be stable
// across eval cycles for the same issue.
func IssueRef(issue github.Issue) string {
	repo := strings.TrimSpace(issue.Repo)
	if repo != "" && issue.Number > 0 {
		return externalRefPrefix + repo + "#" + strconv.Itoa(issue.Number)
	}
	if u := strings.TrimSpace(issue.URL); u != "" {
		return u
	}
	return ""
}

// EpicFromIssue mints (or returns the existing) epic bead for a GitHub issue.
// body is the issue's description text (the github.Issue struct carries no body,
// so the caller supplies it — the dashboard from its request payload, the label
// trigger as ""); it is stored in the epic's Notes so BuildPrompt can plan
// against it.
//
// It is IDEMPOTENT: if an epic already exists for the issue's external ref, that
// epic is returned unchanged rather than duplicated — so it is safe to call on
// every eval cycle (Part B's label trigger) and on repeated clicks (Part A).
//
// The epic carries the issue title, the body in Notes (so BuildPrompt/epicBody
// pick it up), the issue's labels in "labels" metadata (so the classifier can
// route it), and issue_repo/issue_number/issue_url/source metadata for tracing.
// It creates NO children and sets NO plan_status — decomposition is a separate,
// caller-driven step (kick architect + plan-review), keeping this pure and cheap.
func EpicFromIssue(store *beads.Store, issue github.Issue, body string) (*beads.Bead, error) {
	if store == nil {
		return nil, fmt.Errorf("planning: bead store is nil")
	}
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		return nil, fmt.Errorf("planning: issue has no title")
	}

	ref := IssueRef(issue)
	if ref == "" {
		return nil, fmt.Errorf("planning: issue has no stable ref (need repo+number or url)")
	}

	// Idempotency guard: an epic already minted for this issue wins, no dup.
	if existing := store.FindByExternalRef(ref); existing != nil {
		return existing, nil
	}

	// Derive the epic lane from the issue up front so the epic's actor is
	// meaningful even before decomposition; the classifier already knows lanes.
	c := classify.Classify(issue)

	epic, err := store.Create(title, beads.TypeEpic, beads.PriorityMedium, string(c.Lane), ref)
	if err != nil {
		return nil, fmt.Errorf("planning: creating epic for issue %s: %w", ref, err)
	}

	// Carry the issue body so BuildPrompt has DETAILS to plan against. Notes is
	// preferred by epicBody; set it via Update to keep a single persist.
	if b := strings.TrimSpace(body); b != "" {
		if err := store.Update(epic.ID, func(bd *beads.Bead) { bd.Notes = b }); err != nil {
			return nil, fmt.Errorf("planning: setting body on epic %s: %w", epic.ID, err)
		}
	}

	// Trace + state metadata. SetMetadata failures are underlying-persistence
	// errors; a failure here leaves a valid epic (external ref already set), so
	// surface it but the caller can still find the epic next cycle by ref.
	//
	// plan_status=draft + decompose_pending=true is the ACCEPTED-BUT-NOT-YET-BUILT
	// state: the epic shows up immediately in the plan-review flow ("awaiting
	// review") and the PLANNING tile ("pending"), so a queued request is visible
	// even when the architect is paused and hasn't decomposed it yet. Ready() gates
	// the (still non-existent) children on the draft status, so nothing is
	// claimable until a human approves the eventual plan.
	meta := map[string]string{
		MetaSource:           SourceGitHubIssue,
		MetaPlanStatus:       PlanStatusDraft,
		MetaDecomposePending: "true",
		MetaIssueRepo:        strings.TrimSpace(issue.Repo),
		MetaIssueURL:         strings.TrimSpace(issue.URL),
		MetaIssueLabels:      strings.Join(issue.Labels, ","),
	}
	if issue.Number > 0 {
		meta[MetaIssueNumber] = strconv.Itoa(issue.Number)
	}
	for k, v := range meta {
		if v == "" {
			continue
		}
		if err := store.SetMetadata(epic.ID, k, v); err != nil {
			return nil, fmt.Errorf("planning: tagging %s on epic %s: %w", k, epic.ID, err)
		}
	}

	// Return the freshly-loaded epic so callers see the metadata/notes written
	// above (Get cannot fail here — the epic was just created in this store).
	refreshed, err := store.Get(epic.ID)
	if err != nil {
		return nil, fmt.Errorf("planning: reloading epic %s: %w", epic.ID, err)
	}
	return refreshed, nil
}

// DecomposePending reports whether an epic is accepted for planning but not yet
// decomposed by the architect (its decompose_pending marker is set). Callers use
// it to know a plan request is QUEUED — waiting on the architect — so the
// dashboard can surface the "architect paused" message and the label trigger
// knows there is still work to hand off.
func DecomposePending(epic *beads.Bead) bool {
	return epic != nil && epic.Meta(MetaDecomposePending) == "true"
}

// ClearDecomposePending removes the pending marker once the architect has
// decomposed the epic (children materialized). It is a single metadata write; a
// failure is non-fatal (the marker is advisory — the tile would just show the
// epic as pending one cycle longer).
func ClearDecomposePending(store *beads.Store, epicID string) error {
	return store.UnsetMetadata(epicID, MetaDecomposePending)
}

// HasPlanLabel reports whether any of the issue's labels is a "plan" trigger
// label (Part B). It matches the maintainer labels `plan` and `epic`
// case-insensitively. A nil/empty label set returns false.
func HasPlanLabel(issue github.Issue) bool {
	for _, l := range issue.Labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "plan", "epic":
			return true
		}
	}
	return false
}

// ArchitectAgentName is the agent that decomposes epics. It is the general
// architect lane (RFCs, refactor/perf scans) — decomposition is just one more
// job it does — so decomposition RESPECTS the architect's pause: a request for a
// paused architect queues (the epic stays decompose_pending) rather than
// force-unpausing it.
const ArchitectAgentName = "architect"

// PlanningMinACMMLevel is the lowest ACMM level at which issue planning is
// offered. The architect agent that decomposes epics is only SCHEDULED at ACMM
// L5 (4h cadence) and L6 (15m); it has no cadence at L1-L4 (see
// pkg/config/packs/level-{5,6}.yaml). Offering the ⧉ Plan button or honoring
// the `plan` label below L5 would mint an epic that nothing ever decomposes —
// it would sit in decompose_pending forever. Gate all planning entrypoints to
// this level.
const PlanningMinACMMLevel = 5

// PlanningLevelGateMessage is the user-facing explanation returned when planning
// is attempted below PlanningMinACMMLevel. It names the reason (the architect is
// not scheduled that low) and points at the fix.
const PlanningLevelGateMessage = "Planning requires ACMM level 5+ — the architect agent that decomposes plans is not scheduled below L5. Raise the level or manually assign the architect."

// PlanningAllowedAtLevel reports whether issue planning should be offered at the
// given ACMM level. It is the single source of truth for the L5+ gate shared by
// the dashboard handler and the label trigger.
func PlanningAllowedAtLevel(acmmLevel int) bool {
	return acmmLevel >= PlanningMinACMMLevel
}

// DecomposeKicker drives the architect out-of-band and reports whether it is
// currently paused. Both methods must be safe to call from the governor tick /
// an HTTP handler and must NOT touch the agent-launch mutex. *agent.Manager
// satisfies this via SendKick + IsPaused (each takes the manager lock itself,
// called off the launch path — the #1980/#2018/#2021 lesson).
type DecomposeKicker interface {
	SendKick(name, message string) error
	IsPaused(name string) bool
}

// DecomposeState is the outcome of RequestDecompose, so callers can surface the
// right message (toast / plan-review / PLANNING tile).
type DecomposeState string

const (
	// DecomposeKicked: the architect was available and was kicked to decompose now.
	DecomposeKicked DecomposeState = "kicked"
	// DecomposeQueuedPaused: the architect is PAUSED, so the request is queued —
	// the epic stays decompose_pending and will be built when the architect
	// resumes. The user must be told the pause is why nothing is happening.
	DecomposeQueuedPaused DecomposeState = "queued_paused"
	// DecomposeQueuedNoAgent: no architect/manager is wired, so the request is
	// queued for a later cycle (also covers the kick failing transiently).
	DecomposeQueuedNoAgent DecomposeState = "queued_no_agent"
)

// ArchitectPausedMessage is the user-facing explanation shown wherever a plan is
// queued because the architect is paused. It names the deliberate pause as the
// cause and points at the fix.
const ArchitectPausedMessage = "⏸ Architect is paused — this plan won't be built until you resume the architect agent."

// RequestDecompose hands a pending epic off to the architect, RESPECTING its
// pause. It is the shared core behind the dashboard click and the label trigger:
//
//   - architect paused        → do NOT kick, do NOT unpause; leave the epic
//     decompose_pending and return DecomposeQueuedPaused so the caller shows the
//     "architect is paused" message.
//   - architect available     → kick it with the decomposition prompt and return
//     DecomposeKicked; the pending marker stays until the architect materializes
//     children (DecomposeFromOutput clears it).
//   - no manager / kick failed → return DecomposeQueuedNoAgent; the epic stays
//     pending and a later cycle retries.
//
// It never force-unpauses and only ever calls SendKick/IsPaused — never the
// launch-path mutex. epic must be the (already minted) pending epic.
func RequestDecompose(kicker DecomposeKicker, epic *beads.Bead) DecomposeState {
	if kicker == nil {
		return DecomposeQueuedNoAgent
	}
	if kicker.IsPaused(ArchitectAgentName) {
		return DecomposeQueuedPaused
	}
	if err := kicker.SendKick(ArchitectAgentName, BuildPrompt(epic)); err != nil {
		return DecomposeQueuedNoAgent
	}
	return DecomposeKicked
}

// LabelPlanResult summarizes one PlanIssuesFromLabels pass.
type LabelPlanResult struct {
	// Minted is the number of NEW epics created this pass (idempotent — an issue
	// already turned into an epic is not counted again).
	Minted int
	// Kicked is the number of pending epics handed to an available architect.
	Kicked int
	// QueuedPaused is the number left queued because the architect is paused.
	QueuedPaused int
}

// LabelPlanSink observes PlanIssuesFromLabels decisions (audit + logging). Both
// callers implement it; either method may be a no-op. It keeps this function
// free of the dashboard/logging types so it is unit-testable in pkg/planning.
type LabelPlanSink interface {
	// KickedPlan is called when a labeled issue's epic was handed to the architect.
	KickedPlan(epic *beads.Bead)
	// QueuedPlan is called when the plan is queued (architect paused or absent);
	// paused reports which case, so the caller can log the "architect paused"
	// message once.
	QueuedPlan(epic *beads.Bead, paused bool)
}

// PlanIssuesFromLabels is the Phase 4 Part B core: for each issue carrying a
// plan/epic label, mint an epic (idempotent) and, while it is still pending, hand
// it to the architect via RequestDecompose — RESPECTING the architect's pause
// (paused → queued, never force-unpaused). It is a pure, synchronous function
// over the injected store/kicker/sink, so cmd/hive is a thin adapter and the
// whole decision path is unit-tested here. mintErr (nil-safe) is called on a mint
// failure so the caller can log it.
//
// acmmLevel gates the whole pass: below PlanningMinACMMLevel the architect has no
// cadence, so minting epics would strand them in decompose_pending. In that case
// this is a no-op returning the zero result — no epics are minted.
func PlanIssuesFromLabels(store *beads.Store, kicker DecomposeKicker, issues []github.Issue, sink LabelPlanSink, mintErr func(ref string, err error), acmmLevel int) LabelPlanResult {
	var res LabelPlanResult
	if store == nil {
		return res
	}
	// Gate: the architect that decomposes these epics is not scheduled below L5.
	// Minting here would create epics nothing ever decomposes, so skip entirely.
	if !PlanningAllowedAtLevel(acmmLevel) {
		return res
	}
	for _, issue := range issues {
		if !HasPlanLabel(issue) {
			continue
		}
		existing := store.FindByExternalRef(IssueRef(issue))
		// The github.Issue carries no body; the epic plans against title + labels.
		epic, err := EpicFromIssue(store, issue, "")
		if err != nil {
			if mintErr != nil {
				mintErr(IssueRef(issue), err)
			}
			continue
		}
		if existing == nil {
			res.Minted++
		}
		// Only hand off while still pending (not yet decomposed by the architect).
		if !DecomposePending(epic) {
			continue
		}
		switch RequestDecompose(kicker, epic) {
		case DecomposeKicked:
			res.Kicked++
			if sink != nil {
				sink.KickedPlan(epic)
			}
		case DecomposeQueuedPaused:
			res.QueuedPaused++
			if sink != nil {
				sink.QueuedPlan(epic, true)
			}
		case DecomposeQueuedNoAgent:
			if sink != nil {
				sink.QueuedPlan(epic, false)
			}
		}
	}
	return res
}
