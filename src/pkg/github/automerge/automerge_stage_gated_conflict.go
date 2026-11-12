package automerge

// A gated PR's merge conflict goes back to its author like any other.
//
// A sentinel alert or a hold-class label is a gate, not a stage
// (hivecommons/hive#17327): it rides on whatever stage the head's own
// progress earns. The lanes' prefilters return the gate reason
// (contributor-lane:sentinel-blocked, label:hold, …) before they fetch the
// PR, so they never read its mergeability, and the stage evidence's
// Conflict came only from a "conflicting" lane reason or a needs-rebase
// label. A held contributor PR that GitHub reported `dirty` was therefore
// routed as conflict-free: no 072, no nudge, no fix-queue entry, and on a
// green, reviewed head 090 ready-for-human-merge on a head GitHub would
// refuse to merge, while the hold guard told a person to rebase it by hand.
//
// The head's mergeable_state now comes from what the hive already read:
// the lane's own fetch of exactly this head when it made one (noteStagePR),
// otherwise the PR detail the enumeration's CI enrichment fetched for every
// held PR this cycle (hgithub.Client.CachedPRMergeableState). Neither costs
// a request here.

import (
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageMergeableStateReader is the optional transport hook that answers
// GitHub's mergeable_state for a PR head from the hive's PR detail cache
// (hgithub.Client.CachedPRMergeableState) without a request.
type stageMergeableStateReader interface {
	CachedPRMergeableState(repo string, number int, head string) (string, bool)
}

// stageLaneMergeableState is GitHub's mergeable_state for exactly obs's
// head as a lane's own fetch read it this cycle (noteStagePR). ok is false
// when no lane fetched this head, or GitHub had not computed the state yet
// (`unknown` says nothing).
func stageLaneMergeableState(obs stageObservation) (string, bool) {
	head := strings.TrimSpace(obs.HeadSHA)
	if head == "" || obs.Detail.MergeableStateHead != head {
		return "", false
	}
	s := strings.ToLower(strings.TrimSpace(obs.Detail.MergeableState))
	if s == "" || s == "unknown" {
		return "", false
	}
	return s, true
}

// stageObservationGated reports whether a gate stopped the lane (its own
// reason, or the stage reason it was carried into).
func stageObservationGated(obs stageObservation) bool {
	return stageengine.IsHeldGateReason(obs.LaneReason) || stageengine.IsHeldGateReason(obs.Reason)
}

// stageHeadDirty reports whether GitHub calls obs's head `dirty` (it
// conflicts with its base). A lane that fetched the head answers for it.
// A gated PR's lane fetched nothing, so its state is the PR detail the
// enumeration read for the held PR this cycle (stageMergeableStateReader);
// with no computed state there either — the cached detail expired, or a
// webhook dropped it before the next enumeration refetched it — a head the
// stage already returned to its author for exactly this conflict stays
// conflicting, so the route does not flap 072 → 090 → 072 while nobody has
// asked GitHub again. A computed state always wins over that memory.
func (c *Engine) stageHeadDirty(key string, obs stageObservation) bool {
	if state, ok := stageLaneMergeableState(obs); ok {
		return state == "dirty"
	}
	head := strings.TrimSpace(obs.HeadSHA)
	if head == "" || !stageObservationGated(obs) || c == nil {
		return false
	}
	if reader, ok := c.transport.(stageMergeableStateReader); ok {
		if state, ok := reader.CachedPRMergeableState(obs.Owner+"/"+obs.Repo, obs.Number, head); ok {
			return state == "dirty"
		}
	}
	rec, ok := c.stageStoreOrShared().Get(key)
	return ok && rec.ActivityHead == head && stagelabels.AuthorBlocked(rec.Stage) && rec.Reason == stagelabels.LaneReasonConflicting
}

// stageGatedConflict reports a dirty head the gate kept the lane from
// reading: the gate reason stands where "conflicting" would have, under
// the same rule humanConflict applies to the lane's own reason — the stage
// reason for any author (a decision the classifier chose over the gate
// keeps outranking the conflict, hivecommons/hive#14607), and for a
// person's own PR also the lane's reason (hivecommons/hive#15202).
func stageGatedConflict(obs stageObservation, dirty bool) bool {
	if !dirty {
		return false
	}
	return stageengine.IsHeldGateReason(obs.Reason) ||
		obs.AuthorKind == authorkind.Human && stageengine.IsHeldGateReason(obs.LaneReason)
}
