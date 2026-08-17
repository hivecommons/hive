package github

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type RepairPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
	Created bool   `json:"created"`
}

type OpenRepairPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
}

type MergedBaselineBranch struct {
	PRNumber              int    `json:"pr_number"`
	PRURL                 string `json:"pr_url"`
	RepositoryFingerprint string `json:"repository_fingerprint"`
	Branch                string `json:"branch"`
	HeadSHA               string `json:"head_sha"`
}

type managedRepositoryIdentity struct {
	ID       int64
	FullName string
}

type managedPullPriorHead struct {
	Number  int
	HeadSHA string
}

const (
	managedPullHeadRefreshAttempts = 21
	managedPullHeadRefreshDelay    = 250 * time.Millisecond
)

// UpsertRepairPullRequest creates or updates exactly one open repair PR for a
// Hive-owned branch. The marker makes a retry after an ambiguous API response
// idempotent without relying on the local lifecycle transaction having
// completed.
func (c *Client) UpsertRepairPullRequest(ctx context.Context, repository, branch, expectedHeadSHA, base, title, body, marker string) (RepairPullRequest, error) {
	return c.upsertHivePullRequest(ctx, repository, branch, expectedHeadSHA, base, title, body, marker, false, nil)
}

// UpsertSetupPullRequest updates a setup PR after Hive has moved its managed
// branch. GitHub can briefly report the state-bound prior head after the branch
// ref already exposes the expected head, so this path permits only that exact
// prior PR/head pair to converge during a bounded refresh window.
func (c *Client) UpsertSetupPullRequest(ctx context.Context, repository, branch, expectedHeadSHA, base, title, body, marker string, priorNumber int, priorHeadSHA string) (RepairPullRequest, error) {
	priorHeadSHA = strings.ToLower(strings.TrimSpace(priorHeadSHA))
	if (priorNumber == 0) != (priorHeadSHA == "") || priorNumber < 0 || (priorHeadSHA != "" && !exactManagedSHA(priorHeadSHA)) {
		return RepairPullRequest{}, fmt.Errorf("prior setup PR number and exact head SHA must either both be present or both be absent")
	}
	var prior *managedPullPriorHead
	if priorNumber > 0 {
		prior = &managedPullPriorHead{Number: priorNumber, HeadSHA: priorHeadSHA}
	}
	return c.upsertHivePullRequest(ctx, repository, branch, expectedHeadSHA, base, title, body, marker, false, prior)
}

// UpsertReviewPullRequest creates a draft, hold-labeled PR for an operation
// that requires explicit human authority, such as adding visual baselines.
// Hive never promotes the draft or merges it automatically.
func (c *Client) UpsertReviewPullRequest(ctx context.Context, repository, branch, expectedHeadSHA, base, title, body, marker string) (RepairPullRequest, error) {
	pull, err := c.upsertHivePullRequest(ctx, repository, branch, expectedHeadSHA, base, title, body, marker, true, nil)
	if err != nil {
		return RepairPullRequest{}, err
	}
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	live, _, err := c.client.PullRequests.Get(ctx, owner, repo, pull.Number)
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("verify review pull request before labeling: %w", err)
	}
	if err := validateManagedPullRequest(live, identity, repository, pull.Number, branch, expectedHeadSHA, base, marker, title, body); err != nil {
		return RepairPullRequest{}, fmt.Errorf("refusing to label inexact review pull request #%d: %w", pull.Number, err)
	}
	for name, color := range map[string]string{"hold": "B60205", "hive/baseline-review": "D4C5F9"} {
		if err := c.ensureLabelWithColor(ctx, owner, repo, name, color); err != nil {
			return RepairPullRequest{}, err
		}
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, pull.Number, []string{"hold", "hive/baseline-review"}); err != nil {
		return RepairPullRequest{}, fmt.Errorf("label review pull request: %w", err)
	}
	return pull, nil
}

// UpsertHeldPullRequest creates a non-draft but explicitly hold-labeled review
// PR. This allows exact-head checks to run while Hive remains the sole process
// that may remove the hold after its durable human approval binding.
func (c *Client) UpsertHeldPullRequest(ctx context.Context, repository, branch, expectedHeadSHA, base, title, body, marker string) (RepairPullRequest, error) {
	pull, err := c.upsertHivePullRequest(ctx, repository, branch, expectedHeadSHA, base, title, body, marker, false, nil)
	if err != nil {
		return RepairPullRequest{}, err
	}
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	live, _, err := c.client.PullRequests.Get(ctx, owner, repo, pull.Number)
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("verify held pull request before labeling: %w", err)
	}
	if err := validateManagedPullRequest(live, identity, repository, pull.Number, branch, expectedHeadSHA, base, marker, title, body); err != nil || live.GetDraft() {
		if err == nil {
			err = fmt.Errorf("pull request is unexpectedly draft")
		}
		return RepairPullRequest{}, fmt.Errorf("refusing to label inexact held pull request #%d: %w", pull.Number, err)
	}
	for name, color := range map[string]string{"hold": "B60205", "hive/setup-baseline-review": "D4C5F9"} {
		if err := c.ensureLabelWithColor(ctx, owner, repo, name, color); err != nil {
			return RepairPullRequest{}, err
		}
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, pull.Number, []string{"hold", "hive/setup-baseline-review"}); err != nil {
		return RepairPullRequest{}, fmt.Errorf("label held setup baseline pull request: %w", err)
	}
	return pull, nil
}

// ReleaseHeldSetupBaselinePullRequestExact removes only Hive's two exact hold
// labels after re-reading the same-repository marker/ref/head/base identity.
func (c *Client) ReleaseHeldSetupBaselinePullRequestExact(ctx context.Context, repository string, number int, marker, branch, headSHA, base string) error {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return err
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return err
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("read held setup baseline PR #%d: %w", number, err)
	}
	if err := validateManagedPullRequest(pull, identity, repository, number, branch, headSHA, base, marker, "", ""); err != nil || pull.GetState() != "open" || pull.GetMerged() || pull.GetDraft() {
		if err == nil {
			err = fmt.Errorf("pull request is not an open non-draft unmerged review")
		}
		return fmt.Errorf("refusing to release held setup baseline PR #%d: %w", number, err)
	}
	labels, _, err := c.client.Issues.ListLabelsByIssue(ctx, owner, repo, number, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return fmt.Errorf("read held setup baseline PR labels: %w", err)
	}
	present := map[string]bool{}
	for _, label := range labels {
		present[label.GetName()] = true
	}
	for _, label := range []string{"hold", "hive/setup-baseline-review"} {
		if present[label] {
			if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label); err != nil {
				return fmt.Errorf("remove exact setup baseline label %q: %w", label, err)
			}
		}
	}
	return nil
}

func (c *Client) upsertHivePullRequest(ctx context.Context, repository, branch, expectedHeadSHA, base, title, body, marker string, draft bool, prior *managedPullPriorHead) (RepairPullRequest, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(marker) == "" ||
		!exactManagedSHA(expectedHeadSHA) || exactMarkerLineCount(body, marker) != 1 {
		return RepairPullRequest{}, fmt.Errorf("repair branch, exact head SHA, base branch, title, and exactly one marker line are required")
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	// A successful git push can briefly precede GitHub's REST ref view. Wait
	// only for this exact repository-owned branch to expose the already-known
	// commit before inspecting PR marker claimants. Every later verification
	// remains an immediate exact check so external branch movement still fails
	// closed.
	if err := c.waitForManagedBranchHead(ctx, owner, repo, branch, expectedHeadSHA); err != nil {
		return RepairPullRequest{}, err
	}
	// Search every open PR, not only the requested base. A marker claimant on a
	// fork or another base is an ambiguity to stop on, never a candidate to
	// silently ignore before creating a second PR.
	pulls, err := c.listPullRequestsByState(ctx, owner, repo, "", "open")
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("list repair pull requests: %w", err)
	}
	var matched *gh.PullRequest
	for _, pull := range pulls {
		markerCount := exactMarkerLineCount(pull.GetBody(), marker)
		if markerCount == 0 {
			continue
		}
		if !managedPullRepositoriesMatch(pull, identity, repository) {
			c.logIgnoredForeignMarkerClaim(repository, pull, marker)
			continue
		}
		if markerCount != 1 {
			return RepairPullRequest{}, fmt.Errorf("open repair pull request #%d contains marker %q more than once", pull.GetNumber(), marker)
		}
		candidate := pull
		if err := validateManagedPullRequest(candidate, identity, repository, candidate.GetNumber(), branch, expectedHeadSHA, base, marker, "", ""); err != nil {
			// GitHub's open-PR list can briefly retain the previous head after
			// Hive has atomically updated the same managed branch. Re-read only
			// this repository-owned marker claimant before rejecting it; the
			// exact repository, branch, head, base, and marker checks still apply.
			live, _, readErr := c.client.PullRequests.Get(ctx, owner, repo, candidate.GetNumber())
			if readErr != nil {
				return RepairPullRequest{}, fmt.Errorf("re-read inexact managed pull request #%d: %w", candidate.GetNumber(), readErr)
			}
			if liveErr := validateManagedPullRequest(live, identity, repository, live.GetNumber(), branch, expectedHeadSHA, base, marker, "", ""); liveErr != nil {
				if prior == nil || live.GetNumber() != prior.Number || !strings.EqualFold(live.GetHead().GetSHA(), prior.HeadSHA) {
					return RepairPullRequest{}, fmt.Errorf("open pull request #%d preclaims Hive marker %q but is not the exact managed PR: %w", pull.GetNumber(), marker, liveErr)
				}
				if priorErr := validateManagedPullRequest(live, identity, repository, live.GetNumber(), branch, prior.HeadSHA, base, marker, "", ""); priorErr != nil {
					return RepairPullRequest{}, fmt.Errorf("open pull request #%d does not match Hive's exact prior setup binding: %w", pull.GetNumber(), priorErr)
				}
				live, liveErr = c.waitForManagedPullHeadRefresh(ctx, owner, repo, identity, repository, branch, expectedHeadSHA, base, marker, prior, live)
				if liveErr != nil {
					return RepairPullRequest{}, fmt.Errorf("wait for setup pull request #%d to expose its managed branch head: %w", pull.GetNumber(), liveErr)
				}
			}
			candidate = live
		}
		if matched != nil {
			return RepairPullRequest{}, fmt.Errorf("multiple exact open repair pull requests contain marker %q", marker)
		}
		matched = candidate
	}
	if matched != nil {
		live, _, err := c.client.PullRequests.Get(ctx, owner, repo, matched.GetNumber())
		if err != nil {
			return RepairPullRequest{}, fmt.Errorf("re-read repair pull request #%d before update: %w", matched.GetNumber(), err)
		}
		if live.GetHead().GetSHA() != matched.GetHead().GetSHA() {
			return RepairPullRequest{}, fmt.Errorf("repair pull request #%d head changed during discovery", matched.GetNumber())
		}
		if err := validateManagedPullRequest(live, identity, repository, matched.GetNumber(), branch, expectedHeadSHA, base, marker, "", ""); err != nil {
			return RepairPullRequest{}, fmt.Errorf("repair pull request #%d changed before update: %w", matched.GetNumber(), err)
		}
		_, _, err = c.client.PullRequests.Edit(ctx, owner, repo, matched.GetNumber(), &gh.PullRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Base: &gh.PullRequestBranch{Ref: gh.Ptr(base)}})
		if err != nil {
			return RepairPullRequest{}, fmt.Errorf("update repair pull request: %w", err)
		}
		updated, _, err := c.client.PullRequests.Get(ctx, owner, repo, matched.GetNumber())
		if err != nil {
			return RepairPullRequest{}, fmt.Errorf("verify repair pull request #%d after update: %w", matched.GetNumber(), err)
		}
		if err := validateManagedPullRequest(updated, identity, repository, matched.GetNumber(), branch, expectedHeadSHA, base, marker, title, body); err != nil {
			return RepairPullRequest{}, fmt.Errorf("repair pull request #%d was not updated exactly: %w", matched.GetNumber(), err)
		}
		if err := c.verifyManagedBranchHead(ctx, owner, repo, branch, expectedHeadSHA); err != nil {
			return RepairPullRequest{}, err
		}
		return repairPullRequestResult(updated, false), nil
	}
	created, _, err := c.client.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
		Title: gh.Ptr(title), Head: gh.Ptr(branch), Base: gh.Ptr(base), Body: gh.Ptr(body), Draft: gh.Ptr(draft), MaintainerCanModify: gh.Ptr(true),
	})
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("create repair pull request: %w", err)
	}
	if created.GetNumber() <= 0 {
		return RepairPullRequest{}, fmt.Errorf("create repair pull request returned no PR identity")
	}
	live, _, err := c.client.PullRequests.Get(ctx, owner, repo, created.GetNumber())
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("verify created repair pull request #%d: %w", created.GetNumber(), err)
	}
	if err := validateManagedPullRequest(live, identity, repository, created.GetNumber(), branch, expectedHeadSHA, base, marker, title, body); err != nil {
		return RepairPullRequest{}, fmt.Errorf("created repair pull request #%d is not exact: %w", created.GetNumber(), err)
	}
	if err := c.verifyManagedBranchHead(ctx, owner, repo, branch, expectedHeadSHA); err != nil {
		return RepairPullRequest{}, err
	}
	return repairPullRequestResult(live, true), nil
}

func (c *Client) waitForManagedPullHeadRefresh(ctx context.Context, owner, repo string, identity managedRepositoryIdentity, repository, branch, expectedHeadSHA, base, marker string, prior *managedPullPriorHead, current *gh.PullRequest) (*gh.PullRequest, error) {
	for attempt := 0; attempt < managedPullHeadRefreshAttempts; attempt++ {
		if strings.EqualFold(current.GetHead().GetSHA(), expectedHeadSHA) {
			if err := validateManagedPullRequest(current, identity, repository, prior.Number, branch, expectedHeadSHA, base, marker, "", ""); err != nil {
				return nil, err
			}
			return current, nil
		}
		if current.GetNumber() != prior.Number || !strings.EqualFold(current.GetHead().GetSHA(), prior.HeadSHA) {
			return nil, fmt.Errorf("head changed to unbound SHA %q while waiting for %q", current.GetHead().GetSHA(), expectedHeadSHA)
		}
		if err := validateManagedPullRequest(current, identity, repository, prior.Number, branch, prior.HeadSHA, base, marker, "", ""); err != nil {
			return nil, err
		}
		if attempt == managedPullHeadRefreshAttempts-1 {
			break
		}
		timer := time.NewTimer(managedPullHeadRefreshDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		live, _, err := c.client.PullRequests.Get(ctx, owner, repo, prior.Number)
		if err != nil {
			return nil, fmt.Errorf("refresh pull request: %w", err)
		}
		current = live
	}
	return nil, fmt.Errorf("head remained at prior SHA %q after %s", prior.HeadSHA, time.Duration(managedPullHeadRefreshAttempts-1)*managedPullHeadRefreshDelay)
}

func (c *Client) waitForManagedBranchHead(ctx context.Context, owner, repo, branch, expectedHeadSHA string) error {
	branch = strings.TrimSpace(branch)
	expectedHeadSHA = strings.TrimSpace(expectedHeadSHA)
	if branch == "" || expectedHeadSHA == "" {
		return fmt.Errorf("managed branch and exact head SHA are required")
	}
	var observedHeadSHA string
	for attempt := 0; attempt < managedPullHeadRefreshAttempts; attempt++ {
		ref, response, err := c.client.Git.GetRef(ctx, owner, repo, "heads/"+branch)
		if err != nil {
			// A newly pushed branch can briefly return 404 from GitHub's REST
			// ref endpoint even though the git transport already accepted it.
			// Retry only that exact eventual-consistency response; permission,
			// authentication, rate-limit, and other failures remain immediate.
			if response == nil || response.StatusCode != http.StatusNotFound || attempt == managedPullHeadRefreshAttempts-1 {
				return fmt.Errorf("verify managed branch %s: %w", branch, err)
			}
		} else {
			if ref.GetRef() != "refs/heads/"+branch {
				return fmt.Errorf("managed branch %s resolved to unexpected ref %q", branch, ref.GetRef())
			}
			observedHeadSHA = strings.TrimSpace(ref.GetObject().GetSHA())
			if strings.EqualFold(observedHeadSHA, expectedHeadSHA) {
				return nil
			}
		}
		if attempt == managedPullHeadRefreshAttempts-1 {
			break
		}
		timer := time.NewTimer(managedPullHeadRefreshDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf(
		"managed branch %s remained at SHA %q instead of exact head %s after %s",
		branch,
		observedHeadSHA,
		expectedHeadSHA,
		time.Duration(managedPullHeadRefreshAttempts-1)*managedPullHeadRefreshDelay,
	)
}

// ListOpenRepairPullRequests returns every open PR carrying one exact Hive
// fingerprint marker, regardless of branch. Callers use this to repair legacy
// duplicate state and to prove the one-active-PR invariant.
func (c *Client) ListOpenRepairPullRequests(ctx context.Context, repository, base, marker string) ([]OpenRepairPullRequest, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(base) == "" || strings.TrimSpace(marker) == "" {
		return nil, fmt.Errorf("base branch and repair marker are required")
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return nil, err
	}
	pulls, err := c.listPullRequestsByState(ctx, owner, repo, "", "open")
	if err != nil {
		return nil, err
	}
	result := make([]OpenRepairPullRequest, 0)
	for _, pull := range pulls {
		markerCount := exactMarkerLineCount(pull.GetBody(), marker)
		if markerCount == 0 {
			continue
		}
		if !managedPullRepositoriesMatch(pull, identity, repository) {
			c.logIgnoredForeignMarkerClaim(repository, pull, marker)
			continue
		}
		if markerCount != 1 {
			return nil, fmt.Errorf("open repair pull request #%d contains marker %q more than once", pull.GetNumber(), marker)
		}
		branch, head := pull.GetHead().GetRef(), pull.GetHead().GetSHA()
		if err := validateManagedPullRequest(pull, identity, repository, pull.GetNumber(), branch, head, base, marker, "", ""); err != nil {
			return nil, fmt.Errorf("open pull request #%d preclaims Hive marker %q but is not repository-owned: %w", pull.GetNumber(), marker, err)
		}
		if err := c.verifyManagedBranchHead(ctx, owner, repo, branch, head); err != nil {
			return nil, fmt.Errorf("verify exact branch for repair pull request #%d: %w", pull.GetNumber(), err)
		}
		result = append(result, OpenRepairPullRequest{Number: pull.GetNumber(), URL: pull.GetHTMLURL(), Branch: branch, HeadSHA: head})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

// CloseRepairPullRequestExact closes only an open Hive repair PR whose marker,
// branch, and head still match the caller's durable observation.
func (c *Client) CloseRepairPullRequestExact(ctx context.Context, repository string, number int, marker, expectedBranch, expectedHeadSHA string) error {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return err
	}
	if number <= 0 || strings.TrimSpace(marker) == "" || strings.TrimSpace(expectedBranch) == "" || strings.TrimSpace(expectedHeadSHA) == "" {
		return fmt.Errorf("repair PR number, marker, branch, and exact head are required")
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return err
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("get repair pull request #%d: %w", number, err)
	}
	if err := validateManagedPullRequest(pull, identity, repository, number, expectedBranch, expectedHeadSHA, pull.GetBase().GetRef(), marker, "", ""); err != nil {
		return fmt.Errorf("refusing to close repair pull request #%d: %w", number, err)
	}
	if pull.GetState() != "open" {
		return nil
	}
	if err := c.verifyManagedBranchHead(ctx, owner, repo, expectedBranch, expectedHeadSHA); err != nil {
		return fmt.Errorf("refusing to close repair pull request #%d: %w", number, err)
	}
	if _, _, err := c.client.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		return fmt.Errorf("close repair pull request #%d: %w", number, err)
	}
	closed, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("verify closed repair pull request #%d: %w", number, err)
	}
	if err := validateManagedPullRequest(closed, identity, repository, number, expectedBranch, expectedHeadSHA, pull.GetBase().GetRef(), marker, "", ""); err != nil || closed.GetState() != "closed" {
		if err == nil {
			err = fmt.Errorf("state is %q", closed.GetState())
		}
		return fmt.Errorf("repair pull request #%d did not close exactly: %w", number, err)
	}
	return nil
}

// ListMergedBaselineBranches returns only still-existing Hive baseline refs
// that are proven to be the exact heads of merged, labeled baseline-review PRs.
// This recovers branches created by older Hive versions even if their local
// repair checkpoint was later superseded.
func (c *Client) ListMergedBaselineBranches(ctx context.Context, repository, base string) ([]MergedBaselineBranch, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return nil, err
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	refOptions := &gh.ReferenceListOptions{Ref: "heads/hive/baseline-", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		page, response, err := c.client.Git.ListMatchingRefs(ctx, owner, repo, refOptions)
		if err != nil {
			return nil, fmt.Errorf("list Hive baseline refs: %w", err)
		}
		for _, ref := range page {
			branch := strings.TrimPrefix(ref.GetRef(), "refs/heads/")
			if strings.HasPrefix(branch, "hive/baseline-") && ref.GetObject().GetSHA() != "" {
				refs[branch] = ref.GetObject().GetSHA()
			}
		}
		if response.NextPage == 0 {
			break
		}
		refOptions.Page = response.NextPage
	}
	if len(refs) == 0 {
		return nil, nil
	}

	pulls, err := c.listPullRequestsByState(ctx, owner, repo, base, "closed")
	if err != nil {
		return nil, fmt.Errorf("list closed baseline review pull requests: %w", err)
	}
	byBranch := map[string]MergedBaselineBranch{}
	for _, summary := range pulls {
		branch := summary.GetHead().GetRef()
		expectedSHA, exists := refs[branch]
		if !exists || !hasExactLabel(summary.Labels, "hive/baseline-review") {
			continue
		}
		if !managedPullRepositoriesMatch(summary, identity, repository) {
			c.logIgnoredForeignMarkerClaim(repository, summary, "hive/baseline-review")
			continue
		}
		if err := validateManagedPullRequest(summary, identity, repository, summary.GetNumber(), branch, expectedSHA, base, "", "", ""); err != nil {
			return nil, fmt.Errorf("closed baseline review pull request #%d is not repository-owned: %w", summary.GetNumber(), err)
		}
		pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, summary.GetNumber())
		if err != nil {
			return nil, fmt.Errorf("get baseline review pull request #%d: %w", summary.GetNumber(), err)
		}
		fingerprint, marked := baselineReviewFingerprint(pull.GetBody())
		if !pull.GetMerged() || !hasExactLabel(pull.Labels, "hive/baseline-review") || !marked {
			continue
		}
		marker := strings.TrimSpace(strings.SplitN(pull.GetBody(), "\n", 2)[0])
		if err := validateManagedPullRequest(pull, identity, repository, summary.GetNumber(), branch, expectedSHA, base, marker, "", ""); err != nil {
			return nil, fmt.Errorf("merged baseline review pull request #%d changed identity: %w", summary.GetNumber(), err)
		}
		if _, duplicate := byBranch[branch]; duplicate {
			return nil, fmt.Errorf("multiple merged baseline review PRs claim branch %s", branch)
		}
		byBranch[branch] = MergedBaselineBranch{PRNumber: pull.GetNumber(), PRURL: pull.GetHTMLURL(), RepositoryFingerprint: fingerprint, Branch: branch, HeadSHA: expectedSHA}
	}
	if len(byBranch) != len(refs) {
		return nil, fmt.Errorf("a live Hive baseline branch lacks an exact merged baseline-review PR")
	}
	branches := make([]MergedBaselineBranch, 0, len(byBranch))
	for _, branch := range byBranch {
		branches = append(branches, branch)
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].PRNumber < branches[j].PRNumber })
	return branches, nil
}

func (c *Client) listPullRequestsByState(ctx context.Context, owner, repo, base, state string) ([]*gh.PullRequest, error) {
	result := make([]*gh.PullRequest, 0)
	options := &gh.PullRequestListOptions{State: state, Base: base, ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		pulls, response, err := c.client.PullRequests.List(ctx, owner, repo, options)
		if err != nil {
			return nil, err
		}
		result = append(result, pulls...)
		if response.NextPage == 0 {
			return result, nil
		}
		options.Page = response.NextPage
	}
}

func (c *Client) resolveManagedRepositoryIdentity(ctx context.Context, owner, repo, requested string) (managedRepositoryIdentity, error) {
	metadata, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return managedRepositoryIdentity{}, fmt.Errorf("verify managed repository identity: %w", err)
	}
	identity := managedRepositoryIdentity{ID: metadata.GetID(), FullName: strings.TrimSpace(metadata.GetFullName())}
	if identity.ID <= 0 || identity.FullName == "" || !strings.EqualFold(identity.FullName, strings.TrimSpace(requested)) {
		return managedRepositoryIdentity{}, fmt.Errorf("managed repository must resolve to a positive ID and exact full_name")
	}
	return identity, nil
}

func (c *Client) verifyManagedBranchHead(ctx context.Context, owner, repo, branch, expectedHeadSHA string) error {
	branch = strings.TrimSpace(branch)
	expectedHeadSHA = strings.TrimSpace(expectedHeadSHA)
	if branch == "" || expectedHeadSHA == "" {
		return fmt.Errorf("managed branch and exact head SHA are required")
	}
	ref, _, err := c.client.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		return fmt.Errorf("verify managed branch %s: %w", branch, err)
	}
	if ref.GetRef() != "refs/heads/"+branch || !strings.EqualFold(ref.GetObject().GetSHA(), expectedHeadSHA) {
		return fmt.Errorf("managed branch %s no longer points to exact head %s", branch, expectedHeadSHA)
	}
	return nil
}

func validateManagedPullRequest(pull *gh.PullRequest, identity managedRepositoryIdentity, repository string, number int, branch, headSHA, base, marker, exactTitle, exactBody string) error {
	if pull == nil || identity.ID <= 0 || strings.TrimSpace(identity.FullName) == "" || number <= 0 || pull.GetNumber() != number {
		return fmt.Errorf("positive repository and pull request identities are required")
	}
	head, target := pull.GetHead(), pull.GetBase()
	if !managedPullRepositoriesMatch(pull, identity, repository) {
		return fmt.Errorf("head and base repositories must match target repository %s at positive ID %d", identity.FullName, identity.ID)
	}
	if strings.TrimSpace(branch) == "" || head.GetRef() != branch {
		return fmt.Errorf("head branch is %q, expected %q", head.GetRef(), branch)
	}
	if strings.TrimSpace(headSHA) == "" || !strings.EqualFold(head.GetSHA(), headSHA) {
		return fmt.Errorf("head SHA is %q, expected %q", head.GetSHA(), headSHA)
	}
	if strings.TrimSpace(base) == "" || target.GetRef() != base {
		return fmt.Errorf("base branch is %q, expected %q", target.GetRef(), base)
	}
	if marker != "" && exactMarkerLineCount(pull.GetBody(), marker) != 1 {
		return fmt.Errorf("body does not contain exactly one exact Hive marker line")
	}
	if exactTitle != "" && pull.GetTitle() != exactTitle {
		return fmt.Errorf("title changed after managed update")
	}
	if exactBody != "" && pull.GetBody() != exactBody {
		return fmt.Errorf("body changed after managed update")
	}
	return nil
}

func managedPullRepositoriesMatch(pull *gh.PullRequest, identity managedRepositoryIdentity, repository string) bool {
	if pull == nil || identity.ID <= 0 || strings.TrimSpace(identity.FullName) == "" {
		return false
	}
	head, base := pull.GetHead().GetRepo(), pull.GetBase().GetRepo()
	requested := strings.TrimSpace(repository)
	return head.GetID() > 0 && base.GetID() > 0 && head.GetID() == identity.ID && base.GetID() == identity.ID &&
		strings.EqualFold(head.GetFullName(), identity.FullName) && strings.EqualFold(base.GetFullName(), identity.FullName) &&
		strings.EqualFold(head.GetFullName(), requested) && strings.EqualFold(base.GetFullName(), requested)
}

func (c *Client) logIgnoredForeignMarkerClaim(repository string, pull *gh.PullRequest, marker string) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Warn("ignoring foreign pull request that claims a Hive marker",
		"repository", repository,
		"pull_request", pull.GetNumber(),
		"head_repository_id", pull.GetHead().GetRepo().GetID(),
		"head_repository", pull.GetHead().GetRepo().GetFullName(),
		"base_repository_id", pull.GetBase().GetRepo().GetID(),
		"base_repository", pull.GetBase().GetRepo().GetFullName(),
		"marker", marker,
	)
}

func exactMarkerLineCount(body, marker string) int {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == marker {
			count++
		}
	}
	return count
}

func exactManagedSHA(value string) bool {
	if strings.TrimSpace(value) != value || len(value) != 40 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' {
			continue
		}
		lower := character | 0x20
		if lower < 'a' || lower > 'f' {
			return false
		}
	}
	return true
}

func repairPullRequestResult(pull *gh.PullRequest, created bool) RepairPullRequest {
	return RepairPullRequest{Number: pull.GetNumber(), URL: pull.GetHTMLURL(), HeadSHA: pull.GetHead().GetSHA(), Created: created}
}

func hasExactLabel(labels []*gh.Label, expected string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label.GetName()), expected) {
			return true
		}
	}
	return false
}

func baselineReviewFingerprint(body string) (string, bool) {
	const prefix = "<!-- hive-baseline-review: "
	line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return "", false
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) != 40 {
		return "", false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", false
	}
	return parts[0], true
}

// ensureLabelWithColor creates a label with the caller-specified color,
// tolerating already-exists responses. Distinct from client.go ensureLabel,
// which owns the auto-merge label with its fixed color/description.
func (c *Client) ensureLabelWithColor(ctx context.Context, owner, repo, name, color string) error {
	_, _, err := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{Name: gh.Ptr(name), Color: gh.Ptr(color)})
	if err == nil {
		return nil
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil && (response.Response.StatusCode == http.StatusUnprocessableEntity || response.Response.StatusCode == http.StatusAlreadyReported) {
		return nil
	}
	return fmt.Errorf("ensure label %q: %w", name, err)
}
