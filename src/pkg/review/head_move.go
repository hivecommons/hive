package review

import (
	"strconv"
	"strings"
)

// Dispatch rows across a head move (hivecommons/hive#15823).
//
// A pending row names the head its perspective was dispatched against. When
// the PR is pushed before the verdict lands, pruneState drops the row (its
// head is no longer open) and the perspective is owed again at the new head:
// a second reviewer session for a change that, after a rebase or a
// merge-from-base, is the same patch. These helpers move the row to the new
// head instead, so the in-flight review keeps counting.

// RebindPendingHead rewrites the pending rows for one PR perspective from
// oldHead to newHead, returning the updated state and whether any row
// changed. Rows already at newHead are left alone; a row at oldHead whose
// perspective already has a row at newHead is dropped rather than duplicated.
func RebindPendingHead(state DispatchState, repo string, number int, perspective Perspective, oldHead, newHead string) (DispatchState, bool) {
	oldHead, newHead = strings.TrimSpace(oldHead), strings.TrimSpace(newHead)
	if oldHead == "" || newHead == "" || SameHeadSHA(oldHead, newHead) {
		return state, false
	}
	atNew := map[Perspective]bool{}
	for _, p := range state.Pending {
		if samePRHead(p.Repo, p.Number, p.HeadSHA, repo, number, newHead) {
			atNew[p.Perspective] = true
		}
	}
	changed := false
	out := make([]PendingReview, 0, len(state.Pending))
	for _, p := range state.Pending {
		if (perspective != "" && p.Perspective != perspective) || !samePRHead(p.Repo, p.Number, p.HeadSHA, repo, number, oldHead) {
			out = append(out, p)
			continue
		}
		changed = true
		if atNew[p.Perspective] {
			continue
		}
		p.HeadSHA = newHead
		out = append(out, p)
	}
	if !changed {
		return state, false
	}
	state.Pending = out
	return state, true
}

// HeadMoveRebind is one pending row the planner carried from the head it was
// dispatched against to the PR's current head.
type HeadMoveRebind struct {
	Repo        string
	Number      int
	From        string
	To          string
	Perspective Perspective
	Agent       string
}

// CarryPendingAcrossHeadMoves rewrites, for every open PR whose head has
// moved since a pending row was dispatched, that row to the current head when
// samePatch(repo, number, oldHead, newHead) says the two heads carry the same
// change. samePatch is asked once per (PR, old head). Rows for heads that
// moved to a different patch are left for pruneState to drop.
func CarryPendingAcrossHeadMoves(state DispatchState, prs []PullRequest, org string, samePatch func(repo string, number int, oldHead, newHead string) bool) (DispatchState, []HeadMoveRebind) {
	if samePatch == nil || len(state.Pending) == 0 {
		return state, nil
	}
	current := map[string]string{}
	for _, pr := range prs {
		if pr.Number <= 0 || strings.TrimSpace(pr.HeadSHA) == "" {
			continue
		}
		current[strings.ToLower(fullRepoName(pr.Repo, org))+"#"+strconv.Itoa(pr.Number)] = strings.TrimSpace(pr.HeadSHA)
	}
	verdicts := map[string]bool{}
	var rebinds []HeadMoveRebind
	for _, p := range state.Pending {
		head, open := current[strings.ToLower(p.Repo)+"#"+strconv.Itoa(p.Number)]
		if !open || SameHeadSHA(p.HeadSHA, head) || strings.TrimSpace(p.HeadSHA) == "" {
			continue
		}
		key := strings.ToLower(p.Repo) + "#" + strconv.Itoa(p.Number) + "@" + p.HeadSHA
		same, asked := verdicts[key]
		if !asked {
			same = samePatch(p.Repo, p.Number, p.HeadSHA, head)
			verdicts[key] = same
		}
		if !same {
			continue
		}
		rebinds = append(rebinds, HeadMoveRebind{Repo: p.Repo, Number: p.Number, From: p.HeadSHA, To: head, Perspective: p.Perspective, Agent: p.Agent})
	}
	for _, r := range rebinds {
		state, _ = RebindPendingHead(state, r.Repo, r.Number, r.Perspective, r.From, r.To)
	}
	return state, rebinds
}
