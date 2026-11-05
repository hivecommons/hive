package automerge

// An approving review counts for the head only when it approved that
// content (hivecommons/hive#14664's "a new push re-arms the fence").
//
// GitHub moves a person's existing review onto a merge of the base into the
// branch: projectbluefin/utah#1078's review submitted 2026-11-04 01:17Z is
// reported on 9d8ceab, a base merge made 2026-11-05 19:50Z, and the hive
// said "@Danathar approved head 9d8ceab" and merged it. Read alone,
// commit_id cannot tell that from an approval of the head. The hive's own
// base refresh does the opposite: the review stays on the old head, and the
// owner's answer to requires_human was asked for again
// (projectbluefin/utah-packages#877).
//
// So a trusted APPROVED review reaches head when either
//   - GitHub reports it on head and head is not a merge, or it was
//     submitted at or after the head commit's date, or
//   - head is a chain of base merges (BaseMergeParent: the PR's diff
//     unchanged, whoever merged) back to the commit the review approved, or
//     to a commit no newer than the review. The approval is then "carried
//     over a base merge", and the fence comment says so.
// A review with no submitted_at keeps the old commit_id rule.

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// approvalCarryMaxHops bounds the base merges walked back from the head.
const approvalCarryMaxHops = 5

const approvalReachCacheLimit = 4096

// approvalReach is whether a review reaches a head, and whether only by
// being carried over base merges.
type approvalReach struct {
	ok, carried bool
}

type approvalReachCache struct {
	mu sync.Mutex
	m  map[string]approvalReach
}

func (c *approvalReachCache) get(key string) (approvalReach, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[key]
	return r, ok
}

func (c *approvalReachCache) put(key string, r approvalReach) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= approvalReachCacheLimit {
		c.m = map[string]approvalReach{}
	}
	c.m[key] = r
}

// reviewReachesHead answers for one APPROVED review. The answer for a fixed
// review and head never changes, so it is memoised; a lookup failure is
// not memoised and reads as false (the fence stays, the person is asked).
func (c *Engine) reviewReachesHead(ctx context.Context, owner, repo string, number int, head string, rv *gh.PullRequestReview) approvalReach {
	head = strings.TrimSpace(head)
	reviewed := strings.TrimSpace(rv.GetCommitID())
	submitted := rv.GetSubmittedAt().Time
	if submitted.IsZero() {
		return approvalReach{ok: reviewed == head}
	}
	if c == nil || c.gh == nil || head == "" {
		return approvalReach{}
	}
	key := owner + "/" + repo + "#" + strconv.Itoa(number) + "@" + head + "<-" + strconv.FormatInt(rv.GetID(), 10) + ":" + reviewed
	if r, ok := c.approvalReach.get(key); ok {
		return r
	}
	r, known := c.walkApprovalToHead(ctx, owner, repo, number, head, reviewed, submitted)
	if known {
		c.approvalReach.put(key, r)
	}
	return r
}

// walkApprovalToHead does the reads. known is false when GitHub could not be
// read, so the caller does not memoise the answer.
func (c *Engine) walkApprovalToHead(ctx context.Context, owner, repo string, number int, head, reviewed string, submitted time.Time) (approvalReach, bool) {
	ctx = hgithub.WithRESTCaller(ctx, "hive:approval_head_binding")
	commit, _, err := c.gh.Repositories.GetCommit(ctx, owner, repo, head, nil)
	if err != nil || commit == nil {
		return approvalReach{}, false
	}
	if reviewed == head && (len(commit.Parents) < 2 || !submitted.Before(commitDate(commit))) {
		// GitHub moves a review only onto a merge, so a review it reports on
		// a non-merge head approved that head, whatever the pusher's clock
		// wrote as its date.
		return approvalReach{ok: true}, true
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil || pr == nil {
		return approvalReach{}, false
	}
	baseRef := strings.TrimSpace(pr.GetBase().GetRef())
	cur := head
	for hop := 0; hop < approvalCarryMaxHops; hop++ {
		prev, ok := hgithub.BaseMergeParent(ctx, c.gh.Repositories, owner, repo, baseRef, commit, cur)
		if !ok {
			return approvalReach{}, true
		}
		if prev == reviewed {
			return approvalReach{ok: true, carried: true}, true
		}
		if commit, _, err = c.gh.Repositories.GetCommit(ctx, owner, repo, prev, nil); err != nil || commit == nil {
			return approvalReach{}, false
		}
		if reviewed == head && !submitted.Before(commitDate(commit)) {
			// GitHub moved the review off this commit onto the merges above.
			return approvalReach{ok: true, carried: true}, true
		}
		cur = prev
	}
	return approvalReach{}, true
}

func commitDate(commit *gh.RepositoryCommit) time.Time {
	if commit == nil || commit.GetCommit() == nil {
		return time.Time{}
	}
	if d := commit.GetCommit().GetCommitter().GetDate().Time; !d.IsZero() {
		return d
	}
	return commit.GetCommit().GetAuthor().GetDate().Time
}
