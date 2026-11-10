package automerge

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	// AtGate: the lane last saw this exact head at the merge gate — CI
	// green, every required verdict in (mergeTrainAtGate). Only such heads
	// are picked between (hivecommons/hive#17439).
	AtGate bool
	// Terminal: the lane last saw this exact head blocked for good
	// (mergeTrainHeadTerminal). A remembered winner in this state lets
	// the issue go; a merely red check does not.
	Terminal bool
}

// mergeTrainDuplicateHold is the losing side of a pair: Number is held as
// a duplicate of Winner for Issue.
type mergeTrainDuplicateHold struct {
	Number, Winner, Issue int
}

// mergeTrainDuplicateWinner picks the best of the candidates: a green head
// beats a red one; among equals the earlier-opened PR wins (lower number
// on a tie). mergeTrainIssueWinner asks it about the heads at the merge
// gate only.
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

// mergeTrainIssueWinner is the PR one issue's group yields to
// (hivecommons/hive#17439): the winner remembered from an earlier pass
// while it is still in the group and not blocked for good — a check that
// is red mid-run never hands the issue to the other PR — else the best
// of the members at the merge gate; 0 while no member is at the gate, so
// the first PR to reach the merge point takes the issue.
func mergeTrainIssueWinner(cands []mergeTrainDuplicateCandidate, remembered int) int {
	var atGate []mergeTrainDuplicateCandidate
	for _, c := range cands {
		if remembered > 0 && c.Number == remembered && !c.Terminal {
			return remembered
		}
		if c.AtGate {
			atGate = append(atGate, c)
		}
	}
	return mergeTrainDuplicateWinner(atGate)
}

// mergeTrainDuplicatePicks runs the one-PR-per-issue rule over every issue
// two or more candidates close. remembered is the winner the lane kept per
// issue on an earlier pass. winners names each contested issue's winner
// (0: nobody at the gate yet); holds are the members at the merge gate
// that yield to a winner, sorted by PR number (a PR closing several
// issues is held on the first it loses); turns are, per member short of
// the gate, the issues whose turn it must claim at the merge point before
// it may merge (hivecommons/hive#17439).
func mergeTrainDuplicatePicks(byIssue map[int][]mergeTrainDuplicateCandidate, remembered map[int]int) (winners map[int]int, holds []mergeTrainDuplicateHold, turns map[int][]int) {
	winners = map[int]int{}
	turns = map[int][]int{}
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
		winner := mergeTrainIssueWinner(cands, remembered[issue])
		winners[issue] = winner
		for _, c := range cands {
			if c.Number == winner {
				continue
			}
			if !c.AtGate {
				turns[c.Number] = append(turns[c.Number], issue)
				continue
			}
			if _, done := held[c.Number]; !done {
				held[c.Number] = mergeTrainDuplicateHold{Number: c.Number, Winner: winner, Issue: issue}
			}
		}
	}
	for n := range held {
		delete(turns, n)
	}
	holds = make([]mergeTrainDuplicateHold, 0, len(held))
	for _, h := range held {
		holds = append(holds, h)
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].Number < holds[j].Number })
	return winners, holds, turns
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

// mergeTrainSameLine reports whether two PRs target the same base branch.
// The one-PR-per-issue rule only compares PRs on the same line: the same
// fix opened against v5 and v6 both says "Fixes #N", and that is the
// release-line process, not a duplicate (hivecommons/hive#17436). A PR
// with no base recorded compares as the same line so partial fixtures
// keep their old behaviour.
func mergeTrainSameLine(a, b *gh.PullRequest) bool {
	ab, bb := a.GetBase().GetRef(), b.GetBase().GetRef()
	return ab == "" || bb == "" || ab == bb
}

// planDuplicateHolds runs the one-PR-per-issue rule over the lane's
// candidates before the trains are grouped (hivecommons/hive#15395). PRs
// are grouped by base branch first, so siblings on different release
// lines never compete (hivecommons/hive#17436). The rule judges heads at
// the merge gate only (hivecommons/hive#17439): a same-issue candidate
// whose CI is still running or whose review is outstanding is evaluated
// on its normal path, so its row reads CI running / review pending rather
// than "needs a pick", and carries a turn the merge point checks, so it
// still cannot merge past the rule. A candidate at the gate behind its
// issue's winner is held: it leaves the candidate list and the plan
// remembers who it yields to.
func (c *Engine) planDuplicateHolds(ctx context.Context, plan *mergeTrainPlan, lane, owner, repo, displayRepo string, prs []*gh.PullRequest, prefilter func(*gh.PullRequest) string) []*gh.PullRequest {
	candidate := map[int]bool{}
	for _, pr := range prs {
		if pr != nil && pr.GetNumber() > 0 && mergeTrainCandidate(prefilter(pr)) {
			candidate[pr.GetNumber()] = true
		}
	}
	if len(candidate) == 0 {
		return prs
	}
	claimants, whole := c.claimantsForConflicts(ctx, owner, repo, prs)
	if whole {
		c.forgetClosedMergeTrainWinners(lane, owner, repo, claimants)
	}
	prs = c.planClaimConflicts(ctx, plan, lane, owner, repo, displayRepo, prs, claimants, candidate)
	byLine := map[string]map[int][]mergeTrainDuplicateCandidate{}
	for _, pr := range prs {
		if pr == nil || !candidate[pr.GetNumber()] {
			continue
		}
		issues := mergeTrainClosedIssues(pr, displayRepo)
		if len(issues) == 0 {
			continue
		}
		n, sha := pr.GetNumber(), pr.GetHead().GetSHA()
		cand := mergeTrainDuplicateCandidate{
			Number:    n,
			CreatedAt: pr.GetCreatedAt().Time,
			Green:     !mergeTrainDirty(pr.GetMergeableState()) && !c.mergeTrainHeadBlocked(lane, owner, repo, n, sha),
			AtGate:    c.mergeTrainAtGate(lane, owner, repo, pr),
			Terminal:  c.mergeTrainHeadTerminal(lane, owner, repo, n, sha),
		}
		line := pr.GetBase().GetRef()
		byIssue := byLine[line]
		if byIssue == nil {
			byIssue = map[int][]mergeTrainDuplicateCandidate{}
			byLine[line] = byIssue
		}
		for _, issue := range issues {
			byIssue[issue] = append(byIssue[issue], cand)
		}
	}
	var holds []mergeTrainDuplicateHold
	for line, byIssue := range byLine {
		winners, h, turns := mergeTrainDuplicatePicks(byIssue, c.mergeTrainRememberedWinners(lane, owner, repo, line, byIssue))
		c.rememberMergeTrainWinners(lane, owner, repo, line, displayRepo, winners)
		holds = append(holds, h...)
		for n, issues := range turns {
			plan.deferIssueTurn(n, mergeTrainIssueTurn{Base: line, Issues: issues})
			c.debug("merge train: same-issue PR short of the merge gate; evaluated, held at the merge point", "repo", displayRepo, "pr", n, "issues", issues, "lane", plan.lane)
		}
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].Number < holds[j].Number })
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
// available (a bare Engine in tests). whole is false when a wired
// forge-state read failed and the lane's own, possibly event-narrowed,
// list stands in: absence from it does not mean a PR closed.
func (c *Engine) claimantsForConflicts(ctx context.Context, owner, repo string, prs []*gh.PullRequest) (claimants []*gh.PullRequest, whole bool) {
	if c == nil {
		return prs, true
	}
	if c.openPRs == nil {
		c.claimantsMu.Lock()
		all, ok := c.claimants[owner+"/"+repo]
		c.claimantsMu.Unlock()
		if ok && len(all) > 0 {
			return all, true
		}
		return prs, true
	}
	all, skip, err := c.openPullRequests(ctx, owner, repo, nil)
	if err != nil || skip != "" || len(all) == 0 {
		return prs, false
	}
	return all, true
}

// planClaimConflicts holds every lane candidate whose closing issue another
// open PR of the repository also claims, unless a human decision is
// recorded for the candidate's head. Candidates of this lane that claim
// the same issue are left to the one-PR-per-issue winner rule, which sees
// them both; the gate exists for the pair the lanes split — the hive's
// own PR in the self lane and a trusted human's PR in the trusted lane
// both marched toward merge for hivecommons/hive#15773 because each lane
// saw only its own (hivecommons/hive#15810). Only a head at the merge gate
// is held for the decision; one whose CI is still running or whose review
// is outstanding is evaluated on its normal path and meets the same hold
// at the merge point (hivecommons/hive#17439). Either way it leaves the
// list the trains and the winner rule are grouped from.
func (c *Engine) planClaimConflicts(ctx context.Context, plan *mergeTrainPlan, lane, owner, repo, displayRepo string, prs, claimants []*gh.PullRequest, candidate map[int]bool) []*gh.PullRequest {
	byIssue := map[int][]*gh.PullRequest{}
	for _, pr := range claimants {
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
				if o == n || candidate[o] || mergeTrainAdoptionPair(pr, other) || !mergeTrainSameLine(pr, other) {
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
	kept := make([]*gh.PullRequest, 0, len(prs))
	for _, pr := range prs {
		h, conflicted := held[pr.GetNumber()]
		switch {
		case !conflicted:
			kept = append(kept, pr)
		case c.mergeTrainAtGate(lane, owner, repo, pr):
			if plan.claimConflicts == nil {
				plan.claimConflicts = map[int]mergeTrainClaimConflict{}
			}
			plan.claimConflicts[h.Number] = h
		default:
			plan.deferIssueTurn(h.Number, mergeTrainIssueTurn{Conflict: h})
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

// mergeTrainAtGate reports whether the lane last saw this exact head at
// the merge gate — CI green, every required verdict recorded, mergeable
// but for its turn — the readiness a train seat needs too. Only such a
// head is judged by the same-issue rules (hivecommons/hive#17439): a PR
// whose CI is still running or whose review is outstanding is not yet a
// pick anyone could make.
func (c *Engine) mergeTrainAtGate(lane, owner, repo string, pr *gh.PullRequest) bool {
	n, sha := pr.GetNumber(), pr.GetHead().GetSHA()
	return !c.mergeTrainHeadBlocked(lane, owner, repo, n, sha) && c.mergeTrainHeadReadiness(lane, owner, repo, n, sha) == mergeTrainReadinessReady
}

// mergeTrainHeadTerminal reports whether this exact head was last seen by
// the lane blocked for good: a person merges it, or a hold no CI re-run
// clears (not mergeable, a terminal required-check hold, a permanent merge
// refusal). A red check is not terminal — on a head whose CI is still
// running it is noise, and a re-run or the fixer may clear it — so it
// never costs a remembered winner its issue (hivecommons/hive#17439).
func (c *Engine) mergeTrainHeadTerminal(lane, owner, repo string, number int, sha string) bool {
	v, ok := c.mergeTrainVerdictFor(lane, owner, repo, number, sha)
	return ok && (v.humanMerge || (v.blocked && classifyCIReason(v.reason) != ciReasonRed))
}

// mergeTrainIssueTurn is a same-issue PR the lane evaluates on its normal
// path because its head is not at the merge gate yet
// (hivecommons/hive#17439). Its row shows CI and review as they are; if
// the head reaches the merge point it is held there instead of merging:
// on an undecided claim conflict (Conflict.Other set), or behind the PR
// that already holds the turn of one of Issues on base branch Base.
type mergeTrainIssueTurn struct {
	Conflict mergeTrainClaimConflict
	Base     string
	Issues   []int
}

// deferIssueTurn records number as evaluated with a same-issue turn.
func (p *mergeTrainPlan) deferIssueTurn(number int, t mergeTrainIssueTurn) {
	if p.issueTurns == nil {
		p.issueTurns = map[int]mergeTrainIssueTurn{}
	}
	p.issueTurns[number] = t
}

// issueTurnOf reports the same-issue turn number is evaluated with.
func (p *mergeTrainPlan) issueTurnOf(number int) (mergeTrainIssueTurn, bool) {
	if p == nil || p.issueTurns == nil {
		return mergeTrainIssueTurn{}, false
	}
	t, ok := p.issueTurns[number]
	return t, ok
}

// mergeTrainIssueTurnKey is the context key under which a lane carries a
// same-issue PR's turn to the merge point.
type mergeTrainIssueTurnKey struct{}

// mergeTrainEvalContext is the context a lane evaluates number under: its
// same-issue turn, if any, and its train candidate hold. The merge point
// asks mergeTrainCandidateHold, which settles the turn first.
func (c *Engine) mergeTrainEvalContext(ctx context.Context, plan *mergeTrainPlan, number int) context.Context {
	if t, ok := plan.issueTurnOf(number); ok {
		ctx = context.WithValue(ctx, mergeTrainIssueTurnKey{}, func() string {
			return c.mergeTrainIssueTurnHold(plan, number, t)
		})
	}
	return withMergeTrainCandidateHold(ctx, c.mergeTrainCandidateReason(plan, number))
}

// mergeTrainIssueTurnHold is the hold a same-issue PR meets at the merge
// point, "" when it may go on: an undecided claim conflict holds it;
// otherwise it claims each issue's turn in order and yields to the first
// issue another PR already holds — reaching the merge point behind the
// issue's winner is the same verdict as waiting at the gate behind it.
func (c *Engine) mergeTrainIssueTurnHold(plan *mergeTrainPlan, number int, t mergeTrainIssueTurn) string {
	if t.Conflict.Other > 0 {
		return mergeTrainClaimConflictReason(t.Conflict)
	}
	for _, issue := range t.Issues {
		if w := c.claimMergeTrainIssueTurn(plan.lane, plan.owner, plan.repo, t.Base, plan.displayRepo, issue, number); w != number {
			return mergeTrainDuplicateReason(mergeTrainDuplicateHold{Number: number, Winner: w, Issue: issue})
		}
	}
	return ""
}

// mergeTrainIssueWinnerKey keys the PR that holds issue's turn on one
// base branch: release-line siblings never share a turn
// (hivecommons/hive#17436). It extends the lane's verdict key, so
// forgetClosedMergeTrainWinners still finds it by prefix.
func mergeTrainIssueWinnerKey(lane, owner, repo, base string, issue int) string {
	return mergeTrainVerdictKey(lane, owner, repo, issue) + "@" + base
}

// claimMergeTrainIssueTurn is the merge point's one-PR-per-issue check:
// the PR that holds issue's turn on base when it is another; otherwise
// number takes the turn — the first of the issue's PRs at the merge point
// keeps the issue until it closes or is blocked for good — and is
// returned.
func (c *Engine) claimMergeTrainIssueTurn(lane, owner, repo, base, displayRepo string, issue, number int) int {
	key := mergeTrainIssueWinnerKey(lane, owner, repo, base, issue)
	c.mergeTrainMu.Lock()
	if w := c.mergeTrainIssueWinners[key]; w > 0 && w != number {
		c.mergeTrainMu.Unlock()
		return w
	}
	_, had := c.mergeTrainIssueWinners[key]
	if c.mergeTrainIssueWinners == nil {
		c.mergeTrainIssueWinners = map[string]int{}
	}
	c.mergeTrainIssueWinners[key] = number
	c.mergeTrainMu.Unlock()
	if !had {
		c.info("merge train: first at the merge gate keeps the issue", "repo", displayRepo, "pr", number, "issue", issue, "base", base, "lane", lane)
	}
	return number
}

// mergeTrainRememberedWinners reads the winner the lane kept on base on
// an earlier pass for each issue two or more candidates close.
func (c *Engine) mergeTrainRememberedWinners(lane, owner, repo, base string, byIssue map[int][]mergeTrainDuplicateCandidate) map[int]int {
	out := map[int]int{}
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	for issue, cands := range byIssue {
		if len(cands) < 2 {
			continue
		}
		if w, ok := c.mergeTrainIssueWinners[mergeTrainIssueWinnerKey(lane, owner, repo, base, issue)]; ok {
			out[issue] = w
		}
	}
	return out
}

// rememberMergeTrainWinners stores this pass's pick on base per contested
// issue: a winner is kept for the next pass; an issue nobody at the gate
// holds is forgotten, so the first of its PRs at the merge point takes
// it. A new winner is logged once.
func (c *Engine) rememberMergeTrainWinners(lane, owner, repo, base, displayRepo string, winners map[int]int) {
	type pick struct{ issue, winner, was int }
	var picked []pick
	c.mergeTrainMu.Lock()
	for issue, w := range winners {
		key := mergeTrainIssueWinnerKey(lane, owner, repo, base, issue)
		was := c.mergeTrainIssueWinners[key]
		if w <= 0 {
			delete(c.mergeTrainIssueWinners, key)
			continue
		}
		if c.mergeTrainIssueWinners == nil {
			c.mergeTrainIssueWinners = map[string]int{}
		}
		c.mergeTrainIssueWinners[key] = w
		if was != w {
			picked = append(picked, pick{issue: issue, winner: w, was: was})
		}
	}
	c.mergeTrainMu.Unlock()
	sort.Slice(picked, func(i, j int) bool { return picked[i].issue < picked[j].issue })
	for _, p := range picked {
		c.info("merge train: same-issue winner picked at the merge gate", "repo", displayRepo, "issue", p.issue, "base", base, "winner", p.winner, "previous", p.was, "lane", lane)
	}
}

// forgetClosedMergeTrainWinners drops the lane's remembered winners on
// owner/repo whose PR is no longer among the repository's open PRs.
func (c *Engine) forgetClosedMergeTrainWinners(lane, owner, repo string, open []*gh.PullRequest) {
	if len(open) == 0 {
		return
	}
	alive := make(map[int]bool, len(open))
	for _, pr := range open {
		if pr != nil && pr.GetNumber() > 0 && (pr.State == nil || pr.GetState() == "open") {
			alive[pr.GetNumber()] = true
		}
	}
	prefix := mergeTrainVerdictKey(lane, owner, repo, 0)
	prefix = prefix[:strings.LastIndex(prefix, "#")+1]
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	for key, w := range c.mergeTrainIssueWinners {
		if strings.HasPrefix(key, prefix) && !alive[w] {
			delete(c.mergeTrainIssueWinners, key)
		}
	}
}
