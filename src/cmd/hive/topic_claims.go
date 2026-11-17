package main

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// Topic-claim wiring (hivecommons/hive#18493). Agents write claims into their
// own bead store with `bd topic claim`; the governor reads them for the kick
// (scheduler.SetTopicClaimLookup), follows a claimed PR's head branch across
// renames, and releases a claim once its PR merges or closes.

// topicClaimLookup projects every agent's open topic claims for the
// scheduler. Stores are read in agent-name order so the kick is stable.
func topicClaimLookup(stores map[string]*beads.Store) scheduler.TopicClaimLookup {
	return func() []scheduler.TopicClaim {
		names := make([]string, 0, len(stores))
		for name := range stores {
			names = append(names, name)
		}
		sort.Strings(names)
		var out []scheduler.TopicClaim
		for _, name := range names {
			store := stores[name]
			if store == nil {
				continue
			}
			for _, c := range store.OpenTopics() {
				out = append(out, scheduler.TopicClaim{Agent: name, Key: c.Key, Repo: c.Repo, PR: c.PR, Branch: c.Branch})
			}
		}
		return out
	}
}

// releaseTopicClaimsForPR ends every agent's open claim held through
// repo#number once the PR reached a terminal state (beads.TopicStateMerged
// or beads.TopicStateClosed). It returns how many claims were released.
func releaseTopicClaimsForPR(stores map[string]*beads.Store, repo string, number int, state string, logger *slog.Logger) int {
	if repo == "" || number <= 0 {
		return 0
	}
	total := 0
	for name, store := range stores {
		if store == nil {
			continue
		}
		n, err := store.ReleaseTopicsForPR(repo, number, state)
		if err != nil && logger != nil {
			logger.Warn("topic claim release failed", "agent", name, "repo", repo, "pr", number, "error", err)
		}
		if n > 0 && logger != nil {
			logger.Info("topic claim released", "agent", name, "repo", repo, "pr", number, "state", state, "count", n)
		}
		total += n
	}
	return total
}

// topicStateForPR maps a PR's observed state to the claim state that ends a
// claim, or "" while the PR is still open.
func topicStateForPR(pr github.PullRequest) string {
	if !pr.MergedAt.IsZero() || strings.EqualFold(pr.State, github.PRStateMerged) {
		return beads.TopicStateMerged
	}
	if strings.EqualFold(pr.State, github.PRStateClosed) {
		return beads.TopicStateClosed
	}
	return ""
}

// topicStateForTerminal maps a merged/closed observation or audit state to a
// claim state, or "" for anything else.
func topicStateForTerminal(state string) string {
	switch state {
	case github.PRStateMerged:
		return beads.TopicStateMerged
	case github.PRStateClosed:
		return beads.TopicStateClosed
	}
	return ""
}

// topicStateForAudit maps the hive's own merge/close audit action to a claim
// state, or "" for any other action.
func topicStateForAudit(action string) string {
	switch action {
	case github.AuditActionPRMerged:
		return beads.TopicStateMerged
	case github.AuditActionPRClosed:
		return beads.TopicStateClosed
	}
	return ""
}

// syncTopicClaims is the poll path, run each eval cycle after the bead stores
// reload: a claimed PR the snapshot shows merged or closed releases its
// claim, and an open one whose head branch changed updates the claim's
// branch so the record follows a rename.
func syncTopicClaims(stores map[string]*beads.Store, actionable *github.ActionableResult, logger *slog.Logger) {
	if actionable == nil || len(stores) == 0 {
		return
	}
	prs := make([]github.PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held)+len(actionable.PRs.Attributed))
	prs = append(prs, actionable.PRs.Items...)
	prs = append(prs, actionable.PRs.Held...)
	prs = append(prs, actionable.PRs.Attributed...)
	for name, store := range stores {
		if store == nil {
			continue
		}
		for _, claim := range store.OpenTopics() {
			if claim.PR == 0 {
				bindPendingTopicClaim(store, name, prs, claim, logger)
				continue
			}
			pr, ok := findTopicPR(prs, claim)
			if !ok {
				continue
			}
			if state := topicStateForPR(pr); state != "" {
				releaseTopicClaimsForPR(map[string]*beads.Store{name: store}, claim.Repo, claim.PR, state, logger)
				continue
			}
			if pr.HeadRef == "" || pr.HeadRef == claim.Branch {
				continue
			}
			if _, err := store.SetTopicBranch(claim.Repo, claim.PR, pr.HeadRef); err != nil && logger != nil {
				logger.Warn("topic claim branch refresh failed", "agent", name, "topic", claim.Key, "error", err)
			}
		}
	}
}

// bindPendingTopicClaim binds a pending claim (made from a sandboxed kick
// before hive opened its PR, hivecommons/hive#18572) to the PR whose head
// branch is the claim's branch. An open PR wins over a closed one; a PR that
// is already terminal still binds, and the next cycle releases the claim.
func bindPendingTopicClaim(store *beads.Store, agent string, prs []github.PullRequest, claim beads.TopicClaim, logger *slog.Logger) {
	if claim.Branch == "" {
		return
	}
	number := 0
	for _, pr := range prs {
		if pr.HeadRef != claim.Branch || pr.Number <= 0 || !beads.SameTopicRepo(pr.Repo, claim.Repo) {
			continue
		}
		if number == 0 || topicStateForPR(pr) == "" {
			number = pr.Number
		}
	}
	if number == 0 {
		return
	}
	n, err := store.BindTopicBranch(claim.Repo, claim.Branch, number)
	if logger == nil {
		return
	}
	if err != nil {
		logger.Warn("pending topic claim bind failed", "agent", agent, "topic", claim.Key, "error", err)
		return
	}
	if n > 0 {
		logger.Info("pending topic claim bound to its PR", "agent", agent, "topic", claim.Key, "repo", claim.Repo, "pr", number)
	}
}

// findTopicPR returns the snapshot's copy of the claim's PR. A terminal copy
// wins over an open one, since Attributed can carry a PR that left Items.
func findTopicPR(prs []github.PullRequest, claim beads.TopicClaim) (github.PullRequest, bool) {
	var found github.PullRequest
	ok := false
	for _, pr := range prs {
		if pr.Number != claim.PR || !beads.SameTopicRepo(pr.Repo, claim.Repo) {
			continue
		}
		if !ok || topicStateForPR(pr) != "" {
			found, ok = pr, true
		}
	}
	return found, ok
}
