package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/worksource"
)

// freshenKickMessages is the freshness bound in front of a kick (#13375).
// A kick hands an agent work the governor planned from its snapshot; when
// that snapshot is served from the state cache (or is a last-good copy) the
// item may be older than github.DecisionFreshnessBound. For every GitHub
// ref in a message the guard either accepts the cached copy (fresh enough),
// re-reads it once (one targeted GET) and keeps it only if still open, or
// refuses it because the repo's share of the snapshot is stale. Refs that
// fail are dropped from the message; a message whose refs all dropped is
// withheld, since kicking an agent at nothing it can act on only burns a
// turn. Messages with no refs (housekeeping kicks) and refs that are not
// GitHub items pass through untouched. A nil guard returns the input.
func freshenKickMessages(ctx context.Context, guard github.IrreversibleGuard, actionable *github.ActionableResult, messages []scheduler.KickMessage, logger *slog.Logger) []scheduler.KickMessage {
	if guard == nil || len(messages) == 0 {
		return messages
	}
	prRefs := prRefSet(actionable)
	out := make([]scheduler.KickMessage, 0, len(messages))
	for _, msg := range messages {
		if len(msg.IssueRefs) == 0 {
			out = append(out, msg)
			continue
		}
		kept := make([]string, 0, len(msg.IssueRefs))
		allDeferred := true
		for _, raw := range msg.IssueRefs {
			ref, ok := worksource.ParseKey(raw)
			if !ok || !ref.IsGitHubIssue() {
				kept = append(kept, raw)
				continue
			}
			key := ref.Key()
			_, isPR := prRefs[key]
			if err := guard(ctx, ref.Repo, ref.Number, isPR); err != nil {
				deferred := errors.Is(err, github.ErrAPIBudgetCritical) || errors.Is(err, github.ErrRateLimitCaution)
				if !deferred {
					allDeferred = false
				}
				if logger != nil {
					reason := "re-read"
					if errors.Is(err, github.ErrSnapshotStaleForDecision) {
						reason = "stale snapshot"
					}
					// A refusal from the API budget or rate-limit caution
					// window is the designed back-off: the next cycle retries.
					// It is logged at INFO so it does not feed the generic
					// unclassified-error self-report (#15465).
					level := slog.LevelWarn
					if deferred {
						reason = "github read deferred"
						level = slog.LevelInfo
					}
					logger.Log(ctx, level, "kick ref withheld: snapshot not fresh enough to act on",
						slog.String("agent", msg.Agent), slog.String("ref", raw),
						slog.String("reason", reason), slog.Any("error", err))
				}
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) == 0 {
			if logger != nil {
				level := slog.LevelWarn
				if allDeferred {
					level = slog.LevelInfo
				}
				logger.Log(ctx, level, "kick withheld: every ref failed the freshness check", slog.String("agent", msg.Agent))
			}
			continue
		}
		msg.IssueRefs = kept
		out = append(out, msg)
	}
	return out
}

// prRefSet is the set of "repo#N" keys the snapshot classified as PRs, so a
// kick ref can be confirmed through the PR endpoint rather than the issue
// one (which would miss a merge).
func prRefSet(actionable *github.ActionableResult) map[string]struct{} {
	if actionable == nil {
		return nil
	}
	set := make(map[string]struct{}, len(actionable.PRs.Items)+len(actionable.PRs.Attributed))
	add := func(prs []github.PullRequest) {
		for _, pr := range prs {
			set[worksource.Ref{Repo: pr.Repo, Number: pr.Number}.Key()] = struct{}{}
		}
	}
	add(actionable.PRs.Items)
	add(actionable.PRs.Attributed)
	return set
}
