package adminmcp

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// auto_merge.update and auto_merge.promotions: the branch-promotion rules
// (config.PromotionRule) set one repository at a time. A rule lets the
// self-authored sweep's promotion lane merge a pull request from source to
// target, opened by a listed author from a head ref with the prefix, whose
// head tree is the source branch's current tree, with no person, no linked
// issue and no author tier. So every rule a change adds, and every author or
// looser prefix it gives an existing rule, is disclosed as widening.

// autoMergePromotionEntry is one rule as the endpoint reports and takes it.
// The fields are in the order encoding/json sorts map keys, so a value read
// back from the hive and one built here encode the same.
type autoMergePromotionEntry struct {
	Authors       []string `json:"authors"`
	HeadRefPrefix string   `json:"head_ref_prefix"`
	Source        string   `json:"source"`
	Target        string   `json:"target"`
}

func (e autoMergePromotionEntry) rule() config.PromotionRule {
	return config.PromotionRule{Source: e.Source, Target: e.Target, Authors: e.Authors, HeadRefPrefix: e.HeadRefPrefix}
}

// describe names the rule for a preview: "main to stable, opened by
// github-actions[bot] from auto/promote-*".
func (e autoMergePromotionEntry) describe() string {
	return e.Source + " to " + e.Target + ", opened by " + strings.Join(e.Authors, ", ") + " from " + e.HeadRefPrefix + "*"
}

// autoMergePromotionsValue reads promotions as the hive reported it.
func autoMergePromotionsValue(v any) map[string][]autoMergePromotionEntry {
	out := map[string][]autoMergePromotionEntry{}
	switch m := v.(type) {
	case map[string]any:
		for repo, raw := range m {
			list, _ := raw.([]any)
			rules := []autoMergePromotionEntry{}
			for _, item := range list {
				e, _ := item.(map[string]any)
				source, _ := e["source"].(string)
				target, _ := e["target"].(string)
				prefix, _ := e["head_ref_prefix"].(string)
				rules = append(rules, autoMergePromotionEntry{
					Authors:       nilAsEmpty(stringListValueLoose(e["authors"])),
					HeadRefPrefix: prefix,
					Source:        source,
					Target:        target,
				})
			}
			out[repo] = rules
		}
	case map[string][]autoMergePromotionEntry:
		for repo, rules := range m {
			out[repo] = rules
		}
	}
	return out
}

func autoMergePromotionKeys(a, b map[string][]autoMergePromotionEntry) []string {
	keys := map[string]any{}
	for k := range a {
		keys[k] = nil
	}
	for k := range b {
		keys[k] = nil
	}
	return SortKeys(keys)
}

// autoMergeApplyPromotions sets or unsets the named repositories' whole rule
// list and keeps every other repository's rules. null or [] removes the
// repository. Each repository's rules must pass the validation hive.yaml
// loads under (config.ValidatePromotionRules).
func autoMergeApplyPromotions(before, raw any, projectRepos []string) (map[string][]autoMergePromotionEntry, error) {
	const shape = `promotions takes {"owner/repo": [{"source": "main", "target": "stable", "authors": [...], "head_ref_prefix": "auto/promote-"}]}; null or [] removes that repository`
	change, ok := raw.(map[string]any)
	if !ok || len(change) == 0 {
		return nil, fmt.Errorf("%s", shape)
	}
	out := autoMergePromotionsValue(before)
	touched := map[string]bool{}
	for _, key := range SortKeys(change) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), projectRepos)
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " \t") {
			return nil, fmt.Errorf("promotions: %q is not an owner/repo name", key)
		}
		if touched[strings.ToLower(repo)] {
			return nil, fmt.Errorf("promotions names %s twice", repo)
		}
		touched[strings.ToLower(repo)] = true
		existing := ""
		listed := false
		for _, r := range projectRepos {
			if strings.EqualFold(strings.TrimSpace(r), repo) {
				listed = true
			}
		}
		for k := range out {
			if strings.EqualFold(k, repo) {
				existing = k
			}
		}
		list, isList := change[key].([]any)
		if change[key] == nil || (isList && len(list) == 0) {
			if existing == "" {
				return nil, fmt.Errorf("promotions has no entry for %s to remove", repo)
			}
			delete(out, existing)
			continue
		}
		if !isList {
			return nil, fmt.Errorf("promotions[%s] must be a list of rules, or null", repo)
		}
		if !listed {
			// Removing a stale entry is fine; adding a rule needs one of the
			// hive's repositories, as the endpoint requires.
			return nil, fmt.Errorf("promotions: %s is not one of this hive's repositories (project.repos)", repo)
		}
		rules := make([]autoMergePromotionEntry, 0, len(list))
		checked := make([]config.PromotionRule, 0, len(list))
		for i, item := range list {
			entry, err := autoMergePromotionEntryArg(repo, i, item)
			if err != nil {
				return nil, err
			}
			rules = append(rules, entry)
			checked = append(checked, entry.rule())
		}
		if err := config.ValidatePromotionRules(repo, checked); err != nil {
			return nil, err
		}
		if existing != "" {
			repo = existing
		}
		out[repo] = rules
	}
	return out, nil
}

// autoMergePromotionEntryArg reads one rule from the arguments.
func autoMergePromotionEntryArg(repo string, i int, raw any) (autoMergePromotionEntry, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d] must be an object with source, target, authors and head_ref_prefix", repo, i)
	}
	for k := range obj {
		switch k {
		case "source", "target", "authors", "head_ref_prefix":
		default:
			return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d] takes source, target, authors and head_ref_prefix, not %q", repo, i, k)
		}
	}
	entry := autoMergePromotionEntry{Authors: []string{}}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"source", &entry.Source}, {"target", &entry.Target}, {"head_ref_prefix", &entry.HeadRefPrefix}} {
		v, ok := obj[f.name].(string)
		if !ok || strings.TrimSpace(v) == "" {
			return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d].%s must be a non-empty string", repo, i, f.name)
		}
		*f.dst = strings.TrimSpace(v)
	}
	authors, err := stringListValue(obj["authors"])
	if err != nil {
		return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d].authors must be a list of logins", repo, i)
	}
	for _, a := range authors {
		a = strings.TrimSpace(a)
		if a == "" {
			return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d].authors entries must be non-empty", repo, i)
		}
		if !slices.Contains(entry.Authors, a) {
			entry.Authors = append(entry.Authors, a)
		}
	}
	return entry, nil
}

// autoMergePromotionRows splits a promotions change into one row per
// repository whose rules change.
func autoMergePromotionRows(c amPlanned) []amRow {
	before := autoMergePromotionsValue(c.before)
	after, _ := c.after.(map[string][]autoMergePromotionEntry)
	var rows []amRow
	for _, repo := range autoMergePromotionKeys(before, after) {
		b, hadB := before[repo]
		a, hasA := after[repo]
		if hadB == hasA && compactJSON(b) == compactJSON(a) {
			continue
		}
		row := amRow{field: c.field.key + "[" + repo + "]"}
		if hadB {
			row.before = b
		}
		if hasA {
			row.after = a
		}
		rows = append(rows, row)
	}
	return rows
}

// promotionCovered reports whether rule was already allowed by one of was:
// the same source and target (git refs, compared exactly), a head ref prefix
// at least as narrow, and no author the earlier rule did not have (logins,
// compared case-insensitively as GitHub does).
func promotionCovered(rule autoMergePromotionEntry, was []autoMergePromotionEntry) bool {
	for _, b := range was {
		if b.Source != rule.Source || b.Target != rule.Target || !strings.HasPrefix(rule.HeadRefPrefix, b.HeadRefPrefix) {
			continue
		}
		covered := true
		for _, a := range rule.Authors {
			if !slices.ContainsFunc(b.Authors, func(x string) bool { return strings.EqualFold(x, a) }) {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

// autoMergePromotionsWidening names every rule the change adds or loosens:
// each one lets the promotion lane merge pull requests with no person.
// Removing a rule, an author or narrowing a prefix does not widen.
func autoMergePromotionsWidening(before, after any) []string {
	was := autoMergePromotionsValue(before)
	now := autoMergePromotionsValue(after)
	var notes []string
	for _, repo := range autoMergePromotionKeys(now, nil) {
		var prior []autoMergePromotionEntry
		for k, rules := range was {
			if strings.EqualFold(k, repo) {
				prior = rules
			}
		}
		for _, rule := range now[repo] {
			if promotionCovered(rule, prior) {
				continue
			}
			notes = append(notes, "promotions["+repo+"] adds a trust path, "+rule.describe()+": such a pull request whose head tree and test merge equal the current tree of "+rule.Source+" (the head being "+rule.Source+"'s tip or an ancestor of it, or one commit on the live "+rule.Target+" tip), where "+rule.Source+" takes changes only through a pull request, merges into "+rule.Target+" with no person, no linked issue and no author tier, and the sentinel does not flag sensitive_path on it. Trust carries over: "+rule.Target+" inherits exactly "+rule.Source+"'s review guarantee, a pull request required, with the approvals and bypass actors its owner configured (required checks, holds, needs-owner-merge, human_merge_paths, the review verdict hold, the intent gate and squash_only still apply; the hive never updates the promotion branch)")
		}
	}
	return notes
}

// autoMergePromotionsSchema is the operation's schema for promotions.
func autoMergePromotionsSchema() map[string]any {
	rule := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source":          map[string]any{"type": "string", "description": "The branch whose content is promoted, for example main."},
			"target":          map[string]any{"type": "string", "description": "The pull request's base branch, for example stable. Different from source."},
			"authors":         map[string]any{"type": "array", "items": stringSchema(), "description": "Exact logins that open the promotion pull request, for example github-actions[bot]. At least one."},
			"head_ref_prefix": map[string]any{"type": "string", "description": "The start of the promotion branch's name, for example auto/promote-."},
		},
		"required":             []string{"source", "target", "authors", "head_ref_prefix"},
		"additionalProperties": false,
	}
	list := map[string]any{"type": "array", "items": rule}
	return map[string]any{"type": "object", "description": "owner/repo to that repository's whole list of branch-promotion rules; null or [] removes the repository. Repositories you do not name are kept. A rule lets a pull request from source to target, opened by a listed author from a head ref with the prefix, merge with no person when the source branch takes changes only through a pull request and the head (the source tip or an ancestor of it, or one commit on the live target tip) and its test merge carry the source branch's current tree. Repositories must be in project.repos.", "additionalProperties": map[string]any{"anyOf": []any{list, map[string]any{"type": "null"}}}}
}
