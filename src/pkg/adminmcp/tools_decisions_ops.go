package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// decisions.delegate_settings changes the "Let hive decide" settings
// (decisions.delegate) through PUT /api/config/decisions/delegate, the owner
// only endpoint behind the dashboard's Let hive decide controls. That endpoint
// replaces the per-repository override map whole when it is sent, so this op
// takes one change per repository (true, false, or null to drop the override),
// reads the current settings while previewing, and sends the merged map.
// ConfirmGuard reads them again at write_confirm and refuses when a value the
// change sends moved after the preview.
//
// The op starts no decision and merges nothing. It changes where the owner
// delegate may decide and whether the LLM step is consulted; what a delegated
// decision does once it runs is unchanged.
const (
	WriteOpDecisionsDelegateSettings = "decisions.delegate_settings"

	delegateSettingsPath = "/api/config/decisions/delegate"

	// delegateSettingsSection is the settings_read section the preview reads.
	delegateSettingsSection = "decisions_delegate"
)

func decisionsWriteOps() []WriteOp { return []WriteOp{decisionsDelegateSettingsOp{}} }

type decisionsDelegateSettingsOp struct{}

func (decisionsDelegateSettingsOp) Name() string { return WriteOpDecisionsDelegateSettings }
func (decisionsDelegateSettingsOp) Description() string {
	return "Change the \"Let hive decide\" settings (decisions.delegate) through PUT " + delegateSettingsPath + ". Owner only. Pass only what changes: enabled (the hive-wide switch), repos ({\"owner/repo\": true | false | null}, null drops that repository's override so it follows the hive-wide switch; repositories you do not name are kept), llm ({\"enabled\": bool, \"model\": \"name\"}, the LLM step; an empty model uses the governor reviewer model) and effect_timeout (a duration such as 30m, or empty for the default). The preview reads the current settings, shows each change before and after, and says what it lets the hive do: turning on llm sends the item's text to the configured model, and turning the delegate on for a repository lets Let hive decide act there. write_confirm refuses if any of those settings changed after the preview. This operation starts no decision and merges nothing."
}

func (decisionsDelegateSettingsOp) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enabled": map[string]any{"type": "boolean", "description": "decisions.delegate.enabled, the hive-wide switch (default on)."},
			"repos": map[string]any{
				"type":                 "object",
				"description":          "owner/repo (or a project repository's bare name) to true or false to override the hive-wide switch there, or null to drop the override. Repositories you do not name are kept.",
				"additionalProperties": map[string]any{"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "null"}}},
			},
			"llm": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"enabled": map[string]any{"type": "boolean", "description": "decisions.delegate.llm.enabled (default off)."},
					"model":   map[string]any{"type": "string", "description": "decisions.delegate.llm.model, a model name; empty uses the governor reviewer model. Never an API key."},
				},
				"additionalProperties": false,
			},
			"effect_timeout": map[string]any{"type": "string", "description": "decisions.delegate.effect_timeout, a duration above zero such as 30m, or empty for the default (30m)."},
		},
		"required":             []string{},
		"additionalProperties": false,
	}
}

// Preview cannot read the current settings, so it only checks the arguments.
// write_preview uses PreviewWithProvider; write_confirm calls Preview to check
// the request still names this endpoint, and then ConfirmGuard reads again.
func (decisionsDelegateSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := delegateSettingsParseArgs(args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpDecisionsDelegateSettings,
		Summary:             "Change Let hive decide settings (current settings not read)",
		Target:              "Let hive decide settings (decisions.delegate)",
		Request:             WriteRequest{Method: http.MethodPut, Path: delegateSettingsPath},
		WideningDisclosure:  "Potential widening: the current Let hive decide settings were not read, so this preview cannot say what would change.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}, nil
}

func (decisionsDelegateSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	plan, err := delegateSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return WritePreview{}, err
	}
	fields := make([]string, 0, len(plan.rows))
	changes := make([]map[string]any, 0, len(plan.rows))
	lines := make([]string, 0, len(plan.rows))
	for _, row := range plan.rows {
		fields = append(fields, row.field)
		changes = append(changes, map[string]any{"field": row.field, "before": row.before, "after": row.after})
		lines = append(lines, row.field+": "+compactJSON(row.before)+" → "+compactJSON(row.after))
	}
	never := "Neither change merges anything: a delegated decision takes the same actions it always has (on a pull request that can include the queue approval a person's decision would give), and whether anything then merges stays with the merge settings. This operation starts no decision."
	disclosure := "No widening: none of these changes lets the hive decide anything it could not before. " + never
	if len(plan.notes) > 0 {
		disclosure = "Widening: " + strings.Join(plan.notes, " ") + " " + never
	}
	effects := []string{
		"Hive sends PUT " + delegateSettingsPath + " with only the settings listed here. The per-repository map goes as its whole new value, built from the settings read for this preview, so repositories you did not name are kept.",
		"The endpoint validates the change, saves it, records a config_decisions_delegate audit entry with you as the actor, and applies it without a restart. It is owner only.",
		"write_confirm reads these settings again and refuses if any of them changed after this preview.",
	}
	if len(plan.unchanged) > 0 {
		effects = append(effects, "Already as requested, so not sent: "+strings.Join(plan.unchanged, ", ")+".")
	}
	details := map[string]any{"fields": fields, "changes": changes}
	if len(plan.unchanged) > 0 {
		details["unchanged"] = plan.unchanged
	}
	return WritePreview{
		Operation:           WriteOpDecisionsDelegateSettings,
		Summary:             "Change Let hive decide settings: " + strings.Join(fields, ", "),
		Target:              "Let hive decide settings (decisions.delegate)",
		Request:             WriteRequest{Method: http.MethodPut, Path: delegateSettingsPath, Body: plan.body},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing Let hive decide settings: " + strings.Join(lines, "; ") + ".",
		Details:             details,
		StateFingerprint:    plan.fingerprint,
	}, nil
}

// ConfirmGuard reads the settings again and returns the fingerprint of the
// before-values the confirmation would overwrite.
func (decisionsDelegateSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	plan, err := delegateSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the Let hive decide settings again before writing, and that check refused this change; preview it again: %w", err)
	}
	return plan.fingerprint, nil
}

type delegateSettingsArgs struct {
	enabled       *bool
	repos         map[string]*bool // nil value drops the override
	repoOrder     []string
	llmEnabled    *bool
	llmModel      *string
	effectTimeout *string
}

type delegateSettingsRow struct {
	field         string
	before, after any
}

type delegateSettingsPlan struct {
	rows        []delegateSettingsRow
	unchanged   []string
	notes       []string
	body        map[string]any
	fingerprint string
}

func delegateSettingsParseArgs(args map[string]any) (delegateSettingsArgs, error) {
	var out delegateSettingsArgs
	if len(args) == 0 {
		return out, fmt.Errorf("at least one Let hive decide setting is required: enabled, repos, llm or effect_timeout")
	}
	for _, key := range SortKeys(args) {
		raw := args[key]
		switch key {
		case "enabled":
			v, ok := raw.(bool)
			if !ok {
				return out, fmt.Errorf("enabled must be true or false")
			}
			out.enabled = &v
		case "repos":
			m, ok := raw.(map[string]any)
			if !ok || len(m) == 0 {
				return out, fmt.Errorf("repos takes {\"owner/repo\": true | false | null}")
			}
			out.repos = map[string]*bool{}
			for _, repo := range SortKeys(m) {
				name := strings.TrimSpace(repo)
				if name == "" || strings.ContainsAny(name, " \t#") || strings.Count(name, "/") > 1 {
					return out, fmt.Errorf("repos: %q is not an owner/repo name", repo)
				}
				switch v := m[repo].(type) {
				case nil:
					out.repos[name] = nil
				case bool:
					b := v
					out.repos[name] = &b
				default:
					return out, fmt.Errorf("repos[%s] must be true, false or null", name)
				}
				out.repoOrder = append(out.repoOrder, name)
			}
		case "llm":
			m, ok := raw.(map[string]any)
			if !ok || len(m) == 0 {
				return out, fmt.Errorf("llm takes {\"enabled\": bool, \"model\": \"name\"}")
			}
			for _, sub := range SortKeys(m) {
				switch sub {
				case "enabled":
					v, ok := m[sub].(bool)
					if !ok {
						return out, fmt.Errorf("llm.enabled must be true or false")
					}
					out.llmEnabled = &v
				case "model":
					s, ok := m[sub].(string)
					if !ok {
						return out, fmt.Errorf("llm.model must be a string")
					}
					s = strings.TrimSpace(s)
					if err := delegateSettingsCheckModel(s); err != nil {
						return out, err
					}
					out.llmModel = &s
				default:
					return out, fmt.Errorf("unsupported Let hive decide setting %q", "llm."+sub)
				}
			}
		case "effect_timeout":
			s, ok := raw.(string)
			if !ok {
				return out, fmt.Errorf("effect_timeout must be a duration string such as 30m, or empty for the default")
			}
			s = strings.TrimSpace(s)
			if s != "" {
				d, err := time.ParseDuration(s)
				if err != nil || d <= 0 {
					return out, fmt.Errorf("effect_timeout must be a duration above zero, such as 30m or 2h, or empty for the default")
				}
				s = d.String()
			}
			out.effectTimeout = &s
		default:
			return out, fmt.Errorf("unsupported Let hive decide setting %q", key)
		}
	}
	return out, nil
}

// delegateSettingsModelPattern and delegateSettingsKeyRun mirror the endpoint's llm.model
// check, so a pasted key is refused before it reaches a stored confirmation.
var (
	delegateSettingsModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	delegateSettingsKeyRun       = regexp.MustCompile(`[A-Za-z0-9]{32,}`)
	delegateSettingsKeyPrefixes  = []string{"sk-", "sk_", "ghp_", "gho_", "ghs_", "ghu_", "github_pat_", "xoxb-", "xoxp-", "hf_", "akia", "aiza", "bearer"}
)

func delegateSettingsCheckModel(model string) error {
	if model == "" {
		return nil
	}
	lower := strings.ToLower(model)
	for _, p := range delegateSettingsKeyPrefixes {
		if strings.HasPrefix(lower, p) {
			return fmt.Errorf("llm.model looks like an API key or token; it takes a model name only, and the model's key is set on the governor reviewer endpoint by environment variable or file")
		}
	}
	if delegateSettingsKeyRun.MatchString(model) {
		return fmt.Errorf("llm.model looks like an API key or token; it takes a model name only, and the model's key is set on the governor reviewer endpoint by environment variable or file")
	}
	if len(model) > 200 || !delegateSettingsModelPattern.MatchString(model) {
		return fmt.Errorf("llm.model must be a model name such as provider/model or model:tag")
	}
	return nil
}

func delegateSettingsPlanFor(ctx context.Context, args map[string]any, provider Provider) (delegateSettingsPlan, error) {
	parsed, err := delegateSettingsParseArgs(args)
	if err != nil {
		return delegateSettingsPlan{}, err
	}
	current, err := delegateSettingsRead(ctx, provider)
	if err != nil {
		return delegateSettingsPlan{}, err
	}
	plan := delegateSettingsPlan{body: map[string]any{}}
	before := map[string]any{}
	projectRepos := stringListValueLoose(current["project_repos"])

	hiveBefore, ok := current["enabled"].(bool)
	if !ok {
		return delegateSettingsPlan{}, fmt.Errorf("this hive's Let hive decide settings do not report enabled; the hive may run an older build")
	}
	hiveAfter := hiveBefore
	if parsed.enabled != nil {
		before["enabled"] = hiveBefore
		if *parsed.enabled == hiveBefore {
			plan.unchanged = append(plan.unchanged, "enabled")
		} else {
			hiveAfter = *parsed.enabled
			plan.body["enabled"] = hiveAfter
			plan.rows = append(plan.rows, delegateSettingsRow{field: "enabled", before: hiveBefore, after: hiveAfter})
		}
	}

	reposBefore := delegateSettingsRepos(current["repos"])
	reposAfter := reposBefore
	if parsed.repos != nil {
		merged, rows, unchanged, err := delegateSettingsMergeRepos(reposBefore, parsed, projectRepos)
		if err != nil {
			return delegateSettingsPlan{}, err
		}
		before["repos"] = reposBefore
		plan.unchanged = append(plan.unchanged, unchanged...)
		if len(rows) > 0 {
			reposAfter = merged
			body := map[string]any{}
			for repo, v := range merged {
				body[repo] = map[string]any{"enabled": v}
			}
			plan.body["repos"] = body
			plan.rows = append(plan.rows, rows...)
		}
	}

	llm, _ := current["llm"].(map[string]any)
	llmBefore, _ := llm["enabled"].(bool)
	llmAfter := llmBefore
	modelBefore, _ := llm["model"].(string)
	modelMasked, _ := llm["model_masked"].(bool)
	modelShown := any(modelBefore)
	if modelMasked {
		modelShown = "(masked: the stored value looks like a key)"
	}
	modelAfter := modelBefore
	llmBody := map[string]any{}
	if parsed.llmEnabled != nil {
		before["llm.enabled"] = llmBefore
		if *parsed.llmEnabled == llmBefore {
			plan.unchanged = append(plan.unchanged, "llm.enabled")
		} else {
			llmAfter = *parsed.llmEnabled
			llmBody["enabled"] = llmAfter
			plan.rows = append(plan.rows, delegateSettingsRow{field: "llm.enabled", before: llmBefore, after: llmAfter})
		}
	}
	if parsed.llmModel != nil {
		before["llm.model"] = []any{modelBefore, modelMasked}
		if *parsed.llmModel == modelBefore && !modelMasked {
			plan.unchanged = append(plan.unchanged, "llm.model")
		} else {
			modelAfter = *parsed.llmModel
			llmBody["model"] = modelAfter
			plan.rows = append(plan.rows, delegateSettingsRow{field: "llm.model", before: modelShown, after: modelAfter})
		}
	}
	if len(llmBody) > 0 {
		plan.body["llm"] = llmBody
	}

	if parsed.effectTimeout != nil {
		was, _ := current["effect_timeout"].(string)
		before["effect_timeout"] = was
		if delegateSettingsDurationEqual(was, *parsed.effectTimeout) {
			plan.unchanged = append(plan.unchanged, "effect_timeout")
		} else {
			plan.body["effect_timeout"] = *parsed.effectTimeout
			plan.rows = append(plan.rows, delegateSettingsRow{field: "effect_timeout", before: delegateSettingsOrDefault(was), after: delegateSettingsOrDefault(*parsed.effectTimeout)})
		}
	}

	if len(plan.rows) == 0 {
		return delegateSettingsPlan{}, fmt.Errorf("nothing would change: %s already as requested", strings.Join(plan.unchanged, ", "))
	}

	effectiveModel, _ := current["llm_model_effective"].(string)
	if parsed.llmModel != nil {
		effectiveModel = modelAfter
	}
	ready, _ := current["llm_reviewer_ready"].(bool)
	path, _ := current["llm_reviewer_path"].(string)
	reason, _ := current["llm_reviewer_reason"].(string)
	plan.notes = delegateSettingsWidening(delegateSettingsWidenInput{
		hiveBefore: hiveBefore, hiveAfter: hiveAfter,
		reposBefore: reposBefore, reposAfter: reposAfter,
		projectRepos: projectRepos,
		llmBefore:    llmBefore, llmAfter: llmAfter,
		modelChanged: parsed.llmModel != nil && (modelAfter != modelBefore || modelMasked),
		model:        effectiveModel, reviewerReady: ready,
		reviewerPath: path, reviewerReason: reason,
	})
	plan.fingerprint = settingsFingerprint(before)
	return plan, nil
}

// delegateSettingsRead reads settings_read section decisions_delegate (GET
// /api/config/decisions/delegate), which only a verified owner may read.
func delegateSettingsRead(ctx context.Context, provider Provider) (map[string]any, error) {
	if provider == nil {
		return nil, fmt.Errorf("%s reads the current Let hive decide settings first, and this connection cannot read them", WriteOpDecisionsDelegateSettings)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": delegateSettingsSection})
	if err != nil {
		return nil, fmt.Errorf("%s needs the current Let hive decide settings, which only the hive's verified owner can read: %w", WriteOpDecisionsDelegateSettings, err)
	}
	m, ok := data.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("%s could not read the current Let hive decide settings", WriteOpDecisionsDelegateSettings)
	}
	return m, nil
}

// delegateSettingsRepos reads the endpoint's {"owner/repo": {"enabled": bool}}.
func delegateSettingsRepos(v any) map[string]bool {
	out := map[string]bool{}
	m, _ := v.(map[string]any)
	for repo, entry := range m {
		e, _ := entry.(map[string]any)
		if b, ok := e["enabled"].(bool); ok {
			out[repo] = b
		}
	}
	return out
}

// delegateSettingsMergeRepos applies the per-repository changes to the current map.
// A name matches an existing key case-insensitively, a bare name through
// project.repos; a new key is stored as owner/repo.
func delegateSettingsMergeRepos(current map[string]bool, parsed delegateSettingsArgs, projectRepos []string) (map[string]bool, []delegateSettingsRow, []string, error) {
	out := make(map[string]bool, len(current))
	for k, v := range current {
		out[k] = v
	}
	var rows []delegateSettingsRow
	var unchanged []string
	seen := map[string]bool{}
	for _, name := range parsed.repoOrder {
		full := qualifyProjectRepo(name, projectRepos)
		lower := strings.ToLower(full)
		if seen[lower] {
			return nil, nil, nil, fmt.Errorf("repos names %s twice", full)
		}
		seen[lower] = true
		existing := ""
		for _, k := range delegateSettingsSortedKeys(out) {
			if strings.EqualFold(qualifyProjectRepo(k, projectRepos), full) {
				existing = k
			}
		}
		want := parsed.repos[name]
		field := "repos[" + full + "]"
		if want == nil {
			if existing == "" {
				unchanged = append(unchanged, field)
				continue
			}
			rows = append(rows, delegateSettingsRow{field: field, before: out[existing], after: "inherit"})
			delete(out, existing)
			continue
		}
		key := full
		if existing != "" {
			key = existing
			if out[existing] == *want {
				unchanged = append(unchanged, field)
				continue
			}
			rows = append(rows, delegateSettingsRow{field: field, before: out[existing], after: *want})
		} else {
			rows = append(rows, delegateSettingsRow{field: field, before: "inherit", after: *want})
		}
		out[key] = *want
	}
	return out, rows, unchanged, nil
}

type delegateSettingsWidenInput struct {
	hiveBefore, hiveAfter   bool
	reposBefore, reposAfter map[string]bool
	projectRepos            []string
	llmBefore, llmAfter     bool
	modelChanged            bool
	model                   string
	reviewerReady           bool
	// reviewerPath and reviewerReason are llm_reviewer_path and
	// llm_reviewer_reason from the same settings read: where the decider
	// turn runs (governor.reviewer_backend) and why it cannot, if it cannot.
	reviewerPath, reviewerReason string
}

// delegateSettingsWidening says what the change lets the hive do that it could not
// before: decide in more repositories, or send item text to a model.
func delegateSettingsWidening(in delegateSettingsWidenInput) []string {
	var notes []string
	effective := func(repo string, repos map[string]bool, hive bool) bool {
		for k, v := range repos {
			if strings.EqualFold(qualifyProjectRepo(k, in.projectRepos), qualifyProjectRepo(repo, in.projectRepos)) {
				return v
			}
		}
		return hive
	}
	names := map[string]bool{}
	for _, r := range in.projectRepos {
		names[r] = true
	}
	for _, m := range []map[string]bool{in.reposBefore, in.reposAfter} {
		for k := range m {
			names[qualifyProjectRepo(k, in.projectRepos)] = true
		}
	}
	var opened []string
	for _, repo := range delegateSettingsSortedKeys(names) {
		if !effective(repo, in.reposBefore, in.hiveBefore) && effective(repo, in.reposAfter, in.hiveAfter) {
			opened = append(opened, repo)
		}
	}
	if !in.hiveBefore && in.hiveAfter {
		opened = append(opened, "every other repository without its own override")
	}
	if len(opened) > 0 {
		note := "Let hive decide is turned on for " + strings.Join(opened, ", ") + ". There, anyone who may already hold items can have the hive decide a needs-decision item as the owner's delegate (the dashboard's Let hive decide and repository.item_delegate), and the reviewer-question sweep (governor.review.auto_decide_questions) may run it by itself on the hive's own pull requests."
		if in.llmAfter {
			note += " With the LLM step on, those items' text may be sent to the model."
		}
		notes = append(notes, note)
	}
	model := strings.TrimSpace(in.model)
	via := " through the governor reviewer endpoint"
	if in.reviewerPath == "claude-cli" {
		via = " through the Claude CLI on the hive's Claude login, run with every tool off"
		if model == "" {
			model = "the Claude CLI's default model"
		}
	}
	if model == "" {
		model = "the governor reviewer model"
	}
	switch {
	case !in.llmBefore && in.llmAfter:
		note := "The LLM step is turned on: for an item the rules leave at the reversible default, the item's text (title, body, comments and linked items) is sent to " + model + via + ", and its verdict goes through the same safety rails as a rule decision."
		switch {
		case in.reviewerReady:
		case strings.TrimSpace(in.reviewerReason) != "":
			note += " The decider cannot run yet (" + strings.TrimSpace(in.reviewerReason) + "), so until it can, every decision keeps the rules' reversible default."
		default:
			note += " No reviewer endpoint or model is configured yet, so until one is, every decision keeps the rules' reversible default."
		}
		notes = append(notes, note)
	case in.llmAfter && in.modelChanged:
		notes = append(notes, "The LLM step stays on and item text is now sent to "+model+".")
	}
	return notes
}

func delegateSettingsSortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func delegateSettingsDurationEqual(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return a == b
	}
	da, err1 := time.ParseDuration(a)
	db, err2 := time.ParseDuration(b)
	return err1 == nil && err2 == nil && da == db
}

func delegateSettingsOrDefault(s string) string {
	if strings.TrimSpace(s) == "" {
		return "default (30m)"
	}
	return s
}
