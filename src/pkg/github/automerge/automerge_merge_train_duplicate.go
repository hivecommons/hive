package automerge

import (
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
func (c *Engine) planDuplicateHolds(plan *mergeTrainPlan, lane, owner, repo, displayRepo string, prs []*gh.PullRequest, prefilter func(*gh.PullRequest) string) []*gh.PullRequest {
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
