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

// IssueReleaseRefusalError reports a live release precondition that did not
// hold. It is safe to show to an operator; the issue remains parked.
type IssueReleaseRefusalError struct {
	Reason string
}

func (e *IssueReleaseRefusalError) Error() string {
	return e.Reason
}

// approveIssueRelease adapts the shared release operation to a `/hive approve`
// comment. The ordinary command author and permission gates have already run.
func (c *Client) approveIssueRelease(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, candidate *gh.IssueComment, actor string, step *IssueOwnerStep, policy unparkReleasePolicy, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	event, removed, refusal, err := c.executeApprovedIssueRelease(ctx, displayRepo, owner, repo, issue, actor, step, policy)
	if err != nil {
		return false, err
	}
	if refusal != "" {
		if err := c.postUnparkReply(ctx, owner, repo, issue.GetNumber(), candidate.GetID(), renderReleaseApprovalRefusal(step.Tag, refusal)); err != nil {
			return false, err
		}
		result.Replies++
		return true, nil
	}
	body := renderUnparkAcceptedReply(actor, event.Decision, removed) + "\n\nRelease: " + event.ReleaseURL
	if err := c.postUnparkReply(ctx, owner, repo, issue.GetNumber(), candidate.GetID(), body); err != nil {
		return false, err
	}
	event.CommentURL = candidate.GetHTMLURL()
	result.Unparked = append(result.Unparked, event)
	if audit != nil {
		audit(event)
	}
	return true, nil
}

// ApproveIssueRelease executes the same release operation for an authenticated
// dashboard action. It resolves the current trusted owner-step from GitHub
// instead of accepting tag or target data from the browser.
func (c *Client) ApproveIssueRelease(ctx context.Context, displayRepo string, number int, actor, versionSource string) (IssueUnparkEvent, error) {
	if c == nil || c.client == nil {
		return IssueUnparkEvent{}, ErrNoGitHubClient
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "dashboard owner"
	}
	owner, repo := c.splitRepo(displayRepo)
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return IssueUnparkEvent{}, fmt.Errorf("reading release owner-step on %s#%d: %w", displayRepo, number, err)
	}
	if issue == nil || issue.IsPullRequest() || !c.hasParkingLabel(issueLabelNames(issue.Labels)) {
		return IssueUnparkEvent{}, &IssueReleaseRefusalError{Reason: "the issue is no longer parked on an owner step"}
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return IssueUnparkEvent{}, err
	}
	menu := c.unparkMenuFor(issue.GetBody(), issue.GetUser(), comments)
	c.rememberIssueOwnerStep(displayRepo, number, menu.releaseStep)
	step := menu.releaseStep
	if step == nil {
		return IssueUnparkEvent{}, &IssueReleaseRefusalError{Reason: "the issue has no unanswered trusted release owner-step"}
	}
	event, removed, refusal, err := c.executeApprovedIssueRelease(ctx, displayRepo, owner, repo, issue, actor, step, unparkReleasePolicy{enabled: true, versionSource: versionSource})
	if err != nil {
		return event, err
	}
	if refusal != "" {
		body := renderReleaseApprovalRefusal(step.Tag, refusal)
		if err := c.CreateIssueComment(ctx, displayRepo, number, body); err != nil { // writeback:solicited authenticated dashboard release approval response.
			return IssueUnparkEvent{}, err
		}
		return IssueUnparkEvent{}, &IssueReleaseRefusalError{Reason: refusal}
	}
	body := renderUnparkAcceptedReply(actor, event.Decision, removed) + "\n\nRelease: " + event.ReleaseURL
	if err := c.CreateIssueComment(ctx, displayRepo, number, body); err != nil { // writeback:solicited authenticated dashboard release approval response.
		return event, fmt.Errorf("release %s was created but its issue comment failed: %w", step.Tag, err)
	}
	return event, nil
}

// executeApprovedIssueRelease owns every release precondition and write used by
// both `/hive approve` and the dashboard action. It does not decide who may
// invoke the action; each caller applies its own owner/write gate first.
func (c *Client) executeApprovedIssueRelease(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, actor string, step *IssueOwnerStep, policy unparkReleasePolicy) (IssueUnparkEvent, []string, string, error) {
	refusal, err := c.releaseApprovalRefusal(ctx, displayRepo, owner, repo, step, policy)
	if err != nil || refusal != "" {
		return IssueUnparkEvent{}, nil, refusal, err
	}
	release, _, err := c.client.Repositories.CreateRelease(ctx, owner, repo, &gh.RepositoryRelease{
		TagName:              gh.Ptr(step.Tag),
		TargetCommitish:      gh.Ptr(step.Target),
		Name:                 gh.Ptr(step.Tag),
		GenerateReleaseNotes: gh.Ptr(true),
	})
	if err != nil {
		return IssueUnparkEvent{}, nil, "", fmt.Errorf("creating approved release %s in %s/%s: %w", step.Tag, owner, repo, err)
	}
	releaseURL := strings.TrimSpace(release.GetHTMLURL())
	if releaseURL == "" {
		releaseURL = fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", owner, repo, url.PathEscape(step.Tag))
	}
	c.recordWriteAudit(AuditActionReleaseCreated, InvocationMeta{Agent: AttributionAgentGovernor},
		WriteTarget{Repo: displayRepo, Number: issue.GetNumber()},
		"actor", actor, "tag", step.Tag, "target", step.Target, "url", releaseURL)
	removed, err := c.clearIssueParkingLabels(ctx, owner, repo, issue.GetNumber(), issueLabelNames(issue.Labels), actor, false)
	if err != nil {
		return IssueUnparkEvent{}, nil, "", err
	}
	c.rememberIssueOwnerStep(displayRepo, issue.GetNumber(), nil)
	event := IssueUnparkEvent{
		Repo:       displayRepo,
		Number:     issue.GetNumber(),
		Actor:      actor,
		Decision:   fmt.Sprintf("create release `%s` from `%s`", step.Tag, step.Target),
		ReleaseTag: step.Tag,
		ReleaseURL: releaseURL,
	}
	if c.logger != nil {
		c.logger.Info("release created by maintainer approval", "repo", displayRepo, "issue", issue.GetNumber(), "actor", actor, "tag", step.Tag, "target", step.Target, "url", releaseURL)
	}
	return event, removed, "", nil
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
		FailOnAnyFailedCheck:    true,
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
