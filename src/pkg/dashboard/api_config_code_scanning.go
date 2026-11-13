package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/codescanning"
	"github.com/hivecommons/hive/pkg/config"
)

// Code scanning sweep settings: the top-level code_scanning block, owner only
// (src/docs/code-scanning.md).
//
// GET  /api/config/code-scanning returns the block as it is set, the values
// in force once the defaults are resolved, the per-repository overrides, and
// one row per project.repos entry: whether the sweep runs there and whether
// the last sweep pass could read that repository's alerts.
// PUT  /api/config/code-scanning changes it. Every field is optional and an
// absent one is left alone, except repos, which is the FULL per-repository
// override map when sent. Each entry is that repository's whole override:
// {"enabled": true | false | null, "min_severity", "include_tools",
// "exclude_rules", "max_new_issues_per_run"}. enabled null inherits the
// top-level switch and keeps the entry's other fields; an entry that sets
// nothing at all (or is null) is dropped, and {} clears every override. The
// admin MCP operation code_scanning.settings merges one repository's change
// into the current map and sends the whole map.
//
// The candidate block is checked by config.ValidateCodeScanningSettings, the
// same checks and messages hive.yaml loading applies, before anything is
// changed. A change is saved, audited as config_code_scanning (one entry per
// changed key, with the value before and after) and read by the sweep from
// the next config snapshot the governor loop publishes, so it applies without
// a restart. Nothing here starts a sweep pass, files an issue or makes a
// GitHub call.
const auditConfigCodeScanning = "config_code_scanning"

// How a repository's alerts looked to the last sweep pass. The two notice
// kinds (codescanning.NoticeMissingPermission, codescanning.NoticeNotEnabled)
// are the other values of alerts_access.
const (
	// codeScanningAccessOff: the sweep does not run on this repository, so
	// nothing was checked.
	codeScanningAccessOff = "off"
	// codeScanningAccessNotChecked: the sweep is on here but no pass has
	// reached the repository since (it was just turned on, the repository
	// is paused, or the GitHub API budget shed the pass).
	codeScanningAccessNotChecked = "not_checked"
	// codeScanningAccessReadable: the last pass listed the alerts and
	// finished without an error.
	codeScanningAccessReadable = "readable"
	// codeScanningAccessError: the last pass failed for a reason that is
	// not one of the two notices, so it says nothing about access.
	codeScanningAccessError = "error"
)

func (s *Server) registerCodeScanningRoutes() {
	s.mux.HandleFunc("GET /api/config/code-scanning", s.handleCodeScanningConfigGet)
	s.mux.HandleFunc("PUT /api/config/code-scanning", s.handleCodeScanningConfigPut)
}

// codeScanningRepoSetting is one per-repository override, in the GET answer
// and in the PUT body. Every field left empty inherits the top-level value.
type codeScanningRepoSetting struct {
	Enabled            *bool    `json:"enabled"`
	MinSeverity        string   `json:"min_severity"`
	IncludeTools       []string `json:"include_tools"`
	ExcludeRules       []string `json:"exclude_rules"`
	MaxNewIssuesPerRun int      `json:"max_new_issues_per_run"`
}

// codeScanningRepoState is one project.repos entry in the GET answer.
type codeScanningRepoState struct {
	Repo string `json:"repo"`
	// Enabled is whether the sweep runs on this repository. EnabledFrom
	// is "repo" when a per-repository override decides it, else "default".
	Enabled     bool   `json:"enabled"`
	EnabledFrom string `json:"enabled_from"`
	// AlertsAccess is what the last sweep pass recorded: readable,
	// missing_permission, not_enabled, error, not_checked or off. It is
	// never read from GitHub for this answer.
	AlertsAccess       string `json:"alerts_access"`
	AlertsAccessDetail string `json:"alerts_access_detail,omitempty"`
	CheckedAt          string `json:"checked_at,omitempty"`
}

// codeScanningSettings is the GET /api/config/code-scanning answer, also the
// governor settings bundle's code_scanning block.
type codeScanningSettings struct {
	// Org is project.org, which bare repos keys are read under.
	Org     string `json:"org"`
	Enabled bool   `json:"enabled"`
	// Interval, MinSeverity and MaxNewIssuesPerRun are the values as set
	// ("" or 0 when unset). The _effective fields are the values in force
	// and the _default fields what an unset key resolves to.
	Interval                    string   `json:"interval"`
	IntervalEffective           string   `json:"interval_effective"`
	IntervalDefault             string   `json:"interval_default"`
	MinSeverity                 string   `json:"min_severity"`
	MinSeverityEffective        string   `json:"min_severity_effective"`
	MinSeverityDefault          string   `json:"min_severity_default"`
	Severities                  []string `json:"severities"`
	IncludeTools                []string `json:"include_tools"`
	ExcludeRules                []string `json:"exclude_rules"`
	ExcludeRulesDefault         []string `json:"exclude_rules_default"`
	ExcludeRulesEffective       []string `json:"exclude_rules_effective"`
	MaxNewIssuesPerRun          int      `json:"max_new_issues_per_run"`
	MaxNewIssuesPerRunEffective int      `json:"max_new_issues_per_run_effective"`
	MaxNewIssuesPerRunDefault   int      `json:"max_new_issues_per_run_default"`
	// Repos is code_scanning.repos as stored, keyed as in hive.yaml.
	Repos        map[string]codeScanningRepoSetting `json:"repos"`
	ProjectRepos []codeScanningRepoState            `json:"project_repos"`
}

func (s *Server) handleCodeScanningConfigGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.codeScanningSectionResponse(s.deps.Config))
}

// codeScanningSectionResponse renders code_scanning for the dashboard and
// settings_read. The per-repository access state comes from what the sweep
// recorded on its last pass (GHClient.CodeScanningStatus); it makes no GitHub
// call.
func (s *Server) codeScanningSectionResponse(cfg *config.Config) codeScanningSettings {
	cs := cfg.CodeScanning
	org := cfg.Project.Org
	level, _ := codescanning.ParseLevel(cs.MinSeverity)
	policy := codescanning.Policy{MinLevel: level, IncludeTools: cs.IncludeTools, ExcludeRules: cs.ExcludeRules, MaxNewIssues: cs.MaxNewIssuesPerRun}
	out := codeScanningSettings{
		Org:                         org,
		Enabled:                     cs.Enabled,
		Interval:                    settingsDurationString(cs.Interval),
		IntervalEffective:           cs.IntervalOrDefault().String(),
		IntervalDefault:             config.DefaultCodeScanningInterval.String(),
		MinSeverity:                 cs.MinSeverity,
		MinSeverityEffective:        policy.EffectiveMinLevel().String(),
		MinSeverityDefault:          codescanning.DefaultMinLevel.String(),
		Severities:                  codescanning.LevelNames(),
		IncludeTools:                codeScanningList(cs.IncludeTools),
		ExcludeRules:                codeScanningList(cs.ExcludeRules),
		ExcludeRulesDefault:         codeScanningList(codescanning.DefaultExcludeRules),
		ExcludeRulesEffective:       codeScanningList(policy.EffectiveExcludeRules()),
		MaxNewIssuesPerRun:          cs.MaxNewIssuesPerRun,
		MaxNewIssuesPerRunEffective: policy.EffectiveMaxNewIssues(),
		MaxNewIssuesPerRunDefault:   codescanning.DefaultMaxNewIssues,
		Repos:                       map[string]codeScanningRepoSetting{},
		ProjectRepos:                []codeScanningRepoState{},
	}
	for key, rc := range cs.Repos {
		entry := codeScanningRepoSetting{
			MinSeverity:        rc.MinSeverity,
			IncludeTools:       codeScanningList(rc.IncludeTools),
			ExcludeRules:       codeScanningList(rc.ExcludeRules),
			MaxNewIssuesPerRun: rc.MaxNewIssuesPerRun,
		}
		if rc.Enabled != nil {
			v := *rc.Enabled
			entry.Enabled = &v
		}
		out.Repos[key] = entry
	}

	swept := map[string]codescanning.Status{}
	if s != nil && s.deps != nil {
		for _, st := range s.deps.GHClient.CodeScanningStatus().Repos {
			swept[strings.ToLower(st.Repo)] = st
		}
	}
	for _, repo := range cfg.Project.Repos {
		full := config.QualifyRepo(org, repo)
		if full == "" {
			continue
		}
		_, on := cs.For(org, repo)
		row := codeScanningRepoState{Repo: full, Enabled: on, EnabledFrom: "default"}
		want := config.CodeScanningRepoKey(org, repo)
		for key, rc := range cs.Repos {
			if rc.Enabled != nil && config.CodeScanningRepoKey(org, key) == want {
				row.EnabledFrom = "repo"
				break
			}
		}
		st, seen := swept[strings.ToLower(full)]
		row.AlertsAccess, row.AlertsAccessDetail = codeScanningAlertsAccess(on, st, seen)
		if on && seen && !st.CheckedAt.IsZero() {
			row.CheckedAt = st.CheckedAt.UTC().Format(time.RFC3339)
		}
		out.ProjectRepos = append(out.ProjectRepos, row)
	}
	return out
}

// codeScanningAlertsAccess turns the last sweep pass's record of a repository
// into alerts_access. It reports readable only when a pass listed the alerts
// and finished cleanly; a repository the sweep has not reached is not_checked,
// never readable.
func codeScanningAlertsAccess(on bool, st codescanning.Status, seen bool) (state, detail string) {
	switch {
	case !on:
		return codeScanningAccessOff, ""
	case !seen:
		return codeScanningAccessNotChecked, ""
	case st.NoticeKind != "":
		return st.NoticeKind, st.Notice
	case st.Error != "":
		return codeScanningAccessError, st.Error
	}
	return codeScanningAccessReadable, ""
}

// codeScanningList copies a list for the answer; nil becomes [] so the
// dashboard never reads null where it expects a list.
func codeScanningList(in []string) []string {
	return append([]string{}, in...)
}

// codeScanningBody is the PUT body. A nil field is left alone.
type codeScanningBody struct {
	Enabled *bool `json:"enabled"`
	// Interval is a Go duration ("30m", "2h"); "" restores the default.
	Interval *string `json:"interval"`
	// MinSeverity "" restores the default.
	MinSeverity *string `json:"min_severity"`
	// IncludeTools and ExcludeRules are the whole list when sent; [] clears.
	IncludeTools *[]string `json:"include_tools"`
	ExcludeRules *[]string `json:"exclude_rules"`
	// MaxNewIssuesPerRun 0 restores the default.
	MaxNewIssuesPerRun *int `json:"max_new_issues_per_run"`
	// Repos is the FULL override map when present; absent leaves it alone.
	Repos map[string]*codeScanningRepoSetting `json:"repos"`
}

func (s *Server) handleCodeScanningConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body codeScanningBody
	defer closeHTTPBody(r.Body)
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		jsonError(w, "invalid body: expected {enabled, interval, min_severity, include_tools, exclude_rules, max_new_issues_per_run, repos: {owner/repo: {enabled, min_severity, include_tools, exclude_rules, max_new_issues_per_run}}}; no other field is accepted: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		jsonError(w, "invalid body: expected one JSON object", http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	before := cfg.CodeScanning
	next, msg := applyCodeScanningBody(cfg.Project.Org, before, body)
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	// The same checks, with the same messages, as loading hive.yaml, run on
	// the candidate: the live block is untouched until they pass.
	if err := config.ValidateCodeScanningSettings(cfg.Project.Org, cfg.Project.Repos, next); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	changes := config.CodeScanningChanges(before, next)
	if len(changes) == 0 {
		jsonResponse(w, s.codeScanningSectionResponse(cfg))
		return
	}

	cfg.CodeScanning = next
	if err := s.saveConfig(); err != nil {
		cfg.CodeScanning = before
		s.logger.Error("failed to persist config after code_scanning update", "error", err)
		jsonError(w, "failed to persist code scanning settings", http.StatusInternalServerError)
		return
	}
	for _, ch := range changes {
		s.auditFromRequest(r, auditConfigCodeScanning, auditDetail("section", "code_scanning", "key", ch.Key, "before", ch.Before, "after", ch.After), "")
	}
	s.refreshAndPersist()
	jsonResponse(w, s.codeScanningSectionResponse(cfg))
}

// applyCodeScanningBody returns the block the body asks for, built on a copy
// of current so a refused body changes nothing. A non-empty msg is the 400 to
// return. It only shapes the request; whether the result is a valid
// code_scanning block is config.ValidateCodeScanningSettings's call.
func applyCodeScanningBody(org string, current config.CodeScanningConfig, body codeScanningBody) (config.CodeScanningConfig, string) {
	next := current.Clone()
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.Interval != nil {
		raw := strings.TrimSpace(*body.Interval)
		next.Interval = 0
		if raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return current, "interval must be a duration such as 30m or 2h, or empty for the default (" + config.DefaultCodeScanningInterval.String() + ")"
			}
			next.Interval = d
		}
	}
	if body.MinSeverity != nil {
		next.MinSeverity = strings.TrimSpace(*body.MinSeverity)
	}
	if body.IncludeTools != nil {
		list, msg := codeScanningToolList("include_tools", *body.IncludeTools)
		if msg != "" {
			return current, msg
		}
		next.IncludeTools = list
	}
	if body.ExcludeRules != nil {
		list, msg := codeScanningRuleList("exclude_rules", *body.ExcludeRules)
		if msg != "" {
			return current, msg
		}
		next.ExcludeRules = list
	}
	if body.MaxNewIssuesPerRun != nil {
		next.MaxNewIssuesPerRun = *body.MaxNewIssuesPerRun
	}
	if body.Repos != nil {
		repos, msg := codeScanningReposFromBody(org, body.Repos)
		if msg != "" {
			return current, msg
		}
		next.Repos = repos
	}
	return next, ""
}

// codeScanningReposFromBody builds the override map a PUT sends. Keys are
// stored as sent (owner/repo, or the bare name under project.org). An entry
// is dropped only when it sets nothing: enabled null with every other field
// empty, or a null entry. enabled null with other fields keeps those fields
// and inherits the switch. Naming one repository under two spellings is
// refused, because only one of the two entries would ever be read.
func codeScanningReposFromBody(org string, sent map[string]*codeScanningRepoSetting) (map[string]config.CodeScanningRepoConfig, string) {
	keys := make([]string, 0, len(sent))
	for key := range sent {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := map[string]config.CodeScanningRepoConfig{}
	named := map[string]string{}
	for _, key := range keys {
		name := strings.TrimSpace(key)
		if name == "" {
			return nil, "repos: a repository name is empty"
		}
		norm := config.CodeScanningRepoKey(org, name)
		if first, dup := named[norm]; dup {
			return nil, fmt.Sprintf("repos names the same repository twice: %q and %q", first, name)
		}
		named[norm] = name
		entry := sent[key]
		if entry == nil {
			continue
		}
		where := "repos[" + name + "]."
		tools, msg := codeScanningToolList(where+"include_tools", entry.IncludeTools)
		if msg != "" {
			return nil, msg
		}
		rules, msg := codeScanningRuleList(where+"exclude_rules", entry.ExcludeRules)
		if msg != "" {
			return nil, msg
		}
		rc := config.CodeScanningRepoConfig{
			MinSeverity:        strings.TrimSpace(entry.MinSeverity),
			IncludeTools:       tools,
			ExcludeRules:       rules,
			MaxNewIssuesPerRun: entry.MaxNewIssuesPerRun,
		}
		if entry.Enabled != nil {
			v := *entry.Enabled
			rc.Enabled = &v
		}
		if rc.IsEmpty() {
			continue
		}
		out[name] = rc
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// codeScanningToolList trims a tool-name list and drops blank entries.
func codeScanningToolList(field string, in []string) ([]string, string) {
	var out []string
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if msg := codeScanningTextRefusal(field, item); msg != "" {
			return nil, msg
		}
		out = append(out, item)
	}
	return out, ""
}

// codeScanningRuleList trims a rule-pattern list. A blank entry is kept so
// the hive.yaml check refuses it with its own message ("empty rule pattern").
func codeScanningRuleList(field string, in []string) ([]string, string) {
	var out []string
	for _, item := range in {
		item = strings.TrimSpace(item)
		if msg := codeScanningTextRefusal(field, item); msg != "" {
			return nil, msg
		}
		out = append(out, item)
	}
	return out, ""
}

// codeScanningTextRefusal refuses a tool name or rule pattern that hive.yaml
// could not hold as written: loading expands ${NAME} from the environment, so
// a saved placeholder would come back as that variable's value, and a control
// character has no place in either.
func codeScanningTextRefusal(field, value string) string {
	if strings.Contains(value, "${") {
		return field + ": an entry must not contain ${...}"
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return field + ": an entry must not contain control characters"
		}
	}
	return ""
}
