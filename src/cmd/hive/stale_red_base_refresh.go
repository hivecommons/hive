package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	// staleRedRefreshInterval spaces the passes: each one reads the tip and
	// check runs of every base branch a red PR targets.
	staleRedRefreshInterval = 5 * time.Minute
	// staleRedRefreshMaxPerRepo and staleRedRefreshMaxPerPass cap the
	// branch updates one pass makes. A fixed base can leave a dozen PRs red
	// at once; the rest are picked up on later passes.
	staleRedRefreshMaxPerRepo = 2
	staleRedRefreshMaxPerPass = 6
	// staleRedRefreshMemoCap bounds the settled (PR, head, base tip) memo.
	staleRedRefreshMemoCap = 4096
)

// staleRedRefresher is the GitHub side of the pass; *github.Client
// implements it.
type staleRedRefresher interface {
	StaleRedBaseStateFor(ctx context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error)
	RefreshStaleRedPR(ctx context.Context, pr github.PullRequest, base github.StaleRedBaseState) (github.StaleRedRefreshOutcome, error)
}

// staleRedRefreshState is the pass clock and the memo of (PR → head|base
// tip) pairs already settled, so a PR is updated at most once per base tip.
// It is in memory: after a restart a settled pair costs one compare call
// (and GitHub answers an already-current branch without a new commit).
type staleRedRefreshState struct {
	mu      sync.Mutex
	lastRun time.Time
	settled map[string]string
}

var staleRedRefresh = &staleRedRefreshState{}

// refreshStaleRedBehindPRs merges the base into open PRs that are red only
// because of something their base has since fixed: the head is behind the
// base, and every check that failed on it completed green on the current
// base tip (projectbluefin/actions#544: pytest red from a baseline main fixed
// in #734; the PR waited six hours for a person to press "Update branch").
// The automerge lanes already do this for PRs the hive adopted; this pass
// covers every other PR in the actionable set, including repositories with
// auto-merge off.
//
// It leaves alone a PR that is a draft, held or exempt, still running CI,
// deferred to an open shared-CI incident, red on the base as well, a fork
// with "allow edits by maintainers" off, a dependency bot's branch (the bot
// rebases it), returned to its contributor (072, whose hive pushes there)
// or adopted by the hive (074, whose recovery owns it). Each update goes
// through the write-back gate (base_refresh). It runs at most every
// staleRedRefreshInterval and makes at most staleRedRefreshMaxPerRepo
// updates per repository and staleRedRefreshMaxPerPass in all.
//
// It returns the escalation keys of the PRs it updated, so the reaper in the
// same cycle does not also kick a fixer for a head that is about to be
// replaced.
func refreshStaleRedBehindPRs(
	ctx context.Context,
	cfg *config.Config,
	refresher staleRedRefresher,
	actionable *github.ActionableResult,
	audit func(repo string, number int, detail string),
	logger *slog.Logger,
	now time.Time,
) map[string]bool {
	if cfg == nil || refresher == nil || actionable == nil {
		return nil
	}
	st := staleRedRefresh
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.lastRun.IsZero() && now.Sub(st.lastRun) < staleRedRefreshInterval {
		return nil
	}
	st.lastRun = now
	if st.settled == nil || len(st.settled) > staleRedRefreshMemoCap {
		st.settled = map[string]string{}
	}

	type baseKey struct{ repo, ref string }
	type baseRead struct {
		state github.StaleRedBaseState
		ok    bool
	}
	bases := map[baseKey]baseRead{}
	perRepo := map[string]int{}
	total := 0
	var updated map[string]bool
	for _, pr := range actionable.PRs.Items {
		if total >= staleRedRefreshMaxPerPass {
			break
		}
		if staleRedRefreshSkip(pr) != "" {
			continue
		}
		repo := fullRepoName(pr.Repo, cfg.Project.Org)
		if perRepo[repo] >= staleRedRefreshMaxPerRepo {
			continue
		}
		bk := baseKey{repo, pr.BaseRef}
		base, read := bases[bk]
		if !read {
			state, exists, err := refresher.StaleRedBaseStateFor(ctx, repo, pr.BaseRef)
			if err != nil && logger != nil {
				logger.Warn("stale-red base refresh: could not read the base tip; retrying next pass", "repo", repo, "base", pr.BaseRef, "error", err)
			}
			base = baseRead{state: state, ok: err == nil && exists}
			bases[bk] = base
		}
		if !base.ok {
			continue
		}
		memoKey := fmt.Sprintf("%s#%d", strings.ToLower(repo), pr.Number)
		pair := pr.HeadSHA + "|" + base.state.SHA
		if st.settled[memoKey] == pair {
			continue
		}
		if verdict, _ := github.StaleRedBaseVerdictFor(pr.FailingChecks, base.state); verdict != github.StaleRedBaseClears {
			// Nothing is spent on a PR the base does not clear, and the
			// base tip's checks may still complete: read again next pass.
			continue
		}
		pr.Repo = repo
		outcome, err := refresher.RefreshStaleRedPR(ctx, pr, base.state)
		if err != nil {
			if logger != nil {
				logger.Warn("stale-red base refresh: update failed; retrying next pass", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.state.SHA, "error", err)
			}
			continue
		}
		if outcome.Settled {
			st.settled[memoKey] = pair
		}
		if !outcome.Updated {
			if logger != nil && outcome.Skipped != "current" {
				logger.Info("stale-red base refresh skipped", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.state.SHA, "reason", outcome.Skipped)
			}
			continue
		}
		perRepo[repo]++
		total++
		if logger != nil {
			logger.Info("stale-red base refresh: merged the base into a red PR whose failing checks are green on the base tip",
				"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base", pr.BaseRef, "base_sha", base.state.SHA, "behind_by", outcome.BehindBy, "failing_checks", pr.FailingChecks)
		}
		if audit != nil {
			audit(repo, pr.Number, fmt.Sprintf("repo=%s, pr=%d, head=%s, base_sha=%s, behind_by=%d, checks=%s, reason=failing checks green on the base tip",
				repo, pr.Number, pr.HeadSHA, base.state.SHA, outcome.BehindBy, strings.Join(pr.FailingChecks, ",")))
		}
		if updated == nil {
			updated = map[string]bool{}
		}
		updated[escalation.Key(repo, pr.Number)] = true
	}
	return updated
}

// staleRedRefreshSkip names why pr is not a stale-red refresh candidate, or
// "" when it is: an open, non-draft PR whose settled CI is red on named
// checks that are not red on the base, with nothing that owns its branch
// sync elsewhere.
func staleRedRefreshSkip(pr github.PullRequest) string {
	switch {
	case pr.Draft:
		return "draft"
	case github.HasHoldLabel(pr.Labels) || github.HasExemptLabel(pr.Labels, nil):
		return "held"
	case pr.CIStatus != "failure" || len(pr.FailingChecks) == 0:
		return "not-red"
	case pr.CIChecksRunning:
		return "ci-running"
	case pr.SharedCIIncident > 0:
		return "shared-ci-incident-open"
	case len(pr.BaseRedChecks) > 0:
		return "base-red"
	case strings.TrimSpace(pr.HeadSHA) == "" || strings.TrimSpace(pr.BaseRef) == "":
		return "head-or-base-unknown"
	case pr.FromFork && !pr.MaintainerCanModify:
		return "fork-not-editable"
	case github.DependencyBotBranch(pr.HeadRef, pr.Author):
		return "dependency-bot-branch"
	case hasLabel(pr.Labels, stagelabels.ReturnedToContributor.Label()):
		return "returned-to-contributor"
	case hasLabel(pr.Labels, stagelabels.AdoptedByHive.Label()):
		return "adopted-by-hive"
	}
	return ""
}
