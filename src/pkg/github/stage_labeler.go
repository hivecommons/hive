package github

import (
	"context"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageLabeler adapts Client to stagelabels.Labeler so every stage write the
// hub makes — reconciler or claim hook — goes through stagelabels.Enforce.
type stageLabeler struct{ c *Client }

func (s stageLabeler) Labels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if s.c == nil || s.c.client == nil {
		return nil, ErrNoGitHubClient
	}
	issue, _, err := s.c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	return extractLabels(issue.Labels), nil
}

func (s stageLabeler) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	return s.c.AddLabels(ctx, owner+"/"+repo, number, labels)
}

func (s stageLabeler) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	return s.c.RemoveLabel(ctx, owner+"/"+repo, number, label)
}

// StageLabeler exposes the client as a stagelabels.Labeler.
func (c *Client) StageLabeler() stagelabels.Labeler { return stageLabeler{c} }

// SetStageLabel puts exactly stage on an issue or PR, keeping whatever
// author/health labels it carries (hivecommons/hive#12512). It is the entry
// point for writers that learn of a stage change outside the enumeration
// cycle — the claim-ledger hooks — and re-reads the item so a stage another
// writer placed since is replaced, never joined.
func (c *Client) SetStageLabel(ctx context.Context, repo string, number int, stage stagelabels.Stage) (stagelabels.Outcome, error) {
	if c == nil {
		return stagelabels.Outcome{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	return stagelabels.EnforceStage(ctx, c.StageLabeler(), owner, name, number, stage)
}
