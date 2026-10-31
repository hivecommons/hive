package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/gfi"
)

// HardSuppressIssueLabels returns a copy of the escalation labels that park
// an issue on a person. Exposed for the good-first-issue scorer, which must
// never advertise a parked issue (and whose own labels are deliberately NOT
// in this set — `good first issue` is an invitation, not a hold).
func HardSuppressIssueLabels() []string {
	return append([]string(nil), hardSuppressIssueLabels...)
}

// UpsertIssueMarkerComment implements gfi.Forge: it finds the first comment
// on repo#number containing marker, edits it to body when body is non-empty
// and differs, and — only when create is set — posts body when none exists.
// With create false and an empty body it is a pure lookup.
func (c *Client) UpsertIssueMarkerComment(ctx context.Context, repo string, number int, marker, body string, create bool) (gfi.MarkerComment, error) {
	if c == nil {
		return gfi.MarkerComment{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		return gfi.MarkerComment{}, err
	}
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), marker) {
			continue
		}
		res := gfi.MarkerComment{Found: true, Previous: cm.GetBody()}
		if body == "" || cm.GetBody() == body {
			return res, nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repoName, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return res, fmt.Errorf("editing marker comment on %s/%s#%d: %w", owner, repoName, number, err)
		}
		c.noteCommentEdited()
		return res, nil
	}
	if !create || body == "" {
		return gfi.MarkerComment{}, nil
	}
	if err := c.CreateIssueComment(ctx, owner+"/"+repoName, number, body); err != nil {
		return gfi.MarkerComment{}, err
	}
	return gfi.MarkerComment{Created: true}, nil
}

// IssueLabels implements gfi.LabelReader: the issue's labels as GitHub has
// them now, not as the last enumeration recorded them.
func (c *Client) IssueLabels(ctx context.Context, repo string, number int) ([]string, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	return c.StageLabeler().Labels(ctx, owner, repoName, number)
}

var (
	_ gfi.Forge       = (*Client)(nil)
	_ gfi.LabelReader = (*Client)(nil)
)

// MaintainerAppliedLabels implements gfi.MaintainerLabelReader. Only the
// latest application of a currently present label counts; automation cannot
// turn its own advertising into evidence of maintainer endorsement.
func (c *Client) MaintainerAppliedLabels(ctx context.Context, repo string, number int, labels []string) ([]string, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	events, err := c.listIssueLabelEvents(ctx, owner, name, number)
	if err != nil {
		return nil, err
	}
	policy := c.gfiLabelApprovalPolicy(repo, owner, name)
	var verified []string
	for _, label := range labels {
		var latest *issueLabelEvent
		for i := len(events) - 1; i >= 0; i-- {
			if strings.EqualFold(strings.TrimSpace(events[i].Label), strings.TrimSpace(label)) {
				latest = &events[i]
				break
			}
		}
		if latest == nil || latest.Event != "labeled" {
			continue
		}
		_, trusted, err := c.labelApplierTrusted(ctx, owner, name, latest.Actor, policy)
		if err != nil {
			return nil, err
		}
		if trusted {
			verified = append(verified, label)
		}
	}
	return verified, nil
}

// gfiLabelApprovalPolicy is the repo's configured label-approval policy, so
// its trusted-bot allowlist and org-member setting decide who counts as a
// maintainer. A repo with no configured policy keeps the default of trusting
// organisation members.
func (c *Client) gfiLabelApprovalPolicy(repo, owner, name string) LabelApprovalPolicy {
	policy, ok := c.labelApprovalPolicyFor(repo)
	if !ok {
		policy, ok = c.labelApprovalPolicyFor(owner + "/" + name)
	}
	if !ok {
		return LabelApprovalPolicy{TrustOrgMembers: true}
	}
	return policy
}

var _ gfi.MaintainerLabelReader = (*Client)(nil)
