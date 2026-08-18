package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// ManagedPullRequestSnapshot is the immutable evidence Hive records for a
// repository-owned management pull request. It intentionally excludes review
// and check state: uninstall finalization cares only about exact identity,
// content, and whether GitHub reports that exact pull request as merged.
type ManagedPullRequestSnapshot struct {
	Number       int      `json:"number"`
	URL          string   `json:"url"`
	State        string   `json:"state"`
	Merged       bool     `json:"merged"`
	MergeSHA     string   `json:"merge_sha,omitempty"`
	HeadBranch   string   `json:"head_branch"`
	HeadSHA      string   `json:"head_sha"`
	BaseBranch   string   `json:"base_branch"`
	BaseSHA      string   `json:"base_sha"`
	ChangedFiles []string `json:"changed_files"`
	DiffDigest   string   `json:"diff_digest"`
}

// InspectManagedPullRequestExact re-reads one same-repository pull request and
// binds its repository ID, marker, refs, paths, and raw diff. Callers persist
// this snapshot before relying on a later merged response.
func (c *Client) InspectManagedPullRequestExact(ctx context.Context, repository, repositoryID string, number int, marker, branch, headSHA, base string) (ManagedPullRequestSnapshot, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return ManagedPullRequestSnapshot{}, err
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return ManagedPullRequestSnapshot{}, err
	}
	if strconv.FormatInt(identity.ID, 10) != strings.TrimSpace(repositoryID) {
		return ManagedPullRequestSnapshot{}, fmt.Errorf("managed repository ID changed: got %d, expected %s", identity.ID, repositoryID)
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return ManagedPullRequestSnapshot{}, fmt.Errorf("read managed pull request #%d: %w", number, err)
	}
	if err := validateManagedPullRequest(pull, identity, repository, number, branch, headSHA, base, marker, "", ""); err != nil {
		return ManagedPullRequestSnapshot{}, fmt.Errorf("managed pull request #%d identity changed: %w", number, err)
	}
	files, err := c.listPullRequestFiles(ctx, owner, repo, number, pull.GetChangedFiles())
	if err != nil {
		return ManagedPullRequestSnapshot{}, err
	}
	diffDigest, err := c.PullRequestDiffDigest(ctx, repository, number)
	if err != nil {
		return ManagedPullRequestSnapshot{}, err
	}
	return ManagedPullRequestSnapshot{
		Number: number, URL: pull.GetHTMLURL(), State: pull.GetState(), Merged: pull.GetMerged(), MergeSHA: pull.GetMergeCommitSHA(),
		HeadBranch: pull.GetHead().GetRef(), HeadSHA: pull.GetHead().GetSHA(), BaseBranch: pull.GetBase().GetRef(), BaseSHA: pull.GetBase().GetSHA(),
		ChangedFiles: files, DiffDigest: diffDigest,
	}, nil
}

// FindManagedPullRequestExact recovers an exact open or closed management PR
// after a process lost the create response. Multiple exact claims are an
// ambiguity and therefore fail closed.
func (c *Client) FindManagedPullRequestExact(ctx context.Context, repository, repositoryID, marker, branch, headSHA, base string) (ManagedPullRequestSnapshot, bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return ManagedPullRequestSnapshot{}, false, err
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return ManagedPullRequestSnapshot{}, false, err
	}
	if strconv.FormatInt(identity.ID, 10) != strings.TrimSpace(repositoryID) {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed repository ID changed: got %d, expected %s", identity.ID, repositoryID)
	}
	pulls, err := c.listPullRequestsByState(ctx, owner, repo, "", "all")
	if err != nil {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("discover managed pull request: %w", err)
	}
	matched := 0
	matchedNumber := 0
	for _, pull := range pulls {
		if exactMarkerLineCount(pull.GetBody(), marker) == 0 {
			continue
		}
		if err := validateManagedPullRequest(pull, identity, repository, pull.GetNumber(), branch, headSHA, base, marker, "", ""); err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("pull request #%d claims uninstall marker but is not exact: %w", pull.GetNumber(), err)
		}
		matched++
		matchedNumber = pull.GetNumber()
	}
	if matched == 0 {
		return ManagedPullRequestSnapshot{}, false, nil
	}
	if matched != 1 {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("multiple exact pull requests claim uninstall marker %q", marker)
	}
	snapshot, err := c.InspectManagedPullRequestExact(ctx, repository, repositoryID, matchedNumber, marker, branch, headSHA, base)
	return snapshot, err == nil, err
}

// CloseManagedPullRequestForCancellation closes the one unmerged management
// pull request that is still bound to the durable repository, marker, PR URL,
// same-repository managed branch, and base branch. Cancellation deliberately
// does not trust a previously recorded base or head SHA: GitHub's "update
// branch" operation changes those immutable values and is the reason this
// recovery path exists. Callers must separately prove ancestry before deleting
// a moved branch ref.
//
// A zero number and empty URL are accepted only for the prepared/create-response
// recovery window. In that mode every exact marker claimant is inspected and
// more than one match is an ambiguity. Once the PR identity has been recorded,
// both its positive number and exact URL are mandatory.
func (c *Client) CloseManagedPullRequestForCancellation(ctx context.Context, repository, repositoryID string, number int, expectedURL, marker, branch, base string) (ManagedPullRequestSnapshot, bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return ManagedPullRequestSnapshot{}, false, err
	}
	if strings.TrimSpace(repositoryID) == "" || strings.TrimSpace(marker) == "" || strings.TrimSpace(branch) == "" || strings.TrimSpace(base) == "" ||
		(number == 0) != (strings.TrimSpace(expectedURL) == "") || number < 0 {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("cancellation requires repository identity, marker, managed refs, and either both or neither recorded PR number and URL")
	}
	identity, err := c.resolveManagedRepositoryIdentity(ctx, owner, repo, repository)
	if err != nil {
		return ManagedPullRequestSnapshot{}, false, err
	}
	if strconv.FormatInt(identity.ID, 10) != strings.TrimSpace(repositoryID) {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed repository ID changed: got %d, expected %s", identity.ID, repositoryID)
	}

	var pull *gh.PullRequest
	if number > 0 {
		pull, _, err = c.client.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("read recorded managed pull request #%d for cancellation: %w", number, err)
		}
		if pull.GetHTMLURL() != expectedURL {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("recorded managed pull request #%d URL changed: got %q, expected %q", number, pull.GetHTMLURL(), expectedURL)
		}
		if err := validateManagedCancellationPull(pull, identity, repository, number, marker, branch, base); err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("recorded managed pull request #%d is not cancellable: %w", number, err)
		}
	} else {
		pulls, listErr := c.listPullRequestsByState(ctx, owner, repo, "", "all")
		if listErr != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("discover managed pull request for cancellation: %w", listErr)
		}
		for _, candidate := range pulls {
			if exactMarkerLineCount(candidate.GetBody(), marker) == 0 {
				continue
			}
			if err := validateManagedCancellationPull(candidate, identity, repository, candidate.GetNumber(), marker, branch, base); err != nil {
				return ManagedPullRequestSnapshot{}, false, fmt.Errorf("pull request #%d claims cancellation marker but is not exact: %w", candidate.GetNumber(), err)
			}
			if pull != nil {
				return ManagedPullRequestSnapshot{}, false, fmt.Errorf("multiple pull requests claim cancellation marker %q", marker)
			}
			pull = candidate
		}
		if pull == nil {
			return ManagedPullRequestSnapshot{}, false, nil
		}
	}

	if pull.GetMerged() {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed pull request #%d is already merged; cancellation is forbidden", pull.GetNumber())
	}
	if pull.GetState() != "open" && pull.GetState() != "closed" {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed pull request #%d has unsupported state %q", pull.GetNumber(), pull.GetState())
	}
	observedHead := strings.ToLower(strings.TrimSpace(pull.GetHead().GetSHA()))
	if !exactManagedSHA(observedHead) {
		return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed pull request #%d returned no immutable head", pull.GetNumber())
	}
	if pull.GetState() == "open" {
		if err := c.verifyManagedBranchHead(ctx, owner, repo, branch, observedHead); err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("refusing to close managed pull request #%d: %w", pull.GetNumber(), err)
		}
		if _, _, err := c.client.PullRequests.Edit(ctx, owner, repo, pull.GetNumber(), &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("close managed pull request #%d for cancellation: %w", pull.GetNumber(), err)
		}
		closed, _, err := c.client.PullRequests.Get(ctx, owner, repo, pull.GetNumber())
		if err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("verify managed pull request #%d cancellation: %w", pull.GetNumber(), err)
		}
		if closed.GetHTMLURL() != pull.GetHTMLURL() || closed.GetState() != "closed" || closed.GetMerged() || !strings.EqualFold(closed.GetHead().GetSHA(), observedHead) {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed pull request #%d did not close with its exact unmerged identity intact", pull.GetNumber())
		}
		if err := validateManagedCancellationPull(closed, identity, repository, pull.GetNumber(), marker, branch, base); err != nil {
			return ManagedPullRequestSnapshot{}, false, fmt.Errorf("managed pull request #%d changed during cancellation: %w", pull.GetNumber(), err)
		}
		pull = closed
	}

	return managedCancellationSnapshot(pull), true, nil
}

func validateManagedCancellationPull(pull *gh.PullRequest, identity managedRepositoryIdentity, repository string, number int, marker, branch, base string) error {
	currentHead := strings.ToLower(strings.TrimSpace(pull.GetHead().GetSHA()))
	if !exactManagedSHA(currentHead) {
		return fmt.Errorf("pull request head is not an immutable commit")
	}
	if err := validateManagedPullRequest(pull, identity, repository, number, branch, currentHead, base, marker, "", ""); err != nil {
		return err
	}
	if pull.GetMerged() {
		return fmt.Errorf("pull request is already merged")
	}
	return nil
}

func managedCancellationSnapshot(pull *gh.PullRequest) ManagedPullRequestSnapshot {
	return ManagedPullRequestSnapshot{
		Number: pull.GetNumber(), URL: pull.GetHTMLURL(), State: pull.GetState(), Merged: pull.GetMerged(), MergeSHA: pull.GetMergeCommitSHA(),
		HeadBranch: pull.GetHead().GetRef(), HeadSHA: strings.ToLower(strings.TrimSpace(pull.GetHead().GetSHA())),
		BaseBranch: pull.GetBase().GetRef(), BaseSHA: strings.ToLower(strings.TrimSpace(pull.GetBase().GetSHA())),
	}
}
