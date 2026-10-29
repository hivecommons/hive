package automerge

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// Merge train (hivecommons/hive#15117).
//
// Approved PRs to the same base that touch the same files were all merged in
// parallel; the first to land turned every sibling dirty and each lane then
// logged them `conflicting` / `label:needs-rebase` every cycle with nothing
// ordering them. The train makes the lane order them instead:
//
//   - candidates on the same base whose changed files overlap (transitively)
//     form one train (GET /pulls/{n}/files, cached by head SHA);
//   - the train has one front — a head not last seen conflicting, red or
//     held first, then the oldest — that the lane evaluates and merges as
//     before; every sibling is parked as
//     `merge-train:queued-behind#<front>:<pos>/<size>:…` (stage 080, never a
//     human stage); a group whose best member is itself blocked imposes no
//     ordering (fail-open), so nobody waits behind a PR that is not moving;
//   - right after the front merges each sibling is read back: `behind` gets
//     update-branch, `dirty` gets a rebase-resolve request through the
//     RebaseResolver hook, at most MaxRebaseAttempts per PR per front; a
//     conflicting front is sent to the same hook, and past the guard it
//     parks at 085 for a person and stops fronting its train.
//
// Train state (files cache, blocked heads, attempt counters, last merged
// front, transition log) is in-memory per process; a restart re-plans.

// autoMergeReasonMergeTrainQueued is the skip reason of an approved PR that
// shares changed files with another candidate on the same base and is not
// that group's merge-train front (hivecommons/hive#15117). Merging every
// overlapping PR in parallel dirties all but the first; the train merges one
// front per group and leaves the rest for the next pass against the new base.
const autoMergeReasonMergeTrainQueued = "merge-train-queued"

// autoMergeReasonMergeTrainRebasing is the tick-stat bucket of a conflicting
// front the train sent for a rebase (or gave up on); the per-PR stage reason
// is the full prstage.MergeTrain reason with the attempt count.
const autoMergeReasonMergeTrainRebasing = "merge-train-rebasing"

// MergeTrainPolicy is the live auto_merge.merge_train.* policy.
type MergeTrainPolicy struct {
	Enabled           bool
	MaxRebaseAttempts int
}

// DefaultMergeTrainPolicy is the policy without Options hooks: on, two rebase
// attempts per PR per front (config.DefaultMergeTrainMaxRebaseAttempts).
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
}

// String renders a request for logs.
func (r RebaseRequest) String() string {
	return fmt.Sprintf("%s#%d onto %s (behind #%d, attempt %d/%d, %s)", r.Repo, r.Number, r.Base, r.Front, r.Attempt, r.MaxAttempts, r.Lane)
}

// RebaseResolver enqueues a rebase-resolve task for a conflicting PR. The
// rebase-then-merge follow-through (hivecommons/hive#15104, pkg/rebasemerge)
// is the intended implementation: cmd/hive adapts its store here so the
// fixer lane rebases the head and the dashboard ticker merges it on green.
// TODO(hivecommons/hive#15104): wire pkg/rebasemerge.Store as the resolver
// once it lands; until then a nil resolver leaves a dirty front on its
// normal `conflicting` path (which then stops it fronting its train) and
// logs the gap once per PR head.
type RebaseResolver interface {
	RequestRebase(ctx context.Context, req RebaseRequest) error
}

// RebaseResolverFunc adapts a function to RebaseResolver.
type RebaseResolverFunc func(ctx context.Context, req RebaseRequest) error

// RequestRebase implements RebaseResolver.
func (f RebaseResolverFunc) RequestRebase(ctx context.Context, req RebaseRequest) error {
	return f(ctx, req)
}

// mergeTrainMember is one candidate PR as the train planner sees it.
type mergeTrainMember struct {
	Number  int
	Base    string
	Created time.Time
	// Blocked marks a head the sweep last saw unable to merge on its own
	// (conflicting, red, or a terminal hold): it never fronts a train while a
	// mergeable member exists, so it cannot stall its siblings.
	Blocked bool
	Files   []string
	// Head and HeadSHA are the head branch and commit; MergeableState is
	// GitHub's live mergeable_state, read for a front only (the list API
	// does not carry it) to decide whether the train must rebase it.
	Head           string
	HeadSHA        string
	MergeableState string
}

// mergeTrainSlot is a PR's place in its overlap group. Position 1 is the
// front; OverlapFiles are the member's files that another member also changes.
type mergeTrainSlot struct {
	Front        int
	Position     int
	Size         int
	OverlapFiles []string
}

// planMergeTrains groups members per base branch by transitively overlapping
// changed files and orders each group of two or more: mergeable before
// blocked, then oldest, then lowest number. A group whose best member is
// itself blocked gets no slots, so a train never holds back siblings behind a
// front that cannot merge.
func planMergeTrains(members []mergeTrainMember) map[int]mergeTrainSlot {
	parent := make([]int, len(members))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	owner := make(map[string]int)
	for i, m := range members {
		for _, f := range m.Files {
			key := m.Base + "\x00" + f
			if j, ok := owner[key]; ok {
				parent[find(i)] = find(j)
				continue
			}
			owner[key] = i
		}
	}
	groups := make(map[int][]int)
	for i := range members {
		root := find(i)
		groups[root] = append(groups[root], i)
	}
	slots := make(map[int]mergeTrainSlot)
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(a, b int) bool {
			ma, mb := members[group[a]], members[group[b]]
			if ma.Blocked != mb.Blocked {
				return !ma.Blocked
			}
			if !ma.Created.IsZero() && !mb.Created.IsZero() && !ma.Created.Equal(mb.Created) {
				return ma.Created.Before(mb.Created)
			}
			return ma.Number < mb.Number
		})
		front := members[group[0]]
		if front.Blocked {
			continue
		}
		touches := make(map[string]int)
		for _, i := range group {
			for _, f := range uniqueStrings(members[i].Files) {
				touches[f]++
			}
		}
		for pos, i := range group {
			var overlap []string
			for _, f := range uniqueStrings(members[i].Files) {
				if touches[f] > 1 {
					overlap = append(overlap, f)
				}
			}
			sort.Strings(overlap)
			slots[members[i].Number] = mergeTrainSlot{Front: front.Number, Position: pos + 1, Size: len(group), OverlapFiles: overlap}
		}
	}
	return slots
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// mergeTrainGroup is one train as the lanes and the stage reasons see it:
// the members in slot order (front first) and the files at least two of them
// touch. It is a view over planMergeTrains' slots, not a second planner.
type mergeTrainGroup struct {
	Base         string
	Order        []int
	OverlapFiles []string
	members      map[int]mergeTrainMember
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

// mergeTrainGroups folds slots into one group per front, ordered by base and
// then front number so callers iterate deterministically.
func mergeTrainGroups(members []mergeTrainMember, slots map[int]mergeTrainSlot) []mergeTrainGroup {
	byNumber := make(map[int]mergeTrainMember, len(members))
	for _, m := range members {
		byNumber[m.Number] = m
	}
	byFront := make(map[int]*mergeTrainGroup)
	var fronts []int
	for n, slot := range slots {
		g, ok := byFront[slot.Front]
		if !ok {
			g = &mergeTrainGroup{Base: byNumber[slot.Front].Base, Order: make([]int, slot.Size), members: make(map[int]mergeTrainMember, slot.Size)}
			byFront[slot.Front] = g
			fronts = append(fronts, slot.Front)
		}
		if slot.Position >= 1 && slot.Position <= len(g.Order) {
			g.Order[slot.Position-1] = n
		}
		g.members[n] = byNumber[n]
		g.OverlapFiles = append(g.OverlapFiles, slot.OverlapFiles...)
	}
	sort.Slice(fronts, func(a, b int) bool {
		ga, gb := byFront[fronts[a]], byFront[fronts[b]]
		if ga.Base != gb.Base {
			return ga.Base < gb.Base
		}
		return fronts[a] < fronts[b]
	})
	out := make([]mergeTrainGroup, 0, len(fronts))
	for _, f := range fronts {
		g := byFront[f]
		g.OverlapFiles = uniqueStrings(g.OverlapFiles)
		sort.Strings(g.OverlapFiles)
		out = append(out, *g)
	}
	return out
}

// mergeTrainDirty reports whether a mergeable state is a conflict.
func mergeTrainDirty(state string) bool {
	s := strings.ToLower(strings.TrimSpace(state))
	return s == "dirty" || s == "conflicting"
}

// mergeTrainPlan is one lane's trains for one repository this pass.
type mergeTrainPlan struct {
	lane, displayRepo, owner, repo string
	slots                          map[int]mergeTrainSlot
	groups                         []mergeTrainGroup
	byNumber                       map[int]*mergeTrainGroup
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
	if p == nil {
		return false
	}
	_, queued := mergeTrainQueued(p.slots, number)
	return queued
}

// isFront reports whether number leads a train this pass.
func (p *mergeTrainPlan) isFront(number int) bool {
	if p == nil {
		return false
	}
	slot, ok := p.slots[number]
	return ok && slot.Position == 1
}

// mergeTrainQueued reports whether number waits behind another PR's front.
func mergeTrainQueued(slots map[int]mergeTrainSlot, number int) (mergeTrainSlot, bool) {
	slot, ok := slots[number]
	return slot, ok && slot.Position > 1
}

// mergeTrainState is the engine's per-process train memory: changed files per
// PR cached by head SHA, heads last seen blocked, the slot last logged per
// PR so a transition logs once rather than every pass, the rebase attempts
// per PR behind a front and the front that last merged per base.
type mergeTrainState struct {
	mu       sync.Mutex
	files    map[string]mergeTrainFiles
	blocked  map[string]string
	logged   map[string]string
	attempts map[string]int
	merged   map[string]int
}

type mergeTrainFiles struct {
	sha   string
	files []string
}

func (c *Engine) mergeTrainEnabled() bool {
	return c.mergeTrainEnabledFn == nil || c.mergeTrainEnabledFn()
}

// mergeTrainPolicyNow reads the live policy.
func (c *Engine) mergeTrainPolicyNow() MergeTrainPolicy {
	if c == nil {
		return DefaultMergeTrainPolicy
	}
	p := MergeTrainPolicy{Enabled: c.mergeTrainEnabled()}
	if c.mergeTrainMaxRebaseAttemptsFn != nil {
		p.MaxRebaseAttempts = c.mergeTrainMaxRebaseAttemptsFn()
	}
	if p.MaxRebaseAttempts <= 0 {
		p.MaxRebaseAttempts = DefaultMergeTrainPolicy.MaxRebaseAttempts
	}
	return p
}

// mergeTrainCandidate reports whether a lane's prefilter verdict admits a PR
// to the train: a clean pass, or — when a rebase resolver can repair it — a
// `needs-rebase` label, the very PR the train exists to order and repair.
func (c *Engine) mergeTrainCandidate(prefilterReason string) bool {
	if prefilterReason == "" {
		return true
	}
	return prefilterReason == "label:needs-rebase" && c != nil && c.rebaseResolver != nil
}

// mergeTrainBlockingReason reports whether a lane's skip reason means the head
// cannot merge without someone changing it, so it must not front a train.
func mergeTrainBlockingReason(reason string) bool {
	switch reason {
	case "conflicting", "not-mergeable", "label:needs-rebase", autoMergeReasonForkNotEditable, autoMergeReasonClosedSuperseded:
		return true
	}
	return isTerminalRequiredCheckHold(reason) || isPermanentMergeRefusal(reason) || classifyCIReason(reason) == ciReasonRed
}

// noteMergeTrainOutcome records whether the lane's verdict for a head blocks
// it from fronting a train. A merged or non-blocking verdict clears the mark.
func (c *Engine) noteMergeTrainOutcome(owner, repo string, number int, sha, reason string) {
	if c == nil || number <= 0 {
		return
	}
	key := c.evaluatedHeadKey(owner, repo, number)
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	if !mergeTrainBlockingReason(reason) || strings.TrimSpace(sha) == "" {
		delete(c.mergeTrain.blocked, key)
		return
	}
	if c.mergeTrain.blocked == nil {
		c.mergeTrain.blocked = make(map[string]string)
	}
	c.mergeTrain.blocked[key] = sha
}

// noteMergeTrainFrontSkip records a lane skip that stopped a train's front
// before its merge attempt, so a front that cannot move stops fronting.
func (c *Engine) noteMergeTrainFrontSkip(plan *mergeTrainPlan, pr *gh.PullRequest, reason string) {
	if pr == nil || !plan.isFront(pr.GetNumber()) {
		return
	}
	c.noteMergeTrainOutcome(plan.owner, plan.repo, pr.GetNumber(), pr.GetHead().GetSHA(), reason)
}

// mergeTrainHeadBlocked reports whether this exact head was last seen blocked.
func (c *Engine) mergeTrainHeadBlocked(owner, repo string, number int, sha string) bool {
	if c == nil || strings.TrimSpace(sha) == "" {
		return false
	}
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	return c.mergeTrain.blocked[c.evaluatedHeadKey(owner, repo, number)] == sha
}

// mergeTrainChangedFiles returns pr's changed file names, served from the
// cache while the head SHA is unchanged.
func (c *Engine) mergeTrainChangedFiles(ctx context.Context, owner, repo string, pr *gh.PullRequest) ([]string, error) {
	key := c.evaluatedHeadKey(owner, repo, pr.GetNumber())
	sha := pr.GetHead().GetSHA()
	c.mergeTrain.mu.Lock()
	cached, ok := c.mergeTrain.files[key]
	c.mergeTrain.mu.Unlock()
	if ok && cached.sha == sha {
		return cached.files, nil
	}
	changed, err := ListChangedFiles(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_train"), c.gh, owner, repo, pr)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(changed))
	for _, f := range changed {
		files = append(files, f.Filename)
	}
	c.mergeTrain.mu.Lock()
	if c.mergeTrain.files == nil {
		c.mergeTrain.files = make(map[string]mergeTrainFiles)
	}
	c.mergeTrain.files[key] = mergeTrainFiles{sha: sha, files: files}
	c.mergeTrain.mu.Unlock()
	return files, nil
}

// mergeTrainSlots plans the merge trains for one lane's listed PRs on one
// repo. eligible is the lane's cheap list-payload prefilter; only PRs it
// admits, on a base with at least two of them, cost a files lookup, and each
// train's front one live read of its mergeable state. A PR whose files cannot
// be read stays out of every train, so a lookup failure never holds a merge
// back. The plan is never nil; a disabled train plans nothing.
func (c *Engine) mergeTrainSlots(ctx context.Context, lane, displayRepo, owner, repo string, prs []*gh.PullRequest, eligible func(*gh.PullRequest) bool) *mergeTrainPlan {
	plan := &mergeTrainPlan{lane: lane, displayRepo: displayRepo, owner: owner, repo: repo, slots: map[int]mergeTrainSlot{}, byNumber: map[int]*mergeTrainGroup{}}
	if c == nil || c.gh == nil || !c.mergeTrainEnabled() {
		return plan
	}
	perBase := make(map[string][]*gh.PullRequest)
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() <= 0 || !eligible(pr) {
			continue
		}
		base := pr.GetBase().GetRef()
		perBase[base] = append(perBase[base], pr)
	}
	var members []mergeTrainMember
	live := make(map[string]bool)
	listed := make(map[string]bool)
	for _, group := range perBase {
		for _, pr := range group {
			listed[c.evaluatedHeadKey(owner, repo, pr.GetNumber())] = true
		}
	}
	for base, group := range perBase {
		if len(group) < 2 {
			continue
		}
		for _, pr := range group {
			files, err := c.mergeTrainChangedFiles(ctx, owner, repo, pr)
			if err != nil {
				c.warn("merge train could not list PR files; PR stays out of the train", "repo", displayRepo, "pr", pr.GetNumber(), "error", err)
				continue
			}
			key := c.evaluatedHeadKey(owner, repo, pr.GetNumber())
			live[key] = true
			members = append(members, mergeTrainMember{
				Number:         pr.GetNumber(),
				Base:           base,
				Created:        pr.GetCreatedAt().Time,
				Blocked:        c.mergeTrainHeadBlocked(owner, repo, pr.GetNumber(), pr.GetHead().GetSHA()),
				Files:          files,
				Head:           pr.GetHead().GetRef(),
				HeadSHA:        pr.GetHead().GetSHA(),
				MergeableState: pr.GetMergeableState(),
			})
		}
	}
	slots := planMergeTrains(members)
	// Each front's live mergeable state decides whether the train must
	// rebase it; a front closed since the listing leaves and the group
	// re-plans. Bounded: every re-plan has one member fewer.
	read := make(map[int]bool)
	for {
		closed := false
		for n, slot := range slots {
			if slot.Position != 1 || read[n] {
				continue
			}
			read[n] = true
			i := mergeTrainMemberIndex(members, n)
			if i < 0 || members[i].MergeableState != "" {
				continue
			}
			pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_train"), owner, repo, n)
			if err != nil {
				c.warn("merge train could not read the front's state; treated as unknown", "repo", displayRepo, "pr", n, "error", err)
				continue
			}
			if !strings.EqualFold(pr.GetState(), "open") {
				members = append(members[:i], members[i+1:]...)
				closed = true
				continue
			}
			members[i].MergeableState = pr.GetMergeableState()
			if sha := pr.GetHead().GetSHA(); sha != "" {
				members[i].HeadSHA = sha
			}
			if ref := pr.GetHead().GetRef(); ref != "" {
				members[i].Head = ref
			}
		}
		if !closed {
			break
		}
		slots = planMergeTrains(members)
	}
	plan.slots = slots
	plan.groups = mergeTrainGroups(members, slots)
	for i := range plan.groups {
		g := &plan.groups[i]
		for _, n := range g.Order {
			plan.byNumber[n] = g
		}
	}
	c.logMergeTrainTransitions(displayRepo, owner, repo, lane, slots, live, listed)
	return plan
}

func mergeTrainMemberIndex(members []mergeTrainMember, number int) int {
	for i, m := range members {
		if m.Number == number {
			return i
		}
	}
	return -1
}

// logMergeTrainTransitions writes one INFO line when a PR joins a train or its
// slot changes, and forgets cached files and blocked marks of this repo's PRs
// that are no longer listed (live: files were read; listed: lane-eligible).
func (c *Engine) logMergeTrainTransitions(displayRepo, owner, repo, lane string, slots map[int]mergeTrainSlot, live, listed map[string]bool) {
	prefix := strings.ToLower(strings.TrimSpace(owner)) + "/" + strings.ToLower(strings.TrimSpace(repo)) + "#"
	numbers := make([]int, 0, len(slots))
	for n := range slots {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	c.mergeTrain.mu.Lock()
	if c.mergeTrain.logged == nil {
		c.mergeTrain.logged = make(map[string]string)
	}
	var lines [][]any
	current := make(map[string]bool, len(slots))
	for _, n := range numbers {
		slot := slots[n]
		key := c.evaluatedHeadKey(owner, repo, n)
		current[key] = true
		summary := strconv.Itoa(slot.Front) + "/" + strconv.Itoa(slot.Position) + "/" + strconv.Itoa(slot.Size)
		if c.mergeTrain.logged[key] == summary {
			continue
		}
		c.mergeTrain.logged[key] = summary
		lines = append(lines, []any{"repo", displayRepo, "lane", lane, "pr", n, "front", slot.Front, "position", slot.Position, "size", slot.Size, "overlap_files", strings.Join(slot.OverlapFiles, ",")})
	}
	for key := range c.mergeTrain.logged {
		if strings.HasPrefix(key, prefix) && !current[key] {
			delete(c.mergeTrain.logged, key)
		}
	}
	for key := range c.mergeTrain.files {
		if strings.HasPrefix(key, prefix) && !live[key] {
			delete(c.mergeTrain.files, key)
		}
	}
	for key := range c.mergeTrain.blocked {
		if strings.HasPrefix(key, prefix) && !listed[key] {
			delete(c.mergeTrain.blocked, key)
		}
	}
	c.mergeTrain.mu.Unlock()
	for _, args := range lines {
		c.info("automerge merge train", args...)
	}
}

// mergeTrainAttemptKey keys the rebase loop guard: one PR behind one front.
func (c *Engine) mergeTrainAttemptKey(owner, repo string, number, front int) string {
	return c.evaluatedHeadKey(owner, repo, number) + "@" + strconv.Itoa(front)
}

func (c *Engine) mergeTrainAttempts(owner, repo string, number, front int) int {
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	return c.mergeTrain.attempts[c.mergeTrainAttemptKey(owner, repo, number, front)]
}

func (c *Engine) bumpMergeTrainAttempts(owner, repo string, number, front int) int {
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	if c.mergeTrain.attempts == nil {
		c.mergeTrain.attempts = make(map[string]int)
	}
	key := c.mergeTrainAttemptKey(owner, repo, number, front)
	c.mergeTrain.attempts[key]++
	return c.mergeTrain.attempts[key]
}

func mergeTrainBaseKey(owner, repo, base string) string {
	return strings.ToLower(strings.TrimSpace(owner)) + "/" + strings.ToLower(strings.TrimSpace(repo)) + ":" + base
}

// mergeTrainLastFront is the front that last merged on owner/repo:base
// (0 when none this process), the "front" a leading dirty PR is behind.
func (c *Engine) mergeTrainLastFront(owner, repo, base string) int {
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	return c.mergeTrain.merged[mergeTrainBaseKey(owner, repo, base)]
}

func (c *Engine) noteMergeTrainFrontMerged(owner, repo, base string, front int) {
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	if c.mergeTrain.merged == nil {
		c.mergeTrain.merged = make(map[string]int)
	}
	c.mergeTrain.merged[mergeTrainBaseKey(owner, repo, base)] = front
}

// markMergeTrainHeadBlocked stops a head fronting its train until it changes.
func (c *Engine) markMergeTrainHeadBlocked(owner, repo string, number int, sha string) {
	if strings.TrimSpace(sha) == "" {
		return
	}
	c.mergeTrain.mu.Lock()
	defer c.mergeTrain.mu.Unlock()
	if c.mergeTrain.blocked == nil {
		c.mergeTrain.blocked = make(map[string]string)
	}
	c.mergeTrain.blocked[c.evaluatedHeadKey(owner, repo, number)] = sha
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

// mergeTrainFront handles a train's front before the lane evaluates it: a
// conflicting front is sent to the rebase resolver (bounded by the loop
// guard) and parked as rebasing; past the guard it parks for a person and
// is marked blocked, so the next pass fronts a sibling that can move.
// handled=false hands a clean or behind front to the lane's normal path, and
// a dirty one too when no resolver is wired (its `conflicting` verdict then
// marks it blocked through noteMergeTrainOutcome).
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
		c.markMergeTrainHeadBlocked(plan.owner, plan.repo, number, m.HeadSHA)
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
	if plan == nil || c == nil || c.gh == nil {
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
	req := RebaseRequest{Repo: plan.owner + "/" + plan.repo, Number: n, Base: g.Base, Head: live.GetHead().GetRef(), HeadSHA: live.GetHead().GetSHA(), Front: front, Attempt: attempt, MaxAttempts: policy.MaxRebaseAttempts, Lane: plan.lane}
	if err := c.rebaseResolver.RequestRebase(ctx, req); err != nil {
		c.warn("merge train: rebase request failed on sibling", "repo", plan.displayRepo, "pr", n, "front", front, "attempt", attempt, "error", err)
		return
	}
	c.info("merge train: rebase requested for sibling after the front merged", "repo", plan.displayRepo, "pr", n, "front", front, "attempt", attempt, "max_attempts", policy.MaxRebaseAttempts)
}

// mergeTrainSkip is the per-cycle sibling skip reason: recorded on the stage
// tracker (so the row reads "In merge train · 3rd of 4") and logged at debug
// only — the train's INFO line is the transition, not the wait.
func (c *Engine) mergeTrainSkip(plan *mergeTrainPlan, number int) string {
	g, ok := plan.group(number)
	if !ok {
		return autoMergeReasonMergeTrainQueued
	}
	reason := mergeTrainSiblingReason(g, number)
	c.debug("automerge skip", "repo", plan.displayRepo, "pr", number, "reason", reason, "lane", plan.lane)
	return reason
}
