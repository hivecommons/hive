package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

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
	// lane must not merge and Reason says why. When Known is true, Reason may
	// still carry an informational note (classic protection unreadable).
	Known  bool
	Reason string
	// UpToDate is how the up-to-date requirement is enforced.
	UpToDate UpToDateEnforcement
	// MergeQueue is true when a merge_queue rule applies to the branch.
	// MergeQueueKnown is false when the rules endpoint could not be read.
	MergeQueue      bool
	MergeQueueKnown bool
	// RulesetsRead is true when the rules endpoint answered without error, so
	// an unknown UpToDate means no ruleset speaks to it rather than a failed read.
	RulesetsRead bool
}

// ReadBranchRules reads every required-check source for branch: the operator
// config list, classic branch protection and rulesets
// (GET /repos/{o}/{r}/rules/branches/{branch}, Metadata read only). A classic
// "branch not protected" is not read as "nothing required". Native merge-queue
// detection uses the merge_queue rule only; the GraphQL mergeQueue field was
// not verified for App installation tokens and is not used.
//
// Classic protection needs Administration permission the App usually lacks.
// A 403 from it does not discard the set when the rulesets endpoint was read
// and lists required checks: GitHub still enforces any classic rule at merge
// time, so the lane's check stays additive. Other errors fail closed. A 403
// or 404 from classic protection is remembered per repo and branch for
// HIVE_REQUIRED_CHECKS_FORBIDDEN_TTL and replayed without a call.
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
	var classicErr error

	// A remembered 403/404 stands in for the call (branchRulesClassicCache).
	var rsc *gh.RequiredStatusChecks
	classicKey := requiredChecksForbiddenCacheKey(client, owner, repo, branch)
	err := sharedBranchRulesClassicCache.get(classicKey)
	if err == nil {
		rsc, _, err = client.Repositories.GetRequiredStatusChecks(ctx, owner, repo, branch)
		if isRequiredChecksForbiddenCacheable(err) {
			sharedBranchRulesClassicCache.put(classicKey, err)
		}
	}
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
	case isBranchRulesForbidden(err):
		classicErr = err
	default:
		problems = append(problems, fmt.Sprintf("classic branch protection unreadable: %v", err))
	}

	rulesetChecks := 0
	rules, _, err := client.Repositories.GetRulesForBranch(ctx, owner, repo, branch, nil)
	if err != nil {
		problems = append(problems, fmt.Sprintf("branch rulesets unreadable: %v", err))
	} else if rules != nil {
		res.RulesetsRead = true
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
					rulesetChecks++
				}
			}
		}
	} else {
		res.RulesetsRead = true
		res.MergeQueueKnown = true
	}

	var note string
	if classicErr != nil {
		if err == nil && rulesetChecks > 0 {
			note = fmt.Sprintf("classic branch protection unreadable (%v); using ruleset required checks, GitHub enforces classic rules at merge time", classicErr)
		} else {
			problems = append([]string{fmt.Sprintf("classic branch protection unreadable: %v", classicErr)}, problems...)
		}
	}

	switch {
	case len(problems) > 0:
		res.Reason = strings.Join(problems, "; ")
	case len(res.Required) == 0:
		res.Reason = "no required checks found in config, branch protection or rulesets"
	default:
		res.Known = true
		res.Reason = note
	}
	return res
}

// branchRulesClassicCache remembers a 403 or 404 from the classic
// required_status_checks endpoint per client, repo and branch, so
// ReadBranchRules replays the same error instead of asking again. An App
// without Administration permission gets a 403 there on every read, and the
// red-main sweep reads every active repo each pass (~330 failed calls an hour
// on one hive). "Branch not protected" and rate-limit errors are not
// *gh.ErrorResponse 403/404 values, so they are never cached. The TTL is
// HIVE_REQUIRED_CHECKS_FORBIDDEN_TTL (default 1h), shared with
// RequiredStatusCheckPolicy's cache.
type branchRulesClassicCache struct {
	mu      sync.Mutex
	entries map[string]branchRulesClassicEntry
	now     func() time.Time
	ttl     time.Duration
}

type branchRulesClassicEntry struct {
	until time.Time
	err   error
}

var sharedBranchRulesClassicCache = newBranchRulesClassicCache()

func newBranchRulesClassicCache() *branchRulesClassicCache {
	return &branchRulesClassicCache{
		entries: map[string]branchRulesClassicEntry{},
		now:     time.Now,
		ttl:     durationFromEnv(requiredChecksForbiddenTTLEnv, defaultRequiredChecksForbiddenTTL),
	}
}

// get returns the remembered error for key, nil when there is none or it
// has expired.
func (c *branchRulesClassicCache) get(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[key]
	if !ok {
		return nil
	}
	if c.now().After(ent.until) {
		delete(c.entries, key)
		return nil
	}
	return ent.err
}

func (c *branchRulesClassicCache) put(key string, err error) {
	if c.ttl <= 0 || err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, ent := range c.entries {
		if now.After(ent.until) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = branchRulesClassicEntry{until: now.Add(c.ttl), err: err}
}

// isBranchRulesForbidden reports a 403 from GitHub, which an App without
// Administration permission gets from the classic protection endpoint
// ("Resource not accessible by integration").
func isBranchRulesForbidden(err error) bool {
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden
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
