package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// UpToDateEnforcement reports who enforces "branch must be up to date" for a
// target branch.
type UpToDateEnforcement string

const (
	// UpToDateServerEnforced: a ruleset sets strict_required_status_checks_policy.
	UpToDateServerEnforced UpToDateEnforcement = "server-enforced"
	// UpToDateHiveChecked: a ruleset requires checks but not up-to-date branches.
	UpToDateHiveChecked UpToDateEnforcement = "hive-checked"
	// UpToDateUnknown: no ruleset speaks to it. Classic branch protection
	// carries the flag but needs Administration permission the App lacks.
	UpToDateUnknown UpToDateEnforcement = "unknown"
)

// BranchRulesResult is the lane-only view of a branch's rules. It is read-only
// and must not be called from the direct merge paths, which keep using
// RequiredStatusCheckContexts unchanged.
type BranchRulesResult struct {
	// Required is the union of config, classic protection and ruleset required
	// checks. Only meaningful when Known is true.
	Required map[string]bool
	// Known is false when the set could not be established or is empty; the
	// lane must not merge and Reason says why.
	Known  bool
	Reason string
	// UpToDate is how the up-to-date requirement is enforced.
	UpToDate UpToDateEnforcement
	// MergeQueue is true when a merge_queue rule applies to the branch.
	// MergeQueueKnown is false when the rules endpoint could not be read.
	MergeQueue      bool
	MergeQueueKnown bool
}

// ReadBranchRules reads every required-check source for branch: the operator
// config list, classic branch protection and rulesets
// (GET /repos/{o}/{r}/rules/branches/{branch}, Metadata read only). A classic
// "branch not protected" is not read as "nothing required". Native merge-queue
// detection uses the merge_queue rule only; the GraphQL mergeQueue field was
// not verified for App installation tokens and is not used.
func ReadBranchRules(ctx context.Context, client *gh.Client, owner, repo, branch string, configSet map[string]bool, configKnown bool) BranchRulesResult {
	res := BranchRulesResult{Required: map[string]bool{}, UpToDate: UpToDateUnknown}
	if client == nil || strings.TrimSpace(branch) == "" {
		res.Reason = "branch rules unavailable: no client or branch"
		return res
	}
	if configKnown {
		for name := range configSet {
			res.Required[name] = true
		}
	}

	var problems []string

	rsc, _, err := client.Repositories.GetRequiredStatusChecks(ctx, owner, repo, branch)
	switch {
	case err == nil && rsc != nil:
		if rsc.Contexts != nil {
			for _, name := range *rsc.Contexts {
				res.Required[name] = true
			}
		}
		if rsc.Checks != nil {
			for _, check := range *rsc.Checks {
				if check != nil {
					res.Required[check.Context] = true
				}
			}
		}
	case err == nil, errors.Is(err, gh.ErrBranchNotProtected):
	default:
		problems = append(problems, fmt.Sprintf("classic branch protection unreadable: %v", err))
	}

	rules, _, err := client.Repositories.GetRulesForBranch(ctx, owner, repo, branch, nil)
	if err != nil {
		problems = append(problems, fmt.Sprintf("branch rulesets unreadable: %v", err))
	} else if rules != nil {
		res.MergeQueueKnown = true
		res.MergeQueue = len(rules.MergeQueue) > 0
		for _, rule := range rules.RequiredStatusChecks {
			if rule == nil {
				continue
			}
			if res.UpToDate != UpToDateServerEnforced {
				res.UpToDate = UpToDateHiveChecked
			}
			if rule.Parameters.StrictRequiredStatusChecksPolicy {
				res.UpToDate = UpToDateServerEnforced
			}
			for _, check := range rule.Parameters.RequiredStatusChecks {
				if check != nil {
					res.Required[check.Context] = true
				}
			}
		}
	} else {
		res.MergeQueueKnown = true
	}

	switch {
	case len(problems) > 0:
		res.Reason = strings.Join(problems, "; ")
	case len(res.Required) == 0:
		res.Reason = "no required checks found in config, branch protection or rulesets"
	default:
		res.Known = true
	}
	return res
}

// CheckOutcome is how a required check's result counts toward merging.
type CheckOutcome string

const (
	CheckPassed CheckOutcome = "passed"
	CheckFailed CheckOutcome = "failed"
	CheckWait   CheckOutcome = "wait"
)

// ClassifyRequiredCheck classifies a check run like GitHub does: success,
// skipped and neutral pass; failure, cancelled, timed_out and action_required
// fail; everything else (missing, queued, in_progress, unrecognised) waits and
// never passes.
func ClassifyRequiredCheck(status, conclusion string) CheckOutcome {
	if !strings.EqualFold(status, "completed") {
		return CheckWait
	}
	switch strings.ToLower(conclusion) {
	case "success", "skipped", "neutral":
		return CheckPassed
	case "failure", "cancelled", "timed_out", "action_required":
		return CheckFailed
	}
	return CheckWait
}

// SortedRequired returns the required check names in stable order.
func (r BranchRulesResult) SortedRequired() []string {
	names := make([]string, 0, len(r.Required))
	for name := range r.Required {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
