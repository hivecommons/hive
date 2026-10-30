package automerge

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// autoMergeReasonMergeTrainDuplicate is the tick-stat bucket for a PR held
// out of the train because another open PR fixes the same issue.
const autoMergeReasonMergeTrainDuplicate = "merge-train-duplicate"

// autoMergeReasonMergeTrainClaimConflict buckets the candidates held by an
// undecided same-issue claim across lanes (hivecommons/hive#15810).
const autoMergeReasonMergeTrainClaimConflict = "merge-train-claim-conflict"

// mergeTrainDuplicateCandidate is one open PR declaring `Fixes #N`, as the
// one-PR-per-issue rule reads it (hivecommons/hive#15395).
type mergeTrainDuplicateCandidate struct {
	Number    int
	CreatedAt time.Time
	// Green: the head is not blocked (red required check, terminal hold,
	// permanent refusal) and GitHub does not report it conflicting.
	Green bool
}

// mergeTrainDuplicateHold is the losing side of a pair: Number is held as
// a duplicate of Winner for Issue.
type mergeTrainDuplicateHold struct {
	Number, Winner, Issue int
}

// mergeTrainDuplicateWinner picks the PR the queue keeps for one issue:
// a green head beats a red one; among equals the earlier-opened PR wins
// (lower number on a tie). So two green PRs keep the earlier; an earlier
// red one yields to a later green one.
func mergeTrainDuplicateWinner(cands []mergeTrainDuplicateCandidate) int {
	if len(cands) == 0 {
		return 0
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if mergeTrainDuplicateBetter(c, best) {
			best = c
		}
	}
	return best.Number
}

func mergeTrainDuplicateBetter(a, b mergeTrainDuplicateCandidate) bool {
	if a.Green != b.Green {
		return a.Green
	}
	if !a.CreatedAt.Equal(b.CreatedAt) && !a.CreatedAt.IsZero() && !b.CreatedAt.IsZero() {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.Number < b.Number
}

// mergeTrainDuplicateHolds groups the candidates by the issue each one
// closes and holds every PR but the winner per issue. A PR closing several
// issues is held on the first issue it loses. Holds come back sorted by
// PR number so the plan is stable across passes.
func mergeTrainDuplicateHolds(byIssue map[int][]mergeTrainDuplicateCandidate) []mergeTrainDuplicateHold {
	held := map[int]mergeTrainDuplicateHold{}
	issues := make([]int, 0, len(byIssue))
	for issue := range byIssue {
		issues = append(issues, issue)
	}
	sort.Ints(issues)
	for _, issue := range issues {
		cands := byIssue[issue]
		if len(cands) < 2 {
			continue
		}
		winner := mergeTrainDuplicateWinner(cands)
		for _, c := range cands {
			if c.Number == winner {
				continue
			}
			if _, done := held[c.Number]; !done {
				held[c.Number] = mergeTrainDuplicateHold{Number: c.Number, Winner: winner, Issue: issue}
			}
		}
	}
	out := make([]mergeTrainDuplicateHold, 0, len(held))
	for _, h := range held {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// mergeTrainClosedIssues reads the same-repo issues a PR's title and body
// declare closed (Fixes / Closes / Resolves #N).
func mergeTrainClosedIssues(pr *gh.PullRequest, displayRepo string) []int {
	var out []int
	for _, ref := range hgithub.ParseClaimedIssues(pr.GetTitle()+"\n"+pr.GetBody(), displayRepo) {
		if ref.Repo == "" || ref.Repo == displayRepo {
			out = append(out, ref.Issue)
		}
	}
	return out
}

// mergeTrainDuplicateReason is the stage reason the sweep records on a held
// duplicate: merge-train:duplicate-of#<winner>:<issue>/0:0/0:
func mergeTrainDuplicateReason(h mergeTrainDuplicateHold) string {
	return prstage.MergeTrain{State: prstage.MergeTrainStateDuplicateOf, Front: h.Winner, Position: h.Issue}.Reason()
}

// planDuplicateHolds runs the one-PR-per-issue rule over the lane's
// candidates before the trains are grouped: the held PRs leave the
// candidate list and the plan remembers who each one yields to.
func (c *Engine) planDuplicateHolds(ctx context.Context, plan *mergeTrainPlan, lane, owner, repo, displayRepo string, prs []*gh.PullRequest, prefilter func(*gh.PullRequest) string) []*gh.PullRequest {
	prs = c.planClaimConflicts(ctx, plan, owner, repo, displayRepo, prs, prefilter)
	byIssue := map[int][]mergeTrainDuplicateCandidate{}
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() <= 0 || !mergeTrainCandidate(prefilter(pr)) {
			continue
		}
		issues := mergeTrainClosedIssues(pr, displayRepo)
		if len(issues) == 0 {
			continue
		}
		cand := mergeTrainDuplicateCandidate{
			Number:    pr.GetNumber(),
			CreatedAt: pr.GetCreatedAt().Time,
			Green:     !mergeTrainDirty(pr.GetMergeableState()) && !c.mergeTrainHeadBlocked(lane, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA()),
		}
		for _, issue := range issues {
			byIssue[issue] = append(byIssue[issue], cand)
		}
	}
	holds := mergeTrainDuplicateHolds(byIssue)
	if len(holds) == 0 {
		return prs
	}
	plan.duplicates = map[int]mergeTrainDuplicateHold{}
	for _, h := range holds {
		plan.duplicates[h.Number] = h
		c.info("merge train: held as duplicate", "repo", displayRepo, "pr", h.Number, "winner", h.Winner, "issue", h.Issue, "lane", plan.lane)
	}
	kept := make([]*gh.PullRequest, 0, len(prs))
	for _, pr := range prs {
		if _, held := plan.duplicates[pr.GetNumber()]; !held {
			kept = append(kept, pr)
		}
	}
	return kept
}

// duplicateOf reports the PR number this one yields to, 0 when it is not
// held as a duplicate this pass.
func (p *mergeTrainPlan) duplicateOf(number int) (mergeTrainDuplicateHold, bool) {
	if p == nil || p.duplicates == nil {
		return mergeTrainDuplicateHold{}, false
	}
	h, ok := p.duplicates[number]
	return h, ok
}

// mergeTrainDuplicateSkip records the hold on a duplicate and returns the
// stage reason.
func (c *Engine) mergeTrainDuplicateSkip(plan *mergeTrainPlan, number int) string {
	h, _ := plan.duplicateOf(number)
	reason := mergeTrainDuplicateReason(h)
	c.debug("automerge skip", "repo", plan.displayRepo, "pr", number, "reason", reason, "lane", plan.lane, "winner", strconv.Itoa(h.Winner))
	return reason
}

// mergeTrainDuplicateOf reads a recorded lane reason back: the winning PR
// number when reason is the one-PR-per-issue hold, 0 otherwise. The 07x
// label pass uses it to leave a held duplicate alone — no nudge, no
// adoption (hivecommons/hive#15469).
func mergeTrainDuplicateOf(reason string) int {
	train, ok := prstage.ParseMergeTrain(reason)
	if !ok || train.State != prstage.MergeTrainStateDuplicateOf {
		return 0
	}
	return train.Front
}

// mergeTrainClaimConflict is a lane candidate held because another open PR
// of the repository — one this lane does not evaluate, so the one-PR-per-
// issue winner above could not see it — claims the same issue and nobody
// has decided which one lands (hivecommons/hive#15810).
type mergeTrainClaimConflict struct {
	Number, Other, Issue int
}

// mergeTrainClaimConflictReason is the stage reason recorded on a candidate
// held by an undecided same-issue claim:
// merge-train:claim-conflict#<other>:<issue>/0:0/0:
func mergeTrainClaimConflictReason(h mergeTrainClaimConflict) string {
	return prstage.MergeTrain{State: prstage.MergeTrainStateClaimConflict, Front: h.Other, Position: h.Issue}.Reason()
}

// mergeTrainAdoptionPair reports whether a or b declares that it supersedes
// the other (`Supersedes #N`): an adoption copy and its original are one
// change in flight, not two competing fixes.
func mergeTrainAdoptionPair(a, b *gh.PullRequest) bool {
	return hgithub.PRSupersedes(a.GetBody(), b.GetNumber()) || hgithub.PRSupersedes(b.GetBody(), a.GetNumber())
}

// rememberClaimants keeps the full open-PR list the legacy /pulls listing
// returned for fullRepo this pass, before any event narrowing, so a
// targeted (webhook-nudged) pass still sees every claimant.
func (c *Engine) rememberClaimants(fullRepo string, prs []*gh.PullRequest) {
	if c == nil {
		return
	}
	c.claimantsMu.Lock()
	defer c.claimantsMu.Unlock()
	if c.claimants == nil {
		c.claimants = map[string][]*gh.PullRequest{}
	}
	c.claimants[fullRepo] = slices.Clone(prs)
}

// claimantsForConflicts is the set of open PRs the claim-conflict gate
// groups — the whole repository, never the lane's event-narrowed list:
// the forge-state snapshot when one is wired, else the full /pulls list
// the lane's openPullRequests just fetched for this repo (remembered
// before narrowing to targets); the lane's own list only when neither is
// available (a bare Engine in tests).
func (c *Engine) claimantsForConflicts(ctx context.Context, owner, repo string, prs []*gh.PullRequest) []*gh.PullRequest {
	if c == nil {
		return prs
	}
	if c.openPRs == nil {
		c.claimantsMu.Lock()
		all, ok := c.claimants[owner+"/"+repo]
		c.claimantsMu.Unlock()
		if ok && len(all) > 0 {
			return all
		}
		return prs
	}
	all, skip, err := c.openPullRequests(ctx, owner, repo, nil)
	if err != nil || skip != "" || len(all) == 0 {
		return prs
	}
	return all
}

// planClaimConflicts holds every lane candidate whose closing issue another
// open PR of the repository also claims, unless a human decision is
// recorded for the candidate's head. Candidates of this lane that claim
// the same issue are left to the one-PR-per-issue winner rule, which sees
// them both; the gate exists for the pair the lanes split — the hive's
// own PR in the self lane and a trusted human's PR in the trusted lane
// both marched toward merge for hivecommons/hive#15773 because each lane
// saw only its own (hivecommons/hive#15810).
func (c *Engine) planClaimConflicts(ctx context.Context, plan *mergeTrainPlan, owner, repo, displayRepo string, prs []*gh.PullRequest, prefilter func(*gh.PullRequest) string) []*gh.PullRequest {
	candidate := map[int]bool{}
	for _, pr := range prs {
		if pr != nil && pr.GetNumber() > 0 && mergeTrainCandidate(prefilter(pr)) {
			candidate[pr.GetNumber()] = true
		}
	}
	if len(candidate) == 0 {
		return prs
	}
	byIssue := map[int][]*gh.PullRequest{}
	for _, pr := range c.claimantsForConflicts(ctx, owner, repo, prs) {
		if pr == nil || pr.GetNumber() <= 0 || (pr.State != nil && pr.GetState() != "open") {
			continue
		}
		for _, issue := range mergeTrainClosedIssues(pr, displayRepo) {
			byIssue[issue] = append(byIssue[issue], pr)
		}
	}
	held := map[int]mergeTrainClaimConflict{}
	for _, pr := range prs {
		n := pr.GetNumber()
		if !candidate[n] {
			continue
		}
		for _, issue := range mergeTrainClosedIssues(pr, displayRepo) {
			for _, other := range byIssue[issue] {
				o := other.GetNumber()
				if o == n || candidate[o] || mergeTrainAdoptionPair(pr, other) {
					continue
				}
				if _, ok := c.recordedDecisionFor(ctx, owner, repo, n, pr.GetBase().GetRef(), pr.GetHead().GetSHA()); ok {
					c.info("merge train: same-issue claim conflict decided; admitting", "repo", displayRepo, "pr", n, "other", o, "issue", issue, "lane", plan.lane)
					continue
				}
				held[n] = mergeTrainClaimConflict{Number: n, Other: o, Issue: issue}
				c.info("same-issue claim conflict undecided: #"+strconv.Itoa(n)+" vs #"+strconv.Itoa(o), "repo", displayRepo, "pr", n, "other", o, "issue", issue, "lane", plan.lane)
				break
			}
			if _, done := held[n]; done {
				break
			}
		}
	}
	if len(held) == 0 {
		return prs
	}
	plan.claimConflicts = held
	kept := make([]*gh.PullRequest, 0, len(prs))
	for _, pr := range prs {
		if _, h := held[pr.GetNumber()]; !h {
			kept = append(kept, pr)
		}
	}
	return kept
}

// claimConflictOf reports the open PR this candidate is held behind by an
// undecided same-issue claim, false when it is not held this pass.
func (p *mergeTrainPlan) claimConflictOf(number int) (mergeTrainClaimConflict, bool) {
	if p == nil || p.claimConflicts == nil {
		return mergeTrainClaimConflict{}, false
	}
	h, ok := p.claimConflicts[number]
	return h, ok
}

// mergeTrainClaimConflictSkip records the hold and returns the stage reason.
func (c *Engine) mergeTrainClaimConflictSkip(plan *mergeTrainPlan, number int) string {
	h, _ := plan.claimConflictOf(number)
	reason := mergeTrainClaimConflictReason(h)
	c.debug("automerge skip", "repo", plan.displayRepo, "pr", number, "reason", reason, "lane", plan.lane, "other", strconv.Itoa(h.Other))
	return reason
}
