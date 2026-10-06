package mergelane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	ghub "github.com/hivecommons/hive/pkg/github"
)

// mergeMethodKey carries the calling path's merge method from Merge to the
// GitHub adapter's Merge, and the method actually used back.
type mergeMethodKey struct{}

type mergeMethod struct {
	requested string
	used      string
}

// NativeMergeQueueReason is the refusal for a target branch that has GitHub's
// native merge queue (R22): the lane does not run there and Hive makes no
// direct merge into it.
func NativeMergeQueueReason(branch string) string {
	return fmt.Sprintf("target branch %q has GitHub's native merge queue: the serialized lane does not run for it and Hive makes no direct merge into it", branch)
}

// Merge is the ghub.SerializedLaneGate every merge path consults for a
// hive-serialized repository (#10889). The path has already applied all of
// its own gates. A branch with GitHub's native merge queue is refused before
// the lane runs; otherwise the PR records its eligibility and, only when it
// is at the front, runs one lane round that may end in the pinned merge.
func (l *Lane) Merge(ctx context.Context, req ghub.LaneMergeRequest) (ghub.LaneMergeResult, error) {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		p, err := l.opts.GitHub.PullRequest(ctx, req.Repo, req.Number)
		if err != nil {
			return ghub.LaneMergeResult{Outcome: ghub.LaneOutcomeWaiting, Reason: "reading the pull request failed: " + err.Error()}, nil
		}
		branch = p.Base
	}
	if err := validArgs(req.Repo, branch, req.Number); err != nil {
		return ghub.LaneMergeResult{Outcome: ghub.LaneOutcomeRefused, Reason: err.Error()}, err
	}
	if reason := l.nativeMergeQueue(ctx, req.Repo, branch); reason != "" {
		err := l.Release(req.Repo, branch, req.Number, reason)
		l.emit([]Event{{At: l.opts.Now(), Repo: req.Repo, Branch: branch, PR: req.Number, Action: ActionRefusal, Reason: reason}})
		return ghub.LaneMergeResult{Outcome: ghub.LaneOutcomeRefused, Reason: reason}, err
	}
	dec, err := l.Acquire(req.Repo, branch, req.Number, req.Path)
	if err != nil || dec.Outcome == OutcomeDeferred {
		return ghub.LaneMergeResult{Outcome: string(dec.Outcome), Reason: dec.Reason}, err
	}
	method := &mergeMethod{requested: req.Method}
	ctx = context.WithValue(ctx, mergeMethodKey{}, method)
	dec, err = l.Advance(ctx, req.Repo, branch, req.Number, Authorizer(req.Authorize))
	res := ghub.LaneMergeResult{Outcome: string(dec.Outcome), Reason: dec.Reason, SHA: dec.MergeSHA}
	if dec.Outcome == OutcomeMerged {
		res.Method = method.used
	}
	return res, err
}

// nativeMergeQueue returns a refusal when branch has GitHub's native merge
// queue or when that cannot be established (fail closed).
func (l *Lane) nativeMergeQueue(ctx context.Context, repo, branch string) string {
	rules := l.opts.GitHub.RequiredChecks(ctx, repo, branch)
	if !rules.MergeQueueKnown {
		reason := rules.Reason
		if reason == "" {
			reason = "branch rules unreadable"
		}
		return fmt.Sprintf("could not establish whether target branch %q has GitHub's native merge queue (%s): no merge", branch, reason)
	}
	if rules.MergeQueue {
		return NativeMergeQueueReason(branch)
	}
	return ""
}

// RESTGitHub is the lane's GitHub over the hive's App client. Client is read
// on every call so a rebuilt client is followed. Reads are live.
type RESTGitHub struct {
	Client func() *ghub.Client
	// ConfigRequiredChecks returns auto_merge.required_checks and whether it
	// is declared.
	ConfigRequiredChecks func() (map[string]bool, bool)
}

func (g *RESTGitHub) rest(repo string) (*ghub.Client, *gh.Client, string, string, error) {
	var c *ghub.Client
	if g != nil && g.Client != nil {
		c = g.Client()
	}
	api := c.GoGitHub()
	if api == nil {
		return nil, nil, "", "", ghub.ErrNoGitHubClient
	}
	owner, name := c.SplitRepo(repo)
	return c, api, owner, name, nil
}

func githubStatus(err error, codes ...int) bool {
	var ge *gh.ErrorResponse
	if !errors.As(err, &ge) || ge.Response == nil {
		return false
	}
	for _, code := range codes {
		if ge.Response.StatusCode == code {
			return true
		}
	}
	return false
}

// PullRequest reads the PR's live state.
func (g *RESTGitHub) PullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return PullRequest{}, err
	}
	pr, _, err := api.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{
		Number:         number,
		Head:           pr.GetHead().GetSHA(),
		Base:           pr.GetBase().GetRef(),
		Open:           strings.EqualFold(pr.GetState(), "open"),
		Merged:         pr.GetMerged(),
		Draft:          pr.GetDraft(),
		MergeableState: pr.GetMergeableState(),
		Mergeable:      pr.Mergeable,
		Labels:         ghub.ExtractPRLabels(pr.Labels),
	}, nil
}

// BranchTip reads the target branch's current tip.
func (g *RESTGitHub) BranchTip(ctx context.Context, repo, branch string) (string, error) {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return "", err
	}
	b, _, err := api.Repositories.GetBranch(ctx, owner, name, branch, 0)
	if err != nil {
		return "", err
	}
	if sha := b.GetCommit().GetSHA(); sha != "" {
		return sha, nil
	}
	return "", fmt.Errorf("branch %q has no tip commit", branch)
}

// HeadContains compares tip...head: head contains tip when behind_by is 0.
func (g *RESTGitHub) HeadContains(ctx context.Context, repo, tip, head string) (bool, error) {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return false, err
	}
	cmp, _, err := api.Repositories.CompareCommits(ctx, owner, name, tip, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return false, err
	}
	return cmp.GetBehindBy() == 0, nil
}

// RequiredChecks is the lane-only branch-rules reader.
func (g *RESTGitHub) RequiredChecks(ctx context.Context, repo, branch string) ghub.BranchRulesResult {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return ghub.BranchRulesResult{Reason: err.Error()}
	}
	var set map[string]bool
	known := false
	if g.ConfigRequiredChecks != nil {
		set, known = g.ConfigRequiredChecks()
	}
	return ghub.ReadBranchRules(ctx, api, owner, name, branch, set, known)
}

// CheckRuns returns the latest check run per name on head, plus commit
// statuses for contexts no check run reports.
func (g *RESTGitHub) CheckRuns(ctx context.Context, repo, head string) (map[string]CheckRun, error) {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return nil, err
	}
	out := map[string]CheckRun{}
	opts := &gh.ListCheckRunsOptions{Filter: gh.Ptr("latest"), ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := api.Checks.ListCheckRunsForRef(ctx, owner, name, head, opts)
		if err != nil {
			return nil, err
		}
		for _, run := range runs.CheckRuns {
			if _, seen := out[run.GetName()]; !seen && run.GetName() != "" {
				out[run.GetName()] = CheckRun{Status: run.GetStatus(), Conclusion: run.GetConclusion()}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	combined, _, err := api.Repositories.GetCombinedStatus(ctx, owner, name, head, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return nil, err
	}
	for _, st := range combined.Statuses {
		if _, seen := out[st.GetContext()]; seen || st.GetContext() == "" {
			continue
		}
		switch st.GetState() {
		case "success":
			out[st.GetContext()] = CheckRun{Status: "completed", Conclusion: "success"}
		case "failure", "error":
			out[st.GetContext()] = CheckRun{Status: "completed", Conclusion: "failure"}
		default:
			out[st.GetContext()] = CheckRun{Status: st.GetState()}
		}
	}
	return out, nil
}

// UpdateBranch merges the target branch into the PR branch, pinned to
// expectedHead. It never rebases or force-pushes.
func (g *RESTGitHub) UpdateBranch(ctx context.Context, repo string, number int, expectedHead string) error {
	c, api, owner, name, err := g.rest(repo)
	if err != nil {
		return err
	}
	var apiErr error
	_, err = effects.Execute(ctx, c.MutationBoundary(), effects.Claim{
		Repo:   owner + "/" + name,
		Kind:   effects.KindBranchUpdate,
		Target: strconv.Itoa(number),
		Actor:  "mergelane",
		Inputs: map[string]string{"expected_head_sha": expectedHead, "lane": "serialized"},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr = api.PullRequests.UpdateBranch(ctx, owner, name, number, &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: gh.Ptr(expectedHead)})
		var accepted *gh.AcceptedError
		if errors.As(apiErr, &accepted) {
			apiErr = nil
		}
		return effects.Result{Provenance: owner + "/" + name + "#" + strconv.Itoa(number)}, apiErr
	})
	if githubStatus(apiErr, http.StatusUnprocessableEntity, http.StatusConflict) {
		return ErrHeadMoved
	}
	return err
}

// Merge is the REST merge with head pinned. The method is the calling path's;
// squash is upgraded to a merge commit for forward-merge PRs, as MergePR does.
func (g *RESTGitHub) Merge(ctx context.Context, repo string, number int, head string) (string, error) {
	c, api, owner, name, err := g.rest(repo)
	if err != nil {
		return "", err
	}
	method, _ := ctx.Value(mergeMethodKey{}).(*mergeMethod)
	if method == nil {
		method = &mergeMethod{}
	}
	used := strings.TrimSpace(method.requested)
	if used == "" {
		used = "squash"
	}
	if used == "squash" {
		if pr, _, err := api.PullRequests.Get(ctx, owner, name, number); err == nil && ghub.IsForwardMergePR(pr) {
			used = "merge"
		}
	}
	var res *gh.PullRequestMergeResult
	var apiErr error
	out, err := effects.Execute(ctx, c.MutationBoundary(), effects.Claim{
		Repo:   owner + "/" + name,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(number),
		Actor:  "mergelane",
		Inputs: map[string]string{"method": used, "expect_sha": head, "lane": "serialized"},
	}, func(ctx context.Context) (effects.Result, error) {
		res, _, apiErr = api.PullRequests.Merge(ctx, owner, name, number, "", &gh.PullRequestOptions{SHA: head, MergeMethod: used})
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: res.GetSHA()}, nil
	})
	if githubStatus(apiErr, http.StatusConflict) {
		return "", ErrHeadMoved
	}
	if err != nil {
		return "", err
	}
	sha := out.Provenance
	if res != nil {
		if !res.GetMerged() {
			return "", fmt.Errorf("merge not applied: %s", res.GetMessage())
		}
		sha = res.GetSHA()
	}
	if sha == "" {
		return "", errors.New("merge returned no merge commit SHA")
	}
	method.used = used
	return sha, nil
}

// CommitParents returns sha's parent SHAs, first parent first.
func (g *RESTGitHub) CommitParents(ctx context.Context, repo, sha string) ([]string, error) {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return nil, err
	}
	commit, _, err := api.Git.GetCommit(ctx, owner, name, sha)
	if err != nil {
		return nil, err
	}
	parents := make([]string, 0, len(commit.Parents))
	for _, p := range commit.Parents {
		parents = append(parents, p.GetSHA())
	}
	return parents, nil
}

// PullRequestsForCommit lists the PRs a commit belongs to.
func (g *RESTGitHub) PullRequestsForCommit(ctx context.Context, repo, sha string) ([]int, error) {
	_, api, owner, name, err := g.rest(repo)
	if err != nil {
		return nil, err
	}
	prs, _, err := api.PullRequests.ListPullRequestsWithCommit(ctx, owner, name, sha, nil)
	if err != nil {
		return nil, err
	}
	numbers := make([]int, 0, len(prs))
	for _, pr := range prs {
		numbers = append(numbers, pr.GetNumber())
	}
	return numbers, nil
}
