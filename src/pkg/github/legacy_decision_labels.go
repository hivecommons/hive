package github

// `needs-decision` and `needs-direction` are retired (hivecommons/hive#12683).
// Both parked an issue on a maintainer ruling with identical gates — hard-
// suppressed at enumeration, the same `/hive approve` / `/hive decision`
// un-park, the same derived stage/085-needs-decision — so a parked issue wore
// the stage label plus one or two park labels that said the same thing.
// stage/085-needs-decision is now the single park signal: an input the
// enumeration hard-suppresses on, and the stage the reconciler keeps.
//
// Migration is one-shot and per item, inside the issue stage reconciler,
// from the labels the list payload already carries: once Enforce has
// written (or confirmed) stage/085 on an issue carrying a retired label, the
// retired label comes off. Add-then-remove, so a failure between the two
// never leaves the issue un-parked. Readers keep recognising the retired
// spellings (hardSuppressIssueLabels, isIssueParkingLabel, the PR lanes'
// hold modifiers) until they are gone. When a cycle finds no open issue
// still carrying them, the label definitions are deleted from the repo —
// after one confirming list call per label, since deleting a definition
// strips it from every item at once and must never be the thing that
// un-parks a PR the cycle did not see.

import (
	"context"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// removeLegacyDecisionLabels strips the retired park labels from an issue
// that now carries stage/085-needs-decision. carries is the label set after
// the stage write. Returns 1 when at least one retired label came off.
func removeLegacyDecisionLabels(ctx context.Context, client *Client, in IssueStageInput, carries []string, logger *slog.Logger) int {
	if client == nil || !hasLegacyDecisionLabel(in.Labels) || !issueHasLabel(carries, NeedsDecisionStageLabel) {
		return 0
	}
	removed := 0
	for _, label := range in.Labels {
		if !isLegacyDecisionLabel(label) {
			continue
		}
		if err := client.RemoveLabel(ctx, in.Repo, in.Number, strings.TrimSpace(label)); err != nil {
			if logger != nil {
				logger.Warn("legacy decision label: removing retired label failed", "repo", in.Repo, "issue", in.Number, "label", label, "error", err)
			}
			continue
		}
		removed++
	}
	if removed == 0 {
		return 0
	}
	if logger != nil {
		logger.Info("legacy decision label migrated to stage/085-needs-decision", "repo", in.Repo, "issue", in.Number, "removed", removed)
	}
	return 1
}

// retireLegacyDecisionLabelDefinitions deletes the retired park labels from
// a repo's label set once per process, and only when no open item still
// carries them: the reconciler's cycle saw none, and a one-item list per
// label confirms it for the PRs and issues the cycle did not enumerate.
// Returns the number of definitions deleted.
func (c *Client) retireLegacyDecisionLabelDefinitions(ctx context.Context, repo string, logger *slog.Logger) int {
	if c == nil || c.client == nil {
		return 0
	}
	owner, name := c.splitRepo(repo)
	key := strings.ToLower(owner + "/" + name)
	if c.legacyDecisionLabels.has(key) {
		return 0
	}
	deleted := 0
	for _, label := range legacyDecisionLabels {
		items, _, err := c.client.Issues.ListByRepo(ctx, owner, name, &gh.IssueListByRepoOptions{State: "open", Labels: []string{label}, ListOptions: gh.ListOptions{PerPage: 1}})
		if err != nil {
			if logger != nil {
				logger.Warn("legacy decision label: listing open carriers failed", "repo", repo, "label", label, "error", err)
			}
			return deleted
		}
		if len(items) > 0 {
			// Something open (a PR the issue cycle never sees, or an issue
			// labelled since) still carries it; try again next process.
			return deleted
		}
		ok, err := c.deleteLabelDefinition(ctx, owner, name, label)
		if err != nil {
			if logger != nil {
				logger.Warn("legacy decision label: deleting definition failed", "repo", repo, "label", label, "error", err)
			}
			return deleted
		}
		if ok {
			deleted++
			if logger != nil {
				logger.Info("legacy decision label definition deleted", "repo", repo, "label", label)
			}
		}
	}
	c.legacyDecisionLabels.mark(key)
	return deleted
}
