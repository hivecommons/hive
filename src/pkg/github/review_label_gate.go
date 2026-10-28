package github

import (
	"context"

	"github.com/hivecommons/hive/pkg/config"
)

// SetReviewRequireLabels wires review.require_labels into the hive-review
// relay. Read through a func so a live config edit applies to the next
// request without rebuilding the client.
func (c *Client) SetReviewRequireLabels(fn func() []string) {
	if c == nil {
		return
	}
	c.reviewRequireLabelsFn = fn
}

// reviewRequireLabels returns the normalized gate list, nil when off.
func (c *Client) reviewRequireLabels() []string {
	if c == nil || c.reviewRequireLabelsFn == nil {
		return nil
	}
	return config.ReviewConfig{RequireLabels: c.reviewRequireLabelsFn()}.NormalizedRequireLabels()
}

// reviewLabelGateAdmits reports whether the relay may act on repo#number
// under review.require_labels. With the gate off it admits without a fetch;
// otherwise the PR's labels are read fresh with the App token (the relay
// trusts nothing the agent wrote in the request) and matched with
// config.ReviewConfig.AdmitsPRLabels. A fetch error is returned so the caller
// can retry rather than either post or quarantine on a transient failure.
func (c *Client) reviewLabelGateAdmits(ctx context.Context, repo string, number int) (bool, error) {
	required := c.reviewRequireLabels()
	if len(required) == 0 {
		return true, nil
	}
	if c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_label_gate"), owner, repoName, number)
	if err != nil {
		return false, err
	}
	return config.ReviewConfig{RequireLabels: required}.AdmitsPRLabels(labelNames(pr.Labels)), nil
}
