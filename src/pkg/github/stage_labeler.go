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

// observedState renders an item's list-payload updated_at as the state a
// label write is journaled against (see addLabelsObserved). Zero → "": the
// plain, state-less key.
func observedState(updatedAt time.Time) string {
	if updatedAt.IsZero() {
		return ""
	}
	return updatedAt.UTC().Format(time.RFC3339Nano)
}

// RemoveLabelsFresh re-reads the item and removes each of labels it still
// carries, journaled against that read. It is for writers that learn of a
// change outside the enumeration cycle and hold no item snapshot (the claim
// release hooks): the plain RemoveLabel journals "remove X on #N" as applied
// forever, so a label another writer put back could never come off again
// (hivecommons/hive#12512; the same key left hivecommons/hive#12560 with no
// stage label at all). Absent labels cost nothing beyond the read.
func (c *Client) RemoveLabelsFresh(ctx context.Context, repo string, number int, labels ...string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	l := c.StageLabeler()
	fresh, err := l.Labels(ctx, owner, name, number)
	if err != nil {
		return err
	}
	var firstErr error
	for _, label := range labels {
		if label == "" || !issueHasLabel(fresh, label) {
			continue
		}
		if err := l.RemoveLabel(ctx, owner, name, number, label); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// AddLabelsFresh re-reads the item and adds the labels it lacks, journaled
// against that read; see RemoveLabelsFresh. Stage labels are not accepted:
// an item's stage is the stage engine's to write (Client.StageFacts).
func (c *Client) AddLabelsFresh(ctx context.Context, repo string, number int, labels ...string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	l := c.StageLabeler()
	fresh, err := l.Labels(ctx, owner, name, number)
	if err != nil {
		return err
	}
	var add []string
	for _, label := range labels {
		if label != "" && !stagelabels.IsStageLabel(label) && !issueHasLabel(fresh, label) {
			add = append(add, label)
		}
	}
	if len(add) == 0 {
		return nil
	}
	return l.AddLabels(ctx, owner, name, number, add)
}
