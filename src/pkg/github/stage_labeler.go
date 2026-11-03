package github

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
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
	// facts is the write-back gate's view of each item read, keyed by
	// number; suppressed is set when the gate refused a write on it.
	facts      map[int]writeback.Item
	suppressed bool
	reason     string
}

func (s *stageLabeler) Labels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if s.c == nil || s.c.client == nil {
		return nil, ErrNoGitHubClient
	}
	issue, _, err := s.c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	labels := extractLabels(issue.Labels)
	s.mu.Lock()
	s.observed[number] = issue.GetUpdatedAt().UTC().Format(time.RFC3339Nano)
	if s.facts == nil {
		s.facts = map[int]writeback.Item{}
	}
	author := issue.GetUser().GetLogin()
	s.facts[number] = writeback.Item{Number: number, IsPR: issue.IsPullRequest(), Author: author, HiveAuthored: s.c.IsHiveAuthorLogin(author), Labels: labels, CreatedAt: issue.GetCreatedAt().Time}
	s.mu.Unlock()
	return labels, nil
}

// writeAllowed consults the write-back gate for a label write on number.
// Only PRs are gated here (writeback.KindStageLabel): an issue's stage
// labels are the work-intake pipeline the issue filter admits it to, a PR
// the hive did not open is a contributor's and needs the opt-in label
// (hivecommons/hive, torch-spyre 2026-10-28). A PR the labeler never read
// (no facts) is gated with what it knows: number only.
func (s *stageLabeler) writeAllowed(owner, repo string, number int, preview string) bool {
	s.mu.Lock()
	item, ok := s.facts[number]
	s.mu.Unlock()
	if !ok || !item.IsPR {
		return true
	}
	d := s.c.WritebackAllowed(owner+"/"+repo, item, writeback.KindStageLabel, preview)
	if !d.Allowed {
		s.mu.Lock()
		s.suppressed = true
		s.reason = d.Reason
		s.mu.Unlock()
	}
	return d.Allowed
}

func (s *stageLabeler) observedAt(number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed[number]
}

func (s *stageLabeler) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	if !s.writeAllowed(owner, repo, number, "add "+strings.Join(labels, ", ")) {
		return nil
	}
	return s.c.addLabelsObserved(ctx, owner+"/"+repo, number, labels, s.observedAt(number)) // writeback:gated
}

func (s *stageLabeler) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	if !s.writeAllowed(owner, repo, number, "remove "+label) {
		return nil
	}
	return s.c.removeLabelObserved(ctx, owner+"/"+repo, number, label, s.observedAt(number)) // writeback:gated
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
