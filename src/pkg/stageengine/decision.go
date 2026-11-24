package stageengine

import (
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Decision is one derivation's output: a built-in stage or None and why.
// None clears the built-in stage, including for an unnumbered model state.
type Decision struct {
	Stage stagelabels.Stage
	// StageLabel is the selected model state's own spelling of Stage's
	// label (StateLifecycle.StageLabel, hivecommons/hive#19611); empty
	// means the built-in label. Label renders the one written.
	StageLabel string
	// ModelState is the selected custom workflow state, including states
	// that do not have a built-in stage number.
	ModelState string
	// ModelProfile is the immutable profile used for this decision.
	ModelProfile *workflowmodel.Profile
	Role         stagelabels.Role
	Kind         authorkind.Kind
	// KindLabels are the configured author/<kind> label names the writer
	// plans against; zero means the authorkind defaults.
	KindLabels authorkind.Names
	DCO        stagelabels.DCOState
	// MergeCommit is the pass's merge-method verdict (`merge-method/merge`).
	MergeCommit stagelabels.MergeCommitState
	Stalled     bool
	Terminal    bool // the selected state is terminal or the item closed/merged/left
	// Covered is set on an issue whose stage is 040 because an open PR the
	// lanes have staged at 040 or later covers it: the dwell clock then
	// belongs to the PR and the sweep does not judge the issue stalled.
	Covered bool
	// Progress is set when the stage fell in number but the move is
	// forward motion the Store must not book as a regression: an issue
	// leaving 040 for 010 because the PR covering it merged and addressed
	// part of it (stagelabels.PartiallyAddressedLabel).
	Progress bool
	Reason   string             // stored on the Record, rendered in comments/tracker
	Human    *stagelabels.Route // set when the stage is a hand-off (085/090)
	// Abstain means the deriver has no opinion: nothing it saw classifies
	// the item (today's stageRankNone), so the reconciler leaves the labels
	// and the Store untouched. It is distinct from Stage == None, which is a
	// positive "no stage" that clears the hive-owned labels. Additive to the
	// ADR-0025 shape, which has no way to say "skip" otherwise.
	Abstain bool
	// Clear names hive-stamped labels outside the stage families that the
	// derived stage has made stale, removed in the same label write
	// (stagelabels.Desired.Clear): the reporter-trust awaiting label on an
	// issue whose stage settles triage (hivecommons/hive#14586).
	Clear []string
}

// Desired renders the Decision as the full hive-owned label set the writer
// should make the item carry.
func (d Decision) Desired() stagelabels.Desired {
	want := stagelabels.Desired{Stage: d.Stage, StageLabel: d.StageLabel, Role: d.Role, Kind: d.Kind, KindLabels: d.KindLabels, Stalled: d.Stalled, DCO: d.DCO, MergeCommit: d.MergeCommit, Clear: d.Clear}
	modelDesired(&want, d.ModelProfile, d.ModelState)
	return want
}

// Label is the stage label the writer puts on the item: the model state's
// own spelling when it declares one, else the built-in label ("" for None).
func (d Decision) Label() string { return d.Stage.LabelAs(d.StageLabel) }

// abstain is the Decision for "no opinion" with a reason for the log.
func abstain(reason string) Decision { return Decision{Abstain: true, Reason: reason} }
