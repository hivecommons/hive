package github

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/review"
)

// DecisionCarriedOverBaseRefreshMessage is the one INFO line the carry logs
// when a recorded operator decision follows the PR onto a base-refreshed
// head (hivecommons/hive#15649).
const DecisionCarriedOverBaseRefreshMessage = "decision carried over base refresh"

// baseRefreshCacheLimit bounds the per-process (pr, head) memo so a long
// run never grows it without limit; a reset simply re-asks GitHub.
const baseRefreshCacheLimit = 4096

// baseRefreshMergeParents is the parent count of a merge commit.
const baseRefreshMergeParents = 2

// baseRefreshMaxHops bounds how many consecutive hive base refreshes the
// carry walks back through to reach the decided head (hivecommons/hive#15766).
const baseRefreshMaxHops = 5

// BaseRefreshRepos is the slice of the repositories API the base-refresh
// check reads: one commit lookup and two three-dot compares per new head.
type BaseRefreshRepos interface {
	GetCommit(ctx context.Context, owner, repo, sha string, opts *gh.ListOptions) (*gh.RepositoryCommit, *gh.Response, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string, opts *gh.ListOptions) (*gh.CommitsComparison, *gh.Response, error)
}

// BaseRefreshChecker answers whether a PR's new head is nothing but the
// hive's own refresh of the base branch onto the old head, memoised per
// (repo, pr, newHead, oldHead) because the answer for two fixed SHAs never
// changes and every merge lane asks it on every tick.
type BaseRefreshChecker struct {
	mu    sync.Mutex
	cache map[string]bool
}

// HeadIsBaseRefreshOf reports whether newHead on owner/repo#number is a
// merge commit authored by the App (appBotLogin) whose parents are oldHead
// and a commit on baseRef, and whose three-dot diff against baseRef is
// file-for-file identical to oldHead's. That is exactly the head the
// require_current_base gate produces when it merges the base into the
// branch: the PR's content is unchanged, only its base moved. Any other
// head change — a content commit (even one that also merged the base), a
// merge by anyone but the App, a rebase, a diff that differs — is false.
// Lookup failures are false: the caller falls back to asking the person.
func (b *BaseRefreshChecker) HeadIsBaseRefreshOf(ctx context.Context, api BaseRefreshRepos, owner, repo string, number int, baseRef, oldHead, newHead, appBotLogin string) bool {
	oldHead, newHead, baseRef = strings.TrimSpace(oldHead), strings.TrimSpace(newHead), strings.TrimSpace(baseRef)
	if b == nil || api == nil || oldHead == "" || newHead == "" || baseRef == "" || oldHead == newHead || strings.TrimSpace(appBotLogin) == "" {
		return false
	}
	if got, seen := b.Known(owner, repo, number, oldHead, newHead); seen {
		return got
	}
	ok := headIsBaseRefreshChainOf(ctx, api, owner, repo, baseRef, oldHead, newHead, appBotLogin)
	b.mu.Lock()
	if b.cache == nil || len(b.cache) >= baseRefreshCacheLimit {
		b.cache = map[string]bool{}
	}
	b.cache[baseRefreshKey(owner, repo, number, oldHead, newHead)] = ok
	b.mu.Unlock()
	return ok
}

// Known returns the memoised answer for (repo, pr, newHead, oldHead) and
// whether there is one, so callers can skip resolving inputs GitHub would
// otherwise have to be asked for.
func (b *BaseRefreshChecker) Known(owner, repo string, number int, oldHead, newHead string) (ok, seen bool) {
	if b == nil {
		return false, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ok, seen = b.cache[baseRefreshKey(owner, repo, number, strings.TrimSpace(oldHead), strings.TrimSpace(newHead))]
	return ok, seen
}

func baseRefreshKey(owner, repo string, number int, oldHead, newHead string) string {
	return owner + "/" + repo + "#" + strconv.Itoa(number) + "@" + newHead + "<-" + oldHead
}

// headIsBaseRefreshChainOf walks back from newHead along the first-parent
// chain for at most baseRefreshMaxHops hops. Each commit is fetched exactly
// once and must itself be a hive base refresh of its first parent
// (App-authored merge, base parent on the base branch, diff unchanged); the
// walk succeeds when a hop's non-base parent is oldHead. Each hop allows
// oldHead in either parent position (per-hop tolerance).
func headIsBaseRefreshChainOf(ctx context.Context, api BaseRefreshRepos, owner, repo, baseRef, oldHead, newHead, appBotLogin string) bool {
	ctx = WithRESTCaller(ctx, "hive:decision_base_refresh")
	cur := newHead
	for hop := 0; hop < baseRefreshMaxHops; hop++ {
		commit, _, err := api.GetCommit(ctx, owner, repo, cur, nil)
		if err != nil || commit == nil || len(commit.Parents) != baseRefreshMergeParents {
			return false
		}
		first, second := strings.TrimSpace(commit.Parents[0].GetSHA()), strings.TrimSpace(commit.Parents[1].GetSHA())
		prev := first
		if oldHead == first || oldHead == second {
			prev = oldHead
		}
		if prev == "" || prev == cur || !mergeIsBaseRefreshOf(ctx, api, owner, repo, baseRef, commit, prev, cur, appBotLogin) {
			return false
		}
		if prev == oldHead {
			return true
		}
		cur = prev
	}
	return false
}

// mergeIsBaseRefreshOf is the single-hop check over an already-fetched
// commit (the merge commit at newHead): authored by the App, oldHead is one
// parent and the other parent is the merge-base with baseRef, and the
// three-dot diff against baseRef is unchanged from oldHead's.
func mergeIsBaseRefreshOf(ctx context.Context, api BaseRefreshRepos, owner, repo, baseRef string, commit *gh.RepositoryCommit, oldHead, newHead, appBotLogin string) bool {
	if commit == nil || len(commit.Parents) != baseRefreshMergeParents {
		return false
	}
	if !CommitByGitHubLogin(commit, appBotLogin) {
		return false
	}
	first, second := strings.TrimSpace(commit.Parents[0].GetSHA()), strings.TrimSpace(commit.Parents[1].GetSHA())
	baseParent := ""
	switch oldHead {
	case first:
		baseParent = second
	case second:
		baseParent = first
	}
	if baseParent == "" || baseParent == oldHead {
		return false
	}
	newCmp, newFiles, err := compareAllFiles(ctx, api, owner, repo, baseRef, newHead)
	if err != nil || newCmp == nil {
		return false
	}
	// The merge-base of the base branch and the new head is the base commit
	// that was merged in: that is what makes the other parent "a commit on
	// the base branch" rather than anything someone pushed.
	if strings.TrimSpace(newCmp.GetMergeBaseCommit().GetSHA()) != baseParent {
		return false
	}
	_, oldFiles, err := compareAllFiles(ctx, api, owner, repo, baseRef, oldHead)
	if err != nil {
		return false
	}
	return sameChangedFiles(oldFiles, newFiles)
}

// compareAllFiles walks every page of the three-dot compare base...head.
func compareAllFiles(ctx context.Context, api BaseRefreshRepos, owner, repo, base, head string) (*gh.CommitsComparison, []*gh.CommitFile, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var first *gh.CommitsComparison
	var files []*gh.CommitFile
	for {
		cmp, resp, err := api.CompareCommits(ctx, owner, repo, base, head, opts)
		if err != nil {
			return nil, nil, err
		}
		if first == nil {
			first = cmp
		}
		if cmp != nil {
			files = append(files, cmp.Files...)
		}
		if resp == nil || resp.NextPage == 0 {
			return first, files, nil
		}
		opts.Page = resp.NextPage
	}
}

// sameChangedFiles reports whether two three-dot diffs touch the same files
// the same way: name, status, rename source, resulting blob, line counts
// and — when GitHub returned it for both — the patch text itself.
func sameChangedFiles(a, b []*gh.CommitFile) bool {
	if len(a) != len(b) {
		return false
	}
	byName := func(files []*gh.CommitFile) []*gh.CommitFile {
		out := make([]*gh.CommitFile, 0, len(files))
		for _, f := range files {
			if f != nil {
				out = append(out, f)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].GetFilename() < out[j].GetFilename() })
		return out
	}
	a, b = byName(a), byName(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.GetFilename() != y.GetFilename() || x.GetStatus() != y.GetStatus() || x.GetPreviousFilename() != y.GetPreviousFilename() {
			return false
		}
		if x.GetSHA() != y.GetSHA() || x.GetAdditions() != y.GetAdditions() || x.GetDeletions() != y.GetDeletions() || x.GetChanges() != y.GetChanges() {
			return false
		}
		if x.Patch != nil && y.Patch != nil && x.GetPatch() != y.GetPatch() {
			return false
		}
	}
	return true
}

// CommitByGitHubLogin reports whether the commit's GitHub-attributed author
// or committer is login ([bot] suffix tolerated, case-insensitive). Only the
// server-resolved logins count, never the forgeable git name or email.
func CommitByGitHubLogin(commit *gh.RepositoryCommit, login string) bool {
	want := normalizeGitHubLogin(login)
	if commit == nil || want == "" {
		return false
	}
	return normalizeGitHubLogin(commit.GetAuthor().GetLogin()) == want || normalizeGitHubLogin(commit.GetCommitter().GetLogin()) == want
}

func normalizeGitHubLogin(login string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
}

// DecisionCarrier moves a person's recorded decision on a hive-authored PR
// forward when the hive itself changed the head by refreshing the base
// (hivecommons/hive#15649). The decision ledger is head-scoped on purpose —
// a new head is a new tree nobody has approved — but a require_current_base
// merge of the base branch by the App leaves the PR's diff exactly as the
// person saw it, so their approve / instruction still answers the reviewer's
// question and the hold guard's "who approved this head" alike. Anything
// else that moved the head keeps the ledger as it was.
type DecisionCarrier struct {
	Store       review.HumanChangeRequestStore
	API         BaseRefreshRepos
	AppBotLogin string
	Logger      *slog.Logger
	Checker     *BaseRefreshChecker
	// ResolveBase returns the PR's base branch when the caller does not
	// know it; it is consulted only once a carry candidate exists.
	ResolveBase func(ctx context.Context, owner, repo string, number int) (string, error)
}

// Carry returns the decision standing on head, carrying the PR's newest
// decision forward first when head is a pure base refresh of the head it
// was recorded on. baseRef may be "" when unknown. The carried record keeps
// its source, actor, body and kind — so the lanes read it exactly as the
// original — and names the original head in CarriedFromHead. One INFO line
// is logged per carry; later lookups find the record and ask nothing.
func (d *DecisionCarrier) Carry(ctx context.Context, owner, repo string, number int, baseRef, head string) (review.HumanChangeRequest, bool) {
	head = strings.TrimSpace(head)
	if d == nil || d.Store == nil || head == "" {
		return review.HumanChangeRequest{}, false
	}
	fullRepo := owner + "/" + repo
	if req, ok := d.Store.For(fullRepo, number, head); ok {
		return req, true
	}
	newest, ok := d.Store.(review.HumanChangeRequestNewestStore)
	if !ok || d.API == nil {
		return review.HumanChangeRequest{}, false
	}
	prev, found := newest.Newest(fullRepo, number)
	if !found || prev.HeadSHA == head || !prev.CarriesOverBaseRefresh() {
		return review.HumanChangeRequest{}, false
	}
	checker := d.Checker
	if checker == nil {
		checker = &BaseRefreshChecker{}
	}
	if refresh, seen := checker.Known(owner, repo, number, prev.HeadSHA, head); seen && !refresh {
		return review.HumanChangeRequest{}, false
	} else if !seen {
		if baseRef = strings.TrimSpace(baseRef); baseRef == "" && d.ResolveBase != nil {
			ref, err := d.ResolveBase(ctx, owner, repo, number)
			if err != nil {
				return review.HumanChangeRequest{}, false
			}
			baseRef = strings.TrimSpace(ref)
		}
		if !checker.HeadIsBaseRefreshOf(ctx, d.API, owner, repo, number, baseRef, prev.HeadSHA, head, d.AppBotLogin) {
			return review.HumanChangeRequest{}, false
		}
	}
	carried := prev
	carried.HeadSHA = head
	carried.CarriedFromHead = prev.HeadSHA
	if err := d.Store.Record(carried); err != nil {
		if d.Logger != nil {
			d.Logger.Warn("decision could not be carried over base refresh", "repo", fullRepo, "pr", number, "from_head", prev.HeadSHA, "head", head, "error", err)
		}
		return review.HumanChangeRequest{}, false
	}
	if d.Logger != nil {
		d.Logger.Info(DecisionCarriedOverBaseRefreshMessage, "repo", fullRepo, "pr", number, "from_head", prev.HeadSHA, "head", head, "by", prev.Actor, "kind", prev.Directive(), "source", prev.Source)
	}
	return carried, true
}

// decisionCarrier is the Client's carrier over its own REST client and the
// process-wide decision ledger.
func (c *Client) decisionCarrier(store review.HumanChangeRequestStore) *DecisionCarrier {
	if c == nil || c.client == nil {
		return nil
	}
	c.baseRefreshOnce.Do(func() { c.baseRefresh = &BaseRefreshChecker{} })
	return &DecisionCarrier{
		Store: store, API: c.client.Repositories, AppBotLogin: c.AppBotLogin(), Logger: c.logger, Checker: c.baseRefresh,
		ResolveBase: func(ctx context.Context, owner, repo string, number int) (string, error) {
			pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:decision_base_refresh"), owner, repo, number)
			if err != nil {
				return "", err
			}
			return pr.GetBase().GetRef(), nil
		},
	}
}

// RecordedDecisionFor returns the decision in store standing on head of
// repo#number, after carrying the PR's newest decision over the hive's own
// base refresh when that is all that moved the head (see DecisionCarrier).
func (c *Client) RecordedDecisionFor(ctx context.Context, store review.HumanChangeRequestStore, repo string, number int, head string) (review.HumanChangeRequest, bool) {
	if store == nil {
		return review.HumanChangeRequest{}, false
	}
	carrier := c.decisionCarrier(store)
	if carrier == nil {
		return store.For(repo, number, strings.TrimSpace(head))
	}
	owner, name := c.splitRepo(repo)
	return carrier.Carry(ctx, owner, name, number, "", head)
}
