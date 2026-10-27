package github

// `needs-decision` and `needs-direction` are retired (hivecommons/hive#12683).
// Both parked an issue on a maintainer ruling with identical gates — hard-
// suppressed at enumeration, the same `/hive approve` / `/hive decision`
// un-park, the same derived needs-decision stage — so a parked issue wore
// the stage label plus one or two park labels that said the same thing.
// stage/045-needs-decision is now the single issue park signal: an input the
// enumeration hard-suppresses on, and the stage the reconciler keeps. (It
// was stage/085 until the owner ruled that 085 is PR-only; an issue still
// wearing 085 is re-staged to 045 by Plan — the stale stage label comes off
// in the same write — and by the one-shot MigrateIssueNeedsDecisionStage
// sweep for issues the cycle does not enumerate. The 085 DEFINITION is
// never deleted: PRs wear it.)
//
// Migration is one-shot and per item, inside the issue stage reconciler,
// from the labels the list payload already carries: once Enforce has
// written (or confirmed) stage/045 on an issue carrying a retired label, the
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
	"errors"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// legacyDecisionRetireTimeout bounds one repo's definition-retirement pass
// (two list calls and up to two deletes).
const legacyDecisionRetireTimeout = 30 * time.Second

// removeLegacyDecisionLabels strips the retired park labels from an issue
// that now carries stage/045-needs-decision. carries is the label set after
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
		logger.Info("legacy decision label migrated to stage/045-needs-decision", "repo", in.Repo, "issue", in.Number, "removed", removed)
	}
	return 1
}

// retireLegacyDecisionLabelDefinitions deletes the retired park labels from
// a repo's label set once per process, and only when no open item still
// carries them: the reconciler's cycle saw none, and a one-item list per
// label confirms it for the PRs and issues the cycle did not enumerate.
// Returns the number of definitions deleted.
//
// It is housekeeping, so it stands down while the shared rate-limit caution
// window is engaged and its calls are bounded by legacyDecisionRetireTimeout:
// a paced ListByRepo with no deadline once stalled the governor run loop
// (hivecommons/hive#12983). A deadline or cancellation — the 30s bound, the
// caller's sync deadline, or a pacing slot that would miss either — is the
// same stand-down, logged at Debug: it is expected under pacing pressure and
// once warned 42 times in ten minutes (hivecommons/hive#14171). Either way
// the retirement retries next cycle. An explicit integration permission denial
// on deletion instead disables this optional cleanup for the repo until the
// client restarts; retrying with the same credentials cannot repair it.
func (c *Client) retireLegacyDecisionLabelDefinitions(ctx context.Context, repo string, logger *slog.Logger) int {
	if c == nil || c.client == nil {
		return 0
	}
	owner, name := c.splitRepo(repo)
	key := strings.ToLower(owner + "/" + name)
	if c.legacyDecisionLabels.has(key) {
		return 0
	}
	if sharedSlowStartState.cautious(time.Now()) {
		if logger != nil {
			logger.Debug("legacy decision label: retirement deferred while rate-limit caution window is engaged", "repo", repo)
		}
		return 0
	}
	if ctx.Err() != nil {
		// The caller's sync deadline is already spent; a request would only
		// fail. Nothing is marked, so the next cycle retries.
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, legacyDecisionRetireTimeout)
	defer cancel()
	deleted := 0
	for _, label := range legacyDecisionLabels {
		items, _, err := c.client.Issues.ListByRepo(ctx, owner, name, &gh.IssueListByRepoOptions{State: "open", Labels: []string{label}, ListOptions: gh.ListOptions{PerPage: 1}})
		if err != nil {
			if isLegacyDecisionDeferral(err) {
				if logger != nil {
					logger.Debug("legacy decision label: retirement deferred by deadline", "repo", repo, "label", label, "error", err)
				}
				return deleted
			}
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
			if isWriteForbiddenError(err) {
				c.legacyDecisionLabels.mark(key)
				if logger != nil {
					logger.Info("legacy decision label: definition retirement disabled for repository; integration lacks permission", "repo", repo, "label", label, "error", err)
				}
				return deleted
			}
			if isLegacyDecisionDeferral(err) {
				if logger != nil {
					logger.Debug("legacy decision label: retirement deferred by deadline", "repo", repo, "label", label, "error", err)
				}
				return deleted
			}
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

// isLegacyDecisionDeferral reports whether a retirement call failed only
// because its context ran out (directly, or via the pacing transport's
// slot-would-miss-deadline fail-fast, which wraps context.DeadlineExceeded).
func isLegacyDecisionDeferral(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
