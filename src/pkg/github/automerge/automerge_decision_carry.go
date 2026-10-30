package automerge

import (
	"context"
	"strings"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// decisionCarrier is the engine's carrier of recorded operator decisions
// over the hive's own base refresh (hivecommons/hive#15649). nil when the
// engine has no REST client or App identity to verify the refresh with: a
// decision then stands only on the head it was recorded on, as before.
func (c *Engine) decisionCarrier() *hgithub.DecisionCarrier {
	if c == nil || c.humanChangeRequests == nil || c.gh == nil || c.transport == nil {
		return nil
	}
	app := strings.TrimSpace(c.transport.AppBotLogin())
	if app == "" {
		return nil
	}
	return &hgithub.DecisionCarrier{
		Store: c.humanChangeRequests, API: c.gh.Repositories, AppBotLogin: app, Logger: c.logger, Checker: &c.baseRefresh,
		ResolveBase: func(ctx context.Context, owner, repo string, number int) (string, error) {
			pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:decision_base_refresh"), owner, repo, number)
			if err != nil {
				return "", err
			}
			return pr.GetBase().GetRef(), nil
		},
	}
}

// recordedDecisionFor is review.DecisionAnswerFor for head on
// owner/repo#number, carrying the PR's newest decision onto head first when
// head is nothing but the hive's base refresh of the head it was recorded
// on. baseRef may be "" when the caller does not know it.
func (c *Engine) recordedDecisionFor(ctx context.Context, owner, repo string, number int, baseRef, head string) (review.HumanChangeRequest, bool) {
	if c == nil || c.humanChangeRequests == nil {
		return review.HumanChangeRequest{}, false
	}
	fullRepo := owner + "/" + repo
	if req, ok := review.DecisionAnswerFor(c.humanChangeRequests, fullRepo, number, head); ok {
		return req, true
	}
	carrier := c.decisionCarrier()
	if carrier == nil {
		return review.HumanChangeRequest{}, false
	}
	return carrier.Carry(ctx, owner, repo, number, baseRef, head)
}
