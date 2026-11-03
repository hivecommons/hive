package automerge

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Merge train (hivecommons/hive#15117).
//
// Six approved PRs to v5 all touched stage_tracker.go and index.html; the
// moment one merged the rest went `dirty`, and every lane logged
// `conflicting` / `label:needs-rebase` each cycle while a person stacked
// branches by hand. The train makes the lane order them instead:
//
//   - candidates on the same base whose changed files overlap form one
//     train (GET /pulls/{n}/files, cached by head SHA);
//   - the train has one front — clean-mergeable first, then oldest — that
//     the lane evaluates and merges as before; every sibling is parked as
//     `merge-train:queued-behind#<front>:<pos>/<size>:…` (stage 080, never a
//     human stage);
//   - right after the front merges each sibling is read back: `behind` gets
//     update-branch, `dirty` gets a rebase-resolve request through the
//     RebaseResolver hook, at most MaxRebaseAttempts per PR per front; a
//     front that stays dirty past the guard parks at 085 for a person.
//
// Train state (attempt counters, last merged front, transition log
// signatures) is in-memory per process; a restart simply re-plans.

// MergeTrainPolicy is the live auto_merge.merge_train.* policy.
type MergeTrainPolicy struct {
	Enabled           bool
	MaxRebaseAttempts int
}

// DefaultMergeTrainPolicy is the policy without an Options.MergeTrain hook:
// on, two rebase attempts per PR per front (config.DefaultMergeTrainMaxRebaseAttempts).
var DefaultMergeTrainPolicy = MergeTrainPolicy{Enabled: true, MaxRebaseAttempts: 2}

// RebaseRequest names the PR the merge train wants rebased onto its base.
type RebaseRequest struct {
	// Repo is owner/repo; Number the PR; Base and Head the branch names.
	Repo   string
	Number int
	Base   string
	Head   string
	// HeadSHA is the head the train saw conflicting.
	HeadSHA string
	// Front is the PR whose merge made this head conflict (0 when the
	// front itself is the conflicting one at the start of a train).
	Front int
	// Attempt (1-based) of MaxAttempts for this PR behind this front.
	Attempt     int
	MaxAttempts int
	// Lane is the sweep lane that owns the PR (prstage.StageLane*).
	Lane string
	// RequestedBy names the requester the episode is opened for: "" (the
	// merge train) or AdoptionRebaseRequester for the hive's conflicting
	// copy of an adopted PR (hivecommons/hive#15307).
	RequestedBy string
}

// AdoptionRebaseRequester is RebaseRequest.RequestedBy for a rebase the
// adoption lane asks of the fixer on the hive's own copy branch.
const AdoptionRebaseRequester = "adoption"

// RebaseEpisode is what Options.RebaseEpisode reports for a PR with an
// open conflict episode: how many attempts the fixer has used of the
// guard, and whether the guard is exhausted.
type RebaseEpisode struct {
	Attempt     int
	MaxAttempts int
	Exhausted   bool
	RequestedBy string
}

// RebaseResolver enqueues a rebase-resolve task for a conflicting PR.
// cmd/hive adapts pkg/rebasemerge's attempts ledger here (hivecommons/hive#15104):
// the request opens the fixer's rebase episode and the lane merges the clean
// head on its next pass. A resolver that returns an error, or a nil
// resolver, leaves the dirty PR on its normal `conflicting` path, logged
// once per PR head.
type RebaseResolver interface {
	RequestRebase(ctx context.Context, req RebaseRequest) error
}

// HotspotRecorder is the merge-hotspot advisor's intake (pkg/hotspot.Store
// satisfies it through cmd/hive). RecordPRFiles is called once per train
// candidate per sweep with the files the planner already fetched;
// RecordConflict once per sibling rebase the train requests, with the
// files that sibling shares with the front.
type HotspotRecorder interface {
	RecordPRFiles(repo string, number int, files []string, openedAt time.Time)
	RecordConflict(repo string, number int, files []string)
}

// RebaseResolverFunc adapts a function to RebaseResolver.
type RebaseResolverFunc func(ctx context.Context, req RebaseRequest) error

// RequestRebase implements RebaseResolver.
func (f RebaseResolverFunc) RequestRebase(ctx context.Context, req RebaseRequest) error {
	return f(ctx, req)
}

// Tick-stat buckets for the train's park reasons (the full reason is per
// PR and carries the position).
const (
	autoMergeReasonMergeTrainQueued   = "merge-train-queued"
	autoMergeReasonMergeTrainRebasing = "merge-train-rebasing"
)

// mergeTrainFilesPages bounds the /files pagination per PR (100 per page).
const mergeTrainFilesPages = 3

// mergeTrainMember is one candidate the planner groups.
type mergeTrainMember struct {
	Number         int
	Base           string
	Head           string
	HeadSHA        string
	Files          []string
	CreatedAt      time.Time
	MergeableState string
	Fork           bool
	// Blocked marks a head the lane last saw unable to merge on its own
	// (red required check, terminal hold, permanent merge refusal): it
	// does not front a train, and a group whose best member is blocked
	// imposes no ordering at all (fail-open), so nobody waits behind a PR
	// that is not moving. A head the lane leaves to a person's merge
	// (mergeTrainHumanMergeReason) is not even a member.
	Blocked bool
	// Readiness is the member's merge-gate readiness as the lane last saw
	// it (mergeTrainReadiness*): a ready head (green, approved, soaking or
	// waiting for a sweep) leads over one the lane has not evaluated at
	// this head, which leads over one still at CI / Review. The front of a
	// train is always a ready member when the train has one
	// (hivecommons/hive#15741 at CI / Review led #15748 and #15755, both
	// queued at 080, and nothing merged).
	Readiness int
	// SeatedSince is when this exact head first took a seat in a train
	// (zero: not seated before this pass). Earlier seats lead, so a member
	// that left the train and came back ready rides at the back
	// (hivecommons/hive#15825).
	SeatedSince time.Time
}

// mergeTrainSeated reports whether a member may take a train seat: items
// that have not passed the review or CI is not green should not be (a)
// ready for merge, (b) in a merge train (hivecommons/hive#15825). Only a
// head the lane last saw ready — green on the current head, every
// required verdict recorded for it, mergeable — rides; the rest are
// candidates: they share the train's files but hold no position and never
// front it.
func mergeTrainSeated(m mergeTrainMember) bool {
	return !m.Blocked && m.Readiness == mergeTrainReadinessReady
}

// Readiness tiers. The zero value is unknown; mergeTrainReadinessRank
// puts them in merge order (ready, unknown, unready).
const (
	// mergeTrainReadinessUnknown: the lane has not evaluated this head on
	// its normal path (a new PR, or one parked behind a front since it
	// was pushed).
	mergeTrainReadinessUnknown = 0
	// mergeTrainReadinessReady: the lane last saw this head at the merge
	// gate — green and approved, soaking or waiting on a sweep.
	mergeTrainReadinessReady = 1
	// mergeTrainReadinessUnready: the lane last saw this head short of the
	// merge gate — CI running, review pending.
	mergeTrainReadinessUnready = 2
)

// mergeTrainReadinessRank is a tier's place in merge order: a ready head
// leads, one the lane has not evaluated comes next (so it gets evaluated
// and sorted), one seen short of the gate trails.
func mergeTrainReadinessRank(tier int) int {
	switch tier {
	case mergeTrainReadinessReady:
		return 0
	case mergeTrainReadinessUnready:
		return 2
	}
	return 1
}

// mergeTrainGroup is one train: the ordered members (front first) and the
// files at least two of them touch.
type mergeTrainGroup struct {
	Base         string
	Order        []int
	OverlapFiles []string
	// Candidates share the train's files but are not ready on their
	// current head (CI running, a verdict owed, blocked): no position,
	// never front. They are evaluated on the lane's normal path and take a
	// seat at the back once ready (hivecommons/hive#15825).
	Candidates []int
	members    map[int]mergeTrainMember
}

// Front is the PR the train merges next.
func (g mergeTrainGroup) Front() int {
	if len(g.Order) == 0 {
		return 0
	}
	return g.Order[0]
}

// Position is the 1-based slot of number in the train (0 when absent).
func (g mergeTrainGroup) Position(number int) int {
	for i, n := range g.Order {
		if n == number {
			return i + 1
		}
	}
	return 0
}

// Siblings are the members behind the front, in order.
func (g mergeTrainGroup) Siblings() []int {
	if len(g.Order) <= 1 {
		return nil
	}
	return g.Order[1:]
}

func (g mergeTrainGroup) signature() string {
	parts := make([]string, len(g.Order))
	for i, n := range g.Order {
		parts[i] = strconv.Itoa(n)
	}
	return g.Base + ":" + strings.Join(parts, ",")
}

// mergeTrainStateRank orders members for the front: a head GitHub says
// merges cleanly leads, an unknown state next, then behind, then dirty.
func mergeTrainStateRank(state string) int {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "clean", "unstable", "has_hooks", "blocked":
		return 0
	case "", "unknown":
		return 1
	case "behind":
		return 2
	}
	return 3
}

// mergeTrainDirty reports whether a mergeable state is a conflict.
func mergeTrainDirty(state string) bool {
	s := strings.ToLower(strings.TrimSpace(state))
	return s == "dirty" || s == "conflicting"
}

// mergeTrainSeatLess orders the seated members front-first: a head that
// merges trails one that does not need the train's rebase first (a
// conflict the train's own front merge caused is repaired last, after the
// ready heads behind it have merged), then the seat taken earliest leads
// (a member that came back ready re-enters at the back: zero SeatedSince
// trails), then the mergeable-state ordering of hivecommons/hive#15763,
// then age, then number.
func mergeTrainSeatLess(ms []mergeTrainMember) func(a, b int) bool {
	return func(a, b int) bool {
		if da, db := mergeTrainDirty(ms[a].MergeableState), mergeTrainDirty(ms[b].MergeableState); da != db {
			return !da
		}
		sa, sb := ms[a].SeatedSince, ms[b].SeatedSince
		if !sa.Equal(sb) {
			if sa.IsZero() != sb.IsZero() {
				return !sa.IsZero()
			}
			return sa.Before(sb)
		}
		if ra, rb := mergeTrainStateRank(ms[a].MergeableState), mergeTrainStateRank(ms[b].MergeableState); ra != rb {
			return ra < rb
		}
		if !ms[a].CreatedAt.Equal(ms[b].CreatedAt) {
			if ms[a].CreatedAt.IsZero() != ms[b].CreatedAt.IsZero() {
				return !ms[a].CreatedAt.IsZero()
			}
			return ms[a].CreatedAt.Before(ms[b].CreatedAt)
		}
		return ms[a].Number < ms[b].Number
	}
}

// groupMergeTrains groups members per base by overlapping files (union of
// pairwise overlaps) and orders each group front-first. Groups of one are
// dropped: a PR with nothing to collide with is no train.
func groupMergeTrains(members []mergeTrainMember) []mergeTrainGroup {
	byBase := map[string][]mergeTrainMember{}
	bases := []string{}
	for _, m := range members {
		if m.Number <= 0 || len(m.Files) == 0 {
			continue
		}
		if _, ok := byBase[m.Base]; !ok {
			bases = append(bases, m.Base)
		}
		byBase[m.Base] = append(byBase[m.Base], m)
	}
	sort.Strings(bases)
	var out []mergeTrainGroup
	for _, base := range bases {
		list := byBase[base]
		parent := make([]int, len(list))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			for parent[i] != i {
				parent[i] = parent[parent[i]]
				i = parent[i]
			}
			return i
		}
		owner := map[string]int{}
		shared := map[string]bool{}
		for i, m := range list {
			seen := map[string]bool{}
			for _, f := range m.Files {
				f = strings.TrimSpace(f)
				if f == "" || seen[f] {
					continue
				}
				seen[f] = true
				if j, ok := owner[f]; ok {
					shared[f] = true
					parent[find(i)] = find(j)
				} else {
					owner[f] = i
				}
			}
		}
		groups := map[int][]mergeTrainMember{}
		roots := []int{}
		for i, m := range list {
			r := find(i)
			if _, ok := groups[r]; !ok {
				roots = append(roots, r)
			}
			groups[r] = append(groups[r], m)
		}
		sort.Ints(roots)
		for _, r := range roots {
			ms := groups[r]
			if len(ms) < 2 {
				continue
			}
			var seated, candidates []mergeTrainMember
			for _, m := range ms {
				if mergeTrainSeated(m) {
					seated = append(seated, m)
				} else {
					candidates = append(candidates, m)
				}
			}
			if len(seated) == 0 {
				continue
			}
			sort.SliceStable(seated, mergeTrainSeatLess(seated))
			sort.SliceStable(candidates, func(a, b int) bool {
				if ra, rb := mergeTrainReadinessRank(candidates[a].Readiness), mergeTrainReadinessRank(candidates[b].Readiness); ra != rb {
					return ra < rb
				}
				return candidates[a].Number < candidates[b].Number
			})
			g := mergeTrainGroup{Base: base, members: map[int]mergeTrainMember{}}
			count := map[string]int{}
			for _, m := range seated {
				g.Order = append(g.Order, m.Number)
			}
			for _, m := range candidates {
				g.Candidates = append(g.Candidates, m.Number)
			}
			for _, m := range ms {
				g.members[m.Number] = m
				seen := map[string]bool{}
				for _, f := range m.Files {
					if f = strings.TrimSpace(f); f != "" && !seen[f] {
						seen[f] = true
						count[f]++
					}
				}
			}
			for f, n := range count {
				if n >= 2 && shared[f] {
					g.OverlapFiles = append(g.OverlapFiles, f)
				}
			}
			sort.Strings(g.OverlapFiles)
			out = append(out, g)
		}
	}
	return out
}

// mergeTrainPlan is one lane's trains for one repository this pass.
type mergeTrainPlan struct {
	lane, displayRepo, owner, repo string
	groups                         []mergeTrainGroup
	byNumber                       map[int]*mergeTrainGroup
	// candidates are the PRs that share a train's files but are not ready
	// on their current head: they hold no seat (hivecommons/hive#15825).
	candidates map[int]*mergeTrainGroup
	// duplicates are the PRs held out of every train this pass because
	// another open PR fixes the same issue (one PR per issue).
	duplicates map[int]mergeTrainDuplicateHold
	// claimConflicts are the candidates held because an open PR outside
	// this lane claims the same issue and no decision is recorded.
	claimConflicts map[int]mergeTrainClaimConflict
}

// group returns the train number rides in, if any.
func (p *mergeTrainPlan) group(number int) (*mergeTrainGroup, bool) {
	if p == nil || p.byNumber == nil {
		return nil, false
	}
	g, ok := p.byNumber[number]
	return g, ok
}

// isSibling reports whether number waits behind a front this pass.
func (p *mergeTrainPlan) isSibling(number int) bool {
	g, ok := p.group(number)
	return ok && g.Front() != number
}

// isFront reports whether number leads a train this pass.
func (p *mergeTrainPlan) isFront(number int) bool {
	g, ok := p.group(number)
	return ok && g.Front() == number
}

// candidateOf returns the train number is a candidate for (shares files
// with, without a seat), if any.
func (p *mergeTrainPlan) candidateOf(number int) (*mergeTrainGroup, bool) {
	if p == nil || p.candidates == nil {
		return nil, false
	}
	g, ok := p.candidates[number]
	return g, ok && g.Front() > 0
}

// isCandidate reports whether number shares a train's files this pass
// without a seat: the lane evaluates it, but if it turns out ready it is
// held for a seat at the back rather than merged ahead of the front.
func (p *mergeTrainPlan) isCandidate(number int) bool {
	_, ok := p.candidateOf(number)
	return ok
}

// mergeTrainCandidateHoldKey is the context key under which a lane carries
// the hold reason for a train candidate (hivecommons/hive#15825): the lane
// evaluates the PR on its normal path, but a head that turns out ready is
// not merged ahead of the train's front — it is held for a seat at the
// back next pass.
type mergeTrainCandidateHoldKey struct{}

// withMergeTrainCandidateHold marks ctx as evaluating a train candidate.
func withMergeTrainCandidateHold(ctx context.Context, reason string) context.Context {
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, mergeTrainCandidateHoldKey{}, reason)
}

// mergeTrainCandidateHold is the hold reason a merge point returns instead
// of merging when the lane is evaluating a train candidate; "" otherwise.
func mergeTrainCandidateHold(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	reason, _ := ctx.Value(mergeTrainCandidateHoldKey{}).(string)
	return reason
}

// mergeTrainCandidateReason is the lane reason for a candidate the lane
// found ready: `merge-train:admitted#<front>:0/<size>:0/0:<files>`. The
// stage engine reads it as 080 (the PR is green and approved; its wait is
// the train's turn), and mergeTrainReadinessOf reads it as ready, so the
// next plan seats it at the back.
func (c *Engine) mergeTrainCandidateReason(plan *mergeTrainPlan, number int) string {
	g, ok := plan.candidateOf(number)
	if !ok {
		return ""
	}
	return prstage.MergeTrain{State: prstage.MergeTrainStateAdmitted, Front: g.Front(), Position: 0, Size: len(g.Order), OverlapFiles: g.OverlapFiles}.Reason()
}

// mergeTrainPolicyNow reads the live policy.
func (c *Engine) mergeTrainPolicyNow() MergeTrainPolicy {
	if c == nil || c.mergeTrainPolicy == nil {
		return DefaultMergeTrainPolicy
	}
	p := c.mergeTrainPolicy()
	if p.MaxRebaseAttempts <= 0 {
		p.MaxRebaseAttempts = DefaultMergeTrainPolicy.MaxRebaseAttempts
	}
	return p
}

// mergeTrainCandidate reports whether a prefilter verdict admits a PR to
// the train: a clean pass, or a `needs-rebase` label — the very PR the
// train exists to order and repair.
func mergeTrainCandidate(prefilterReason string) bool {
	return prefilterReason == "" || prefilterReason == "label:needs-rebase"
}

// planMergeTrains groups this lane's candidates on owner/repo into trains.
// prefilter is the lane's own cheap gate; API reads (files, then the
// mergeable state of grouped members) are spent only where two or more
// candidates share a base. Errors are logged and leave the PR out of any
// train, so a failed read never blocks a merge the lane would have made.
func (c *Engine) planMergeTrains(ctx context.Context, lane, displayRepo, owner, repo string, prs []*gh.PullRequest, prefilter func(*gh.PullRequest) string) *mergeTrainPlan {
	plan := &mergeTrainPlan{lane: lane, displayRepo: displayRepo, owner: owner, repo: repo, byNumber: map[int]*mergeTrainGroup{}, candidates: map[int]*mergeTrainGroup{}}
	if !c.mergeTrainPolicyNow().Enabled || c == nil || c.gh == nil {
		return plan
	}
	defer c.mergeTrainSettleSeats(lane, owner, repo, displayRepo, prs, plan)
	prs = c.planDuplicateHolds(ctx, plan, lane, owner, repo, displayRepo, prs, prefilter)
	perBase := map[string][]*gh.PullRequest{}
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() <= 0 || pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
			continue
		}
		if !mergeTrainCandidate(prefilter(pr)) {
			continue
		}
		if c.preparingBase(owner, repo, pr.GetBase().GetRef(), pr.GetNumber()) {
			continue // preparation is not a current-head-ready train seat
		}
		if why := c.mergeTrainUnderRepair(owner, repo, pr); why != "" {
			c.mergeTrainSkipLogged(owner, repo, displayRepo, pr.GetNumber(), pr.GetHead().GetSHA(), why)
			continue
		}
		if c.mergeTrainHeadHumanMerge(lane, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA()) {
			c.mergeTrainSkipLogged(owner, repo, displayRepo, pr.GetNumber(), pr.GetHead().GetSHA(), "human-merge")
			continue
		}
		base := pr.GetBase().GetRef()
		perBase[base] = append(perBase[base], pr)
	}
	var members []mergeTrainMember
	for base, list := range perBase {
		if len(list) < 2 {
			continue
		}
		for _, pr := range list {
			files, err := c.mergeTrainFiles(ctx, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA())
			if err != nil {
				c.warn("merge train: could not list PR files; left out of the train", "repo", displayRepo, "pr", pr.GetNumber(), "error", err)
				continue
			}
			if c.hotspotRecorder != nil {
				c.hotspotRecorder.RecordPRFiles(owner+"/"+repo, pr.GetNumber(), files, pr.GetCreatedAt().Time)
			}
			if c.integrationFilesObserved != nil {
				c.integrationFilesObserved(owner+"/"+repo, pr.GetHead().GetSHA(), append([]string(nil), files...))
			}
			members = append(members, mergeTrainMember{
				Number:         pr.GetNumber(),
				Base:           base,
				Head:           pr.GetHead().GetRef(),
				HeadSHA:        pr.GetHead().GetSHA(),
				Files:          files,
				CreatedAt:      pr.GetCreatedAt().Time,
				MergeableState: pr.GetMergeableState(),
				Fork:           c.isForkPR(pr),
				Blocked:        c.mergeTrainHeadBlocked(lane, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA()),
				Readiness:      c.mergeTrainHeadReadiness(lane, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA()),
			})
		}
	}
	if len(members) == 0 {
		return plan
	}
	// A first grouping on files alone says which members need their live
	// mergeable state (the list API does not carry it); then regroup so
	// the front is chosen on what GitHub reports now.
	needState := map[int]bool{}
	for _, g := range groupMergeTrains(members) {
		for _, n := range g.Order {
			needState[n] = true
		}
	}
	for i := range members {
		m := &members[i]
		if !needState[m.Number] || m.MergeableState != "" {
			continue
		}
		live, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_train"), owner, repo, m.Number)
		if err != nil {
			c.warn("merge train: could not read PR state; treated as unknown", "repo", displayRepo, "pr", m.Number, "error", err)
			continue
		}
		if !strings.EqualFold(live.GetState(), "open") {
			m.Number = 0 // closed between listing and here: not a member
			continue
		}
		m.MergeableState = live.GetMergeableState()
		if live.GetHead() != nil && live.GetHead().GetSHA() != "" {
			m.HeadSHA = live.GetHead().GetSHA()
		}
	}
	for i := range members {
		m := &members[i]
		if m.Number > 0 && c.mergeTrainConflictOutsider(owner, repo, m.Base, *m) {
			c.mergeTrainSkipLogged(owner, repo, displayRepo, m.Number, m.HeadSHA, "conflicting")
			m.Number = 0
		}
	}
	for i := range members {
		m := &members[i]
		if seat, ok := c.mergeTrainSeatFor(lane, owner, repo, m.Number); ok && seat.sha == m.HeadSHA {
			m.SeatedSince = seat.since
		}
	}
	plan.groups = groupMergeTrains(members)
	for i := range plan.groups {
		g := &plan.groups[i]
		for _, n := range g.Order {
			plan.byNumber[n] = g
		}
		for _, n := range g.Candidates {
			plan.candidates[n] = g
		}
		c.logMergeTrainTransition(displayRepo, lane, *g)
	}
	return plan
}

// mergeTrainUnderRepairStages are the stages whose PR is somebody's to
// repair before it may ride a train: 070 fixing (the hive's own fixer),
// 071 needs-author, 072 returned-to-contributor and 085 needs-decision.
var mergeTrainUnderRepairStages = []stagelabels.Stage{stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor, stagelabels.NeedsDecision}

// mergeTrainUnderRepair names the repair stage a PR wears (its label, or
// the stage store's record), or "" when it may ride a train. Such a PR
// never takes a slot: hive's own #15307 (conflicting, returned) was
// counted "4 of 6" and drew the train's stall comments
// (hivecommons/hive#15395), and #15405 led a train's header as "1st"
// while a maintainer's changes-requested had it at 070 fixing.
func (c *Engine) mergeTrainUnderRepair(owner, repo string, pr *gh.PullRequest) string {
	if pr == nil {
		return ""
	}
	labels := labelNames(pr.Labels)
	for _, st := range mergeTrainUnderRepairStages {
		want := st.Label()
		for _, l := range labels {
			if strings.EqualFold(strings.TrimSpace(l), want) {
				return st.Slug()
			}
		}
	}
	if store := c.stageStoreOrShared(); store != nil {
		if rec, ok := store.Get(stagelabels.Key(owner+"/"+repo, pr.GetNumber())); ok && slices.Contains(mergeTrainUnderRepairStages, rec.Stage) {
			return rec.Stage.Slug()
		}
	}
	return ""
}

// mergeTrainConflictOutsider reports a conflicting member the train must
// not carry: a PR GitHub says is dirty is admitted only while the train
// itself is repairing it — a rebase attempt is on record for it behind
// this base's last merged front and the loop guard is not spent. A PR
// that arrived conflicting on its own is the author's to fix, and it
// neither takes a place in the queue nor counts toward the queue's size.
func (c *Engine) mergeTrainConflictOutsider(owner, repo, base string, m mergeTrainMember) bool {
	if !mergeTrainDirty(m.MergeableState) {
		return false
	}
	behind := c.mergeTrainLastFront(owner, repo, base)
	if behind <= 0 || c.rebaseResolver == nil {
		return true
	}
	attempts := c.mergeTrainAttempts(owner, repo, m.Number, behind)
	return attempts <= 0 || attempts >= c.mergeTrainPolicyNow().MaxRebaseAttempts
}

// mergeTrainSkipLogged logs one INFO line per head for a PR kept out of
// every train.
func (c *Engine) mergeTrainSkipLogged(owner, repo, displayRepo string, number int, headSHA, why string) {
	if c.markTerminalHoldLogged(owner, repo, number, headSHA, "merge-train-skip:"+why) {
		c.info("merge train: PR left out of every train", "repo", displayRepo, "pr", number, "why", why)
	}
}

// mergeTrainFiles lists a PR's changed paths, cached by head SHA.
func (c *Engine) mergeTrainFiles(ctx context.Context, owner, repo string, number int, headSHA string) ([]string, error) {
	key := owner + "/" + repo + "#" + strconv.Itoa(number)
	c.mergeTrainMu.Lock()
	if c.mergeTrainFilesCache == nil {
		c.mergeTrainFilesCache = map[string]mergeTrainFilesEntry{}
	}
	if e, ok := c.mergeTrainFilesCache[key]; ok && e.sha == headSHA {
		c.mergeTrainMu.Unlock()
		return e.files, nil
	}
	c.mergeTrainMu.Unlock()
	var files []string
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < mergeTrainFilesPages; page++ {
		list, resp, err := c.gh.PullRequests.ListFiles(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_train"), owner, repo, number, opts)
		if err != nil {
			return nil, err
		}
		for _, f := range list {
			if name := f.GetFilename(); name != "" {
				files = append(files, name)
			}
			if prev := f.GetPreviousFilename(); prev != "" {
				files = append(files, prev)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.mergeTrainMu.Lock()
	c.mergeTrainFilesCache[key] = mergeTrainFilesEntry{sha: headSHA, files: files}
	c.mergeTrainMu.Unlock()
	return files, nil
}

type mergeTrainFilesEntry struct {
	sha   string
	files []string
}

// logMergeTrainTransition logs one INFO line when a train's order changes
// (first sight, a new front, a member joining or leaving), never per cycle.
func (c *Engine) logMergeTrainTransition(displayRepo, lane string, g mergeTrainGroup) {
	if len(g.Order) == 0 {
		return
	}
	key := displayRepo + "|" + lane + "|" + g.Base + "|" + strconv.Itoa(c.mergeTrainMemberRoot(g))
	sig := g.signature()
	c.mergeTrainMu.Lock()
	if c.mergeTrainLogged == nil {
		c.mergeTrainLogged = map[string]string{}
	}
	prev := c.mergeTrainLogged[key]
	c.mergeTrainLogged[key] = sig
	c.mergeTrainMu.Unlock()
	if prev == sig {
		return
	}
	c.info("merge train", "repo", displayRepo, "lane", lane, "base", g.Base, "front", g.Front(), "order", g.Order, "size", len(g.Order), "overlap_files", g.OverlapFiles)
}

// mergeTrainMemberRoot is the lowest member number: the key that names a
// train across passes while its front changes.
func (c *Engine) mergeTrainMemberRoot(g mergeTrainGroup) int {
	root := 0
	for _, n := range g.Order {
		if root == 0 || n < root {
			root = n
		}
	}
	return root
}

// mergeTrainAttemptKey keys the rebase loop guard: one PR behind one front.
func mergeTrainAttemptKey(owner, repo string, number, front int) string {
	return owner + "/" + repo + "#" + strconv.Itoa(number) + "@" + strconv.Itoa(front)
}

func (c *Engine) mergeTrainAttempts(owner, repo string, number, front int) int {
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	return c.mergeTrainAttemptCount[mergeTrainAttemptKey(owner, repo, number, front)]
}

func (c *Engine) bumpMergeTrainAttempts(owner, repo string, number, front int) int {
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	if c.mergeTrainAttemptCount == nil {
		c.mergeTrainAttemptCount = map[string]int{}
	}
	key := mergeTrainAttemptKey(owner, repo, number, front)
	c.mergeTrainAttemptCount[key]++
	return c.mergeTrainAttemptCount[key]
}

// mergeTrainLastFront is the front that last merged on owner/repo:base
// (0 when none this process), the "front" a leading dirty PR is behind.
func (c *Engine) mergeTrainLastFront(owner, repo, base string) int {
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	return c.mergeTrainMerged[owner+"/"+repo+":"+base]
}

func (c *Engine) noteMergeTrainFrontMerged(owner, repo, base string, front int) {
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	if c.mergeTrainMerged == nil {
		c.mergeTrainMerged = map[string]int{}
	}
	c.mergeTrainMerged[owner+"/"+repo+":"+base] = front
}

// mergeTrainSiblingReason is the park reason for a PR behind the front.
func mergeTrainSiblingReason(g *mergeTrainGroup, number int) string {
	return prstage.MergeTrain{
		State:        prstage.MergeTrainStateQueuedBehind,
		Front:        g.Front(),
		Position:     g.Position(number),
		Size:         len(g.Order),
		OverlapFiles: g.OverlapFiles,
	}.Reason()
}

// mergeTrainFrontReason is the reason for a dirty front the train is
// rebasing (or gave up on).
func mergeTrainFrontReason(g *mergeTrainGroup, state string, attempt, maxAttempts int) string {
	return prstage.MergeTrain{
		State:        state,
		Front:        g.Front(),
		Position:     1,
		Size:         len(g.Order),
		Attempt:      attempt,
		MaxAttempts:  maxAttempts,
		OverlapFiles: g.OverlapFiles,
	}.Reason()
}

// mergeTrainFront handles a train's front before the lane evaluates it:
// a conflicting front is sent to the rebase resolver (bounded by the loop
// guard) and parked as rebasing; past the guard it parks for a person.
// handled=false hands a clean or behind front to the lane's normal path,
// and a dirty one too when no resolver is wired.
func (c *Engine) mergeTrainFront(ctx context.Context, plan *mergeTrainPlan, pr *gh.PullRequest) (reason string, handled bool) {
	if plan == nil || pr == nil {
		return "", false
	}
	number := pr.GetNumber()
	g, ok := plan.group(number)
	if !ok || g.Front() != number {
		return "", false
	}
	m := g.members[number]
	if !mergeTrainDirty(m.MergeableState) {
		return "", false
	}
	policy := c.mergeTrainPolicyNow()
	behind := c.mergeTrainLastFront(plan.owner, plan.repo, g.Base)
	attempts := c.mergeTrainAttempts(plan.owner, plan.repo, number, behind)
	if attempts >= policy.MaxRebaseAttempts {
		return mergeTrainFrontReason(g, prstage.MergeTrainStateRebaseExhausted, attempts, policy.MaxRebaseAttempts), true
	}
	if c.rebaseResolver == nil {
		if c.markTerminalHoldLogged(plan.owner, plan.repo, number, m.HeadSHA, "merge-train-no-resolver") {
			c.info("merge train: front is conflicting and no rebase resolver is wired; leaving it on the conflicting path", "repo", plan.displayRepo, "pr", number, "base", g.Base)
		}
		return "", false
	}
	attempt := c.bumpMergeTrainAttempts(plan.owner, plan.repo, number, behind)
	req := RebaseRequest{Repo: plan.owner + "/" + plan.repo, Number: number, Base: g.Base, Head: m.Head, HeadSHA: m.HeadSHA, Front: behind, Attempt: attempt, MaxAttempts: policy.MaxRebaseAttempts, Lane: plan.lane}
	if err := c.rebaseResolver.RequestRebase(ctx, req); err != nil {
		c.warn("merge train: rebase request failed", "repo", plan.displayRepo, "pr", number, "attempt", attempt, "error", err)
	} else {
		c.info("merge train: rebase requested for the front", "repo", plan.displayRepo, "pr", number, "base", g.Base, "behind", behind, "attempt", attempt, "max_attempts", policy.MaxRebaseAttempts)
	}
	return mergeTrainFrontReason(g, prstage.MergeTrainStateRebasing, attempt, policy.MaxRebaseAttempts), true
}

// mergeTrainAfterFrontMerge runs right after a train's front merged: every
// sibling is read back and, as GitHub now reports it, updated (behind) or
// sent for a rebase (dirty, within the loop guard). A sibling GitHub has
// not re-evaluated yet (unknown) is left to the next pass, which replans.
func (c *Engine) mergeTrainAfterFrontMerge(ctx context.Context, plan *mergeTrainPlan, front int) {
	if plan == nil {
		return
	}
	g, ok := plan.group(front)
	if !ok || g.Front() != front {
		return
	}
	c.noteMergeTrainFrontMerged(plan.owner, plan.repo, g.Base, front)
	policy := c.mergeTrainPolicyNow()
	for _, n := range g.Siblings() {
		live, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_train"), plan.owner, plan.repo, n)
		if err != nil {
			if !isGitHubStatus(err, http.StatusNotFound) {
				c.warn("merge train: could not read sibling after the front merged", "repo", plan.displayRepo, "pr", n, "front", front, "error", err)
			}
			continue
		}
		if !strings.EqualFold(live.GetState(), "open") {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(live.GetMergeableState()))
		head := live.GetHead().GetSHA()
		switch {
		case state == "behind":
			if c.requireCurrentBaseFor(plan.owner + "/" + plan.repo) {
				continue // the next admitted preparation front owns its refresh
			}
			if err := c.executeUpdateBranchEffect(ctx, plan.owner, plan.repo, n, head, "merge-train:front-merged#"+strconv.Itoa(front)); err != nil {
				switch classifyUpdateBranchError(err, c.isForkPR(live)) {
				case updateBranchUpToDate:
				case updateBranchConflict:
					c.mergeTrainRequestSiblingRebase(ctx, plan, g, live, front, policy)
				default:
					c.warn("merge train: update-branch failed on sibling", "repo", plan.displayRepo, "pr", n, "front", front, "error", err)
				}
				continue
			}
			c.info("merge train: sibling branch updated after the front merged", "repo", plan.displayRepo, "pr", n, "front", front)
		case mergeTrainDirty(state):
			c.mergeTrainRequestSiblingRebase(ctx, plan, g, live, front, policy)
		}
	}
}

func (c *Engine) mergeTrainRequestSiblingRebase(ctx context.Context, plan *mergeTrainPlan, g *mergeTrainGroup, live *gh.PullRequest, front int, policy MergeTrainPolicy) {
	n := live.GetNumber()
	if c.rebaseResolver == nil {
		return
	}
	if c.mergeTrainAttempts(plan.owner, plan.repo, n, front) >= policy.MaxRebaseAttempts {
		return
	}
	attempt := c.bumpMergeTrainAttempts(plan.owner, plan.repo, n, front)
	if c.hotspotRecorder != nil {
		c.hotspotRecorder.RecordConflict(plan.owner+"/"+plan.repo, n, mergeTrainSharedFiles(g, n))
	}
	req := RebaseRequest{Repo: plan.owner + "/" + plan.repo, Number: n, Base: g.Base, Head: live.GetHead().GetRef(), HeadSHA: live.GetHead().GetSHA(), Front: front, Attempt: attempt, MaxAttempts: policy.MaxRebaseAttempts, Lane: plan.lane}
	if err := c.rebaseResolver.RequestRebase(ctx, req); err != nil {
		c.warn("merge train: rebase request failed on sibling", "repo", plan.displayRepo, "pr", n, "front", front, "attempt", attempt, "error", err)
		return
	}
	c.info("merge train: rebase requested for sibling after the front merged", "repo", plan.displayRepo, "pr", n, "front", front, "attempt", attempt, "max_attempts", policy.MaxRebaseAttempts)
}

// mergeTrainSharedFiles returns the overlap files the member itself
// touches — the paths a conflict on it can be blamed on.
func mergeTrainSharedFiles(g *mergeTrainGroup, number int) []string {
	if g == nil {
		return nil
	}
	m, ok := g.members[number]
	if !ok {
		return g.OverlapFiles
	}
	mine := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		mine[f] = true
	}
	var out []string
	for _, f := range g.OverlapFiles {
		if mine[f] {
			out = append(out, f)
		}
	}
	return out
}

// mergeTrainSkip is the per-cycle sibling skip: recorded on the stage
// tracker (so the row reads "In merge train · 3rd of 4") and logged at
// debug only — the train's INFO line is the transition, not the wait.
func (c *Engine) mergeTrainSkip(plan *mergeTrainPlan, number int) string {
	g, _ := plan.group(number)
	reason := mergeTrainSiblingReason(g, number)
	c.debug("automerge skip", "repo", plan.displayRepo, "pr", number, "reason", reason, "lane", plan.lane)
	return reason
}

// mergeTrainState is the engine's in-memory train bookkeeping.
type mergeTrainState struct {
	mergeTrainMu           sync.Mutex
	mergeTrainFilesCache   map[string]mergeTrainFilesEntry
	mergeTrainAttemptCount map[string]int
	mergeTrainMerged       map[string]int
	mergeTrainLogged       map[string]string
	// mergeTrainVerdicts remembers, per lane and PR, the last head whose
	// verdict keeps it from fronting (or riding) a train.
	mergeTrainVerdicts map[string]mergeTrainVerdict
	// mergeTrainSeats remembers, per lane and PR, the head that holds a
	// train seat and since when (hivecommons/hive#15825): the seat orders
	// the train (earliest first) and its loss is the ejection the log and
	// the stage store report.
	mergeTrainSeats map[string]mergeTrainSeat
	// mergeTrainNow is the planner's clock (tests inject one).
	mergeTrainNow func() time.Time
}

// mergeTrainSeat is one PR's train seat: the head seated and since when.
type mergeTrainSeat struct {
	sha   string
	since time.Time
}

// Ejection reasons: why a seated member left the train
// (hivecommons/hive#15825). Logged once per ejection and recorded on the
// stage store so the tracker row says what happened.
const (
	mergeTrainEjectHeadMoved   = "head-moved"
	mergeTrainEjectCIPending   = "ci-pending"
	mergeTrainEjectCIRed       = "ci-red"
	mergeTrainEjectVerdictOwed = "verdict-owed"
	mergeTrainEjectConflicting = "conflicting"
	mergeTrainEjectUnderRepair = "under-repair"
	mergeTrainEjectHumanMerge  = "human-merge"
	mergeTrainEjectHeld        = "held"
)

func (c *Engine) mergeTrainClock() time.Time {
	if c != nil && c.mergeTrainNow != nil {
		return c.mergeTrainNow()
	}
	return time.Now()
}

func (c *Engine) mergeTrainSeatFor(lane, owner, repo string, number int) (mergeTrainSeat, bool) {
	if c == nil {
		return mergeTrainSeat{}, false
	}
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	seat, ok := c.mergeTrainSeats[mergeTrainVerdictKey(lane, owner, repo, number)]
	return seat, ok
}

// mergeTrainSettleSeats reconciles the seat memory with this pass's plan:
// every seated member keeps (or takes) its seat at its current head; a
// PR that held a seat and is open but not seated now is ejected — once,
// at INFO, with the why — and the stage store is told so the row reads
// "left the merge train — new push, CI re-running" while it sits back in
// the CI / Review column. A seat whose PR is gone (merged, closed) is
// forgotten silently.
func (c *Engine) mergeTrainSettleSeats(lane, owner, repo, displayRepo string, prs []*gh.PullRequest, plan *mergeTrainPlan) {
	if c == nil || plan == nil {
		return
	}
	now := c.mergeTrainClock()
	open := map[int]*gh.PullRequest{}
	for _, pr := range prs {
		if pr != nil && pr.GetNumber() > 0 {
			open[pr.GetNumber()] = pr
		}
	}
	store := c.stageStoreOrShared()
	type ejection struct {
		number int
		sha    string
		why    string
	}
	var ejected []ejection
	var seatedNow []int
	c.mergeTrainMu.Lock()
	if c.mergeTrainSeats == nil {
		c.mergeTrainSeats = map[string]mergeTrainSeat{}
	}
	prefix := mergeTrainVerdictKey(lane, owner, repo, 0)
	prefix = prefix[:strings.LastIndex(prefix, "#")+1]
	for key, seat := range c.mergeTrainSeats {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil || number <= 0 {
			delete(c.mergeTrainSeats, key)
			continue
		}
		if g, ok := plan.group(number); ok && g.Position(number) > 0 {
			continue
		}
		delete(c.mergeTrainSeats, key)
		pr, isOpen := open[number]
		if !isOpen {
			continue
		}
		sha := pr.GetHead().GetSHA()
		if why := c.mergeTrainEjectWhyLocked(lane, owner, repo, number, sha, seat, pr, plan); why != "" {
			ejected = append(ejected, ejection{number: number, sha: sha, why: why})
		}
	}
	for n, g := range plan.byNumber {
		if g.Position(n) == 0 {
			continue
		}
		key := mergeTrainVerdictKey(lane, owner, repo, n)
		sha := g.members[n].HeadSHA
		if seat, ok := c.mergeTrainSeats[key]; !ok || seat.sha != sha {
			c.mergeTrainSeats[key] = mergeTrainSeat{sha: sha, since: now}
		}
		seatedNow = append(seatedNow, n)
	}
	c.mergeTrainMu.Unlock()
	sort.Slice(ejected, func(a, b int) bool { return ejected[a].number < ejected[b].number })
	for _, e := range ejected {
		c.info("merge train: member ejected", "repo", displayRepo, "pr", e.number, "why", e.why, "lane", lane)
		if store != nil {
			store.NoteTrainEjected(stagelabels.Key(owner+"/"+repo, e.number), e.sha, e.why, now)
		}
	}
	if store != nil {
		for _, n := range seatedNow {
			store.ClearTrainEjected(stagelabels.Key(owner+"/"+repo, n))
		}
	}
}

// mergeTrainEjectWhyLocked names why a seated PR lost its seat; the
// caller holds mergeTrainMu. "" means no ejection worth reporting: the
// train itself dissolved (the front merged and one PR remains), so the PR
// simply merges on its own.
func (c *Engine) mergeTrainEjectWhyLocked(lane, owner, repo string, number int, sha string, seat mergeTrainSeat, pr *gh.PullRequest, plan *mergeTrainPlan) string {
	switch {
	case sha != seat.sha:
		return mergeTrainEjectHeadMoved
	case mergeTrainDirty(pr.GetMergeableState()):
		return mergeTrainEjectConflicting
	}
	if v, ok := c.mergeTrainVerdicts[mergeTrainVerdictKey(lane, owner, repo, number)]; ok && v.sha == sha {
		switch {
		case v.humanMerge:
			return mergeTrainEjectHumanMerge
		case v.blocked:
			return mergeTrainEjectCIRed
		case classifyCIReason(v.reason) == ciReasonPending:
			return mergeTrainEjectCIPending
		case v.readiness == mergeTrainReadinessUnready:
			return mergeTrainEjectVerdictOwed
		}
	}
	if c.mergeTrainUnderRepair(owner, repo, pr) != "" {
		return mergeTrainEjectUnderRepair
	}
	if _, candidate := plan.candidates[number]; candidate {
		return mergeTrainEjectVerdictOwed
	}
	if len(plan.groups) == 0 {
		return ""
	}
	return mergeTrainEjectHeld
}

// mergeTrainVerdict is the remembered lane verdict for one head: Blocked
// heads never front; HumanMerge heads are not train members at all;
// Readiness orders the members that do ride (mergeTrainReadiness*);
// reason is the verdict itself, so an ejection can say "ci-pending".
type mergeTrainVerdict struct {
	sha        string
	blocked    bool
	humanMerge bool
	readiness  int
	reason     string
}

// mergeTrainBlockingReason reports whether a lane's verdict means the head
// cannot merge without someone changing it. A conflict is NOT blocking: the
// train's own rebase step repairs it.
func mergeTrainBlockingReason(reason string) bool {
	switch reason {
	case "not-mergeable", autoMergeReasonForkNotEditable, autoMergeReasonClosedSuperseded, autoMergeReasonClosedEmptyDiff:
		return true
	}
	return isTerminalRequiredCheckHold(reason) || isPermanentMergeRefusal(reason) || classifyCIReason(reason) == ciReasonRed
}

// mergeTrainReadinessOf reads a lane verdict as a readiness tier: a merge
// (""), a soak, a merge-queue wait or a release hold is ready — the head
// would merge but for its turn; CI pending, no reviewer verdict yet or a
// review gate still waiting is unready; any other reason says nothing
// about readiness (unknown).
func mergeTrainReadinessOf(reason string) int {
	if mt, ok := prstage.ParseMergeTrain(reason); ok && mt.State == prstage.MergeTrainStateAdmitted {
		return mergeTrainReadinessReady
	}
	switch {
	case reason == "", strings.HasPrefix(reason, prstage.ContributorLaneReasonSoaking),
		stageengine.MergeQueueReason(reason):
		return mergeTrainReadinessReady
	case classifyCIReason(reason) == ciReasonPending, reason == prstage.ContributorLaneReasonNoVerdict,
		reason == prstage.AutoMergeReasonReviewGatePrefix+prstage.ReviewGateOutcomeAwaiting,
		reason == prstage.AutoMergeReasonReviewGatePrefix+prstage.ReviewGateOutcomeNoBundle:
		return mergeTrainReadinessUnready
	}
	return mergeTrainReadinessUnknown
}

// mergeTrainHumanMergeReason reports whether a lane's verdict means a
// person, not the hive, merges this PR: the author is below the trust the
// lane needs (untrusted-author-role, contributor-lane:no-linked-issue, …),
// the title/label human-merge rule, a human-merge path, a merge GitHub
// refused for a person's review, and the other prstage.HumanMergeReason*
// codes. Such a PR is never a train member: hive's #15425 (untrusted
// author, parked at 090) fronted a train of three and the two PRs behind
// it waited on a merge that was never going to happen. Every code but the
// catch-all HumanMergeReasonPolicy counts — the catch-all is what any
// ordinary skip (soaking, CI pending) maps to.
func mergeTrainHumanMergeReason(reason string) bool {
	code := prstage.HumanMergeReason(reason)
	return code != "" && code != prstage.HumanMergeReasonPolicy
}

func mergeTrainVerdictKey(lane, owner, repo string, number int) string {
	return lane + "|" + owner + "/" + repo + "#" + strconv.Itoa(number)
}

// noteMergeTrainOutcome remembers, per lane and head, the lane's verdict
// on the head: whether it blocks it from fronting a train or excludes it
// altogether, and its readiness tier. A verdict that says none of those
// (a handoff, an error) clears the mark. Keyed by lane so the
// trusted-author lane's "untrusted-author-role" for a PR the contributor
// lane admits never touches the contributor lane's train.
func (c *Engine) noteMergeTrainOutcome(lane, owner, repo string, number int, sha, reason string) {
	if c == nil || number <= 0 {
		return
	}
	key := mergeTrainVerdictKey(lane, owner, repo, number)
	v := mergeTrainVerdict{sha: sha, blocked: mergeTrainBlockingReason(reason), humanMerge: mergeTrainHumanMergeReason(reason), readiness: mergeTrainReadinessOf(reason), reason: reason}
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	if (!v.humanMerge && !v.blocked && v.readiness == mergeTrainReadinessUnknown) || strings.TrimSpace(sha) == "" {
		delete(c.mergeTrainVerdicts, key)
		return
	}
	if c.mergeTrainVerdicts == nil {
		c.mergeTrainVerdicts = map[string]mergeTrainVerdict{}
	}
	c.mergeTrainVerdicts[key] = v
}

func (c *Engine) mergeTrainVerdictFor(lane, owner, repo string, number int, sha string) (mergeTrainVerdict, bool) {
	if c == nil || sha == "" {
		return mergeTrainVerdict{}, false
	}
	c.mergeTrainMu.Lock()
	defer c.mergeTrainMu.Unlock()
	v, ok := c.mergeTrainVerdicts[mergeTrainVerdictKey(lane, owner, repo, number)]
	return v, ok && v.sha == sha
}

// mergeTrainHeadBlocked reports whether this exact head was last seen by
// the lane unable to merge on its own: it does not front a train.
func (c *Engine) mergeTrainHeadBlocked(lane, owner, repo string, number int, sha string) bool {
	v, ok := c.mergeTrainVerdictFor(lane, owner, repo, number, sha)
	return ok && (v.blocked || v.humanMerge)
}

// mergeTrainHeadReadiness is the readiness tier the lane last saw this
// exact head at (mergeTrainReadiness*): the lane's own verdict on the
// head first; else the stage store's resolved stage for the PR when a
// lane evaluated it on its normal path (a stage at or past the merge gate
// is ready, one short of it unready); unknown when the PR has only ever
// been parked behind a front, or never seen.
func (c *Engine) mergeTrainHeadReadiness(lane, owner, repo string, number int, sha string) int {
	if v, ok := c.mergeTrainVerdictFor(lane, owner, repo, number, sha); ok {
		return v.readiness
	}
	if seat, ok := c.mergeTrainSeatFor(lane, owner, repo, number); ok && seat.sha != sha {
		// The head moved since the train seated it: CI and the verdicts
		// are owed again, whatever the stage record still says
		// (hivecommons/hive#15825).
		return mergeTrainReadinessUnknown
	}
	store := c.stageStoreOrShared()
	if store == nil {
		return mergeTrainReadinessUnknown
	}
	rec, ok := store.Get(stagelabels.Key(owner+"/"+repo, number))
	if !ok || rec.Stage == stagelabels.None {
		return mergeTrainReadinessUnknown
	}
	if _, parked := prstage.ParseMergeTrain(rec.Reason); parked {
		return mergeTrainReadinessUnknown
	}
	if rec.TrainEjectedWhy != "" && strings.EqualFold(rec.TrainEjectedHead, sha) {
		// Ejected at this very head: only the lane's own verdict on it
		// (above) re-admits it.
		return mergeTrainReadinessUnknown
	}
	if rec.ActivityHead != "" && !strings.EqualFold(rec.ActivityHead, sha) {
		// The record describes an earlier head: a push since then owes
		// CI and the verdicts again (hivecommons/hive#15825).
		return mergeTrainReadinessUnknown
	}
	if rec.LastPushSha != "" && !strings.EqualFold(rec.LastPushSha, sha) {
		return mergeTrainReadinessUnknown
	}
	if rec.Stage.Number() >= stagelabels.AwaitingMergeAdmission.Number() {
		return mergeTrainReadinessReady
	}
	return mergeTrainReadinessUnready
}

// mergeTrainHeadHumanMerge reports whether this exact head was last seen
// by the lane as a person's merge: it rides no train at all.
func (c *Engine) mergeTrainHeadHumanMerge(lane, owner, repo string, number int, sha string) bool {
	v, ok := c.mergeTrainVerdictFor(lane, owner, repo, number, sha)
	return ok && v.humanMerge
}

// String renders a request for logs.
func (r RebaseRequest) String() string {
	return fmt.Sprintf("%s#%d onto %s (behind #%d, attempt %d/%d, %s)", r.Repo, r.Number, r.Base, r.Front, r.Attempt, r.MaxAttempts, r.Lane)
}
