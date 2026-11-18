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
)

// missingCheckRefresher is the GitHub side of the pass; *github.Client
// implements it.
type missingCheckRefresher interface {
	StaleRedBaseStateFor(ctx context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error)
	RefreshMissingCheckPR(ctx context.Context, pr github.PullRequest, base github.StaleRedBaseState, check string) (github.StaleRedRefreshOutcome, error)
}

// missingCheckRefreshState is the pass clock and two in-memory memos:
// settled holds the (PR → head|base tip) pairs already decided, as in the
// stale-red refresh, and refreshed holds the (PR, check) pairs the pass has
// already updated a branch for. The second one is the loop guard: a check
// that a fresh head still does not run (a path filter, a GITHUB_TOKEN-made
// PR, see #11877) is not chased with another update every time the base
// moves. One update marks every check missing at the time, so a PR missing
// many checks is updated once, not once per check. After a restart both are
// empty, which costs at most one more update per PR.
type missingCheckRefreshState struct {
	mu        sync.Mutex
	lastRun   time.Time
	settled   map[string]string
	refreshed map[string]bool
}

var missingCheckRefresh = &missingCheckRefreshState{}

// refreshMissingCheckBehindPRs merges the base into open PRs that wait only
// on a required check their head never ran, because the head predates the
// workflow that reports it (projectbluefin/testsuite#1245: `dashboard-build`
// became required while the PR was open; it sat at required-check-missing for
// about 29 hours until a person pressed "Update branch"). A missing check is
// a skip, not a failure, so the stale-red refresh never sees these PRs.
//
// A candidate has a required check with no check run and no commit status at
// all on its head (ProtectionFacts.MissingRequiredChecks, which is only set
// after both were read), CI otherwise settled green with nothing running, and
// evidence the check exists today: it has a run in any state on the base tip,
// or on another open PR's head against the same base. The update itself
// (shared with the stale-red refresh) only happens when the head is behind
// the base tip.
//
// It leaves alone a PR that is a draft, held, exempt or parked on a human
// decision, red, conflicting, waiting on workflow approval (GitHub holds its
// runs, and an update would only make a new head whose runs are held again),
// a fork with "allow edits by maintainers" off, a dependency bot's branch,
// returned to its contributor (072) or adopted by the hive (074). It shares
// the stale-red refresh's interval and caps, and updates a PR at most once
// per (head, base tip) and once per set of missing checks.
//
// It returns the escalation keys of the PRs it updated.
func refreshMissingCheckBehindPRs(
	ctx context.Context,
	cfg *config.Config,
	refresher missingCheckRefresher,
	actionable *github.ActionableResult,
	audit func(repo string, number int, detail string),
	logger *slog.Logger,
	now time.Time,
) map[string]bool {
	if cfg == nil || refresher == nil || actionable == nil {
		return nil
	}
	st := missingCheckRefresh
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.lastRun.IsZero() && now.Sub(st.lastRun) < staleRedRefreshInterval {
		return nil
	}
	st.lastRun = now
	if st.settled == nil || len(st.settled) > staleRedRefreshMemoCap {
		st.settled = map[string]string{}
	}
	if st.refreshed == nil || len(st.refreshed) > staleRedRefreshMemoCap {
		st.refreshed = map[string]bool{}
	}

	type baseKey struct{ repo, ref string }
	type baseRead struct {
		state github.StaleRedBaseState
		ok    bool
	}
	var seenOnPRs map[baseKey]map[string]bool
	bases := map[baseKey]baseRead{}
	perRepo := map[string]int{}
	total := 0
	var updated map[string]bool
	for _, pr := range actionable.PRs.Items {
		if total >= staleRedRefreshMaxPerPass {
			break
		}
		if missingCheckRefreshSkip(pr) != "" {
			continue
		}
		repo := fullRepoName(pr.Repo, cfg.Project.Org)
		if perRepo[repo] >= staleRedRefreshMaxPerRepo {
			continue
		}
		memoKey := fmt.Sprintf("%s#%d", strings.ToLower(repo), pr.Number)
		var pending []string
		for _, check := range pr.Protection.MissingRequiredChecks {
			if !st.refreshed[memoKey+"|"+check] {
				pending = append(pending, check)
			}
		}
		if len(pending) == 0 {
			continue
		}
		bk := baseKey{strings.ToLower(repo), pr.BaseRef}
		base, read := bases[bk]
		if !read {
			state, exists, err := refresher.StaleRedBaseStateFor(ctx, repo, pr.BaseRef)
			if err != nil && logger != nil {
				logger.Warn("missing-check base refresh: could not read the base tip; retrying next pass", "repo", repo, "base", pr.BaseRef, "error", err)
			}
			base = baseRead{state: state, ok: err == nil && exists}
			bases[bk] = base
		}
		if !base.ok {
			continue
		}
		pair := pr.HeadSHA + "|" + base.state.SHA
		if st.settled[memoKey] == pair {
			continue
		}
		if seenOnPRs == nil {
			seenOnPRs = map[baseKey]map[string]bool{}
			for _, other := range actionable.PRs.Items {
				k := baseKey{strings.ToLower(fullRepoName(other.Repo, cfg.Project.Org)), other.BaseRef}
				for name := range checksOnHead(other) {
					if seenOnPRs[k] == nil {
						seenOnPRs[k] = map[string]bool{}
					}
					seenOnPRs[k][name] = true
				}
			}
		}
		check, evidence := "", ""
		for _, name := range pending {
			switch {
			case base.state.Reported[name]:
				check, evidence = name, "base-tip"
			case seenOnPRs[bk][name]:
				check, evidence = name, "open-pr"
			}
			if check != "" {
				break
			}
		}
		if check == "" {
			// Nothing shows the check runs anywhere today: it may be
			// retired, misnamed or reported by something an update cannot
			// start. The base tip's runs may still appear; read again next
			// pass.
			continue
		}
		pr.Repo = repo
		outcome, err := refresher.RefreshMissingCheckPR(ctx, pr, base.state, check)
		if err != nil {
			if logger != nil {
				logger.Warn("missing-check base refresh: update failed; retrying next pass", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.state.SHA, "missing_check", check, "error", err)
			}
			continue
		}
		if outcome.Settled {
			st.settled[memoKey] = pair
		}
		if !outcome.Updated {
			if logger != nil && outcome.Skipped != "current" {
				logger.Info("missing-check base refresh skipped", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.state.SHA, "missing_check", check, "reason", outcome.Skipped)
			}
			continue
		}
		// One update answers every check missing at the time: a fresh head
		// that still lacks them will not grow them on the next base move
		// either, so none of them earns another update
		// (projectbluefin/server#686 has 12 missing checks).
		for _, name := range pending {
			st.refreshed[memoKey+"|"+name] = true
		}
		perRepo[repo]++
		total++
		if logger != nil {
			logger.Info("missing-check base refresh: merged the base into a PR whose head never ran a required check",
				"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base", pr.BaseRef, "base_sha", base.state.SHA, "behind_by", outcome.BehindBy, "missing_check", check, "evidence", evidence)
		}
		if audit != nil {
			audit(repo, pr.Number, fmt.Sprintf("repo=%s, pr=%d, head=%s, base_sha=%s, behind_by=%d, check=%s, evidence=%s, reason=required check never reported on the head",
				repo, pr.Number, pr.HeadSHA, base.state.SHA, outcome.BehindBy, check, evidence))
		}
		if updated == nil {
			updated = map[string]bool{}
		}
		updated[escalation.Key(repo, pr.Number)] = true
	}
	return updated
}

// missingCheckRefreshSkip names why pr is not a missing-check refresh
// candidate, or "" when it is: an open, non-draft PR whose only CI blocker
// is a required check with no run or status at all on its head, with
// nothing that owns its branch sync elsewhere.
func missingCheckRefreshSkip(pr github.PullRequest) string {
	switch {
	case pr.Draft:
		return "draft"
	case github.HasHoldLabel(pr.Labels) || github.HasExemptLabel(pr.Labels, nil):
		return "held"
	case needsDecisionParkLabel(pr.Labels) != "":
		return "parked"
	case pr.Protection == nil || !pr.Protection.RequiredChecksKnown || len(pr.Protection.MissingRequiredChecks) == 0:
		return "no-missing-required-check"
	case pr.CIStatus == "failure" || len(pr.Protection.FailingRequiredChecks) > 0:
		return "red"
	case pr.CIStatus != "success" || pr.CIChecksRunning:
		return "ci-not-settled"
	case pr.CIActivity != nil && pr.CIActivity.WorkflowApproval != nil:
		return "workflow-approval-pending"
	case missingCheckSeenOnHead(pr):
		return "check-reported"
	case conflictMergeState(pr):
		return "conflict"
	}
	return baseRefreshBranchOwnerSkip(pr)
}

// missingCheckSeenOnHead reports whether any of pr's missing required checks
// has a run or status on the head after all: CIActivity carries what webhooks
// delivered since the listing the protection facts were built from, and a
// check that is queued or running is slow, not missing.
func missingCheckSeenOnHead(pr github.PullRequest) bool {
	if pr.Protection == nil {
		return false
	}
	seen := checksOnHead(pr)
	for _, name := range pr.Protection.MissingRequiredChecks {
		if seen[name] {
			return true
		}
	}
	return false
}

// checksOnHead names every check run or commit status, in any state, that
// pr's CI activity records on its current head.
func checksOnHead(pr github.PullRequest) map[string]bool {
	out := map[string]bool{}
	if pr.CIActivity == nil {
		return out
	}
	for _, c := range pr.CIActivity.Checks {
		if c.Name == "" || (c.Head != "" && !strings.EqualFold(c.Head, pr.HeadSHA)) {
			continue
		}
		out[c.Name] = true
	}
	return out
}
