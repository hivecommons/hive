// Package hivedefault generates the `hive-default` workflow model: Hive's
// own lifecycle expressed in the workflow-model schema
// (hivecommons/hive#17620). Nothing here is hand-maintained: states come
// from stagelabels.Table, transitions from stageengine.DiagramEdges and
// the columns from the Activity Monitor's layout constants, so the model
// is the schema's conformance test — if it cannot express Hive's graph,
// the schema is not done.
//
// The model declares derivation mode builtin: an item's state is still
// decided by the Go derivers, and DeriveFromModel only checks that what
// they produce is a state the model knows. That is what keeps the stage
// goldens byte-for-byte and the v5/v6 monitor pixel-identical for
// repositories that bind nothing.
package hivedefault

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Name is the model's metadata.name; Version its spec.meta.version. The
// version is bumped by hand when the generator's output changes shape.
const (
	Name    = "hive-default"
	Version = "1"
)

// Column ids. These are the Activity Monitor's columns in order; the
// anchor numbers mirror STAGE_TRACKER_PATH = [0, 10, 20, 30, 50, 80] plus
// the terminal Merged cap (agent-now.js, tracker-motion.js).
const (
	ColumnOpen         = "open"
	ColumnActionable   = "actionable"
	ColumnClaimed      = "claimed"
	ColumnPRInProgress = "pr-in-progress"
	ColumnCIReview     = "ci-review"
	ColumnMergeQueue   = "merge-queue"
	ColumnMerged       = "merged"
)

// Terminal state ids (no stage label; observed, not written).
const (
	StateOpen   = "open"
	StateMerged = "merged"
	StateClosed = "closed"
)

// Role ids.
const (
	RoleMaintainer  = "maintainer"
	RoleAuthor      = "author"
	RoleReviewSwarm = "review-swarm"
	RoleHiveAgent   = "hive-agent"
	RoleCI          = "ci"
)

// Gate and event ids.
const (
	GateMaintainerDecision = "maintainer-decision"
	GateMaintainerApprove  = "maintainer-approve"
	GateAuthorPush         = "author-push"
	EventOpened            = "opened"
	EventMerged            = "merged"
	EventClosed            = "closed"
	EventStageChange       = "stage-change"
)

// columnAnchors maps a stage number to the column that lights for it,
// mirroring stageTrackerColumn / tracker-row-anchor.js. Every Table
// number must be here; Generate panics otherwise.
var columnAnchors = map[int]string{
	10: ColumnActionable, 15: ColumnActionable, 17: ColumnActionable, 18: ColumnActionable, 45: ColumnActionable,
	20: ColumnClaimed,
	30: ColumnPRInProgress, 40: ColumnPRInProgress,
	50: ColumnCIReview, 60: ColumnCIReview, 70: ColumnCIReview, 71: ColumnCIReview, 72: ColumnCIReview, 74: ColumnCIReview,
	80: ColumnMergeQueue, 85: ColumnMergeQueue, 90: ColumnMergeQueue, 95: ColumnMergeQueue, 100: ColumnMergeQueue,
}

// replies are the human-gate verbs the monitor offers in a stage today
// (tracker-actions.js); declared here so a model can carry its own.
var replies = map[int][]string{
	15: {"accept-triage", "decide:close-issue"},
	17: {"decide:approve", "decide:close-superseded"},
	45: {"decide:delegate", "decide:approve", "decide:direction", "decide:close-superseded", "decide:close-wontfix", "hold"},
	85: {"decide:delegate", "decide:approve", "decide:direction", "decide:merge-now", "decide:queue-merge", "decide:adopt", "decide:close-superseded", "hold"},
	90: {"decide:approve", "decide:merge-now"},
}

// owner says who moves work out of a stage.
func owner(d stagelabels.Definition) workflowmodel.Owner {
	switch d.Stage {
	case stagelabels.TriageNeeded, stagelabels.PossiblyExisting, stagelabels.OpenForDiscussion, stagelabels.IssueNeedsDecision,
		stagelabels.NeedsDecision, stagelabels.ReadyForHumanMerge, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor:
		return workflowmodel.OwnerHuman
	case stagelabels.ReviewPending, stagelabels.Reviewed, stagelabels.AwaitingMergeAdmission, stagelabels.PreMergeSoak, stagelabels.MergeQueue:
		return workflowmodel.OwnerExternalService
	case stagelabels.CoveredByPR:
		return workflowmodel.OwnerExternalObservation
	}
	return workflowmodel.OwnerRegisteredAgent
}

// StateID is the model state id for a stage: its slug, suffixed with the
// item kind where two stages share a slug (045 and 085 needs-decision).
func StateID(s stagelabels.Stage) string {
	switch s {
	case stagelabels.None:
		return ""
	case stagelabels.IssueNeedsDecision:
		return "needs-decision-issue"
	case stagelabels.NeedsDecision:
		return "needs-decision-pr"
	}
	return s.Slug()
}

// Generate builds the hive-default model from the live stage table.
func Generate() *workflowmodel.Model {
	m := workflowmodel.New(Name, workflowmodel.Scope{Forge: "github.com", Repository: "*"})
	m.Spec.Meta = workflowmodel.Meta{Version: Version, Author: "hivecommons", Description: "Hive's built-in issue and pull-request lifecycle, generated from the stage table", License: "Apache-2.0", Source: workflowmodel.SourceBuiltIn, Provenance: workflowmodel.ProvenanceBuiltIn}
	m.Spec.Initial = StateOpen
	m.Spec.Derivation = workflowmodel.Derivation{Mode: workflowmodel.DerivationBuiltin}
	m.Spec.Labels.Prefix = stagelabels.Prefix

	order := []workflowmodel.Column{
		{ID: ColumnOpen, Label: "Open", Hint: "Filed; not yet staged", States: []string{StateOpen}},
		{ID: ColumnActionable, Label: "Actionable", Hint: "Triaged or parked on a person"},
		{ID: ColumnClaimed, Label: "Claimed", Hint: "An agent or contributor holds it"},
		{ID: ColumnPRInProgress, Label: "PR in progress", Hint: "A PR is under way or open"},
		{ID: ColumnCIReview, Label: "CI / Review", Hint: "Verdict and checks on the current head"},
		{ID: ColumnMergeQueue, Label: "Merge queue", Hint: "Admitted; waiting on soak, tide or a human merge"},
		{ID: ColumnMerged, Label: "Merged", Hint: "Done", Terminal: true, States: []string{StateMerged, StateClosed}},
	}
	cols := map[string]*workflowmodel.Column{}
	for i := range order {
		cols[order[i].ID] = &order[i]
	}

	m.Spec.States = append(m.Spec.States, workflowmodel.State{ID: StateOpen, Label: "Open", Owner: workflowmodel.OwnerExternalObservation, Origin: workflowmodel.OriginHiveDefault, Lifecycle: workflowmodel.StateLifecycle{Column: ColumnOpen}})

	table := append([]stagelabels.Definition(nil), stagelabels.Table...)
	sort.SliceStable(table, func(i, j int) bool { return table[i].Number < table[j].Number })
	for _, d := range table {
		id := StateID(d.Stage)
		col, ok := columnAnchors[d.Number]
		if !ok {
			panic(fmt.Sprintf("hivedefault: stage %d has no column anchor", d.Number))
		}
		var kinds []workflowmodel.ItemKind
		if d.Issue {
			kinds = append(kinds, workflowmodel.KindIssue)
		}
		if d.PR {
			kinds = append(kinds, workflowmodel.KindPR)
		}
		st := workflowmodel.State{ID: id, Label: label(d), Owner: owner(d), Origin: workflowmodel.OriginHiveDefault, Lifecycle: workflowmodel.StateLifecycle{Column: col, Number: d.Number, Kinds: kinds, Replies: replies[d.Number]}}
		if d.Dwell > 0 {
			st.Lifecycle.Waiting = []string{"stalled after " + d.Dwell.String()}
		}
		m.Spec.States = append(m.Spec.States, st)
		cols[col].States = append(cols[col].States, id)
		m.Spec.Labels.Entries = append(m.Spec.Labels.Entries, workflowmodel.LabelEntry{Name: d.Stage.Label(), Description: d.Description, State: id})
	}
	m.Spec.States = append(m.Spec.States,
		workflowmodel.State{ID: StateMerged, Label: "Merged", Owner: workflowmodel.OwnerExternalObservation, Type: workflowmodel.StateFinal, Origin: workflowmodel.OriginHiveDefault, Lifecycle: workflowmodel.StateLifecycle{Column: ColumnMerged, Kinds: []workflowmodel.ItemKind{workflowmodel.KindPR}}},
		workflowmodel.State{ID: StateClosed, Label: "Closed", Owner: workflowmodel.OwnerExternalObservation, Type: workflowmodel.StateFinal, Origin: workflowmodel.OriginHiveDefault, Lifecycle: workflowmodel.StateLifecycle{Column: ColumnMerged}},
	)
	m.Spec.Columns = order

	m.Spec.Roles = []workflowmodel.RoleDef{
		{ID: RoleMaintainer, Label: "Maintainer", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "write"}, Human: true},
		{ID: RoleAuthor, Label: "Author", Kind: workflowmodel.RoleAuthorKind, Params: map[string]string{"kind": "human"}, Human: true},
		{ID: RoleReviewSwarm, Label: "Review swarm", Kind: workflowmodel.RoleBot, Params: map[string]string{"login": "hive"}},
		{ID: RoleHiveAgent, Label: "Hive agent", Kind: workflowmodel.RoleBot, Params: map[string]string{"login": "hive"}},
		{ID: RoleCI, Label: "CI", Kind: workflowmodel.RoleBot},
	}
	m.Spec.Gates = []workflowmodel.Gate{
		{ID: GateMaintainerDecision, ReleasedBy: RoleMaintainer, Reply: "/hive decision"},
		{ID: GateMaintainerApprove, ReleasedBy: RoleMaintainer, Reply: "/lgtm /approve", Conditions: []workflowmodel.Predicate{{Fact: "pr.checks", Op: workflowmodel.OpIs, Value: "green"}}},
		{ID: GateAuthorPush, ReleasedBy: RoleAuthor},
	}
	m.Spec.Events = []workflowmodel.Event{
		{ID: EventOpened, Label: "Item opened", Fact: workflowmodel.Predicate{Fact: "open", Op: workflowmodel.OpTrue}},
		{ID: EventMerged, Label: "PR merged", Fact: workflowmodel.Predicate{Fact: "merged", Op: workflowmodel.OpTrue}},
		{ID: EventClosed, Label: "Item closed unmerged", Fact: workflowmodel.Predicate{Fact: "open", Op: workflowmodel.OpFalse}},
		{ID: "claimed", Label: "Claim taken", Fact: workflowmodel.Predicate{Fact: "issue.claimed", Op: workflowmodel.OpTrue}},
		{ID: "covered", Label: "PR covers the issue", Fact: workflowmodel.Predicate{Fact: "issue.covered", Op: workflowmodel.OpTrue}},
		{ID: "checks-green", Label: "Checks green", Fact: workflowmodel.Predicate{Fact: "pr.checks", Op: workflowmodel.OpIs, Value: "green"}},
		{ID: "checks-red", Label: "Checks red", Fact: workflowmodel.Predicate{Fact: "pr.checks", Op: workflowmodel.OpIs, Value: "red"}},
		{ID: "review-approved", Label: "Verdict approve", Fact: workflowmodel.Predicate{Fact: "pr.review", Op: workflowmodel.OpIs, Value: "approved"}},
		{ID: "changes-requested", Label: "Changes requested", Fact: workflowmodel.Predicate{Fact: "pr.changes_requested", Op: workflowmodel.OpTrue}},
		{ID: "held", Label: "Hold label applied", Fact: workflowmodel.Predicate{Fact: "hold", Op: workflowmodel.OpTrue}},
		{ID: "parked", Label: "Park label applied", Fact: workflowmodel.Predicate{Fact: "park", Op: workflowmodel.OpTrue}},
		{ID: EventStageChange, Label: "Stage re-derived", Fact: workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: stagelabels.Prefix + "*"}},
	}
	m.Spec.Agents = []workflowmodel.AgentGrant{
		{Profile: "scanner", States: []string{StateID(stagelabels.Actionable), StateID(stagelabels.TriageNeeded), StateID(stagelabels.OpenForDiscussion)}, Produces: []string{"claim"}},
		{Profile: "worker", States: []string{StateID(stagelabels.Claimed), StateID(stagelabels.PRInProgress), StateID(stagelabels.Fixing), StateID(stagelabels.AdoptedByHive)}, Produces: []string{"pull-request", "push"}},
		{Profile: "review-swarm", States: []string{StateID(stagelabels.ReviewPending)}, Produces: []string{"review"}},
	}

	// Transitions from the expected-edge table; gates on the edges a human
	// releases.
	for _, e := range stageengine.DiagramEdges() {
		t := workflowmodel.Transition{From: StateID(e.From), To: StateID(e.To), On: EventStageChange, Note: fmt.Sprintf("%s → %s", e.From, e.To)}
		switch {
		case e.From == stagelabels.IssueNeedsDecision || e.From == stagelabels.NeedsDecision || e.From == stagelabels.PossiblyExisting:
			t.GateRef = GateMaintainerDecision
		case e.From == stagelabels.ReadyForHumanMerge && e.To == stagelabels.MergeQueue:
			t.GateRef = GateMaintainerApprove
		case e.From == stagelabels.NeedsAuthor || e.From == stagelabels.ReturnedToContributor:
			t.GateRef = GateAuthorPush
		}
		m.Spec.Transitions = append(m.Spec.Transitions, t)
	}
	// Intake and terminal edges, which the diagram draws as [*].
	m.Spec.Transitions = append(m.Spec.Transitions,
		workflowmodel.Transition{From: StateOpen, To: StateID(stagelabels.Actionable), On: EventOpened, Note: "triaged intake"},
		workflowmodel.Transition{From: StateOpen, To: StateID(stagelabels.TriageNeeded), On: EventOpened, Note: "reporter-trust gate"},
		workflowmodel.Transition{From: StateOpen, To: StateID(stagelabels.ReviewPending), On: EventOpened, Note: "PR opened"},
	)
	for _, d := range table {
		switch d.Stage {
		case stagelabels.MergeQueue:
			m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: StateID(d.Stage), To: StateMerged, On: EventMerged, GateRef: GateMaintainerApprove, Note: "tide / lane merges"})
		case stagelabels.ReadyForHumanMerge:
			m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: StateID(d.Stage), To: StateMerged, On: EventMerged, GateRef: GateMaintainerApprove, Note: "maintainer merges"})
		}
		m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: StateID(d.Stage), To: StateClosed, On: EventClosed})
	}
	return m
}

// label is the display label for a stage: its slug, humanised.
func label(d stagelabels.Definition) string {
	s := strings.ReplaceAll(d.Slug, "-", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// Profile generates and compiles the model. It panics on a compile
// error: the generator and the schema ship together, so a refusal is a
// programming error, not an operator one.
func Profile() *workflowmodel.Profile {
	p, diags, err := workflowmodel.Compile(Generate())
	if err != nil {
		panic(fmt.Sprintf("hivedefault: %v (%d diagnostics)", err, len(diags)))
	}
	return p
}
