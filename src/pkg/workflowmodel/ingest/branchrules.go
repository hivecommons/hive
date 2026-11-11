package ingest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// BranchRulesPath is the synthetic tree entry a repository fetcher may
// add to carry the default branch's protection or ruleset settings (JSON
// of BranchRules) through a files map, so callers that only pass a tree
// (POST /api/workflows/ingest, the survey) still bind them. It is not a
// file GitHub serves; ReadTree never produces it.
const BranchRulesPath = ".hive/branch-rules.json"

// Branch rule sources: which API answered, or the settings file read.
const (
	BranchRulesProtection = "protection"
	BranchRulesRulesets   = "rulesets"
	BranchRulesSettings   = "settings"
)

// BranchRules are the default branch's merge requirements as the forge
// enforces them (GET /repos/{o}/{r}/branches/{b}/protection, falling back
// to GET /repos/{o}/{r}/rules/branches/{b}) or as a probot settings.yml
// declares them.
type BranchRules struct {
	Branch string `json:"branch"`
	// Source is BranchRulesProtection, BranchRulesRulesets or
	// BranchRulesSettings.
	Source string `json:"source"`
	// Endpoint is the API path (or file path) the rules were read from,
	// used as the citation.
	Endpoint               string   `json:"endpoint"`
	RequiredChecks         []string `json:"requiredChecks,omitempty"`
	RequiredApprovals      int      `json:"requiredApprovals,omitempty"`
	RequireCodeOwnerReview bool     `json:"requireCodeOwnerReview,omitempty"`
	DismissStaleReviews    bool     `json:"dismissStaleReviews,omitempty"`
	EnforceAdmins          bool     `json:"enforceAdmins,omitempty"`
	LinearHistory          bool     `json:"linearHistory,omitempty"`
	SignedCommits          bool     `json:"signedCommits,omitempty"`
}

// Empty reports whether the rules require nothing the model expresses.
func (r *BranchRules) Empty() bool {
	return r == nil || (len(r.RequiredChecks) == 0 && r.RequiredApprovals == 0 && !r.RequireCodeOwnerReview && !r.DismissStaleReviews && !r.EnforceAdmins && !r.LinearHistory && !r.SignedCommits)
}

// BranchRulesFromFiles returns the rules carried at BranchRulesPath, or
// nil when absent or unreadable.
func BranchRulesFromFiles(files map[string][]byte) *BranchRules {
	data, ok := files[BranchRulesPath]
	if !ok {
		return nil
	}
	var r BranchRules
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	return &r
}

// branchRulesFromSettings reads the first default-branch protection
// block from a probot settings.yml (branches[].protection) when the API
// did not answer.
func branchRulesFromSettings(p string, data []byte) *BranchRules {
	var s struct {
		Repository struct {
			DefaultBranch string `yaml:"default_branch"`
		} `yaml:"repository"`
		Branches []struct {
			Name       string `yaml:"name"`
			Protection *struct {
				RequiredStatusChecks *struct {
					Contexts []string `yaml:"contexts"`
				} `yaml:"required_status_checks"`
				RequiredPullRequestReviews *struct {
					RequiredApprovingReviewCount int  `yaml:"required_approving_review_count"`
					RequireCodeOwnerReviews      bool `yaml:"require_code_owner_reviews"`
					DismissStaleReviews          bool `yaml:"dismiss_stale_reviews"`
				} `yaml:"required_pull_request_reviews"`
				EnforceAdmins         bool `yaml:"enforce_admins"`
				RequiredLinearHistory bool `yaml:"required_linear_history"`
			} `yaml:"protection"`
		} `yaml:"branches"`
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil
	}
	def := s.Repository.DefaultBranch
	for _, b := range s.Branches {
		if b.Protection == nil || (def != "" && b.Name != def && b.Name != "default") {
			continue
		}
		r := &BranchRules{Branch: b.Name, Source: BranchRulesSettings, Endpoint: fmt.Sprintf("%s:%d", p, keyLine(data, "branches")), EnforceAdmins: b.Protection.EnforceAdmins, LinearHistory: b.Protection.RequiredLinearHistory}
		if c := b.Protection.RequiredStatusChecks; c != nil {
			r.RequiredChecks = append(r.RequiredChecks, c.Contexts...)
		}
		if pr := b.Protection.RequiredPullRequestReviews; pr != nil {
			r.RequiredApprovals = pr.RequiredApprovingReviewCount
			r.RequireCodeOwnerReview = pr.RequireCodeOwnerReviews
			r.DismissStaleReviews = pr.DismissStaleReviews
		}
		if r.Empty() {
			return nil
		}
		return r
	}
	return nil
}

// applyBranchRules binds the rules to the Review / Merge edges: required
// status checks become required CI checks and a required-checks guard,
// required approvals set the review gate's minimum, code-owner review
// adds its own gate, and the merge-shape rules (admins, linear history,
// signatures, stale dismissal) are bookkeeping notes on merge.
func applyBranchRules(m *workflowmodel.Model, r *BranchRules, roles map[string]bool) {
	if r.Empty() {
		return
	}
	c := func(text string) []workflowmodel.Citation {
		if r.Source == BranchRulesSettings {
			return []workflowmodel.Citation{{Path: r.Endpoint, Text: text}}
		}
		return []workflowmodel.Citation{citeAPI(r.Endpoint, text)}
	}
	source := r.Endpoint
	if r.Source != BranchRulesSettings {
		source = "api:" + r.Endpoint
	}
	if len(r.RequiredChecks) > 0 {
		checks := append([]string(nil), r.RequiredChecks...)
		sort.Strings(checks)
		for _, ctx := range checks {
			m.Spec.Checks = append(m.Spec.Checks, workflowmodel.Check{ID: "check-" + truncate(workflowmodel.Slug(ctx), maxCheckSlug), Name: ctx, Kind: workflowmodel.CheckCI, Required: true, Source: source})
		}
		m.Spec.Guards = append(m.Spec.Guards, workflowmodel.Guard{ID: GuardRequiredChecks, Kind: workflowmodel.GuardRequiredChecks, Params: map[string]string{"contexts": strings.Join(checks, ",")}, Source: source})
		for _, i := range mergeTransitions(m) {
			if m.Spec.Transitions[i].GuardRef == "" {
				m.Spec.Transitions[i].GuardRef = GuardRequiredChecks
			}
		}
	}
	if r.RequiredApprovals > 0 {
		g := ensureReviewGate(m, releaser(roles))
		g.MinApprovals = r.RequiredApprovals
		g.Citations = append(g.Citations, c(fmt.Sprintf("required_approving_review_count: %d", r.RequiredApprovals))...)
	}
	if r.RequireCodeOwnerReview {
		m.Spec.Gates = append(m.Spec.Gates, workflowmodel.Gate{
			ID: GateCodeownerReview, ReleasedBy: releaser(roles),
			Conditions: []workflowmodel.Predicate{{Fact: "pr.review", Op: workflowmodel.OpIs, Value: "approved"}},
			Confidence: workflowmodel.ConfidenceInferred, Citations: c("require_code_owner_reviews: true"),
		})
	}
	note := func(action, text string) {
		m.Spec.Bookkeeping = append(m.Spec.Bookkeeping, workflowmodel.Effect{On: hivedefault.EventMerged, Action: action, Params: map[string]string{"note": text, "source": source}})
	}
	if r.EnforceAdmins {
		note("enforce-admins", "branch rules apply to administrators")
	}
	if r.LinearHistory {
		note("require-linear-history", "merge commits are refused; squash or rebase")
	}
	if r.SignedCommits {
		note("require-signed-commits", "every commit must carry a verified signature")
	}
	if r.DismissStaleReviews {
		note("dismiss-stale-reviews", "a new push dismisses earlier approvals")
	}
}
