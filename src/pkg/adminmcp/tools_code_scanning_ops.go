package adminmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// code_scanning.settings changes the code scanning sweep settings
// (code_scanning, src/docs/code-scanning.md) through PUT
// /api/config/code-scanning, the owner only endpoint behind the dashboard's
// Settings → Features → Code scanning issues controls. Read them with
// settings_read section code_scanning.
//
// The endpoint replaces the per-repository override map whole when it is
// sent, so this op takes one change per repository (the fields to set, or null
// to drop the override), reads the current settings while previewing, and
// sends the merged map. The candidate is checked with
// config.ValidateCodeScanningSettings, the checks hive.yaml loading and the
// endpoint apply, so a preview is refused with the same message the endpoint
// would give. ConfirmGuard reads the settings again at write_confirm and
// refuses when any of them moved after the preview.
//
// The op starts no sweep pass and files nothing itself. It changes where the
// sweep runs and what it files on its next pass.
const (
	WriteOpCodeScanningSettings = "code_scanning.settings"

	codeScanningSettingsPath = "/api/config/code-scanning"

	// codeScanningSettingsSection is the settings_read section the preview reads.
	codeScanningSettingsSection = "code_scanning"

	codeScanningSettingsTarget = "Code scanning sweep settings (code_scanning)"
)

// codeScanningSettingsFields and codeScanningSettingsRepoFields are the body
// fields of PUT /api/config/code-scanning and of one repos entry. The op
// offers exactly these; the dashboard package checks them against the
// handler's body type.
var (
	codeScanningSettingsFields     = []string{"enabled", "interval", "min_severity", "include_tools", "exclude_rules", "max_new_issues_per_run", "repos"}
	codeScanningSettingsRepoFields = []string{"enabled", "min_severity", "include_tools", "exclude_rules", "max_new_issues_per_run"}
)

// CodeScanningSettingsBodyFields returns the fields code_scanning.settings
// takes: the top-level ones, and those of one repos entry.
func CodeScanningSettingsBodyFields() (top, repo []string) {
	return append([]string(nil), codeScanningSettingsFields...), append([]string(nil), codeScanningSettingsRepoFields...)
}

func codeScanningWriteOps() []WriteOp { return []WriteOp{codeScanningSettingsOp{}} }

type codeScanningSettingsOp struct{}

func (codeScanningSettingsOp) Name() string { return WriteOpCodeScanningSettings }
func (codeScanningSettingsOp) Description() string {
	return "Change the code scanning sweep settings (code_scanning) through PUT " + codeScanningSettingsPath + ". Owner only. Read them first with settings_read section " + codeScanningSettingsSection + ". Pass only what changes: enabled (the sweep on in every repository), interval (a duration of at least 10m such as 30m, or empty for the default), min_severity (critical, high, medium, low or note; empty for the default), include_tools and exclude_rules (the whole list; [] clears it), max_new_issues_per_run (0 for the default) and repos ({\"owner/repo\": {\"enabled\": true | false | null, \"min_severity\", \"include_tools\", \"exclude_rules\", \"max_new_issues_per_run\"}}, or null to drop that repository's override). In a repos entry only the fields you name change, enabled null makes the repository follow the hive-wide switch and keeps its other overrides, and repositories you do not name are kept. The preview reads the current settings, checks the result as hive.yaml loading does, shows each change before and after, and names the repositories the sweep is turned on for. write_confirm refuses if any code scanning setting changed after the preview. The sweep opens issues only; this operation starts no pass and files nothing itself."
}

func (codeScanningSettingsOp) InputSchema() map[string]any {
	list := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enabled":                map[string]any{"type": "boolean", "description": "code_scanning.enabled: the sweep on in every repository in project.repos (default off). A repos entry wins."},
			"interval":               map[string]any{"type": "string", "description": "code_scanning.interval: time between passes, a duration of at least 10m such as 30m or 2h, or empty for the default (1h)."},
			"min_severity":           map[string]any{"type": "string", "description": "code_scanning.min_severity: the lowest severity filed (critical, high, medium, low or note), or empty for the default (high)."},
			"include_tools":          list("code_scanning.include_tools: file only alerts from these tools (CodeQL, Scorecard, Trivy, ...). The whole list; [] files every tool."),
			"exclude_rules":          list("code_scanning.exclude_rules: rule-ID patterns left out on top of the default list; \"!<rule>\" takes a default back out. The whole list; [] clears it."),
			"max_new_issues_per_run": map[string]any{"type": "integer", "minimum": 0, "description": "code_scanning.max_new_issues_per_run: the most new issues per repository per pass, or 0 for the default (5)."},
			"repos": map[string]any{
				"type":        "object",
				"description": "owner/repo (or a project repository's bare name) to the override fields to set there, or null to drop that repository's override. Fields you do not name keep their value; repositories you do not name are kept.",
				"additionalProperties": map[string]any{"anyOf": []any{
					map[string]any{"type": "null"},
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"enabled":                map[string]any{"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "null"}}, "description": "true or false to turn the sweep on or off here, null to follow the hive-wide switch."},
							"min_severity":           map[string]any{"type": "string", "description": "Empty inherits the top-level value."},
							"include_tools":          list("[] inherits the top-level list."),
							"exclude_rules":          list("[] inherits the top-level list."),
							"max_new_issues_per_run": map[string]any{"type": "integer", "minimum": 0, "description": "0 inherits the top-level value."},
						},
						"additionalProperties": false,
					},
				}},
			},
		},
		"required":             []string{},
		"additionalProperties": false,
	}
}

// Preview cannot read the current settings, so it only checks the arguments.
// write_preview uses PreviewWithProvider; write_confirm calls Preview to check
// the request still names this endpoint, and then ConfirmGuard reads again.
func (codeScanningSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := codeScanningSettingsParseArgs(args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpCodeScanningSettings,
		Summary:             "Change code scanning sweep settings (current settings not read)",
		Target:              codeScanningSettingsTarget,
		Request:             WriteRequest{Method: http.MethodPut, Path: codeScanningSettingsPath},
		WideningDisclosure:  "Potential widening: the current code scanning settings were not read, so this preview cannot say what would change.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}, nil
}

func (codeScanningSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	plan, err := codeScanningSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return WritePreview{}, err
	}
	fields := make([]string, 0, len(plan.changes))
	changes := make([]map[string]any, 0, len(plan.changes))
	lines := make([]string, 0, len(plan.changes))
	for _, ch := range plan.changes {
		fields = append(fields, ch.Key)
		changes = append(changes, map[string]any{"field": ch.Key, "before": ch.Before, "after": ch.After})
		lines = append(lines, ch.Key+": "+ch.Before+" → "+ch.After)
	}
	return WritePreview{
		Operation: WriteOpCodeScanningSettings,
		Summary:   "Change code scanning sweep settings: " + strings.Join(fields, ", "),
		Target:    codeScanningSettingsTarget,
		Request:   WriteRequest{Method: http.MethodPut, Path: codeScanningSettingsPath, Body: plan.body},
		Effects: []string{
			"Hive sends PUT " + codeScanningSettingsPath + " with only the settings listed here. The per-repository map goes as its whole new value, built from the settings read for this preview, so repositories you did not name are kept.",
			"The endpoint checks the change as hive.yaml loading does, saves it, records one config_code_scanning audit entry per changed key with you as the actor, and the sweep reads it on its next pass, without a restart. It is owner only.",
			"write_confirm reads the code scanning settings again and refuses if any of them changed after this preview.",
		},
		WideningDisclosure:  plan.disclosure,
		ConfirmationMessage: "Confirm changing code scanning sweep settings: " + strings.Join(lines, "; ") + ".",
		Details:             map[string]any{"fields": fields, "changes": changes},
		StateFingerprint:    plan.fingerprint,
	}, nil
}

// ConfirmGuard reads the settings again and returns their fingerprint.
func (codeScanningSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	plan, err := codeScanningSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the code scanning settings again before writing, and that check refused this change; preview it again: %w", err)
	}
	return plan.fingerprint, nil
}

// codeScanningSettingsRepoArgs is one repos argument: the fields to set in
// that repository's override. A nil pointer leaves the field as it is.
type codeScanningSettingsRepoArgs struct {
	// drop removes the whole override (the argument was null).
	drop bool
	// enabledSet is true when enabled was named; enabled nil then means
	// "follow the hive-wide switch".
	enabledSet   bool
	enabled      *bool
	minSeverity  *string
	includeTools *[]string
	excludeRules *[]string
	maxNew       *int
}

type codeScanningSettingsArgs struct {
	Enabled            *bool                      `json:"enabled"`
	Interval           *string                    `json:"interval"`
	MinSeverity        *string                    `json:"min_severity"`
	IncludeTools       *[]string                  `json:"include_tools"`
	ExcludeRules       *[]string                  `json:"exclude_rules"`
	MaxNewIssuesPerRun *int                       `json:"max_new_issues_per_run"`
	Repos              map[string]json.RawMessage `json:"repos"`

	repos     map[string]codeScanningSettingsRepoArgs
	repoOrder []string
}

// codeScanningSettingsDecode decodes one JSON value into dst, refusing any
// field dst does not have.
func codeScanningSettingsDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func codeScanningSettingsParseArgs(args map[string]any) (codeScanningSettingsArgs, error) {
	var out codeScanningSettingsArgs
	if len(args) == 0 {
		return out, fmt.Errorf("at least one code scanning setting is required: %s", strings.Join(codeScanningSettingsFields, ", "))
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return out, fmt.Errorf("code scanning settings are not valid JSON: %w", err)
	}
	if err := codeScanningSettingsDecode(raw, &out); err != nil {
		return out, fmt.Errorf("unsupported or malformed code scanning setting (takes %s): %w", strings.Join(codeScanningSettingsFields, ", "), err)
	}
	if _, named := args["repos"]; named && len(out.Repos) == 0 {
		return out, fmt.Errorf("repos takes {\"owner/repo\": {\"enabled\": true | false | null, ...}} or {\"owner/repo\": null}")
	}
	out.repos = map[string]codeScanningSettingsRepoArgs{}
	for _, key := range sortedRawKeys(out.Repos) {
		name := strings.TrimSpace(key)
		if name == "" || strings.ContainsAny(name, " \t#") || strings.Count(name, "/") > 1 {
			return out, fmt.Errorf("repos: %q is not an owner/repo name", key)
		}
		entry, err := codeScanningSettingsParseRepo(name, out.Repos[key])
		if err != nil {
			return out, err
		}
		out.repos[name] = entry
		out.repoOrder = append(out.repoOrder, name)
	}
	return out, nil
}

func sortedRawKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func codeScanningSettingsParseRepo(name string, raw json.RawMessage) (codeScanningSettingsRepoArgs, error) {
	var out codeScanningSettingsRepoArgs
	if text := strings.TrimSpace(string(raw)); text == "" || text == "null" {
		out.drop = true
		return out, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return out, fmt.Errorf("repos[%s] takes the override fields to set (%s), or null to drop the override", name, strings.Join(codeScanningSettingsRepoFields, ", "))
	}
	var typed struct {
		Enabled            *bool     `json:"enabled"`
		MinSeverity        *string   `json:"min_severity"`
		IncludeTools       *[]string `json:"include_tools"`
		ExcludeRules       *[]string `json:"exclude_rules"`
		MaxNewIssuesPerRun *int      `json:"max_new_issues_per_run"`
	}
	if err := codeScanningSettingsDecode(raw, &typed); err != nil {
		return out, fmt.Errorf("repos[%s]: unsupported or malformed override field (takes %s): %w", name, strings.Join(codeScanningSettingsRepoFields, ", "), err)
	}
	_, out.enabledSet = fields["enabled"]
	out.enabled = typed.Enabled
	out.minSeverity = typed.MinSeverity
	out.includeTools = typed.IncludeTools
	out.excludeRules = typed.ExcludeRules
	out.maxNew = typed.MaxNewIssuesPerRun
	return out, nil
}

// codeScanningSettingsView is the part of GET /api/config/code-scanning the
// op builds on: the block as it is set, project.org and project.repos. The
// effective values and the per-repository access state are left out, so a
// sweep pass between preview and confirm does not make the preview stale.
type codeScanningSettingsView struct {
	Org                string   `json:"org"`
	Enabled            *bool    `json:"enabled"`
	Interval           string   `json:"interval"`
	MinSeverity        string   `json:"min_severity"`
	IncludeTools       []string `json:"include_tools"`
	ExcludeRules       []string `json:"exclude_rules"`
	MaxNewIssuesPerRun int      `json:"max_new_issues_per_run"`
	Repos              map[string]struct {
		Enabled            *bool    `json:"enabled"`
		MinSeverity        string   `json:"min_severity"`
		IncludeTools       []string `json:"include_tools"`
		ExcludeRules       []string `json:"exclude_rules"`
		MaxNewIssuesPerRun int      `json:"max_new_issues_per_run"`
	} `json:"repos"`
	ProjectRepos []struct {
		Repo string `json:"repo"`
	} `json:"project_repos"`
}

func (v codeScanningSettingsView) projectRepos() []string {
	out := make([]string, 0, len(v.ProjectRepos))
	for _, p := range v.ProjectRepos {
		out = append(out, p.Repo)
	}
	return out
}

// block is the view as a code_scanning block.
func (v codeScanningSettingsView) block() (config.CodeScanningConfig, error) {
	cs := config.CodeScanningConfig{
		MinSeverity:        v.MinSeverity,
		IncludeTools:       codeScanningSettingsList(v.IncludeTools),
		ExcludeRules:       codeScanningSettingsList(v.ExcludeRules),
		MaxNewIssuesPerRun: v.MaxNewIssuesPerRun,
	}
	if v.Enabled != nil {
		cs.Enabled = *v.Enabled
	}
	if v.Interval != "" {
		d, err := time.ParseDuration(v.Interval)
		if err != nil {
			return cs, fmt.Errorf("the hive reports a code scanning interval that is not a duration: %q", v.Interval)
		}
		cs.Interval = d
	}
	if len(v.Repos) > 0 {
		cs.Repos = make(map[string]config.CodeScanningRepoConfig, len(v.Repos))
		for key, r := range v.Repos {
			cs.Repos[key] = config.CodeScanningRepoConfig{
				Enabled:            r.Enabled,
				MinSeverity:        r.MinSeverity,
				IncludeTools:       codeScanningSettingsList(r.IncludeTools),
				ExcludeRules:       codeScanningSettingsList(r.ExcludeRules),
				MaxNewIssuesPerRun: r.MaxNewIssuesPerRun,
			}
		}
	}
	return cs, nil
}

// codeScanningSettingsList copies a list; an empty one becomes nil, the form
// an unset list has in the config.
func codeScanningSettingsList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}

// codeScanningSettingsTrim trims every entry and, when dropBlank is set,
// leaves blank ones out. Rule patterns keep a blank entry so the hive.yaml
// check refuses it with its own message.
func codeScanningSettingsTrim(in []string, dropBlank bool) []string {
	var out []string
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" && dropBlank {
			continue
		}
		out = append(out, item)
	}
	return out
}

type codeScanningSettingsPlan struct {
	changes     []config.CodeScanningChange
	body        map[string]any
	disclosure  string
	fingerprint string
}

func codeScanningSettingsPlanFor(ctx context.Context, args map[string]any, provider Provider) (codeScanningSettingsPlan, error) {
	parsed, err := codeScanningSettingsParseArgs(args)
	if err != nil {
		return codeScanningSettingsPlan{}, err
	}
	view, err := codeScanningSettingsRead(ctx, provider)
	if err != nil {
		return codeScanningSettingsPlan{}, err
	}
	before, err := view.block()
	if err != nil {
		return codeScanningSettingsPlan{}, err
	}
	after, err := parsed.apply(view.Org, before)
	if err != nil {
		return codeScanningSettingsPlan{}, err
	}
	projectRepos := view.projectRepos()
	// The checks, and the messages, the endpoint and hive.yaml loading use.
	if err := config.ValidateCodeScanningSettings(view.Org, projectRepos, after); err != nil {
		return codeScanningSettingsPlan{}, err
	}
	changes := config.CodeScanningChanges(before, after)
	if len(changes) == 0 {
		return codeScanningSettingsPlan{}, fmt.Errorf("nothing would change: the code scanning settings are already as requested")
	}
	plan := codeScanningSettingsPlan{changes: changes, body: map[string]any{}}
	filterChanged, reposChanged := false, false
	for _, ch := range changes {
		switch ch.Key {
		case "enabled":
			plan.body["enabled"] = after.Enabled
		case "interval":
			plan.body["interval"] = ""
			if after.Interval != 0 {
				plan.body["interval"] = after.Interval.String()
			}
		case "min_severity":
			plan.body["min_severity"] = after.MinSeverity
			filterChanged = true
		case "include_tools":
			plan.body["include_tools"] = append([]string{}, after.IncludeTools...)
			filterChanged = true
		case "exclude_rules":
			plan.body["exclude_rules"] = append([]string{}, after.ExcludeRules...)
			filterChanged = true
		case "max_new_issues_per_run":
			plan.body["max_new_issues_per_run"] = after.MaxNewIssuesPerRun
			filterChanged = true
		default:
			// repos[<key>]: the endpoint takes the map whole.
			reposChanged = true
		}
	}
	if reposChanged {
		repos := map[string]any{}
		for key, rc := range after.Repos {
			entry := map[string]any{
				"enabled":                nil,
				"min_severity":           rc.MinSeverity,
				"include_tools":          append([]string{}, rc.IncludeTools...),
				"exclude_rules":          append([]string{}, rc.ExcludeRules...),
				"max_new_issues_per_run": rc.MaxNewIssuesPerRun,
			}
			if rc.Enabled != nil {
				entry["enabled"] = *rc.Enabled
			}
			repos[key] = entry
		}
		plan.body["repos"] = repos
	}

	var opened []string
	stillOn := false
	for _, repo := range projectRepos {
		_, was := before.For(view.Org, repo)
		_, is := after.For(view.Org, repo)
		if is {
			stillOn = true
		}
		if !was && is {
			opened = append(opened, repo)
		}
	}
	never := "The sweep opens issues only: it never opens a pull request, dismisses an alert or changes the scanning setup, and each of its writes still asks the write-back gate. This operation starts no pass and files nothing itself."
	switch {
	case len(opened) > 0:
		plan.disclosure = "Widening: the code scanning sweep is turned on for " + strings.Join(opened, ", ") + ". On its next pass the hive reads those repositories' open code scanning alerts and files grouped issues from them, then keeps those issues in step with the alerts. " + never
	case stillOn && (filterChanged || reposChanged):
		plan.disclosure = "Possible widening: this changes what the sweep files where it is on (the severity floor, the tools, the excluded rules or the per-pass cap), so it may file issues it left out before. " + never
	default:
		plan.disclosure = "No widening: none of these changes makes the sweep run anywhere new or file anything it left out before. " + never
	}
	plan.fingerprint = settingsFingerprint(map[string]any{"code_scanning": view})
	return plan, nil
}

// apply returns the block the arguments ask for, built on a copy of before.
func (a codeScanningSettingsArgs) apply(org string, before config.CodeScanningConfig) (config.CodeScanningConfig, error) {
	next := before.Clone()
	if a.Enabled != nil {
		next.Enabled = *a.Enabled
	}
	if a.Interval != nil {
		raw := strings.TrimSpace(*a.Interval)
		next.Interval = 0
		if raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return before, fmt.Errorf("interval must be a duration such as 30m or 2h, or empty for the default")
			}
			next.Interval = d
		}
	}
	if a.MinSeverity != nil {
		next.MinSeverity = strings.TrimSpace(*a.MinSeverity)
	}
	if a.IncludeTools != nil {
		next.IncludeTools = codeScanningSettingsTrim(*a.IncludeTools, true)
	}
	if a.ExcludeRules != nil {
		next.ExcludeRules = codeScanningSettingsTrim(*a.ExcludeRules, false)
	}
	if a.MaxNewIssuesPerRun != nil {
		next.MaxNewIssuesPerRun = *a.MaxNewIssuesPerRun
	}
	named := map[string]string{}
	for _, name := range a.repoOrder {
		norm := config.CodeScanningRepoKey(org, name)
		if first, dup := named[norm]; dup {
			return before, fmt.Errorf("repos names the same repository twice: %q and %q", first, name)
		}
		named[norm] = name
		// A repository already in the map keeps the key it is stored under.
		key := name
		for _, held := range sortedRepoConfigKeys(next.Repos) {
			if config.CodeScanningRepoKey(org, held) == norm {
				key = held
				break
			}
		}
		change := a.repos[name]
		if change.drop {
			delete(next.Repos, key)
			continue
		}
		rc := next.Repos[key]
		if change.enabledSet {
			rc.Enabled = nil
			if change.enabled != nil {
				v := *change.enabled
				rc.Enabled = &v
			}
		}
		if change.minSeverity != nil {
			rc.MinSeverity = strings.TrimSpace(*change.minSeverity)
		}
		if change.includeTools != nil {
			rc.IncludeTools = codeScanningSettingsTrim(*change.includeTools, true)
		}
		if change.excludeRules != nil {
			rc.ExcludeRules = codeScanningSettingsTrim(*change.excludeRules, false)
		}
		if change.maxNew != nil {
			rc.MaxNewIssuesPerRun = *change.maxNew
		}
		if rc.IsEmpty() {
			delete(next.Repos, key)
			continue
		}
		if next.Repos == nil {
			next.Repos = map[string]config.CodeScanningRepoConfig{}
		}
		next.Repos[key] = rc
	}
	if len(next.Repos) == 0 {
		next.Repos = nil
	}
	return next, nil
}

func sortedRepoConfigKeys(m map[string]config.CodeScanningRepoConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// codeScanningSettingsRead reads settings_read section code_scanning (GET
// /api/config/code-scanning), which only a verified owner may read.
func codeScanningSettingsRead(ctx context.Context, provider Provider) (codeScanningSettingsView, error) {
	var view codeScanningSettingsView
	if provider == nil {
		return view, fmt.Errorf("%s reads the current code scanning settings first, and this connection cannot read them", WriteOpCodeScanningSettings)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": codeScanningSettingsSection})
	if err != nil {
		return view, fmt.Errorf("%s needs the current code scanning settings, which only the hive's verified owner can read: %w", WriteOpCodeScanningSettings, err)
	}
	raw, err := json.Marshal(data)
	if err == nil {
		err = json.Unmarshal(raw, &view)
	}
	if err != nil || view.Enabled == nil {
		return view, fmt.Errorf("%s could not read the current code scanning settings; the hive may run an older build", WriteOpCodeScanningSettings)
	}
	return view, nil
}
