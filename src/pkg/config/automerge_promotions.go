package config

import (
	"fmt"
	"strings"
)

// PromotionRule is one auto_merge.promotions entry: a pull request into
// Target, opened by one of Authors from a head ref starting with
// HeadRefPrefix, that promotes Source. The rule only names which pull
// requests to check. What makes one eligible is content equality, proven
// live against GitHub every time (github.VerifyPromotion and
// VerifyPromotionMerge): Source takes changes only through a pull request,
// the head commit's tree equals the current tip tree of Source, the head is
// the Source tip (or an ancestor of it) or a single commit on the live
// Target tip, and GitHub's test merge onto Target carries that same tree.
// Trust carries over: Target inherits exactly Source's review guarantee.
//
// Neither the author nor a label is ever a trust signal on its own. An
// author listed here is not trusted anywhere else, and a pull request from
// it that does not pass the content check takes the path it takes today.
type PromotionRule struct {
	// Source is the branch whose content is promoted, for example main.
	Source string `yaml:"source" json:"source"`
	// Target is the pull request's base branch, for example stable.
	Target string `yaml:"target" json:"target"`
	// Authors are the exact logins (case-insensitive, as GitHub treats
	// logins) that open the promotion pull request, for example
	// github-actions[bot]. At least one is required.
	Authors []string `yaml:"authors" json:"authors"`
	// HeadRefPrefix is the start of the promotion branch's name, for
	// example auto/promote-.
	HeadRefPrefix string `yaml:"head_ref_prefix" json:"head_ref_prefix"`
	// InheritHumanMergePaths lets a promotion the lane owns merge although
	// it changes a file on the repository's auto_merge.human_merge_paths
	// list. Off (the default) keeps that list binding: such a promotion is
	// left for a person. On, the list is not asked on the way into Target.
	// The hive proves only that the promotion's tree is Source's current
	// tree; it does not check how or when a listed file's content reached
	// Source, so the option is not a record that a person merged it there
	// (the path may have been listed later, or the file landed by a merge
	// the list does not bind, a human_merge_pin_only exception or a bypass
	// actor; src/docs/hive-merge.md lists the cases). It lifts only the
	// hive's own human-merge-path stop, and only for a pull request that
	// passes both live tree checks; a person's hold or needs-owner-merge
	// label and every other gate still apply.
	InheritHumanMergePaths bool `yaml:"inherit_human_merge_paths,omitempty" json:"inherit_human_merge_paths,omitempty"`
}

// Normalized returns r with every field trimmed and empty authors dropped.
func (r PromotionRule) Normalized() PromotionRule {
	out := PromotionRule{
		Source:                 strings.TrimSpace(r.Source),
		Target:                 strings.TrimSpace(r.Target),
		HeadRefPrefix:          strings.TrimSpace(r.HeadRefPrefix),
		InheritHumanMergePaths: r.InheritHumanMergePaths,
	}
	for _, a := range r.Authors {
		if a = strings.TrimSpace(a); a != "" {
			out.Authors = append(out.Authors, a)
		}
	}
	return out
}

// HasAuthor reports whether login is one of the rule's authors, compared as
// a whole login (case-insensitive), never as a pattern.
func (r PromotionRule) HasAuthor(login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	for _, a := range r.Authors {
		if strings.EqualFold(strings.TrimSpace(a), login) {
			return true
		}
	}
	return false
}

func (r PromotionRule) validate() error {
	n := r.Normalized()
	switch {
	case n.Source == "":
		return fmt.Errorf("source is required")
	case n.Target == "":
		return fmt.Errorf("target is required")
	case n.Source == n.Target: // git refs are case-sensitive
		return fmt.Errorf("source and target must be different branches (both %q)", n.Source)
	case n.HeadRefPrefix == "":
		return fmt.Errorf("head_ref_prefix is required")
	case len(n.Authors) == 0:
		return fmt.Errorf("at least one author is required")
	}
	for _, a := range r.Authors {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("authors contains an empty login")
		}
		if strings.ContainsAny(strings.TrimSpace(a), " \t*?") {
			return fmt.Errorf("author %q is not a login (patterns are not accepted)", a)
		}
	}
	for _, branch := range []string{n.Source, n.Target} {
		if strings.ContainsAny(branch, " \t*?") {
			return fmt.Errorf("%q is not a branch name", branch)
		}
	}
	return nil
}

// ValidatePromotionRules checks one repository's auto_merge.promotions
// entry: every rule complete, source and target different, at least one
// author, and no target named twice (a pull request into a target must
// match at most one rule). repo only words the error.
func ValidatePromotionRules(repo string, rules []PromotionRule) error {
	if len(rules) == 0 {
		return fmt.Errorf("auto_merge.promotions[%s]: at least one rule is required (remove the repository to turn promotion off)", repo)
	}
	targets := map[string]bool{}
	for i, rule := range rules {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("auto_merge.promotions[%s][%d]: %w", repo, i, err)
		}
		target := rule.Normalized().Target
		if targets[target] {
			return fmt.Errorf("auto_merge.promotions[%s]: target %q is named by more than one rule", repo, rule.Normalized().Target)
		}
		targets[target] = true
	}
	return nil
}

// validateAutoMergePromotions refuses an auto_merge.promotions key that is
// not owner/repo or appears twice in different case, and any invalid rule.
// A key naming a repository outside project.repos is not refused here: a
// repository can leave project.repos (a hub heartbeat, Settings) without
// this map being edited, and a config that then fails to load would take
// the whole hive down. Config.PromotionRulesFor ignores such a key at run
// time; the dashboard and admin MCP refuse to write one.
func (c *Config) validateAutoMergePromotions() error {
	seen := map[string]bool{}
	for repo, rules := range c.AutoMerge.Promotions {
		key := strings.ToLower(strings.TrimSpace(repo))
		owner, name, ok := strings.Cut(key, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(key, " \t*") || seen[key] {
			return fmt.Errorf("auto_merge.promotions: invalid or duplicate repository %q (want owner/repo)", repo)
		}
		seen[key] = true
		if err := ValidatePromotionRules(repo, rules); err != nil {
			return err
		}
	}
	return nil
}

// PromotionRulesFor is AutoMergeConfig.PromotionRulesFor for one of the
// hive's own repositories (IsManagedRepo): a key left behind for a
// repository no longer in project.repos never applies.
func (c *Config) PromotionRulesFor(repo string) []PromotionRule {
	if c == nil || !c.IsManagedRepo(repo) {
		return nil
	}
	return c.AutoMerge.PromotionRulesFor(repo)
}

// PromotionRulesFor returns repo's (owner/repo, case-insensitive) normalized
// promotion rules, or nil when the repository has none. Ambiguous keys and
// an invalid entry fail closed to nil even before config validation, so a
// bad entry never widens anything.
func (a AutoMergeConfig) PromotionRulesFor(repo string) []PromotionRule {
	repo = strings.TrimSpace(repo)
	if repo == "" || len(a.Promotions) == 0 {
		return nil
	}
	var found []PromotionRule
	matches := 0
	for key, rules := range a.Promotions {
		if strings.EqualFold(strings.TrimSpace(key), repo) {
			matches++
			found = rules
		}
	}
	if matches != 1 || ValidatePromotionRules(repo, found) != nil {
		return nil
	}
	out := make([]PromotionRule, 0, len(found))
	for _, rule := range found {
		out = append(out, rule.Normalized())
	}
	return out
}
