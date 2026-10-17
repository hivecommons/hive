package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// unparkReleasePolicy is resolved once per repository by the unpark sweep.
// enabled is deliberately false unless the operator opted this repo in.
type unparkReleasePolicy struct {
	enabled       bool
	versionSource string
}

// approveIssueRelease executes a typed release owner step after the ordinary
// /hive command author and permission gates have passed. Every live repository
// precondition is re-read before the only release write.
func (c *Client) approveIssueRelease(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, candidate *gh.IssueComment, actor string, step *IssueOwnerStep, policy unparkReleasePolicy, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	refusal, err := c.releaseApprovalRefusal(ctx, displayRepo, owner, repo, step, policy)
	if err != nil {
		return false, err
	}
	if refusal != "" {
		body := renderReleaseApprovalRefusal(step.Tag, refusal)
		if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), body); err != nil {
			return false, err
		}
		result.Replies++
		return true, nil
	}

	release, _, err := c.client.Repositories.CreateRelease(ctx, owner, repo, &gh.RepositoryRelease{
		TagName:              gh.Ptr(step.Tag),
		TargetCommitish:      gh.Ptr(step.Target),
		Name:                 gh.Ptr(step.Tag),
		GenerateReleaseNotes: gh.Ptr(true),
	})
	if err != nil {
		return false, fmt.Errorf("creating approved release %s in %s/%s: %w", step.Tag, owner, repo, err)
	}
	releaseURL := strings.TrimSpace(release.GetHTMLURL())
	if releaseURL == "" {
		releaseURL = fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", owner, repo, url.PathEscape(step.Tag))
	}
	c.recordWriteAudit(AuditActionReleaseCreated, InvocationMeta{Agent: AttributionAgentGovernor},
		WriteTarget{Repo: displayRepo, Number: number},
		"actor", actor, "tag", step.Tag, "target", step.Target, "url", releaseURL)

	removed, err := c.clearIssueParkingLabels(ctx, owner, repo, number, issueLabelNames(issue.Labels))
	if err != nil {
		return false, err
	}
	decision := fmt.Sprintf("create release `%s` from `%s`", step.Tag, step.Target)
	body := renderUnparkAcceptedReply(actor, decision, removed) + "\n\nRelease: " + releaseURL
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), body); err != nil {
		return false, err
	}
	event := IssueUnparkEvent{
		Repo:       displayRepo,
		Number:     number,
		Actor:      actor,
		Decision:   decision,
		CommentURL: candidate.GetHTMLURL(),
		ReleaseTag: step.Tag,
		ReleaseURL: releaseURL,
	}
	result.Unparked = append(result.Unparked, event)
	if audit != nil {
		audit(event)
	}
	if c.logger != nil {
		c.logger.Info("release created by maintainer approval", "repo", displayRepo, "issue", number, "actor", actor, "tag", step.Tag, "target", step.Target, "url", releaseURL)
	}
	return true, nil
}

// releaseApprovalRefusal returns a human-facing reason when a live release
// precondition does not hold. Operational API errors are returned separately so
// a transient outage is retried instead of consuming the approval command.
func (c *Client) releaseApprovalRefusal(ctx context.Context, displayRepo, owner, repo string, step *IssueOwnerStep, policy unparkReleasePolicy) (string, error) {
	branch, err := c.DefaultBranch(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolving the default branch before release: %w", err)
	}
	live, _, err := c.client.Repositories.GetBranch(ctx, owner, repo, branch, 3)
	if err != nil {
		return "", fmt.Errorf("reading default branch %s before release: %w", branch, err)
	}
	head := live.GetCommit().GetSHA()
	if head == "" {
		return "", fmt.Errorf("default branch %s returned no head SHA", branch)
	}
	if head != step.Target {
		comparison, _, compareErr := c.client.Repositories.CompareCommits(ctx, owner, repo, step.Target, head, nil)
		if compareErr != nil {
			return "", fmt.Errorf("checking whether release target %s is an ancestor of %s: %w", step.Target, branch, compareErr)
		}
		if comparison.GetStatus() != "ahead" {
			return fmt.Sprintf("target `%s` is no longer the `%s` head or an ancestor of it", step.Target, branch), nil
		}
	}

	if _, resp, refErr := c.client.Git.GetRef(ctx, owner, repo, "tags/"+step.Tag); refErr == nil {
		return fmt.Sprintf("tag `%s` already exists", step.Tag), nil
	} else if resp == nil || resp.StatusCode != http.StatusNotFound {
		return "", fmt.Errorf("checking release tag %s: %w", step.Tag, refErr)
	}

	configured, configKnown := c.configRequiredChecksForRepo(displayRepo)
	required, known, fromConfig, _, _ := RequiredStatusCheckContextsDetailedWithSource(ctx, c.client, owner, repo, branch, configured, configKnown)
	if !known {
		return "the required-check set could not be determined", nil
	}
	ci, err := EvaluateCommitCI(ctx, c.client, owner, repo, step.Target, CommitCIOptions{
		Required:                required,
		RequiredKnown:           true,
		RequiredKnownFromConfig: fromConfig,
	})
	if err != nil {
		return "", fmt.Errorf("checking release target CI: %w", err)
	}
	if !ci.Green {
		return fmt.Sprintf("required checks are not green on `%s` (%s)", step.Target, ci.Reason), nil
	}

	source, err := cleanReleaseVersionSource(policy.versionSource)
	if err != nil {
		return fmt.Sprintf("the configured version source is invalid (%v)", err), nil
	}
	if source != "" {
		file, _, resp, contentErr := c.client.Repositories.GetContents(ctx, owner, repo, source, &gh.RepositoryContentGetOptions{Ref: step.Target})
		if contentErr != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return fmt.Sprintf("version source `%s` does not exist at `%s`", source, step.Target), nil
			}
			return "", fmt.Errorf("reading release version source %s at %s: %w", source, step.Target, contentErr)
		}
		if file == nil {
			return fmt.Sprintf("version source `%s` at `%s` is not a file", source, step.Target), nil
		}
		version, contentErr := file.GetContent()
		if contentErr != nil {
			return fmt.Sprintf("version source `%s` at `%s` could not be decoded", source, step.Target), nil
		}
		version = strings.TrimSpace(version)
		if !releaseTagMatchesVersion(step.Tag, version) {
			return fmt.Sprintf("tag `%s` does not match `%s` in version source `%s`", step.Tag, version, source), nil
		}
	}
	return "", nil
}

func cleanReleaseVersionSource(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}
	cleaned := path.Clean(source)
	if cleaned != source || cleaned == "." || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "../") {
		return "", fmt.Errorf("must be a normalized repository-relative path")
	}
	return source, nil
}

func releaseTagMatchesVersion(tag, version string) bool {
	normalize := func(s string) string {
		return strings.TrimPrefix(strings.TrimSpace(s), "v")
	}
	return normalize(tag) != "" && normalize(tag) == normalize(version)
}

func renderReleaseApprovalRefusal(tag, reason string) string {
	return fmt.Sprintf("Release `%s` was not created: %s. The issue remains parked; resolve this condition and reply `/hive approve` again.", tag, reason)
}
