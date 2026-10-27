package stageengine

import (
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Decision is one derivation's output: exactly one stage (None = leave the
// pipeline, remove every hive-owned label) and why.
type Decision struct {
	Stage stagelabels.Stage
	Role  stagelabels.Role
	Kind  authorkind.Kind
	// KindLabels are the configured author/<kind> label names the writer
	// plans against; zero means the authorkind defaults.
	KindLabels authorkind.Names
	DCO        stagelabels.DCOState
	// MergeCommit is the pass's merge-method verdict (`merge-method/merge`).
	MergeCommit stagelabels.MergeCommitState
	Stalled     bool
	Terminal    bool // Stage == None because the item closed/merged/left
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
}

// Desired renders the Decision as the full hive-owned label set the writer
// should make the item carry.
func (d Decision) Desired() stagelabels.Desired {
	return stagelabels.Desired{Stage: d.Stage, Role: d.Role, Kind: d.Kind, KindLabels: d.KindLabels, Stalled: d.Stalled, DCO: d.DCO, MergeCommit: d.MergeCommit}
}

// abstain is the Decision for "no opinion" with a reason for the log.
func abstain(reason string) Decision { return Decision{Abstain: true, Reason: reason} }
