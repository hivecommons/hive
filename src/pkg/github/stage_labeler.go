package github

import (
	"context"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageLabeler adapts Client to stagelabels.Labeler so every stage write the
// hub makes — reconciler or claim hook — goes through stagelabels.Enforce.
// It remembers the updated_at of each item it read so the writes that
// follow are journaled against that state (see addLabelsObserved): a stage
// label another writer put back after the hub removed it once is removable
// again, which the plain label mutation claim did not allow.
type stageLabeler struct {
	c        *Client
	mu       sync.Mutex
	observed map[int]string
}

func (s *stageLabeler) Labels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if s.c == nil || s.c.client == nil {
		return nil, ErrNoGitHubClient
	}
	issue, _, err := s.c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.observed[number] = issue.GetUpdatedAt().UTC().Format(time.RFC3339Nano)
	s.mu.Unlock()
	return extractLabels(issue.Labels), nil
}

func (s *stageLabeler) observedAt(number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed[number]
}

func (s *stageLabeler) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	return s.c.addLabelsObserved(ctx, owner+"/"+repo, number, labels, s.observedAt(number))
}

func (s *stageLabeler) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	return s.c.removeLabelObserved(ctx, owner+"/"+repo, number, label, s.observedAt(number))
}

// StageLabeler exposes the client as a stagelabels.Labeler. Each call returns
// a fresh adapter; use one per Enforce so the read and its writes pair up.
func (c *Client) StageLabeler() stagelabels.Labeler {
	return &stageLabeler{c: c, observed: map[int]string{}}
}

// ApplyStageLabel puts exactly stage on an issue or PR, keeping whatever
// author/health labels it carries (hivecommons/hive#12512). It is the entry
// point for writers that learn of a stage change outside the enumeration
// cycle — the claim-ledger hooks — and re-reads the item so a stage another
// writer placed since is replaced, never joined.
func (c *Client) ApplyStageLabel(ctx context.Context, repo string, number int, stage stagelabels.Stage) (stagelabels.Outcome, error) {
	if c == nil {
		return stagelabels.Outcome{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	return stagelabels.EnforceStage(ctx, c.StageLabeler(), owner, name, number, stage)
}
