package github

import (
	"context"
	"errors"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// CommitCIState is the shared CI-evaluation result behind the self-merge
// sweep's commitGreen (pkg/github/automerge) and the merge-request watcher's
// positive-confirmation gate (merge_ci_gate.go, #6173). It walks the commit
// statuses and check runs on a SHA exactly as commitGreen always has, and
// additionally reports HOW MUCH evidence it saw and which required checks
// never reported at all, so a caller that must fail closed on "no CI" can do
// so without a second round of API calls.
//
// It lives in this package rather than in automerge because automerge imports
// this package (the transport, the ignore lists) and the watcher lives here;
// a core both can call has to sit below both.
type CommitCIState struct {
	// Green is true when every gating status/check run has succeeded (or is
	// ignorable). Reason names the first blocker when false: "status-pending",
	// "check-pending", "status-<state>" or "check-<conclusion>". Zero statuses
	// and zero check runs is Green HERE (the sweep runs behind GitHub's own
	// mergeable gate); only the watcher treats that as unverified.
	Green  bool
	Reason string
	// Evidence counts every commit status and check run observed on the SHA,
	// gating or not, in any state. Zero means GitHub has NOTHING to say about
	// this commit: no workflow ever produced a job for it.
	Evidence int
	// MissingRequired lists the config/protection-declared required check
	// names (when that set is known) for which NO status and NO check run
	// exists on the SHA. A required check that has not been created yet is
	// "expected" in GitHub's vocabulary: not failed, not passed, not present.
	MissingRequired []string
}

// RequiredStatusCheckContexts returns the set of status-check contexts /
// check-run names that are actually required for branch, and whether that
// set could be determined at all. Membership in this set is what makes a
// check "required" (must be green) versus ignorable (any state/conclusion,
// never blocks).
//
// Precedence (first available source wins):
//  1. Config: configSet/configKnown, i.e. the operator-declared
//     auto_merge.required_checks list (config.AutoMergeConfig.RequiredCheckSet).
//     This needs NO GitHub API call and NO administration:read scope, so it
//     is checked first and is the primary path in practice.
//  2. GitHub's branch-protection API (Repositories.GetRequiredStatusChecks).
//     gh.ErrBranchNotProtected means "this branch legitimately requires
//     nothing" - that IS a known, empty required set, not a failure to
//     determine it, so requiredKnown is true with an empty map.
//  3. Neither available (no config list AND branch empty / API call failed
//     for a reason other than "not protected") -> requiredKnown=false. The
//     caller must fall back to the isMetaCheck/isIgnorableCICheck allowlist.
func RequiredStatusCheckContexts(ctx context.Context, client *gh.Client, owner, repo, branch string, configSet map[string]bool, configKnown bool) (map[string]bool, bool) {
	if configKnown {
		return configSet, true
	}
	if client == nil || strings.TrimSpace(branch) == "" {
		return nil, false
	}
	rsc, _, err := client.Repositories.GetRequiredStatusChecks(ctx, owner, repo, branch)
	if err != nil {
		if errors.Is(err, gh.ErrBranchNotProtected) {
			return map[string]bool{}, true
		}
		return nil, false
	}
	if rsc == nil {
		return map[string]bool{}, true
	}
	required := make(map[string]bool)
	if rsc.Contexts != nil {
		for _, name := range *rsc.Contexts {
			required[name] = true
		}
	}
	if rsc.Checks != nil {
		for _, check := range *rsc.Checks {
			if check == nil {
				continue
			}
			required[check.Context] = true
		}
	}
	return required, true
}

// EvaluateCommitCI walks every commit status and check run on sha and
// reports the CommitCIState. required/requiredKnown come from
// RequiredStatusCheckContexts. When requiredKnown is false the fail-closed
// allowlist fallback applies: meta checks are skipped and only the
// isIgnorableCICheck names may be non-green without blocking. Later blockers
// are still walked after the first so that Evidence and MissingRequired are
// complete for the caller. A non-nil error means the evidence could not be
// gathered; Reason then names the failing API ("status-check" or
// "check-runs").
func EvaluateCommitCI(ctx context.Context, client *gh.Client, owner, repo, sha string, required map[string]bool, requiredKnown bool) (CommitCIState, error) {
	var st CommitCIState
	if client == nil {
		st.Reason = "status-check"
		return st, ErrNoGitHubClient
	}
	seen := make(map[string]bool)
	// block records the first blocker; later ones are still walked so that
	// Evidence/seen are complete for the caller.
	block := func(reason string) {
		if st.Reason == "" {
			st.Reason = reason
		}
	}

	statusOpts := &gh.ListOptions{PerPage: 100}
	for {
		status, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			st.Reason = "status-check"
			return st, err
		}
		for _, s := range status.Statuses {
			ctxName := s.GetContext()
			st.Evidence++
			seen[ctxName] = true
			if requiredKnown {
				// Required-checks-only gating: skip anything not on the
				// branch's actual required list, no matter its state.
				if !required[ctxName] {
					continue
				}
			} else if isMetaCheck(ctxName) {
				// Fail-closed fallback path (required set unavailable).
				continue
			}
			switch s.GetState() {
			case "success":
			case "pending":
				block("status-pending")
			default: // "failure", "error"
				if !requiredKnown && isIgnorableCICheck(ctxName) {
					continue
				}
				block("status-" + s.GetState())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		checkRuns, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			st.Reason = "check-runs"
			return st, err
		}
		for _, cr := range checkRuns.CheckRuns {
			name := cr.GetName()
			st.Evidence++
			seen[name] = true
			if requiredKnown {
				if !required[name] {
					continue
				}
			} else if isMetaCheck(name) {
				continue
			}
			if cr.GetStatus() != "completed" {
				if !requiredKnown && isIgnorableCICheck(name) {
					continue
				}
				block("check-pending")
				continue
			}
			switch cr.GetConclusion() {
			case "success", "neutral", "skipped":
			default:
				if !requiredKnown && isIgnorableCICheck(name) {
					continue
				}
				block("check-" + cr.GetConclusion())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if requiredKnown {
		for name := range required {
			if !seen[name] {
				st.MissingRequired = append(st.MissingRequired, name)
			}
		}
		sort.Strings(st.MissingRequired)
	}
	st.Green = st.Reason == ""
	return st, nil
}
