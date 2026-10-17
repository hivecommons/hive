package github

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// coveredByPRLabelState memoises, per "owner/repo", that the covered-by-PR
// stage label exists, so the per-issue writers do not re-check on every tick.
type coveredByPRLabelState struct {
	mu   sync.Mutex
	done map[string]struct{}
}

func (s *coveredByPRLabelState) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.done[key]
	return ok
}

func (s *coveredByPRLabelState) mark(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = map[string]struct{}{}
	}
	s.done[key] = struct{}{}
}

// legacyCoveredByPRLabels are the retired spellings (hivecommons/hive#12436):
// the bee label that replaced `hive/covered-by-pr` in #11888, and that older
// name. Readers still RECOGNISE them for one migration cycle; nothing writes
// them, and the stage reconciler removes them from every open item it labels.
var legacyCoveredByPRLabels = []string{stagelabels.LegacyCoveredByPR, stagelabels.LegacyCoveredByPRASCII}

// HasCoveredByPRLabel reports whether labels say an open PR covers the
// issue: the `stage/040-covered-by-pr` label, a PR stage past it that an
// older build mirrored onto the issue (hivecommons/hive#12451; the
// reconciler clamps it back to 040), or one of the retired spellings still
// awaiting migration. stage/085 never means covered: on an issue it is the
// park signal itself (hivecommons/hive#12683) — an agent, the relay, a
// person or the reconciler put it there to say a human ruling is owed, with
// or without a PR.
func HasCoveredByPRLabel(labels []string) bool {
	if cur := stagelabels.Current(labels); cur != stagelabels.None && !cur.Before(stagelabels.CoveredByPR) && cur != stagelabels.NeedsDecision {
		return true
	}
	for _, legacy := range legacyCoveredByPRLabels {
		if issueHasLabel(labels, legacy) {
			return true
		}
	}
	return false
}

// removeCoveredByPRLabel strips the covered-by-PR stage label and the
// retired spellings from an in-memory label set.
func removeCoveredByPRLabel(labels []string) []string {
	out := removeIssueLabel(labels, CoveredByPRLabel)
	for _, legacy := range legacyCoveredByPRLabels {
		out = removeIssueLabel(out, legacy)
	}
	return out
}

// EnsureCoveredByPRLabel makes sure repo defines `stage/040-covered-by-pr`
// with the stage family colour. One API round-trip, memoised per repo for the
// life of the process. The retired labels are not renamed: the stage
// reconciler migrates open issues item by item (a rename would collide with
// the label the automerge sweep provisions for the same family).
func (c *Client) EnsureCoveredByPRLabel(ctx context.Context, repo string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" {
		return fmt.Errorf("EnsureCoveredByPRLabel: repo is required")
	}
	key := strings.ToLower(owner + "/" + name)
	if c.coveredByPRLabel.has(key) {
		return nil
	}
	if err := c.EnsureIssueLabel(ctx, repo, CoveredByPRLabel, CoveredByPRLabelColor, CoveredByPRLabelDescription); err != nil {
		return err
	}
	c.coveredByPRLabel.mark(key)
	return nil
}
