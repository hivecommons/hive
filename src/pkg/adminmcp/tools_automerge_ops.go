package adminmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// auto_merge.update changes the hive's merge settings through the endpoint the
// dashboard's merge controls use, PATCH /api/config/auto-merge
// (handleAutoMergePut). That endpoint replaces every list and map it is sent
// whole: squash_only ["a/b"] drops every other repository. So this op takes
// changes instead (entries to add and remove, one repository's human-merge
// paths or pin-only exception to set or unset), reads the current settings
// while previewing, and sends each changed list or map as its whole merged
// value. The preview shows
// every changed setting before and after. ConfirmGuard reads the settings
// again at write_confirm and refuses when any of those before-values moved, so
// a merge built from a stale read never overwrites someone else's change.
//
// The op never merges a pull request. It changes which pull requests the merge
// sweeps may merge later, and the endpoint validates, saves and audits the
// change exactly as it does for the dashboard, for the owner only.
const (
	WriteOpAutoMergeUpdate = "auto_merge.update"

	autoMergeSettingsPath = "/api/config/auto-merge"

	// defaultAutoMergeMaxMerges mirrors automerge.DefaultAutoMergeSweepMaxMerges,
	// what max_merges 0 means. It only words the widening check.
	defaultAutoMergeMaxMerges = 3
)

func autoMergeWriteOps() []WriteOp { return []WriteOp{autoMergeUpdateOp{}} }

type autoMergeUpdateOp struct{}

type amKind int

const (
	amBool amKind = iota
	amInt
	amDuration
	amEnum
	amList
	amRepoPaths
	amPinOnly
	amFenceScopeRepos // per-repository map, sent as a patch (tools_automerge_fence_repos.go)
)

// amEntry is how a list's entries are compared.
type amEntry int

const (
	amEntryExact amEntry = iota
	amEntryFold
	amEntryRepo // owner/repo or a bare project repo name, case-insensitive
	amEntryBase // owner/repo#base, the repository part case-insensitive
)

// amWiden is the rule that decides whether a change lets the hive merge more
// on its own.
type amWiden int

const (
	amWidenNever     amWiden = iota
	amWidenOn                // a boolean turned on
	amWidenOff               // a boolean turned off
	amWidenRaise             // a number raised (0 means def)
	amWidenLower             // a number or duration lowered (0 means def)
	amWidenAdd               // a list gains an entry
	amWidenRemove            // a list loses an entry
	amWidenAllowList         // a repository allow-list where empty means every repository
	amWidenChecks            // required_checks
	amWidenTiers             // contributor_lane.require_tiers, where empty means the default tiers
	amWidenRole              // trusted_authors.require_role from owner to merger
	amWidenScope             // contributor_lane.auto_promotion_scope from repo to hive
	amWidenPaths             // human_merge_paths loses a pattern
	amWidenPinOnly           // human_merge_pin_only admits more repositories, paths, kinds or authors
)

type amField struct {
	key      string // body path, dotted for a nested object
	kind     amKind
	entry    amEntry
	enum     []string
	def      int  // what 0 means, for amWidenRaise and amWidenLower on a number
	positive bool // a duration must be above zero
	widen    amWiden
	why      string // what the widening lets the hive do
}

// autoMergeFields is every body field handleAutoMergePut reads. A preview
// lists the changes in the order of the argument names.
var autoMergeFields = []amField{
	{key: "self_authored", kind: amBool, widen: amWidenOn, why: "the App merges its own CI-green pull requests with no human queue approval"},
	{key: "max_merges", kind: amInt, def: defaultAutoMergeMaxMerges, widen: amWidenRaise, why: "more merges per sweep pass"},
	{key: "min_head_age", kind: amDuration, positive: true, widen: amWidenLower, why: "a new head may merge sooner after a push, before slow checks have registered"},
	{key: "required_checks", kind: amList, widen: amWidenChecks, why: "the self-merge sweep gates on fewer named checks"},
	{key: "allow_unprotected_base", kind: amList, entry: amEntryRepo},
	{key: "no_ci_ok", kind: amList, entry: amEntryRepo, widen: amWidenAdd, why: "a repository with no CI at all counts as green to the merge-request relay"},
	{key: "trusted_bot_authors", kind: amList, entry: amEntryFold, widen: amWidenAdd, why: "these bots' green pull requests merge like the App's own"},
	{key: "squash_only", kind: amList, entry: amEntryRepo, widen: amWidenRemove, why: "a mixed-author pull request there merges with a merge commit instead of going back to a person"},
	{key: "require_current_base", kind: amList, entry: amEntryRepo, widen: amWidenRemove, why: "pull requests there may merge without the current base tip"},
	{key: "human_merge_paths", kind: amRepoPaths, widen: amWidenPaths, why: "pull requests touching these paths no longer wait for a person to merge them"},
	{key: "human_merge_pin_only", kind: amPinOnly, widen: amWidenPinOnly},
	{key: "bot_takeover_enabled", kind: amBool, widen: amWidenOn, why: "the App re-authors trusted github-actions[bot] pull requests and merges its copy"},
	{key: "require_current_base_default", kind: amBool, widen: amWidenOff, why: "repositories without an override may merge without the current base tip"},
	{key: "merge_train.enabled", kind: amBool},
	{key: "merge_train.max_rebase_attempts", kind: amInt},
	{key: "native_merge_queue.enabled", kind: amBool, widen: amWidenOn, why: "eligible pull requests are handed to GitHub's merge queue"},
	{key: "native_merge_queue.repos", kind: amList, entry: amEntryRepo, widen: amWidenAllowList, why: "more repositories hand pull requests to GitHub's merge queue"},
	{key: "native_merge_queue.bases", kind: amList, entry: amEntryBase, widen: amWidenAdd, why: "more base branches hand pull requests to GitHub's merge queue"},
	{key: "native_merge_queue.skip_current_base_gate", kind: amBool, widen: amWidenOn, why: "queue-enabled bases skip the current-base gate"},
	{key: "native_merge_queue.probe_ttl", kind: amDuration},
	{key: "trusted_authors.enabled", kind: amBool, widen: amWidenOn, why: "pull requests by authors who hold merge authority merge with no review"},
	{key: "trusted_authors.repos", kind: amList, entry: amEntryRepo, widen: amWidenAllowList, why: "the trusted-author lane merges in more repositories"},
	{key: "trusted_authors.require_role", kind: amEnum, enum: []string{config.RoleMerger, config.RoleOwner}, widen: amWidenRole, why: "authors at the merger role qualify, not only owners"},
	{key: "trusted_authors.require_github_permission", kind: amBool, widen: amWidenOff, why: "authors qualify without GitHub write permission on the repository"},
	{key: "trusted_authors.exclude_labels", kind: amList, entry: amEntryFold, widen: amWidenRemove, why: "pull requests with these labels are no longer kept out of the trusted-author lane"},
	{key: "contributor_lane.enabled", kind: amBool, widen: amWidenOn, why: "contributor pull requests the hive dispatched merge once green, reviewed and soaked"},
	{key: "contributor_lane.soak_minutes", kind: amInt, def: config.DefaultContributorLaneSoakMinutes, widen: amWidenLower, why: "contributor pull requests merge after a shorter soak"},
	{key: "contributor_lane.require_reviewer_verdict", kind: amBool, widen: amWidenOff, why: "contributor pull requests merge without an approving reviewer verdict"},
	{key: "contributor_lane.repos", kind: amList, entry: amEntryRepo, widen: amWidenAllowList, why: "the contributor lane merges in more repositories"},
	{key: "contributor_lane.exclude_labels", kind: amList, entry: amEntryFold, widen: amWidenRemove, why: "pull requests with these labels are no longer kept out of the contributor lane"},
	{key: "contributor_lane.require_tiers", kind: amList, entry: amEntryFold, widen: amWidenTiers, why: "contributors at more trust tiers qualify for unattended merge"},
	{key: "contributor_lane.registry_tiers_sufficient", kind: amBool, widen: amWidenOn, why: "a registry tier alone admits a pull request, with no claim marker"},
	{key: "contributor_lane.auto_promotion_scope", kind: amEnum, enum: config.ContributorLaneAutoPromotionScopes, widen: amWidenScope, why: "merges in every repository count toward automatic promotion"},
	{key: "contributor_lane.approve_held_runs_for_registry_trusted", kind: amBool, widen: amWidenOn, why: "the hive approves held workflow runs for registry-trusted authors"},
	{key: "contributor_lane.owner_approval_releases_workflow_fence", kind: amBool, widen: amWidenOn, why: "an owner's approval lets the lane merge workflow-file changes"},
	// Widening is named in amFenceReposFinish, which also needs the hive-wide
	// scope and the human-merge paths.
	{key: amFenceReposKey, kind: amFenceScopeRepos},
}

func (autoMergeUpdateOp) Name() string { return WriteOpAutoMergeUpdate }
func (autoMergeUpdateOp) Description() string {
	return "Change merge settings (auto_merge) through PATCH /api/config/auto-merge, the endpoint the dashboard's merge controls use. Owner only. Pass only what changes, shaped like the endpoint's body. A list takes {\"add\": [...], \"remove\": [...]}; human_merge_paths takes {\"owner/repo\": [every pattern for that repository]}, with null or [] removing that repository. human_merge_pin_only takes {\"owner/repo\": {\"enabled\": true, \"paths\": [...], \"kinds\": [...], \"trusted_authors\": [...]}}, the whole exception for that repository, with null removing it; the repository needs human_merge_paths. contributor_lane.workflow_fence_scope_repos takes {\"owner/repo\": \"all\" | \"human_merge_paths\"}, one repository's own workflow fence scope, with null returning it to the hive-wide scope; only the named repositories change. The preview reads the current settings and sends each changed list or map whole, keeping every entry you did not name, shows each change before and after, and says which changes let the hive merge more on its own. write_confirm refuses if any of those settings changed after the preview. This operation never merges a pull request."
}

func (autoMergeUpdateOp) InputSchema() map[string]any {
	props := map[string]any{}
	groups := map[string]map[string]any{}
	for i := range autoMergeFields {
		f := &autoMergeFields[i]
		group, name, nested := strings.Cut(f.key, ".")
		if !nested {
			props[f.key] = f.schema()
			continue
		}
		if groups[group] == nil {
			groups[group] = map[string]any{}
		}
		groups[group][name] = f.schema()
	}
	for group, sub := range groups {
		props[group] = map[string]any{"type": "object", "properties": sub, "additionalProperties": false}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{}, "additionalProperties": false}
}

func (f *amField) schema() map[string]any {
	list := map[string]any{"type": "array", "items": stringSchema()}
	switch f.kind {
	case amBool:
		return map[string]any{"type": "boolean"}
	case amInt:
		return map[string]any{"type": "integer", "minimum": 0}
	case amDuration:
		return map[string]any{"type": "string", "description": "A duration such as 3m or 1h30m."}
	case amEnum:
		return map[string]any{"type": "string", "enum": f.enum}
	case amFenceScopeRepos:
		return amFenceReposSchema()
	case amRepoPaths:
		return map[string]any{"type": "object", "description": "owner/repo to every glob pattern that repository should hold; null or [] removes the repository. Repositories you do not name are kept.", "additionalProperties": map[string]any{"anyOf": []any{list, map[string]any{"type": "null"}}}}
	case amPinOnly:
		entry := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"enabled":         map[string]any{"type": "boolean"},
				"paths":           map[string]any{"type": "array", "items": stringSchema(), "description": "Glob patterns, within the repository's human_merge_paths, a pin bump may touch."},
				"kinds":           map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": config.PinOnlyKinds}, "description": autoMergePinOnlyKindsHelp},
				"trusted_authors": map[string]any{"type": "array", "items": stringSchema(), "description": "Logins that must author the pull request and every commit behind its head."},
			},
			"required":             []string{"enabled"},
			"additionalProperties": false,
		}
		return map[string]any{"type": "object", "description": "owner/repo to that repository's whole pin-only exception; null removes it. Repositories you do not name are kept. An enabled exception lets a pull request whose only change is a pin bump on a human-merge path merge without a hand merge.", "additionalProperties": map[string]any{"anyOf": []any{entry, map[string]any{"type": "null"}}}}
	default:
		return map[string]any{"type": "object", "description": "Entries to add and to remove. Entries you do not name are kept.", "properties": map[string]any{"add": list, "remove": list}, "additionalProperties": false}
	}
}

// Preview cannot read the current settings, so it only checks that every
// argument names a field this op knows. write_preview uses
// PreviewWithProvider; write_confirm calls Preview to check the request still
// names this endpoint, and then ConfirmGuard reads the settings again.
func (autoMergeUpdateOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseAutoMergeArgs(args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpAutoMergeUpdate,
		Summary:             "Change merge settings (current settings not read)",
		Target:              "merge settings (auto_merge)",
		Request:             WriteRequest{Method: http.MethodPatch, Path: autoMergeSettingsPath},
		WideningDisclosure:  "Potential widening: the current merge settings were not read, so this preview cannot say what would change.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}, nil
}

// PreviewWithProvider reads the current merge settings, merges the requested
// changes into them and previews the request that sends the result.
func (autoMergeUpdateOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	plan, err := planAutoMergeUpdate(ctx, args, provider)
	if err != nil {
		return WritePreview{}, err
	}
	fields := make([]string, 0, len(plan.changes))
	changes := make([]map[string]any, 0, len(plan.changes))
	lines := make([]string, 0, len(plan.changes))
	var notes []string
	for _, c := range plan.changes {
		fields = append(fields, c.field.key)
		for _, row := range c.rows() {
			changes = append(changes, map[string]any{"field": row.field, "before": row.before, "after": row.after})
			lines = append(lines, row.field+": "+compactJSON(row.before)+" → "+compactJSON(row.after))
		}
		notes = append(notes, c.notes...)
	}
	disclosure := "No widening: none of these changes lets the hive merge more on its own. This operation merges nothing."
	if len(notes) > 0 {
		disclosure = "Widening: " + strings.Join(notes, "; ") + ". The merge sweeps may then merge these pull requests with no person involved. This operation itself merges nothing."
	}
	effects := []string{
		"Hive sends PATCH " + autoMergeSettingsPath + " with only the settings listed here. Each list or map goes as its whole new value, built from the current setting read for this preview, so entries you did not name are kept.",
		"The endpoint validates the change with the same rules as the dashboard's merge settings, saves it, applies it without a restart and records a config_auto_merge audit entry. It is owner only.",
		"This changes which pull requests the merge sweeps may merge from now on. It merges nothing itself.",
		"write_confirm reads these settings again and refuses if any of them changed after this preview.",
	}
	effects = append(effects, plan.effects...)
	if len(plan.unchanged) > 0 {
		effects = append(effects, "Already as requested, so not sent: "+strings.Join(plan.unchanged, ", ")+".")
	}
	details := map[string]any{"fields": fields, "changes": changes}
	if len(plan.unchanged) > 0 {
		details["unchanged"] = plan.unchanged
	}
	return WritePreview{
		Operation:           WriteOpAutoMergeUpdate,
		Summary:             "Change merge settings: " + strings.Join(fields, ", "),
		Target:              "merge settings (auto_merge)",
		Request:             WriteRequest{Method: http.MethodPatch, Path: autoMergeSettingsPath, Body: plan.body},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing merge settings: " + strings.Join(lines, "; ") + ".",
		Details:             details,
		StateFingerprint:    plan.fingerprint,
	}, nil
}

// ConfirmGuard reads the merge settings again and returns the fingerprint of
// the before-values the confirmation would overwrite.
func (autoMergeUpdateOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	plan, err := planAutoMergeUpdate(ctx, args, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the merge settings again before writing, and that check refused this change; preview it again: %w", err)
	}
	return plan.fingerprint, nil
}

type amArg struct {
	field *amField
	raw   any
}

type amPlanned struct {
	field  *amField
	before any
	after  any
	notes  []string
	// fenceHiveWide is the hive-wide workflow_fence_scope, which a
	// per-repository fence scope row names when the repository has none.
	fenceHiveWide string
}

type amPlan struct {
	changes     []amPlanned
	unchanged   []string
	body        map[string]any
	fingerprint string
	effects     []string // extra preview effects a change adds
}

type amRow struct {
	field         string
	before, after any
}

// rows splits a human_merge_paths or human_merge_pin_only change into one row
// per repository that changes, so a preview names repositories as values
// rather than as keys.
func (c amPlanned) rows() []amRow {
	if c.field.kind == amFenceScopeRepos {
		return amFenceReposRows(c, c.fenceHiveWide)
	}
	if c.field.kind == amPinOnly {
		before := autoMergePinOnlyValue(c.before)
		after, _ := c.after.(map[string]autoMergePinOnlyEntry)
		var rows []amRow
		for _, repo := range autoMergePinOnlyKeys(before, after) {
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
	if c.field.kind != amRepoPaths {
		return []amRow{{field: c.field.key, before: c.before, after: c.after}}
	}
	before := repoPathsValue(c.before)
	after, _ := c.after.(map[string][]string)
	var rows []amRow
	for _, repo := range unionRepoKeys(before, after) {
		b, a := before[repo], after[repo]
		if compactJSON(b) == compactJSON(a) {
			continue
		}
		rows = append(rows, amRow{field: c.field.key + "[" + repo + "]", before: nilAsEmpty(b), after: nilAsEmpty(a)})
	}
	return rows
}

func parseAutoMergeArgs(args map[string]any) ([]amArg, error) {
	top := map[string]*amField{}
	nested := map[string]map[string]*amField{}
	for i := range autoMergeFields {
		f := &autoMergeFields[i]
		group, name, ok := strings.Cut(f.key, ".")
		if !ok {
			top[f.key] = f
			continue
		}
		if nested[group] == nil {
			nested[group] = map[string]*amField{}
		}
		nested[group][name] = f
	}
	var out []amArg
	for _, key := range SortKeys(args) {
		if sub, ok := nested[key]; ok {
			obj, isObj := args[key].(map[string]any)
			if !isObj {
				return nil, fmt.Errorf("%s must be an object", key)
			}
			for _, name := range SortKeys(obj) {
				f := sub[name]
				if f == nil {
					return nil, unknownAutoMergeField(key + "." + name)
				}
				out = append(out, amArg{field: f, raw: obj[name]})
			}
			continue
		}
		f := top[key]
		if f == nil {
			return nil, unknownAutoMergeField(key)
		}
		out = append(out, amArg{field: f, raw: args[key]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one merge setting is required")
	}
	return out, nil
}

func unknownAutoMergeField(key string) error {
	return fmt.Errorf("unsupported merge setting %q", key)
}

func planAutoMergeUpdate(ctx context.Context, args map[string]any, provider Provider) (amPlan, error) {
	parsed, err := parseAutoMergeArgs(args)
	if err != nil {
		return amPlan{}, err
	}
	current, err := readAutoMergeSettings(ctx, provider)
	if err != nil {
		return amPlan{}, err
	}
	projectRepos := stringListValueLoose(current["project_repos"])
	plan := amPlan{body: map[string]any{}}
	beforeValues := map[string]any{}
	for _, a := range parsed {
		f := a.field
		before, ok := lookupSetting(current, f.key)
		if !ok {
			return amPlan{}, fmt.Errorf("this hive's merge settings do not report %s, so the change cannot be previewed against its current value; the hive may run an older build", f.key)
		}
		if f.kind == amFenceScopeRepos {
			before = amFenceRepoOverrides(before)
		}
		after, err := f.apply(before, a.raw, projectRepos)
		if err != nil {
			return amPlan{}, err
		}
		if compactJSON(before) == compactJSON(after) {
			plan.unchanged = append(plan.unchanged, f.key)
			continue
		}
		send := after
		if f.kind == amFenceScopeRepos {
			send = amFenceReposPatch(amFenceRepoOverrides(before), amFenceRepoOverrides(after))
		}
		setSetting(plan.body, f.key, send)
		beforeValues[f.key] = before
		plan.changes = append(plan.changes, amPlanned{field: f, before: before, after: after, notes: f.widening(before, after, projectRepos)})
	}
	if len(plan.changes) == 0 {
		return amPlan{}, fmt.Errorf("nothing would change: %s already as requested", strings.Join(plan.unchanged, ", "))
	}
	if err := autoMergeCheckPinOnlyGuarded(plan, current); err != nil {
		return amPlan{}, err
	}
	if err := amFenceReposFinish(&plan, current, beforeValues); err != nil {
		return amPlan{}, err
	}
	plan.fingerprint = settingsFingerprint(beforeValues)
	return plan, nil
}

// readAutoMergeSettings reads what the dashboard's merge settings show
// (settings_read section auto_merge, GET /api/config/auto-merge), which only a
// verified owner may read. Without it there is nothing to merge a change into,
// so the preview is refused rather than guessed.
func readAutoMergeSettings(ctx context.Context, provider Provider) (map[string]any, error) {
	if provider == nil {
		return nil, fmt.Errorf("%s reads the current merge settings first, and this connection cannot read them", WriteOpAutoMergeUpdate)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": "auto_merge"})
	if err != nil {
		return nil, fmt.Errorf("%s needs the current merge settings, which only the hive's verified owner can read: %w", WriteOpAutoMergeUpdate, err)
	}
	m, ok := data.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("%s could not read the current merge settings", WriteOpAutoMergeUpdate)
	}
	return m, nil
}

func lookupSetting(m map[string]any, key string) (any, bool) {
	group, name, nested := strings.Cut(key, ".")
	if !nested {
		v, ok := m[key]
		return v, ok
	}
	sub, _ := m[group].(map[string]any)
	if sub == nil {
		return nil, false
	}
	v, ok := sub[name]
	return v, ok
}

func setSetting(body map[string]any, key string, value any) {
	group, name, nested := strings.Cut(key, ".")
	if !nested {
		body[key] = value
		return
	}
	sub, _ := body[group].(map[string]any)
	if sub == nil {
		sub = map[string]any{}
		body[group] = sub
	}
	sub[name] = value
}

// settingsFingerprint hashes the before-value of every field a change sends.
// encoding/json sorts map keys, so equal settings hash equally.
func settingsFingerprint(before map[string]any) string {
	b, _ := json.Marshal(before)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// apply returns the value to send for f: the argument itself for a scalar,
// the merged whole value for a list or map.
func (f *amField) apply(before, raw any, projectRepos []string) (any, error) {
	switch f.kind {
	case amBool:
		v, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("%s must be true or false", f.key)
		}
		return v, nil
	case amInt:
		n, ok := wholeNumber(raw)
		if !ok || n < 0 {
			return nil, fmt.Errorf("%s must be a whole number of at least 0", f.key)
		}
		return n, nil
	case amDuration:
		s, _ := raw.(string)
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil || d < 0 || (f.positive && d == 0) {
			bound := "0 or more"
			if f.positive {
				bound = "above zero"
			}
			return nil, fmt.Errorf("%s must be a duration such as 3m, %s", f.key, bound)
		}
		return d.String(), nil
	case amEnum:
		s, _ := raw.(string)
		s = strings.ToLower(strings.TrimSpace(s))
		for _, allowed := range f.enum {
			if s == allowed {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%s must be one of %s", f.key, strings.Join(f.enum, ", "))
	case amRepoPaths:
		return applyRepoPaths(before, raw, projectRepos)
	case amPinOnly:
		return autoMergeApplyPinOnly(before, raw, projectRepos)
	case amFenceScopeRepos:
		return amFenceReposApply(before, raw, projectRepos)
	default:
		return f.applyList(before, raw, projectRepos)
	}
}

func (f *amField) applyList(before, raw any, projectRepos []string) ([]string, error) {
	change, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s takes {\"add\": [...], \"remove\": [...]}", f.key)
	}
	for k := range change {
		if k != "add" && k != "remove" {
			return nil, fmt.Errorf("%s takes only add and remove, not %q", f.key, k)
		}
	}
	add, err := f.entries(change["add"], "add", projectRepos)
	if err != nil {
		return nil, err
	}
	remove, err := f.entries(change["remove"], "remove", projectRepos)
	if err != nil {
		return nil, err
	}
	if len(add)+len(remove) == 0 {
		return nil, fmt.Errorf("%s needs entries to add or remove", f.key)
	}
	cur := stringListValueLoose(before)
	have := map[string]bool{}
	for _, c := range cur {
		have[f.norm(c, projectRepos)] = true
	}
	drop := map[string]bool{}
	for _, r := range remove {
		k := f.norm(r, projectRepos)
		if !have[k] {
			return nil, fmt.Errorf("%s does not hold %q (now: %s)", f.key, r, listOrNone(cur))
		}
		drop[k] = true
	}
	out := make([]string, 0, len(cur)+len(add))
	for _, c := range cur {
		if !drop[f.norm(c, projectRepos)] {
			out = append(out, c)
		}
	}
	for _, a := range add {
		k := f.norm(a, projectRepos)
		if drop[k] {
			return nil, fmt.Errorf("%s: %q is both added and removed", f.key, a)
		}
		if have[k] {
			continue
		}
		have[k] = true
		out = append(out, a)
	}
	return out, nil
}

// entries checks one add or remove list and returns its entries in the form
// the endpoint stores.
func (f *amField) entries(raw any, which string, projectRepos []string) ([]string, error) {
	list, err := stringListValue(raw)
	if err != nil {
		return nil, fmt.Errorf("%s.%s must be a list of strings", f.key, which)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		e = strings.TrimSpace(e)
		if e == "" {
			return nil, fmt.Errorf("%s.%s entries must be non-empty", f.key, which)
		}
		switch f.entry {
		case amEntryRepo:
			if strings.ContainsAny(e, " \t#") {
				return nil, fmt.Errorf("%s: %q is not a repository name", f.key, e)
			}
			e = qualifyProjectRepo(e, projectRepos)
		case amEntryBase:
			repo, base, ok := strings.Cut(e, "#")
			if !ok || repo == "" || base == "" || strings.ContainsAny(e, " \t") {
				return nil, fmt.Errorf("%s: %q is not an owner/repo#base pair", f.key, e)
			}
			e = strings.ToLower(repo) + "#" + base
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *amField) norm(e string, projectRepos []string) string {
	e = strings.TrimSpace(e)
	switch f.entry {
	case amEntryFold:
		return strings.ToLower(e)
	case amEntryRepo:
		return strings.ToLower(qualifyProjectRepo(e, projectRepos))
	case amEntryBase:
		repo, base, _ := strings.Cut(e, "#")
		return strings.ToLower(repo) + "#" + base
	}
	return e
}

// applyRepoPaths sets or unsets the named repositories' human-merge paths and
// keeps every other repository's entry.
func applyRepoPaths(before, raw any, projectRepos []string) (map[string][]string, error) {
	change, ok := raw.(map[string]any)
	if !ok || len(change) == 0 {
		return nil, fmt.Errorf("human_merge_paths takes {\"owner/repo\": [every pattern for it]}; null or [] removes that repository")
	}
	out := map[string][]string{}
	for repo, patterns := range repoPathsValue(before) {
		out[repo] = patterns
	}
	touched := map[string]bool{}
	for _, key := range SortKeys(change) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), projectRepos)
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " \t") {
			return nil, fmt.Errorf("human_merge_paths: %q is not an owner/repo name", key)
		}
		if touched[strings.ToLower(repo)] {
			return nil, fmt.Errorf("human_merge_paths names %s twice", repo)
		}
		touched[strings.ToLower(repo)] = true
		list, err := stringListValue(change[key])
		if err != nil {
			return nil, fmt.Errorf("human_merge_paths[%s] must be a list of glob patterns or null", repo)
		}
		patterns := make([]string, 0, len(list))
		seen := map[string]bool{}
		for _, p := range list {
			p = strings.TrimSpace(p)
			if p == "" {
				return nil, fmt.Errorf("human_merge_paths[%s] patterns must be non-empty", repo)
			}
			if _, err := filepath.Match(p, ""); err != nil {
				return nil, fmt.Errorf("human_merge_paths[%s]: %s is not a valid glob", repo, p)
			}
			if !seen[p] {
				seen[p] = true
				patterns = append(patterns, p)
			}
		}
		existing := ""
		for k := range out {
			if strings.EqualFold(k, repo) {
				existing = k
			}
		}
		if len(patterns) == 0 {
			if existing == "" {
				return nil, fmt.Errorf("human_merge_paths has no entry for %s to remove", repo)
			}
			delete(out, existing)
			continue
		}
		if existing != "" {
			repo = existing
		}
		out[repo] = patterns
	}
	return out, nil
}

// autoMergePinOnlyEntry is one repository's auto_merge.human_merge_pin_only entry as the
// endpoint reports and takes it. The fields are in the order encoding/json
// sorts map keys, so a value read back from the hive and one built here
// encode the same.
type autoMergePinOnlyEntry struct {
	Enabled        bool     `json:"enabled"`
	Kinds          []string `json:"kinds"`
	Paths          []string `json:"paths"`
	TrustedAuthors []string `json:"trusted_authors"`
}

// autoMergePinOnlyValue reads human_merge_pin_only as the hive reported it.
func autoMergePinOnlyValue(v any) map[string]autoMergePinOnlyEntry {
	out := map[string]autoMergePinOnlyEntry{}
	switch m := v.(type) {
	case map[string]any:
		for repo, raw := range m {
			e, _ := raw.(map[string]any)
			enabled, _ := e["enabled"].(bool)
			out[repo] = autoMergePinOnlyEntry{
				Enabled:        enabled,
				Kinds:          nilAsEmpty(stringListValueLoose(e["kinds"])),
				Paths:          nilAsEmpty(stringListValueLoose(e["paths"])),
				TrustedAuthors: nilAsEmpty(stringListValueLoose(e["trusted_authors"])),
			}
		}
	case map[string]autoMergePinOnlyEntry:
		for repo, e := range m {
			out[repo] = e
		}
	}
	return out
}

func autoMergePinOnlyKeys(a, b map[string]autoMergePinOnlyEntry) []string {
	keys := map[string]any{}
	for k := range a {
		keys[k] = nil
	}
	for k := range b {
		keys[k] = nil
	}
	return SortKeys(keys)
}

// autoMergeApplyPinOnly sets or unsets the named repositories' whole pin-only
// exception and keeps every other repository's entry. The merged map must
// pass the same validation hive.yaml loads under.
func autoMergeApplyPinOnly(before, raw any, projectRepos []string) (map[string]autoMergePinOnlyEntry, error) {
	const shape = `human_merge_pin_only takes {"owner/repo": {"enabled": true, "paths": [...], "kinds": [...], "trusted_authors": [...]}}; null removes that repository`
	change, ok := raw.(map[string]any)
	if !ok || len(change) == 0 {
		return nil, fmt.Errorf("%s", shape)
	}
	out := autoMergePinOnlyValue(before)
	touched := map[string]bool{}
	for _, key := range SortKeys(change) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), projectRepos)
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " \t") {
			return nil, fmt.Errorf("human_merge_pin_only: %q is not an owner/repo name", key)
		}
		if touched[strings.ToLower(repo)] {
			return nil, fmt.Errorf("human_merge_pin_only names %s twice", repo)
		}
		touched[strings.ToLower(repo)] = true
		existing := ""
		for k := range out {
			if strings.EqualFold(k, repo) {
				existing = k
			}
		}
		if change[key] == nil {
			if existing == "" {
				return nil, fmt.Errorf("human_merge_pin_only has no entry for %s to remove", repo)
			}
			delete(out, existing)
			continue
		}
		entry, err := autoMergePinOnlyEntryArg(repo, change[key])
		if err != nil {
			return nil, err
		}
		if existing != "" {
			repo = existing
		}
		out[repo] = entry
	}
	check := config.AutoMergeConfig{HumanMergePinOnly: map[string]config.PinOnlyException{}}
	for repo, e := range out {
		check.HumanMergePinOnly[repo] = config.PinOnlyException{Enabled: e.Enabled, Paths: e.Paths, Kinds: e.Kinds, TrustedAuthors: e.TrustedAuthors}
	}
	if err := check.ValidateHumanMergePinOnly(); err != nil {
		return nil, err
	}
	return out, nil
}

// autoMergePinOnlyEntryArg reads one repository's exception from the arguments.
func autoMergePinOnlyEntryArg(repo string, raw any) (autoMergePinOnlyEntry, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return autoMergePinOnlyEntry{}, fmt.Errorf("human_merge_pin_only[%s] must be an object with enabled, paths, kinds and trusted_authors, or null", repo)
	}
	for k := range obj {
		switch k {
		case "enabled", "paths", "kinds", "trusted_authors":
		default:
			return autoMergePinOnlyEntry{}, fmt.Errorf("human_merge_pin_only[%s] takes enabled, paths, kinds and trusted_authors, not %q", repo, k)
		}
	}
	enabled, ok := obj["enabled"].(bool)
	if !ok {
		return autoMergePinOnlyEntry{}, fmt.Errorf("human_merge_pin_only[%s].enabled must be true or false", repo)
	}
	entry := autoMergePinOnlyEntry{Enabled: enabled}
	for _, f := range []struct {
		name string
		dst  *[]string
	}{{"kinds", &entry.Kinds}, {"paths", &entry.Paths}, {"trusted_authors", &entry.TrustedAuthors}} {
		list, err := stringListValue(obj[f.name])
		if err != nil {
			return autoMergePinOnlyEntry{}, fmt.Errorf("human_merge_pin_only[%s].%s must be a list of strings", repo, f.name)
		}
		vals := []string{}
		for _, v := range list {
			v = strings.TrimSpace(v)
			if v == "" {
				return autoMergePinOnlyEntry{}, fmt.Errorf("human_merge_pin_only[%s].%s entries must be non-empty", repo, f.name)
			}
			if !slices.Contains(vals, v) {
				vals = append(vals, v)
			}
		}
		*f.dst = vals
	}
	return entry, nil
}

// autoMergeCheckPinOnlyGuarded refuses an enabled pin-only exception the change adds
// or alters for a repository that has no human_merge_paths after the change,
// as the endpoint does: the exception only lifts a human-merge-path hold, so
// there it would never apply.
func autoMergeCheckPinOnlyGuarded(plan amPlan, current map[string]any) error {
	var pin *amPlanned
	for i := range plan.changes {
		if plan.changes[i].field.kind == amPinOnly {
			pin = &plan.changes[i]
		}
	}
	if pin == nil {
		return nil
	}
	paths := repoPathsValue(current["human_merge_paths"])
	if sent, ok := plan.body["human_merge_paths"]; ok {
		paths = repoPathsValue(sent)
	}
	before := autoMergePinOnlyValue(pin.before)
	after, _ := pin.after.(map[string]autoMergePinOnlyEntry)
	for _, repo := range autoMergePinOnlyKeys(after, nil) {
		e := after[repo]
		if b, had := before[repo]; !e.Enabled || (had && compactJSON(b) == compactJSON(e)) {
			continue
		}
		guarded := false
		for k, patterns := range paths {
			if strings.EqualFold(k, repo) && len(patterns) > 0 {
				guarded = true
			}
		}
		if !guarded {
			return fmt.Errorf("human_merge_pin_only[%s]: %s has no human_merge_paths, so a pin-only exception there would never apply; set its human-merge paths first (or in the same change)", repo, repo)
		}
	}
	return nil
}

// autoMergePinOnlyKindsHelp describes each kind in the operation's schema.
const autoMergePinOnlyKindsHelp = "Which pin bumps qualify. action-sha: a uses: owner/repo@<40-hex> line. image-digest: a FROM or image: line pinned by @sha256 digest. version-pin: a literal version: or ARG *_VERSION= value. " + autoMergePinOnlySHARefHelp + ". Nothing is on unless listed."

// autoMergePinOnlySHARefHelp says what action-sha-ref admits, for the schema
// and for the preview of a change that turns it on. It has no closing full
// stop: the preview joins notes into one sentence.
const autoMergePinOnlySHARefHelp = "action-sha-ref: a bare \"key: <40-hex>\" line (a commit passed on as an input, for example) only when it carries the same old and new commit as a uses: line bumped in the same pull request; it admits nothing by itself, so it needs action-sha"

// autoMergePinOnlyKindNote is appended to a widening note whose exception
// holds action-sha-ref, so the preview says what that kind lets through and
// when it can never apply.
func autoMergePinOnlyKindNote(kinds []string) string {
	if !slices.Contains(kinds, "action-sha-ref") {
		return ""
	}
	note := "; " + autoMergePinOnlySHARefHelp
	if !slices.Contains(kinds, "action-sha") {
		note += " (this exception does not list action-sha, so action-sha-ref has no effect)"
	}
	return note
}

// autoMergePinOnlyWidening names what a human_merge_pin_only change lets merge without
// a hand merge: a repository whose exception turns on, or an enabled one that
// gains paths, kinds or trusted authors. Removing any of those narrows it.
func autoMergePinOnlyWidening(before, after any) []string {
	was := autoMergePinOnlyValue(before)
	now := autoMergePinOnlyValue(after)
	var notes []string
	for _, repo := range autoMergePinOnlyKeys(now, nil) {
		e := now[repo]
		if !e.Enabled {
			continue
		}
		b, had := was[repo]
		bump := "pin bumps (" + strings.Join(e.Kinds, ", ") + ") to " + strings.Join(e.Paths, ", ") + " in " + repo + " by " + strings.Join(e.TrustedAuthors, ", ")
		if !had || !b.Enabled {
			notes = append(notes, "human_merge_pin_only["+repo+"] turned on: "+bump+" will merge without a hand merge"+autoMergePinOnlyKindNote(e.Kinds))
			continue
		}
		var gained []string
		for _, f := range []struct {
			name     string
			was, now []string
		}{{"paths", b.Paths, e.Paths}, {"kinds", b.Kinds, e.Kinds}, {"trusted_authors", b.TrustedAuthors, e.TrustedAuthors}} {
			var added []string
			for _, v := range f.now {
				if !slices.Contains(f.was, v) {
					added = append(added, v)
				}
			}
			if len(added) > 0 {
				gained = append(gained, f.name+" "+strings.Join(added, ", "))
			}
		}
		if len(gained) > 0 {
			notes = append(notes, "human_merge_pin_only["+repo+"] adds "+strings.Join(gained, "; ")+": "+bump+" will merge without a hand merge"+autoMergePinOnlyKindNote(e.Kinds))
		}
	}
	return notes
}

// widening names what a change lets the hive merge on its own, if anything.
func (f *amField) widening(before, after any, projectRepos []string) []string {
	note := func(what string) []string { return []string{f.key + " " + what + ": " + f.why} }
	switch f.widen {
	case amWidenOn:
		if b, _ := before.(bool); !b && after == true {
			return note("turned on")
		}
	case amWidenOff:
		if b, _ := before.(bool); b && after == false {
			return note("turned off")
		}
	case amWidenRaise, amWidenLower:
		from, to, ok := f.compare(before, after)
		if ok && ((f.widen == amWidenRaise && to > from) || (f.widen == amWidenLower && to < from)) {
			return note(compactJSON(before) + " → " + compactJSON(after))
		}
	case amWidenAdd:
		if added := f.diff(after, before, projectRepos); len(added) > 0 {
			return note("adds " + strings.Join(added, ", "))
		}
	case amWidenRemove, amWidenChecks:
		removed := f.diff(before, after, projectRepos)
		if len(removed) > 0 {
			return note("removes " + strings.Join(removed, ", "))
		}
		if f.widen == amWidenChecks && len(stringListValueLoose(before)) == 0 {
			return []string{f.key + " is declared for the first time: the self-merge sweep then gates on this list instead of the branch's own required checks, which may hold fewer"}
		}
	case amWidenAllowList:
		was, now := stringListValueLoose(before), stringListValueLoose(after)
		if len(was) > 0 && len(now) == 0 {
			return []string{f.key + " becomes empty, which means every repository: " + f.why}
		}
		if added := f.diff(after, before, projectRepos); len(was) > 0 && len(added) > 0 {
			return note("adds " + strings.Join(added, ", "))
		}
	case amWidenTiers:
		now := after
		if len(stringListValueLoose(after)) == 0 {
			now = config.DefaultContributorLaneRequireTiers
		}
		if added := f.diff(now, before, projectRepos); len(added) > 0 {
			return note("adds " + strings.Join(added, ", "))
		}
	case amWidenRole:
		if before == config.RoleOwner && after == config.RoleMerger {
			return note("owner → merger")
		}
	case amWidenScope:
		if before == config.ContributorLaneAutoPromotionScopeRepo && after == config.ContributorLaneAutoPromotionScopeHive {
			return note("repo → hive")
		}
	case amWidenPinOnly:
		return autoMergePinOnlyWidening(before, after)
	case amWidenPaths:
		was := repoPathsValue(before)
		now, _ := after.(map[string][]string)
		var notes []string
		for _, repo := range unionRepoKeys(was, now) {
			kept := map[string]bool{}
			for _, p := range now[repo] {
				kept[p] = true
			}
			var removed []string
			for _, p := range was[repo] {
				if !kept[p] {
					removed = append(removed, p)
				}
			}
			if len(removed) > 0 {
				notes = append(notes, f.key+"["+repo+"] removes "+strings.Join(removed, ", ")+": "+f.why)
			}
		}
		return notes
	}
	return nil
}

// compare returns a number or duration setting's before and after as
// comparable values, with 0 standing for the default where it does.
func (f *amField) compare(before, after any) (int64, int64, bool) {
	if f.kind == amDuration {
		b, err1 := time.ParseDuration(fmt.Sprint(before))
		a, err2 := time.ParseDuration(fmt.Sprint(after))
		return int64(b), int64(a), err1 == nil && err2 == nil
	}
	b, ok1 := wholeNumber(before)
	a, ok2 := wholeNumber(after)
	if f.def > 0 {
		if b == 0 {
			b = f.def
		}
		if a == 0 {
			a = f.def
		}
	}
	return int64(b), int64(a), ok1 && ok2
}

// diff returns the entries of a that b does not hold.
func (f *amField) diff(a, b any, projectRepos []string) []string {
	have := map[string]bool{}
	for _, e := range stringListValueLoose(b) {
		have[f.norm(e, projectRepos)] = true
	}
	var out []string
	for _, e := range stringListValueLoose(a) {
		if !have[f.norm(e, projectRepos)] {
			out = append(out, e)
		}
	}
	return out
}

// qualifyProjectRepo turns a bare repository name into the project
// repository's owner/repo, as the endpoint does for project.repos.
func qualifyProjectRepo(repo string, projectRepos []string) string {
	if strings.Contains(repo, "/") {
		return repo
	}
	for _, full := range projectRepos {
		if _, name, ok := strings.Cut(full, "/"); ok && strings.EqualFold(name, repo) {
			return full
		}
	}
	return repo
}

func repoPathsValue(v any) map[string][]string {
	out := map[string][]string{}
	switch m := v.(type) {
	case map[string]any:
		for repo, patterns := range m {
			out[repo] = stringListValueLoose(patterns)
		}
	case map[string][]string:
		for repo, patterns := range m {
			out[repo] = patterns
		}
	}
	return out
}

func unionRepoKeys(a, b map[string][]string) []string {
	keys := map[string]any{}
	for k := range a {
		keys[k] = nil
	}
	for k := range b {
		keys[k] = nil
	}
	return SortKeys(keys)
}

// stringListValue reads a JSON list of strings; nil is an empty list.
func stringListValue(v any) ([]string, error) {
	switch list := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), list...), nil
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("not a string")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("not a list")
}

// stringListValueLoose reads a list the hive returned, skipping anything that
// is not a string.
func stringListValueLoose(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func nilAsEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func listOrNone(list []string) string {
	if len(list) == 0 {
		return "empty"
	}
	return strings.Join(list, ", ")
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
