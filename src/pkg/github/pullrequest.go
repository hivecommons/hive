package github

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// CreatePRResult is what CreatePR returns: the opened (or pre-existing) PR.
type CreatePRResult struct {
	Number int
	URL    string
	// Author is the login GitHub recorded on the created (or pre-existing) PR —
	// the App bot the hive relayed as (e.g. "kubestellar-hive[bot]"). Taken from
	// the API response, not assumed, so the audit log records the true author.
	Author string
	// AlreadyExisted is true when an open PR already covers this request, so we
	// returned it instead of opening a duplicate. Two different findings set it:
	// an open PR for the same HEAD BRANCH, and (see DuplicateTree) an open PR
	// carrying the same CONTENT from a different branch.
	AlreadyExisted bool
	// DuplicateTree narrows AlreadyExisted to the content case (#5111): the
	// reuse was decided because an open PR on the same base has an identical
	// tree SHA, not because the head branch matched. Callers that report the
	// outcome keep the two apart because they mean different things to whoever
	// reads the result — "your retry was absorbed" versus "this change is
	// already proposed as #N, go comment on it".
	DuplicateTree bool
}

// CreatePR opens a pull request on behalf of THIS hive's GitHub App — so the PR
// is authored by the App bot ("<slug>[bot]"), deterministically, regardless of
// which agent/CLI produced the branch. This is the hive-opens-the-PR path: an
// agent pushes its branch (the credential helper already uses the App token for
// the push), then the hive opens the PR here rather than the agent running
// `gh pr create` (which would attribute the PR to the Copilot login user).
//
// repo may be "owner/repo" or a bare repo name (owner defaults to the hive org).
// head is the branch the agent pushed; an empty base is RESOLVED from the
// target repository's own default branch (see DefaultBranch) rather than
// assumed to be "main" — a repo whose default is "testing" or "develop" would
// otherwise get every PR based on the wrong branch, carrying the full
// divergence between the two branches in its diff (kubestellar/hive#4928).
//
// A base the caller explicitly passed is always honored as-is and never
// second-guessed against the resolved default. When no base is given and the
// resolution fails, CreatePR returns an error rather than silently falling
// back to "main" — a wrong base is worse than a delayed PR, and the caller
// (the pr-request watcher) already surfaces a returned error into the audit
// log and the request's .result.json, and retries with backoff.
//
// It is idempotent: if an OPEN PR for head already exists, it returns that PR
// with AlreadyExisted=true instead of erroring or opening a duplicate — this
// makes the file-watcher safe to retry a request that partially succeeded.
//
// It also refuses to file the same CONTENT twice (kubestellar/hive#5111): if an
// open PR on the same base already carries an identical tree SHA, that PR is
// returned with AlreadyExisted and DuplicateTree set. Same-branch idempotency
// cannot see this case, because the duplicate arrives on a fresh branch.
func (c *Client) CreatePR(ctx context.Context, repo, head, base, title, body string) (CreatePRResult, error) {
	if c == nil || c.client == nil {
		return CreatePRResult{}, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return CreatePRResult{}, fmt.Errorf("CreatePR: head branch is required")
	}
	if base = strings.TrimSpace(base); base == "" {
		resolved, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return CreatePRResult{}, fmt.Errorf("CreatePR %s/%s: could not resolve the repository's default branch and no explicit base was given (refusing to guess \"%s\"): %w", owner, repo, FallbackDefaultBranch, err)
		}
		base = resolved
	}
	if strings.TrimSpace(title) == "" {
		return CreatePRResult{}, fmt.Errorf("CreatePR: title is required")
	}
	if leak, ok := c.scanCanaryText(title+"\n"+body, "hive-open-pr:"+repo); ok {
		if c.canaryFailClosed {
			return CreatePRResult{}, fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	title = logscrub.ScrubString(title)
	body = logscrub.ScrubString(body)

	// Idempotency: don't open a second PR for a branch that already has an open
	// one. GitHub itself 422s on a duplicate, but checking first lets us return
	// the existing PR cleanly (the watcher may re-process a request after a
	// crash between "PR opened" and "request file deleted").
	if existing, err := c.findOpenPRForHead(ctx, owner, repo, head); err != nil {
		c.logger.Warn("CreatePR: dedupe lookup failed, proceeding to create",
			slog.String("repo", repo), slog.String("head", head), slog.String("error", err.Error()))
	} else if existing != nil {
		c.logger.Info("CreatePR: open PR already exists for head, reusing",
			slog.String("repo", repo), slog.String("head", head), slog.Int("number", existing.GetNumber()))
		return CreatePRResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), Author: existing.GetUser().GetLogin(), AlreadyExisted: true}, nil
	}

	// Content identity (#5111): the check above only recognises a duplicate that
	// reuses the SAME branch. Agents also copy a change forward onto a NEW branch
	// and file it again — five such PRs on tuna-os/tromso, two with identical
	// trees, so `git diff` between their tips was empty. Compare the tree SHA
	// against the open PRs on this same base and reuse the match instead.
	//
	// A failed lookup proceeds to create. Refusing to publish work because an
	// auxiliary read failed would be a worse bug than the duplicate: the guard
	// is here to stop redundant PRs, not to become a new way to lose one.
	if existing, err := c.findOpenPRWithIdenticalTree(ctx, owner, repo, head, base); err != nil {
		c.logger.Warn("CreatePR: duplicate-tree lookup failed, proceeding to create",
			slog.String("repo", repo), slog.String("head", head), slog.String("error", err.Error()))
	} else if existing != nil {
		c.logger.Info("CreatePR: an open PR already carries this exact tree, reusing it instead of opening a duplicate",
			slog.String("repo", repo), slog.String("head", head),
			slog.String("base", base), slog.Int("number", existing.GetNumber()))
		return CreatePRResult{
			Number:         existing.GetNumber(),
			URL:            existing.GetHTMLURL(),
			Author:         existing.GetUser().GetLogin(),
			AlreadyExisted: true,
			DuplicateTree:  true,
		}, nil
	}

	var pr *gh.PullRequest
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestCreate,
		Target: head,
		Inputs: map[string]string{"head": head, "base": base, "title": effects.StableDigest(title), "body": effects.StableDigest(body)},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		pr, _, apiErr = c.client.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
			Title: gh.Ptr(title),
			Head:  gh.Ptr(head),
			Base:  gh.Ptr(base),
			Body:  gh.Ptr(body),
		})
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: pr.GetHTMLURL()}, nil
	})
	if err != nil {
		// A concurrent creator may have raced us; treat an existing-PR 422 as a
		// reuse rather than a hard failure.
		if strings.Contains(err.Error(), "A pull request already exists") {
			if existing, lookErr := c.findOpenPRForHead(ctx, owner, repo, head); lookErr == nil && existing != nil {
				return CreatePRResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), Author: existing.GetUser().GetLogin(), AlreadyExisted: true}, nil
			}
		}
		return CreatePRResult{}, fmt.Errorf("creating PR %s/%s %s->%s: %w", owner, repo, head, base, err)
	}
	c.logger.Info("CreatePR: opened PR as the App bot",
		slog.String("repo", repo), slog.String("head", head), slog.Int("number", pr.GetNumber()))
	return CreatePRResult{Number: pr.GetNumber(), URL: pr.GetHTMLURL(), Author: pr.GetUser().GetLogin()}, nil
}

// MergePRResult is what MergePR returns after a successful merge.
type MergePRResult struct {
	SHA     string // the resulting merge commit SHA
	Merged  bool
	Message string
}

// MergePR merges an open pull request on behalf of THIS hive's GitHub App, over
// REST (PUT /repos/{owner}/{repo}/pulls/{n}/merge).
//
// This exists because the GitHub GraphQL `mergePullRequest` mutation — which the
// GitHub MCP server's merge_pull_request tool issues — returns "Resource not
// accessible by integration" for App installation tokens even when the token
// holds contents:write + pull_requests:write. The equivalent REST call with the
// SAME token succeeds. Routing merges through the hive over REST (mirroring the
// hive-open-pr authorship relay) makes the merge deterministic and App-authored,
// and sidesteps that GraphQL limitation.
//
// It does NOT force-merge: with admin bypass disabled, GitHub still enforces
// branch protection (required checks like build-gate), so a PR whose required
// checks are not green returns an error here rather than merging. mergeMethod
// defaults to "squash" when empty. When expectSHA is non-empty it is passed as
// the required head SHA, so the merge fails cleanly (409) instead of merging an
// unexpected commit if the head moved after eligibility was computed.
func (c *Client) MergePR(ctx context.Context, repo string, number int, mergeMethod, expectSHA string) (MergePRResult, error) {
	if c == nil || c.client == nil {
		return MergePRResult{}, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	if number <= 0 {
		return MergePRResult{}, fmt.Errorf("MergePR: pr number is required")
	}
	if mergeMethod = strings.TrimSpace(mergeMethod); mergeMethod == "" {
		mergeMethod = "squash"
	}

	opts := &gh.PullRequestOptions{MergeMethod: mergeMethod}
	if expectSHA = strings.TrimSpace(expectSHA); expectSHA != "" {
		opts.SHA = expectSHA
	}
	var res *gh.PullRequestMergeResult
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{"method": mergeMethod, "expect_sha": expectSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		res, _, apiErr = c.client.PullRequests.Merge(ctx, owner, repo, number, "", opts)
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: res.GetSHA()}, nil
	})
	if err != nil {
		return MergePRResult{}, fmt.Errorf("merging PR %s/%s#%d (%s): %w", owner, repo, number, mergeMethod, err)
	}
	c.logger.Info("MergePR: merged PR as the App bot over REST",
		slog.String("repo", owner+"/"+repo), slog.Int("number", number),
		slog.String("method", mergeMethod), slog.String("sha", res.GetSHA()))
	// Audit the merge UNCONDITIONALLY so the dashboard audit log shows the full
	// create→merge loop. The merge is performed by the hive itself (not a single
	// coding agent), so it is attributed to the governor flow, mirroring the
	// hive-issue-created attribution in advisory.go.
	c.recordCreationAudit(AuditActionPRMerged, InvocationMeta{Agent: AttributionAgentGovernor},
		"repo", owner+"/"+repo,
		"number", strconv.Itoa(number),
		"method", mergeMethod,
		"sha", res.GetSHA())
	return MergePRResult{SHA: res.GetSHA(), Merged: res.GetMerged(), Message: res.GetMessage()}, nil
}

// UpdateBranch syncs a PR's head branch with its base (PUT
// /repos/{owner}/{repo}/pulls/{n}/update-branch), resolving the common
// "behind main" case before a merge. Like MergePR it goes over REST as the App.
func (c *Client) UpdateBranch(ctx context.Context, repo string, number int) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchUpdate,
		Target: strconv.Itoa(number),
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.client.PullRequests.UpdateBranch(ctx, owner, repo, number, nil)
		return effects.Result{Provenance: owner + "/" + repo + "#" + strconv.Itoa(number)}, apiErr
	})
	if err != nil {
		return fmt.Errorf("updating branch for PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// findOpenPRForHead returns the open PR whose head branch matches, or nil. The
// head filter is "owner:branch" per the GitHub API.
func (c *Client) findOpenPRForHead(ctx context.Context, owner, repo, head string) (*gh.PullRequest, error) {
	prs, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       "open",
		Head:        owner + ":" + head,
		ListOptions: gh.ListOptions{PerPage: 5},
	})
	if err != nil {
		return nil, err
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	return nil, nil
}
