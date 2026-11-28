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
// looser prefix it gives an existing rule, is disclosed as widening. So is
// turning a rule's inherit_human_merge_paths on: files on the repository's
// human-merge list then reach the target with no person. Turning it off
// narrows and is not disclosed.

// autoMergePromotionEntry is one rule as the endpoint reports and takes it.
// The fields are in the order encoding/json sorts map keys, so a value read
// back from the hive and one built here encode the same.
type autoMergePromotionEntry struct {
	Authors       []string `json:"authors"`
	HeadRefPrefix string   `json:"head_ref_prefix"`
	// InheritHumanMergePaths is omitted when off, as the endpoint omits it,
	// so a rule that never set it encodes as it always did.
	InheritHumanMergePaths bool   `json:"inherit_human_merge_paths,omitempty"`
	Source                 string `json:"source"`
	Target                 string `json:"target"`
}

func (e autoMergePromotionEntry) rule() config.PromotionRule {
	return config.PromotionRule{Source: e.Source, Target: e.Target, Authors: e.Authors, HeadRefPrefix: e.HeadRefPrefix, InheritHumanMergePaths: e.InheritHumanMergePaths}
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
				inherit, _ := e["inherit_human_merge_paths"].(bool)
				rules = append(rules, autoMergePromotionEntry{
					Authors:                nilAsEmpty(stringListValueLoose(e["authors"])),
					HeadRefPrefix:          prefix,
					InheritHumanMergePaths: inherit,
					Source:                 source,
					Target:                 target,
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

// autoMergePromotionsArgRules reads the rules a promotions argument named,
// per repository key as the caller wrote it. An entry the preview would
// have refused is skipped: this runs after a write the preview accepted.
func autoMergePromotionsArgRules(raw any) map[string][]autoMergePromotionEntry {
	out := map[string][]autoMergePromotionEntry{}
	change, _ := raw.(map[string]any)
	for key, v := range change {
		list, _ := v.([]any)
		for i, item := range list {
			if entry, err := autoMergePromotionEntryArg(key, i, item); err == nil {
				out[key] = append(out[key], entry)
			}
		}
	}
	return out
}

// autoMergePromotionsArgInherits reports whether a promotions argument
// names a rule with inherit_human_merge_paths on.
func autoMergePromotionsArgInherits(raw any) bool {
	for _, list := range autoMergePromotionsArgRules(raw) {
		for _, rule := range list {
			if rule.InheritHumanMergePaths {
				return true
			}
		}
	}
	return false
}

// autoMergePromotionsInheritWarning is the read-back warning for a rule the
// hive stored without the option the request set.
const autoMergePromotionsInheritWarning = "the hive stored the rule without inherit_human_merge_paths; it may run an older build; the option is off"

// autoMergePromotionsInheritResult is write_confirm's "after" for a request
// that turned inherit_human_merge_paths on: what the hive, read back as
// current, now holds for the repositories the request named, and whether
// every rule sent with the option came back with it (same repository,
// source and target; a target is named by one rule). A hive on an older
// build accepts the rule and drops the key, so the rule comes back without
// it: inherit_human_merge_paths_stored is then false and warning says so,
// naming each such rule.
func autoMergePromotionsInheritResult(raw any, current map[string]any) map[string]any {
	projectRepos := stringListValueLoose(current["project_repos"])
	stored := autoMergePromotionsValue(current["promotions"])
	storedFor := func(repo string) []autoMergePromotionEntry {
		for storedRepo, list := range stored {
			if strings.EqualFold(storedRepo, repo) {
				return list
			}
		}
		return []autoMergePromotionEntry{}
	}
	named := autoMergePromotionsArgRules(raw)
	holds := map[string]any{}
	var missing []string
	for _, key := range autoMergePromotionKeys(named, nil) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), projectRepos)
		got := storedFor(repo)
		holds[repo] = got
		for _, rule := range named[key] {
			if !rule.InheritHumanMergePaths {
				continue
			}
			found := false
			for _, g := range got {
				if g.Source == rule.Source && g.Target == rule.Target && g.InheritHumanMergePaths {
					found = true
				}
			}
			if !found {
				missing = append(missing, "promotions["+repo+"] "+rule.Source+" to "+rule.Target)
			}
		}
	}
	out := map[string]any{"promotions": holds, "inherit_human_merge_paths_stored": len(missing) == 0}
	if len(missing) > 0 {
		out["warning"] = autoMergePromotionsInheritWarning + ": " + strings.Join(missing, "; ") + ". With the option off, human_merge_paths still leaves that rule's promotions for a person. Upgrade the hive, then send the change again"
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
	const shape = `promotions takes {"owner/repo": [{"source": "main", "target": "stable", "authors": [...], "head_ref_prefix": "auto/promote-", "inherit_human_merge_paths": false}]}; inherit_human_merge_paths is optional (default false); null or [] removes that repository`
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
		case "source", "target", "authors", "head_ref_prefix", "inherit_human_merge_paths":
		default:
			return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d] takes source, target, authors, head_ref_prefix and inherit_human_merge_paths, not %q", repo, i, k)
		}
	}
	entry := autoMergePromotionEntry{Authors: []string{}}
	if raw, set := obj["inherit_human_merge_paths"]; set && raw != nil {
		// A rule is sent whole, so leaving the key out (or null) means off.
		inherit, ok := raw.(bool)
		if !ok {
			return autoMergePromotionEntry{}, fmt.Errorf("promotions[%s][%d].inherit_human_merge_paths must be true or false", repo, i)
		}
		entry.InheritHumanMergePaths = inherit
	}
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
// compared case-insensitively as GitHub does). It does not look at
// inherit_human_merge_paths; promotionInheritCovered does.
func promotionCovered(rule autoMergePromotionEntry, was []autoMergePromotionEntry) bool {
	return promotionCoveredBy(rule, was, false)
}

// promotionInheritCovered reports whether rule, with
// inherit_human_merge_paths on, was already allowed that by one of was: an
// earlier rule that covers it (promotionCovered) and itself had the option
// on.
func promotionInheritCovered(rule autoMergePromotionEntry, was []autoMergePromotionEntry) bool {
	return promotionCoveredBy(rule, was, true)
}

// promotionCoveredBy is promotionCovered, counting only earlier rules with
// inherit_human_merge_paths on when inheriting.
func promotionCoveredBy(rule autoMergePromotionEntry, was []autoMergePromotionEntry, inheriting bool) bool {
	for _, b := range was {
		if b.Source != rule.Source || b.Target != rule.Target || !strings.HasPrefix(rule.HeadRefPrefix, b.HeadRefPrefix) || (inheriting && !b.InheritHumanMergePaths) {
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

// promotionInheritDisclosure says in plain words what a rule's
// inherit_human_merge_paths lets through, what the hive proves for it and
// what it does not, the cases that follow from the gap, and what still
// stops such a promotion. It claims nothing the hive does not check: in
// particular not that a person merged the listed files into the source.
func promotionInheritDisclosure(rule autoMergePromotionEntry) string {
	src, dst := rule.Source, rule.Target
	return "files on the repository's human-merge list (auto_merge.human_merge_paths) reach " + dst + " with no person. " +
		"The hive proves only that the promotion's tree is " + src + "'s current tree; it does not check how or when a listed file's content reached " + src + ". " +
		"So a listed file can reach " + dst + " although no person merged that content under the list, when: " +
		"(1) the path was added to human_merge_paths after the content was already on " + src + ", " +
		"(2) something the list does not bind landed it on " + src + " (the list binds only the hive's own merges, not another bot's or GitHub's own auto-merge, nor a person with merge rights), " +
		"(3) a human_merge_pin_only exception or a bypass actor landed it on " + src + ", " +
		"(4) " + src + "'s own protection is weaker than " + dst + "'s, " +
		"(5) the promotion is a single commit on the " + dst + " tip and a person had changed the listed file only on " + dst + " (a hot-fix), which " + src + "'s version then replaces (a promotion whose head is the " + src + " tip is handed back to a person instead, because its test merge differs), or " +
		"(6) " + src + " is itself the target of a rule with the option on. " +
		"Workflows triggered by a push to " + dst + " run as soon as the hive merges, so the merge into " + src + " is the last look a person gets. " +
		"A hold or needs-owner-merge label a person adds, the sentinel label, the review verdict hold, the intent gate, required checks, squash_only and both live tree checks still apply, and a pull request that stops matching " + src + " is left for a person as before"
}

// autoMergePromotionsWidening names every rule the change adds or loosens:
// each one lets the promotion lane merge pull requests with no person.
// Removing a rule, an author or narrowing a prefix does not widen. A rule
// that has inherit_human_merge_paths on where no earlier rule covering it
// had is disclosed too, whether the rule is new or only the option is;
// turning the option off does not widen.
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
				if rule.InheritHumanMergePaths && !promotionInheritCovered(rule, prior) {
					notes = append(notes, "promotions["+repo+"] turns on inherit_human_merge_paths for "+rule.describe()+": "+promotionInheritDisclosure(rule))
				}
				continue
			}
			stillApply := "required checks, holds, needs-owner-merge, human_merge_paths, the review verdict hold, the intent gate and squash_only still apply; the hive never updates the promotion branch"
			inherit := ""
			if rule.InheritHumanMergePaths {
				stillApply = "required checks, holds, needs-owner-merge, the review verdict hold, the intent gate and squash_only still apply, human_merge_paths does not; the hive never updates the promotion branch"
				inherit = ". The rule has inherit_human_merge_paths on: " + promotionInheritDisclosure(rule)
			}
			notes = append(notes, "promotions["+repo+"] adds a trust path, "+rule.describe()+": such a pull request whose head tree and test merge equal the current tree of "+rule.Source+" (the head being "+rule.Source+"'s tip or an ancestor of it, or one commit on the live "+rule.Target+" tip), where "+rule.Source+" takes changes only through a pull request, merges into "+rule.Target+" with no person, no linked issue and no author tier, and the sentinel does not flag sensitive_path on it. Trust carries over: "+rule.Target+" inherits exactly "+rule.Source+"'s review guarantee, a pull request required, with the approvals and bypass actors its owner configured ("+stillApply+")"+inherit)
		}
	}
	return notes
}

// autoMergePromotionsSchema is the operation's schema for promotions.
func autoMergePromotionsSchema() map[string]any {
	rule := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source":                    map[string]any{"type": "string", "description": "The branch whose content is promoted, for example main."},
			"target":                    map[string]any{"type": "string", "description": "The pull request's base branch, for example stable. Different from source."},
			"authors":                   map[string]any{"type": "array", "items": stringSchema(), "description": "Exact logins that open the promotion pull request, for example github-actions[bot]. At least one."},
			"head_ref_prefix":           map[string]any{"type": "string", "description": "The start of the promotion branch's name, for example auto/promote-."},
			"inherit_human_merge_paths": map[string]any{"type": "boolean", "description": "Optional, default false. False keeps the repository's human_merge_paths binding on a promotion: one that changes a listed file is left for a person. True lets a promotion the lane owns merge although it changes a listed file. The hive proves only that the promotion's tree is the source branch's current tree; it does not check how or when a listed file's content reached the source, so this is not a record that a person merged it there (the path may have been listed later, or the file landed by a merge the list does not bind, a human_merge_pin_only exception or a bypass actor). Turning it on is disclosed as widening, and the preview lists the cases."},
		},
		"required":             []string{"source", "target", "authors", "head_ref_prefix"},
		"additionalProperties": false,
	}
	list := map[string]any{"type": "array", "items": rule}
	return map[string]any{"type": "object", "description": "owner/repo to that repository's whole list of branch-promotion rules; null or [] removes the repository. Repositories you do not name are kept. A rule lets a pull request from source to target, opened by a listed author from a head ref with the prefix, merge with no person when the source branch takes changes only through a pull request and the head (the source tip or an ancestor of it, or one commit on the live target tip) and its test merge carry the source branch's current tree. A rule is sent whole: leave inherit_human_merge_paths out and it is off. Repositories must be in project.repos.", "additionalProperties": map[string]any{"anyOf": []any{list, map[string]any{"type": "null"}}}}
}
