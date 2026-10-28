package automerge

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// autoMergeReasonMergeTrainQueued is the skip reason of an approved PR that
// shares changed files with another candidate on the same base and is not
// that group's merge-train front (hivecommons/hive#15117). Merging every
// overlapping PR in parallel dirties all but the first; the train merges one
// front per group and leaves the rest for the next pass against the new base.
const autoMergeReasonMergeTrainQueued = "merge-train-queued"

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

// mergeTrainState is the engine's per-process train memory: changed files per
// PR cached by head SHA, heads last seen blocked, and the slot last logged per
// PR so a transition logs once rather than every pass.
type mergeTrainState struct {
	mu      sync.Mutex
	files   map[string]mergeTrainFiles
	blocked map[string]string
	logged  map[string]string
}

type mergeTrainFiles struct {
	sha   string
	files []string
}

func (c *Engine) mergeTrainEnabled() bool {
	return c.mergeTrainEnabledFn == nil || c.mergeTrainEnabledFn()
}

// mergeTrainBlockingReason reports whether a lane's skip reason means the head
// cannot merge without someone changing it, so it must not front a train.
func mergeTrainBlockingReason(reason string) bool {
	switch reason {
	case "conflicting", "not-mergeable", autoMergeReasonForkNotEditable, autoMergeReasonClosedSuperseded:
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

// mergeTrainSlots plans the merge trains for one repo's listed PRs. eligible
// is the lane's cheap list-payload prefilter; only PRs it admits, on a base
// with at least two of them, cost a files lookup. A PR whose files cannot be
// read stays out of every train, so a lookup failure never holds a merge back.
func (c *Engine) mergeTrainSlots(ctx context.Context, displayRepo, owner, repo string, prs []*gh.PullRequest, eligible func(*gh.PullRequest) bool) map[int]mergeTrainSlot {
	if c == nil || c.gh == nil || !c.mergeTrainEnabled() {
		return nil
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
			c.mergeTrain.mu.Lock()
			blocked := c.mergeTrain.blocked[key] == pr.GetHead().GetSHA()
			c.mergeTrain.mu.Unlock()
			members = append(members, mergeTrainMember{
				Number:  pr.GetNumber(),
				Base:    base,
				Created: pr.GetCreatedAt().Time,
				Blocked: blocked,
				Files:   files,
			})
		}
	}
	slots := planMergeTrains(members)
	c.logMergeTrainTransitions(displayRepo, owner, repo, slots, live, listed)
	return slots
}

// logMergeTrainTransitions writes one INFO line when a PR joins a train or its
// slot changes, and forgets cached files and blocked marks of this repo's PRs
// that are no longer listed (live: files were read; listed: lane-eligible).
func (c *Engine) logMergeTrainTransitions(displayRepo, owner, repo string, slots map[int]mergeTrainSlot, live, listed map[string]bool) {
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
		lines = append(lines, []any{"repo", displayRepo, "pr", n, "front", slot.Front, "position", slot.Position, "size", slot.Size, "overlap_files", strings.Join(slot.OverlapFiles, ",")})
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

// mergeTrainQueued reports whether number waits behind another PR's front.
func mergeTrainQueued(slots map[int]mergeTrainSlot, number int) (mergeTrainSlot, bool) {
	slot, ok := slots[number]
	return slot, ok && slot.Position > 1
}
