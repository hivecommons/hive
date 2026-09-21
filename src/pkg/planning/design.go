package planning

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

// Gate 1 — the design step in front of decomposition (RFC hivecommons/hive#7993).
//
//	issue labeled <design label>
//	        │
//	        ▼
//	architect posts a design comment on the issue        design_status=requested
//	        │
//	        ├─ human re-applies the design label ──► revision (≤ MaxRevisions)
//	        │                                          past the cap: needs_human
//	        ▼
//	human applies <approved label>                       design_status=approved
//	        │
//	        ▼
//	decompose (Gate 2, unchanged)
//
// Every signal is a label on the GitHub issue, read on the eval cycle from the
// same enumerated issue set the plan label uses. Nothing here calls GitHub:
// the architect posts its design through the gh wrapper's comment relay, and
// approval is a label a person with triage applies (or the dashboard applies
// on their behalf, with the owner gate in front of it).

// Metadata keys on the epic bead that carry the design state.
const (
	// MetaDesignStatus is one of the DesignStatus* values, or "" when the epic
	// never entered the design step (plan-label or button epics).
	MetaDesignStatus = "design_status"
	// MetaDesignRevision counts how many design kicks have been sent (1 = the
	// first design, 2+ = revisions).
	MetaDesignRevision = "design_revision"
	// MetaDesignKickedAt is the RFC3339 time of the last design kick.
	MetaDesignKickedAt = "design_kicked_at"
	// MetaDesignLabelAbsent is "true" once the design label has been observed
	// missing from a requested epic's issue. A later re-appearance of the label
	// is the "please revise" signal.
	MetaDesignLabelAbsent = "design_label_absent"
)

// Design statuses.
const (
	// DesignStatusQueued: the design label is on the issue but the concurrency
	// cap is full; the epic waits its turn (no kick sent).
	DesignStatusQueued = "queued"
	// DesignStatusRequested: the architect has been asked to post a design;
	// waiting for a human to approve (or request a revision).
	DesignStatusRequested = "requested"
	// DesignStatusApproved: a human applied the approved label; decomposition
	// (Gate 2) proceeds as for any plan-labeled issue.
	DesignStatusApproved = "approved"
	// DesignStatusNeedsHuman: the revision cap was hit; nothing more is sent
	// until a person intervenes (apply the approved label, or remove the design
	// label and start over from the dashboard).
	DesignStatusNeedsHuman = "needs_human"
)

// DesignConfig is the label vocabulary and the caps for Gate 1. Zero values
// fall back to the Default* constants via the *OrDefault helpers so a bare
// struct is safe.
type DesignConfig struct {
	// PlanLabels mean "break this down" (Gate 2 only).
	PlanLabels []string
	// DesignLabels mean "design first" (Gate 1 then Gate 2).
	DesignLabels []string
	// ApprovedLabel is what a human applies to approve the posted design.
	ApprovedLabel string
	// MaxRevisions caps design kicks per epic (first design + revisions).
	MaxRevisions int
	// MaxConcurrent caps epics in DesignStatusRequested at once.
	MaxConcurrent int
}

// Defaults mirror pkg/config's Default* constants; duplicated here so planning
// stays importable from config (no cycle).
const (
	defaultPlanLabel           = "hive-plan"
	defaultDesignLabel         = "hive-design"
	defaultDesignApprovedLabel = "design-approved"
	defaultMaxDesignRevisions  = 3
	defaultMaxConcurrentDesign = 3
)

// DefaultDesignConfig is the out-of-the-box vocabulary.
func DefaultDesignConfig() DesignConfig {
	return DesignConfig{
		PlanLabels:    []string{defaultPlanLabel},
		DesignLabels:  []string{defaultDesignLabel},
		ApprovedLabel: defaultDesignApprovedLabel,
		MaxRevisions:  defaultMaxDesignRevisions,
		MaxConcurrent: defaultMaxConcurrentDesign,
	}
}

func (c DesignConfig) planLabels() []string {
	if len(c.PlanLabels) > 0 {
		return c.PlanLabels
	}
	return []string{defaultPlanLabel}
}

func (c DesignConfig) designLabels() []string {
	if len(c.DesignLabels) > 0 {
		return c.DesignLabels
	}
	return []string{defaultDesignLabel}
}

// ApprovedLabelOrDefault is the approval label, defaulted.
func (c DesignConfig) ApprovedLabelOrDefault() string {
	if c.ApprovedLabel != "" {
		return c.ApprovedLabel
	}
	return defaultDesignApprovedLabel
}

// DesignLabelOrDefault is the FIRST design label — what the dashboard applies
// when an operator asks for "design first".
func (c DesignConfig) DesignLabelOrDefault() string {
	return c.designLabels()[0]
}

func (c DesignConfig) maxRevisions() int {
	if c.MaxRevisions > 0 {
		return c.MaxRevisions
	}
	return defaultMaxDesignRevisions
}

func (c DesignConfig) maxConcurrent() int {
	if c.MaxConcurrent > 0 {
		return c.MaxConcurrent
	}
	return defaultMaxConcurrentDesign
}

// hasAnyLabel reports whether the issue carries any of the wanted labels,
// case-insensitively and ignoring surrounding whitespace.
func hasAnyLabel(issue github.Issue, wanted []string) bool {
	for _, l := range issue.Labels {
		have := strings.ToLower(strings.TrimSpace(l))
		for _, w := range wanted {
			if have == strings.ToLower(strings.TrimSpace(w)) {
				return true
			}
		}
	}
	return false
}

// HasPlanLabelIn reports whether the issue carries one of cfg's plan labels.
func HasPlanLabelIn(issue github.Issue, cfg DesignConfig) bool {
	return hasAnyLabel(issue, cfg.planLabels())
}

// HasDesignLabel reports whether the issue carries one of cfg's design labels.
func HasDesignLabel(issue github.Issue, cfg DesignConfig) bool {
	return hasAnyLabel(issue, cfg.designLabels())
}

// HasDesignApprovedLabel reports whether the issue carries cfg's approval label.
func HasDesignApprovedLabel(issue github.Issue, cfg DesignConfig) bool {
	return hasAnyLabel(issue, []string{cfg.ApprovedLabelOrDefault()})
}

// DesignStatus returns the epic's design_status ("" when never designed).
func DesignStatus(epic *beads.Bead) string { return metaString(epic, MetaDesignStatus) }

// DesignRevision returns how many design kicks the epic has had.
func DesignRevision(epic *beads.Bead) int {
	n, _ := strconv.Atoi(metaString(epic, MetaDesignRevision))
	return n
}

// DesignGated reports whether Gate 1 is still in front of this epic: it has a
// design status that is not approved. Such an epic must not be decomposed.
func DesignGated(epic *beads.Bead) bool {
	st := DesignStatus(epic)
	return st != "" && st != DesignStatusApproved
}

// setDesignStatus writes design_status on the epic.
func setDesignStatus(store *beads.Store, epicID, status string) error {
	return store.SetMetadata(epicID, MetaDesignStatus, status)
}

// recordDesignKick bumps the revision counter, stamps the kick time, sets the
// status to requested, and clears the label-absent flag.
func recordDesignKick(store *beads.Store, epicID string, now time.Time) error {
	return store.Update(epicID, func(b *beads.Bead) {
		if b.Metadata == nil {
			b.Metadata = make(map[string]interface{})
		}
		n, _ := strconv.Atoi(metaString(b, MetaDesignRevision))
		b.Metadata[MetaDesignRevision] = strconv.Itoa(n + 1)
		b.Metadata[MetaDesignKickedAt] = now.UTC().Format(time.RFC3339)
		b.Metadata[MetaDesignStatus] = DesignStatusRequested
		delete(b.Metadata, MetaDesignLabelAbsent)
	})
}

// ApproveDesign marks the epic's design approved so Gate 2 can proceed. It is
// what the label path calls when it sees the approval label, and what the
// dashboard's owner-gated "approve design" calls after applying that label.
func ApproveDesign(store *beads.Store, epicID string) error {
	epic, err := loadEpic(store, epicID)
	if err != nil {
		return err
	}
	if DesignStatus(epic) == "" {
		return fmt.Errorf("planning: epic %s has no design to approve", epicID)
	}
	return setDesignStatus(store, epicID, DesignStatusApproved)
}

// RequestDesign puts an epic into the design step from outside the label loop
// (the dashboard's "design first" affordance). It only sets the status; the
// label path sends the kick on the next cycle once it sees the design label the
// caller applied, so there is exactly one code path that talks to the architect.
func RequestDesign(store *beads.Store, epicID string) error {
	epic, err := loadEpic(store, epicID)
	if err != nil {
		return err
	}
	if !DecomposePending(epic) {
		return fmt.Errorf("planning: epic %s is already decomposed; nothing to design", epicID)
	}
	if st := DesignStatus(epic); st != "" && st != DesignStatusNeedsHuman {
		return nil // already in the design step
	}
	return store.Update(epicID, func(b *beads.Bead) {
		if b.Metadata == nil {
			b.Metadata = make(map[string]interface{})
		}
		b.Metadata[MetaDesignStatus] = DesignStatusQueued
		delete(b.Metadata, MetaDesignRevision)
		delete(b.Metadata, MetaDesignKickedAt)
		delete(b.Metadata, MetaDesignLabelAbsent)
	})
}

// countDesignsInFlight counts epics in the store currently waiting on a
// posted design (requested, not yet approved) — the concurrency cap's basis.
func countDesignsInFlight(store *beads.Store) int {
	n := 0
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeEpic && DesignStatus(b) == DesignStatusRequested {
			n++
		}
	}
	return n
}

// BuildDesignPrompt is the architect's Gate 1 kick: read the issue, write the
// design, post it on the issue as a comment through the gh wrapper (which
// relays it server-side), and stop — do NOT decompose, do NOT create beads.
// The footer tells a newcomer reading the design what happens next, which is
// the whole point of doing the design in public.
func BuildDesignPrompt(epic *beads.Bead, cfg DesignConfig, revision int) string {
	var b strings.Builder
	b.WriteString("DESIGN REQUEST — write a design for this epic and post it on the issue. Do not implement it and do not break it into tasks yet.\n\n")
	fmt.Fprintf(&b, "EPIC ID: %s\n", epic.ID)
	fmt.Fprintf(&b, "TITLE: %s\n", epic.Title)
	if url := epicIssueURL(epic); url != "" {
		fmt.Fprintf(&b, "ISSUE: %s\n", url)
	}
	if revision > 1 {
		fmt.Fprintf(&b, "REVISION: %d — a maintainer re-applied the `%s` label after your previous design. Read the newest comments on the issue for their feedback and address it.\n", revision, cfg.DesignLabelOrDefault())
	}
	b.WriteString("\nSteps:\n")
	b.WriteString("0. Read the full issue and every comment on it (`gh issue view <url> --comments`). The discussion is the brief.\n")
	b.WriteString("1. Write the design: problem framing, options considered (at least two), recommended approach and why, phased implementation, blast radius, what is explicitly excluded, open questions.\n")
	b.WriteString("2. Keep it readable by someone who has never opened this codebase — that reader is the reviewer.\n")
	if revision > 1 {
		fmt.Fprintf(&b, "3. Title the comment `## Design (revision %d)` and open with a short *What changed since the last revision* list.\n", revision)
	} else {
		b.WriteString("3. Title the comment `## Design`.\n")
	}
	fmt.Fprintf(&b, "4. End the comment with exactly this footer:\n\n---\n_To approve this design, apply the `%s` label to the issue. To ask for changes, reply on this thread and re-apply the `%s` label. Once approved, the hive breaks the design into tasks for a second review before any work starts._\n\n",
		cfg.ApprovedLabelOrDefault(), cfg.DesignLabelOrDefault())
	b.WriteString("5. Post it with `gh issue comment <url> --body-file <file>` (the hive relays the comment for you).\n")
	b.WriteString("6. Do NOT open PRs, do NOT run `bd decompose`, do NOT create beads. The design is the deliverable; a human decides what happens next.\n")
	return b.String()
}

// DesignPlanResult counts what one label pass did for Gate 1.
type DesignPlanResult struct {
	// DesignKicked counts design (or revision) prompts sent this pass.
	DesignKicked int
	// DesignQueued counts epics held back by the concurrency cap.
	DesignQueued int
	// DesignWaiting counts epics with a design posted, waiting on a human.
	DesignWaiting int
	// DesignApproved counts epics whose approval label was first seen this pass.
	DesignApproved int
	// DesignNeedsHuman counts epics past the revision cap.
	DesignNeedsHuman int
}

// DesignSink receives Gate 1 events for audit/logging.
type DesignSink interface {
	// KickedDesign is called when a design (revision ≥ 1) prompt was sent.
	KickedDesign(epic *beads.Bead, revision int)
	// ApprovedDesign is called once when the approval label is first seen.
	ApprovedDesign(epic *beads.Bead)
	// DesignNeedsHuman is called once when the revision cap is hit.
	DesignNeedsHuman(epic *beads.Bead, revisions int)
}

// stepDesign advances one epic through Gate 1 for this eval cycle. It returns
// true when the epic is still gated (the caller must NOT decompose it) and
// false when Gate 1 is passed or was never in play. inFlight is the live
// count of requested designs, updated in place when a kick is sent.
func stepDesign(store *beads.Store, kicker DecomposeKicker, issue github.Issue, epic *beads.Bead, cfg DesignConfig, sink DesignSink, res *DesignPlanResult, inFlight *int) bool {
	labeled := HasDesignLabel(issue, cfg)
	approved := HasDesignApprovedLabel(issue, cfg)
	st := DesignStatus(epic)

	// Nothing to do: never in the design step and no design label.
	if st == "" && !labeled {
		return false
	}
	// Approval wins only after a design has actually been requested/posted. If
	// someone applies both labels at once, still ask the architect for the
	// design first; Gate 1 is not decorative.
	if approved {
		if st == DesignStatusApproved {
			return false
		}
		if st == DesignStatusRequested || st == DesignStatusNeedsHuman {
			_ = setDesignStatus(store, epic.ID, DesignStatusApproved)
			res.DesignApproved++
			if sink != nil {
				sink.ApprovedDesign(epic)
			}
			return false
		}
	}
	switch st {
	case DesignStatusApproved:
		return false
	case DesignStatusNeedsHuman:
		res.DesignNeedsHuman++
		return true
	case DesignStatusRequested:
		if !labeled {
			// Label removed after the design was posted: remember it so a
			// re-apply reads as "revise", and keep waiting.
			_ = store.SetMetadata(epic.ID, MetaDesignLabelAbsent, "true")
			res.DesignWaiting++
			return true
		}
		if metaString(epic, MetaDesignLabelAbsent) != "true" {
			res.DesignWaiting++
			return true
		}
		// Re-applied: a revision was requested.
		if DesignRevision(epic) >= cfg.maxRevisions() {
			_ = setDesignStatus(store, epic.ID, DesignStatusNeedsHuman)
			res.DesignNeedsHuman++
			if sink != nil {
				sink.DesignNeedsHuman(epic, DesignRevision(epic))
			}
			return true
		}
		return kickDesign(store, kicker, epic, cfg, sink, res)
	default: // "" with the label, or queued
		if !labeled {
			res.DesignQueued++
			return true
		}
		if *inFlight >= cfg.maxConcurrent() {
			if st != DesignStatusQueued {
				_ = setDesignStatus(store, epic.ID, DesignStatusQueued)
			}
			res.DesignQueued++
			return true
		}
		if kickDesign(store, kicker, epic, cfg, sink, res) {
			*inFlight++
		}
		return true
	}
}

// kickDesign sends the design prompt (respecting the architect's pause) and
// records it. Returns true (still gated) in every case; a paused/absent
// architect simply leaves the epic where it is for the next cycle.
func kickDesign(store *beads.Store, kicker DecomposeKicker, epic *beads.Bead, cfg DesignConfig, sink DesignSink, res *DesignPlanResult) bool {
	if kicker == nil || kicker.IsPaused(ArchitectAgentName) {
		res.DesignQueued++
		return true
	}
	rev := DesignRevision(epic) + 1
	if err := kicker.SendKick(ArchitectAgentName, BuildDesignPrompt(epic, cfg, rev)); err != nil {
		res.DesignQueued++
		return true
	}
	_ = recordDesignKick(store, epic.ID, decomposeNow())
	res.DesignKicked++
	if sink != nil {
		sink.KickedDesign(epic, rev)
	}
	return true
}

// AutoApproveDrafts approves every decomposed draft plan in the store (Gate 2
// off). It is the governor-side enforcement of the ACMM pack's
// plan_auto_approve knob (RFC hivecommons/hive#7993 §4): the architect runs
// `bd decompose` inside its own container and cannot know the hive's level, so
// the eval cycle applies the pack's decision after the children land. Epics
// still in the design step or still pending decomposition are untouched.
// Returns the epic IDs it approved.
func AutoApproveDrafts(store *beads.Store) []string {
	if store == nil {
		return nil
	}
	var out []string
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type != beads.TypeEpic || b.Meta(MetaPlanStatus) != PlanStatusDraft {
			continue
		}
		if DecomposePending(b) || DesignGated(b) {
			continue
		}
		if err := ApprovePlan(store, b.ID); err == nil {
			out = append(out, b.ID)
		}
	}
	return out
}
