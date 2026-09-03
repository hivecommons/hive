package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Sampling-ratio bounds for the tracing head-based sampler. The config treats a
// zero value as "sample everything" (see TracingConfig.SampleRatio), so the
// accepted operator range is the full closed interval [0.0, 1.0].
const (
	minTracingSampleRatio = 0.0
	maxTracingSampleRatio = 1.0
)

// handleGovernorFeatures updates non-security-posture feature/observability
// settings from the Governor config dialog so operators do not have to hand-edit
// hive.yaml:
//
//   - ioscan            (legacy API compatibility; UI moved to Security)
//   - otel/tracing      (Config.OTel/Tracing Enabled + advanced exporter fields)
//   - retro             (Config.Retro enabled + analysis model)
//   - mint              (Config.Mint.Enabled + Issuer — NOT KeyPath: the signing
//     key is a secret/PEM path and the dashboard overlay is deliberately
//     secret-free)
//   - plan_from_label   (Config.Planning.PlanFromLabel, a *bool tri-state)
//
// Every field is a pointer so an absent key leaves the corresponding config
// untouched — the same "only what you send is changed" contract the other
// governor-config handlers use (see handleGovernorHealth / handleGovernorTrajectory).
// This handler only exposes the features' configuration; it changes none of
// their runtime behavior. saveConfig() persists a secret-free overlay to the
// PVC that the entrypoint merges on restart, so a hosted hive picks the change
// up on its next boot.
//
// OWNER-ONLY (audit F22, 2026-08-14). The body carries OTelEndpoint,
// OTelHeaders, OTelInsecure and TracingEndpoint, which feed a live OTLP
// exporter (pkg/tracing/tracing.go WithEndpointURL/WithHeaders/WithInsecure).
// Un-gated, any read-write or merger member could redirect the hive's entire
// trace stream — spans carry hive.agent, issue refs, model names and token
// counts (pkg/tracing/semconv.go) — to an attacker-controlled collector, and
// flip OTelInsecure to strip TLS off it. Eleven of the twelve governor-config
// writers already called requireOwnerRole; this one was the lone outlier,
// exactly the asymmetry that established F16.
func (s *Server) handleGovernorFeatures(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		IoscanEnabled *bool `json:"ioscanEnabled"`

		TracingEnabled     *bool              `json:"tracingEnabled"`
		TracingEndpoint    *string            `json:"tracingEndpoint"`
		TracingSampleRatio *float64           `json:"tracingSampleRatio"`
		OTelEnabled        *bool              `json:"otelEnabled"`
		OTelEndpoint       *string            `json:"otelEndpoint"`
		OTelServiceName    *string            `json:"otelServiceName"`
		OTelInsecure       *bool              `json:"otelInsecure"`
		OTelSampleRatio    *float64           `json:"otelSampleRatio"`
		OTelHeaders        *map[string]string `json:"otelHeaders"`

		RetroEnabled       *bool   `json:"retroEnabled"`
		RetroAnalysisModel *string `json:"retroAnalysisModel"`

		MintEnabled *bool   `json:"mintEnabled"`
		MintIssuer  *string `json:"mintIssuer"`

		PlanFromLabel *bool `json:"planFromLabel"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	endpoint := firstStringPtr(body.OTelEndpoint, body.TracingEndpoint)
	if endpoint != nil {
		ep := strings.TrimSpace(*endpoint)
		if ep != "" {
			u, err := url.Parse(ep)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				jsonError(w, "otel endpoint must be empty or an http(s) URL", http.StatusBadRequest)
				return
			}
		}
	}
	sampleRatio := firstFloatPtr(body.OTelSampleRatio, body.TracingSampleRatio)
	if sampleRatio != nil {
		if *sampleRatio < minTracingSampleRatio || *sampleRatio > maxTracingSampleRatio {
			jsonError(w, "otel sample_ratio must be between 0.0 and 1.0", http.StatusBadRequest)
			return
		}
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.IoscanEnabled != nil {
		v := *body.IoscanEnabled
		cfg.Ioscan.Enabled = &v
	}
	otelEnabled := firstBoolPtr(body.OTelEnabled, body.TracingEnabled)
	if otelEnabled != nil || endpoint != nil || body.OTelServiceName != nil || body.OTelInsecure != nil || sampleRatio != nil || body.OTelHeaders != nil {
		merged := cfg.EffectiveOTel()
		if otelEnabled != nil {
			merged.Enabled = *otelEnabled
		}
		if endpoint != nil {
			merged.Endpoint = strings.TrimSpace(*endpoint)
		}
		if body.OTelServiceName != nil {
			merged.ServiceName = strings.TrimSpace(*body.OTelServiceName)
		}
		if body.OTelInsecure != nil {
			merged.Insecure = *body.OTelInsecure
		}
		if sampleRatio != nil {
			merged.SampleRatio = *sampleRatio
		}
		if body.OTelHeaders != nil {
			merged.Headers = sanitizedHeaderMap(*body.OTelHeaders)
		}
		cfg.OTel = merged
		cfg.Tracing = merged
	}
	if body.RetroEnabled != nil {
		cfg.Retro.Enabled = *body.RetroEnabled
	}
	if body.RetroAnalysisModel != nil {
		cfg.Retro.AnalysisModel = strings.TrimSpace(*body.RetroAnalysisModel)
	}
	if body.MintEnabled != nil {
		cfg.Mint.Enabled = *body.MintEnabled
	}
	if body.MintIssuer != nil {
		cfg.Mint.Issuer = strings.TrimSpace(*body.MintIssuer)
	}
	if body.PlanFromLabel != nil {
		v := *body.PlanFromLabel
		cfg.Planning.PlanFromLabel = &v
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after features update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_features", auditDetail("section", "features"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// featuresSectionResponse builds the opt-in-features payload for the governor
// config GET so the Features dialog can prefill its controls. The mint signing
// key (Config.Mint.KeyPath) is intentionally never returned — only whether mint
// is enabled and its issuer, keeping the response secret-free.
//
// planFromLabel is reported as a tri-state: null when the key is unset (falls
// back to the ACMM-level gate), otherwise the explicit true/false the operator
// chose, so the dialog can show "default" versus an explicit override.
func featuresSectionResponse(cfg *config.Config) map[string]interface{} {
	var planFromLabel interface{}
	if cfg.Planning.PlanFromLabel != nil {
		planFromLabel = *cfg.Planning.PlanFromLabel
	}
	otelCfg := cfg.EffectiveOTel()
	return map[string]interface{}{
		"ioscanEnabled":      cfg.Ioscan.IsEnabled(),
		"tracingEnabled":     otelCfg.Enabled,
		"tracingEndpoint":    otelCfg.Endpoint,
		"tracingSampleRatio": otelCfg.SampleRatio,
		"otelEnabled":        otelCfg.Enabled,
		"otelEndpoint":       otelCfg.Endpoint,
		"otelServiceName":    otelCfg.ServiceName,
		"otelInsecure":       otelCfg.Insecure,
		"otelSampleRatio":    otelCfg.SampleRatio,
		"otelHasHeaders":     len(otelCfg.Headers) > 0,
		"retroEnabled":       cfg.Retro.Enabled,
		"retroAnalysisModel": cfg.Retro.AnalysisModel,
		"mintEnabled":        cfg.Mint.Enabled,
		"mintIssuer":         cfg.Mint.Issuer,
		"planFromLabel":      planFromLabel,
	}
}

func firstBoolPtr(primary, fallback *bool) *bool {
	if primary != nil {
		return primary
	}
	return fallback
}

func firstFloatPtr(primary, fallback *float64) *float64 {
	if primary != nil {
		return primary
	}
	return fallback
}

func firstStringPtr(primary, fallback *string) *string {
	if primary != nil {
		return primary
	}
	return fallback
}

// Bounds for the advisory settings the dashboard may set.
//
// MaxFindings has a floor of 1: 0 is the UNSET sentinel that config defaulting
// resolves back to 10, so accepting it here would silently discard the
// operator's edit on the next reload. Rendering everything is ShowAll's job.
// StalenessDays has a floor of one day because a sub-day window would close
// findings between eval cycles, before the agent that reports them has had a
// chance to run again.
const (
	minAdvisoryMaxFindings   = 1
	minAdvisoryStalenessDays = 1
)

// handleGovernorAdvisoryGet returns the advisory digest settings so the Governor
// dialog can prefill its controls.
//
// OWNER-ONLY, matching the write side and the rest of the governor-config
// surface: these settings decide what the hive reports to repo owners, and
// lowering max_findings or shortening the staleness window is a way to make real
// findings disappear from the digest.
func (s *Server) handleGovernorAdvisoryGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, advisorySectionResponse(s.deps.Config))
}

// handleGovernorAdvisoryPut updates the advisory digest settings. Every field is
// a pointer, so an absent key leaves that setting untouched — the same "only
// what you send is changed" contract the other governor-config writers use.
func (s *Server) handleGovernorAdvisoryPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		MaxFindings     *int    `json:"max_findings"`
		ShowAll         *bool   `json:"show_all"`
		StalenessDays   *int    `json:"staleness_days"`
		PRAutoClose     *bool   `json:"pr_autoclose"`
		UpdateIntervalS *int    `json:"update_interval_s"`
		Target          *string `json:"target"`
		LinearIssue     *string `json:"linear_issue"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	// Normalize the digest target up front so the validation below and the
	// stored value agree. Empty means "unset" (GitHub) and is stored as such
	// so hive.yaml keeps round-tripping without the key.
	if body.Target != nil {
		v := strings.ToLower(strings.TrimSpace(*body.Target))
		body.Target = &v
	}
	if body.LinearIssue != nil {
		v := strings.TrimSpace(*body.LinearIssue)
		body.LinearIssue = &v
	}

	// --- validate before mutating anything ---
	if body.MaxFindings != nil && *body.MaxFindings < minAdvisoryMaxFindings {
		jsonError(w, "max_findings must be 1 or greater — use show_all to render every finding", http.StatusBadRequest)
		return
	}
	if body.StalenessDays != nil && *body.StalenessDays < minAdvisoryStalenessDays {
		jsonError(w, "staleness_days must be 1 or greater", http.StatusBadRequest)
		return
	}
	// 0 is the explicit "default cadence" sentinel; anything else must land in
	// the allowed band. Rejecting (rather than silently clamping) an
	// out-of-range dashboard edit matches the max_findings/staleness_days
	// contract above: the operator sees the rule instead of a mystery value.
	if body.UpdateIntervalS != nil && *body.UpdateIntervalS != 0 &&
		(*body.UpdateIntervalS < config.MinAdvisoryUpdateIntervalS || *body.UpdateIntervalS > config.MaxAdvisoryUpdateIntervalS) {
		jsonError(w, fmt.Sprintf("update_interval_s must be 0 (default cadence) or between %d and %d seconds",
			config.MinAdvisoryUpdateIntervalS, config.MaxAdvisoryUpdateIntervalS), http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	if body.Target != nil && *body.Target != "" &&
		*body.Target != config.AdvisoryTargetGitHub && *body.Target != config.AdvisoryTargetLinear {
		jsonError(w, fmt.Sprintf("target must be %q or %q", config.AdvisoryTargetGitHub, config.AdvisoryTargetLinear), http.StatusBadRequest)
		return
	}
	// Selecting Linear without an issue to post to would fail closed on
	// every cycle; catch it here where the operator can see the rule instead
	// of in the governor log. The effective values are whatever this request
	// sets, falling back to what is already stored.
	effectiveTarget := cfg.Governor.Advisory.ResolvedTarget()
	if body.Target != nil {
		effectiveTarget = (config.AdvisoryConfig{Target: *body.Target}).ResolvedTarget()
	}
	effectiveLinearIssue := cfg.Governor.Advisory.LinearIssue
	if body.LinearIssue != nil {
		effectiveLinearIssue = *body.LinearIssue
	}
	if effectiveTarget == config.AdvisoryTargetLinear && effectiveLinearIssue == "" {
		jsonError(w, "linear_issue (e.g. ONB-123) is required when target is linear", http.StatusBadRequest)
		return
	}

	// --- apply ---
	if body.Target != nil {
		cfg.Governor.Advisory.Target = *body.Target
	}
	if body.LinearIssue != nil {
		cfg.Governor.Advisory.LinearIssue = *body.LinearIssue
	}
	if body.MaxFindings != nil {
		cfg.Governor.Advisory.MaxFindings = *body.MaxFindings
	}
	if body.ShowAll != nil {
		cfg.Governor.Advisory.ShowAll = *body.ShowAll
	}
	if body.StalenessDays != nil {
		cfg.Governor.Advisory.StalenessDays = *body.StalenessDays
	}
	if body.PRAutoClose != nil {
		v := *body.PRAutoClose
		cfg.Governor.Advisory.PRAutoClose = &v
	}
	if body.UpdateIntervalS != nil {
		cfg.Governor.Advisory.UpdateIntervalS = *body.UpdateIntervalS
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after advisory update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_advisory", auditDetail("section", "advisory"), "")
	s.refreshAndPersist()
	jsonResponse(w, advisorySectionResponse(cfg))
}

// advisorySectionResponse renders AdvisoryConfig for the dashboard, resolving
// pr_autoclose's tri-state to the boolean the hive actually acts on so the UI
// never has to know the default.
func advisorySectionResponse(cfg *config.Config) map[string]interface{} {
	a := cfg.Governor.Advisory
	return map[string]interface{}{
		"max_findings":      a.MaxFindings,
		"show_all":          a.ShowAll,
		"staleness_days":    a.StalenessDays,
		"pr_autoclose":      a.PRAutoCloseEnabled(),
		"update_interval_s": a.UpdateIntervalS,
		"target":            a.ResolvedTarget(),
		"linear_issue":      a.LinearIssue,
	}
}

// handleGovernorReplanGet returns the stall-replan lane settings so the
// Governor dialog can prefill its controls. OWNER-ONLY, matching the rest of
// the governor-config surface.
func (s *Server) handleGovernorReplanGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, replanSectionResponse(s.deps.Config))
}

// handleGovernorReplanPut updates the stall-replan lane settings. Every field
// is a pointer, so an absent key leaves that setting untouched — the same
// "only what you send is changed" contract the other governor-config writers
// use.
func (s *Server) handleGovernorReplanPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled         *bool `json:"enabled"`
		IntervalS       *int  `json:"interval_s"`
		StallThresholdS *int  `json:"stall_threshold_s"`
		MaxReplans      *int  `json:"max_replans"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.IntervalS != nil && *body.IntervalS < 0 {
		jsonError(w, "interval_s must be 0 (default) or greater", http.StatusBadRequest)
		return
	}
	if body.StallThresholdS != nil && *body.StallThresholdS < 0 {
		jsonError(w, "stall_threshold_s must be 0 (default) or greater", http.StatusBadRequest)
		return
	}
	if body.MaxReplans != nil && *body.MaxReplans < 0 {
		jsonError(w, "max_replans must be 0 (default) or greater", http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.Enabled != nil {
		v := *body.Enabled
		cfg.Governor.Replan.Enabled = &v
	}
	if body.IntervalS != nil {
		cfg.Governor.Replan.IntervalS = *body.IntervalS
	}
	if body.StallThresholdS != nil {
		cfg.Governor.Replan.StallThresholdS = *body.StallThresholdS
	}
	if body.MaxReplans != nil {
		cfg.Governor.Replan.MaxReplans = *body.MaxReplans
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after replan update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_replan", auditDetail("section", "replan"), "")
	s.refreshAndPersist()
	jsonResponse(w, replanSectionResponse(cfg))
}

// replanSectionResponse renders ReplanConfig for the dashboard, resolving
// enabled's tri-state (nil = on by default) to the boolean the hive actually
// acts on so the UI never has to know the default.
func replanSectionResponse(cfg *config.Config) map[string]interface{} {
	rp := cfg.Governor.Replan
	return map[string]interface{}{
		"enabled":           rp.IsEnabled(),
		"interval_s":        rp.IntervalS,
		"stall_threshold_s": rp.StallThresholdS,
		"max_replans":       rp.MaxReplans,
	}
}

// handleGovernorWorkSourceGet returns the work_source config so the Governor
// dialog's Work Source tab can prefill its controls. OWNER-ONLY, matching the
// rest of the governor-config surface.
func (s *Server) handleGovernorWorkSourceGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, workSourceSectionResponse(s.deps.Config))
}

// handleGovernorWorkSourcePut updates the work_source config. The type and
// per-source credential/setting fields are accepted; for Linear the full
// team→repo map (teams), session_agent and assigned_only are accepted too, so
// a Linear-sourced hive can be configured end-to-end from the dashboard
// without falling back to the ConfigMap seed. List-valued fields (hold_labels,
// teams) replace the stored list when present and are left alone when absent.
func (s *Server) handleGovernorWorkSourcePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Type           *string `json:"type"`
		GitHubProjects *struct {
			Org            *string  `json:"org"`
			ProjectNumber  *int     `json:"project_number"`
			States         []string `json:"states"`
			PriorityField  *string  `json:"priority_field"`
			IterationField *string  `json:"iteration_field"`
			DefaultRepo    *string  `json:"default_repo"`
		} `json:"github_projects"`
		Linear *struct {
			APIKey       *string                         `json:"api_key"`
			HoldLabels   []string                        `json:"hold_labels"`
			SessionAgent *string                         `json:"session_agent"`
			AssignedOnly *bool                           `json:"assigned_only"`
			Teams        []config.LinearTeamSourceConfig `json:"teams"`
		} `json:"linear"`
		Jira *struct {
			BaseURL     *string  `json:"base_url"`
			Email       *string  `json:"email"`
			APIToken    *string  `json:"api_token"`
			ProjectKeys []string `json:"project_keys"`
			JQL         *string  `json:"jql"`
			Repo        *string  `json:"repo"`
			HoldLabels  []string `json:"hold_labels"`
		} `json:"jira"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.Type != nil {
		switch *body.Type {
		case "", "github", "github_projects", "linear", "jira":
		default:
			jsonError(w, "type must be one of: github, github_projects, linear, jira", http.StatusBadRequest)
			return
		}
	}
	cfg := s.deps.Config
	if body.Linear != nil {
		if msg := validateLinearWorkSourcePatch(cfg, body.Linear.SessionAgent, body.Linear.AssignedOnly, body.Linear.Teams, s.linearStoredViewerID()); msg != "" {
			jsonError(w, msg, http.StatusBadRequest)
			return
		}
	}

	// --- apply ---
	ws := &cfg.Governor.WorkSource
	if body.Type != nil {
		ws.Type = *body.Type
	}
	if body.GitHubProjects != nil {
		g := body.GitHubProjects
		if g.Org != nil {
			ws.GitHubProjects.Org = *g.Org
		}
		if g.ProjectNumber != nil {
			ws.GitHubProjects.ProjectNumber = *g.ProjectNumber
		}
		if g.States != nil {
			ws.GitHubProjects.States = g.States
		}
		if g.PriorityField != nil {
			ws.GitHubProjects.PriorityField = *g.PriorityField
		}
		if g.IterationField != nil {
			ws.GitHubProjects.IterationField = *g.IterationField
		}
		if g.DefaultRepo != nil {
			ws.GitHubProjects.DefaultRepo = *g.DefaultRepo
		}
	}
	if body.Linear != nil {
		l := body.Linear
		if l.APIKey != nil {
			ws.Linear.APIKey = *l.APIKey
		}
		if l.HoldLabels != nil {
			ws.Linear.HoldLabels = l.HoldLabels
		}
		if l.SessionAgent != nil {
			ws.Linear.SessionAgent = strings.TrimSpace(*l.SessionAgent)
		}
		if l.AssignedOnly != nil {
			ws.Linear.AssignedOnly = *l.AssignedOnly
		}
		if l.Teams != nil {
			ws.Linear.Teams = l.Teams
		}
	}
	if body.Jira != nil {
		j := body.Jira
		if j.BaseURL != nil {
			ws.Jira.BaseURL = *j.BaseURL
		}
		if j.Email != nil {
			ws.Jira.Email = *j.Email
		}
		if j.APIToken != nil {
			ws.Jira.APIToken = *j.APIToken
		}
		if j.ProjectKeys != nil {
			ws.Jira.ProjectKeys = j.ProjectKeys
		}
		if j.JQL != nil {
			ws.Jira.JQL = *j.JQL
		}
		if j.Repo != nil {
			ws.Jira.Repo = *j.Repo
		}
		if j.HoldLabels != nil {
			ws.Jira.HoldLabels = j.HoldLabels
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after work-source update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_work_source", auditDetail("section", "work_source"), "")
	s.refreshAndPersist()
	jsonResponse(w, workSourceSectionResponse(cfg))
}

// workSourceSectionResponse renders WorkSourceConfig for the dashboard's
// Work Source tab and the governor config GET payload.
func workSourceSectionResponse(cfg *config.Config) map[string]interface{} {
	ws := cfg.Governor.WorkSource
	return map[string]interface{}{
		"type": ws.Type,
		"github_projects": map[string]interface{}{
			"org":             ws.GitHubProjects.Org,
			"project_number":  ws.GitHubProjects.ProjectNumber,
			"states":          ws.GitHubProjects.States,
			"priority_field":  ws.GitHubProjects.PriorityField,
			"iteration_field": ws.GitHubProjects.IterationField,
			"default_repo":    ws.GitHubProjects.DefaultRepo,
		},
		"linear": map[string]interface{}{
			// The key itself is never echoed: the form only needs to know
			// whether one is stored (leave the field blank to keep it).
			"api_key_set":   ws.Linear.APIKey != "",
			"hold_labels":   ws.Linear.HoldLabels,
			"session_agent": ws.Linear.SessionAgent,
			"assigned_only": ws.Linear.AssignedOnly,
			"teams":         linearTeamsResponse(ws.Linear.Teams),
		},
		"jira": map[string]interface{}{
			"base_url":     ws.Jira.BaseURL,
			"email":        ws.Jira.Email,
			"api_token":    ws.Jira.APIToken,
			"project_keys": ws.Jira.ProjectKeys,
			"jql":          ws.Jira.JQL,
			"repo":         ws.Jira.Repo,
			"hold_labels":  ws.Jira.HoldLabels,
		},
	}
}

// linearTeamsResponse renders the team list as plain maps so the form always
// receives a JSON array (never null) with every field present.
func linearTeamsResponse(teams []config.LinearTeamSourceConfig) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(teams))
	for _, t := range teams {
		projects := make([]map[string]interface{}, 0, len(t.Projects))
		for _, p := range t.Projects {
			projects = append(projects, map[string]interface{}{"name": p.Name, "repo": p.Repo})
		}
		states := t.States
		if states == nil {
			states = []string{}
		}
		out = append(out, map[string]interface{}{
			"key":      t.Key,
			"repo":     t.Repo,
			"states":   states,
			"cycles":   t.Cycles,
			"projects": projects,
		})
	}
	return out
}

// linearStoredViewerID returns the Dependencies install probe, or nil.
func (s *Server) linearStoredViewerID() func() (string, string) {
	if s.deps == nil {
		return nil
	}
	return s.deps.LinearStoredViewerID
}

// validateLinearWorkSourcePatch checks the Linear fields of a work-source PUT
// before anything is mutated. It mirrors the rules the work-source factory
// and the session responder enforce at runtime, so the dashboard refuses to
// persist a config that would only fail at the next reload:
//   - every team needs a key and a repo (the team→repo map is what makes
//     enumeration work at all);
//   - session_agent must name a configured agent (unknownSessionAgentError);
//   - assigned_only requires a connected Linear agent (fail closed, same as
//     worksource.New).
//
// Returns the 400 message, or "" when the patch is acceptable.
// storedViewerID probes the persisted Linear install (viewer id + store
// path); nil disables the probe (bare test servers), reading as no install.
func validateLinearWorkSourcePatch(cfg *config.Config, sessionAgent *string, assignedOnly *bool, teams []config.LinearTeamSourceConfig, storedViewerID func() (string, string)) string {
	for i, t := range teams {
		if strings.TrimSpace(t.Key) == "" {
			return fmt.Sprintf("linear.teams[%d].key is required", i)
		}
		if strings.TrimSpace(t.Repo) == "" {
			return fmt.Sprintf("linear.teams[%d].repo is required (owner/name agents clone for team %s)", i, t.Key)
		}
		if t.Cycles != "" && t.Cycles != "current" {
			return fmt.Sprintf("linear.teams[%d].cycles must be empty or \"current\"", i)
		}
		for j, p := range t.Projects {
			if strings.TrimSpace(p.Name) == "" {
				return fmt.Sprintf("linear.teams[%d].projects[%d].name is required", i, j)
			}
		}
	}
	if sessionAgent != nil {
		if name := strings.TrimSpace(*sessionAgent); name != "" {
			if _, ok := cfg.Agents[name]; !ok {
				return (&unknownSessionAgentError{name: name}).Error()
			}
		}
	}
	if assignedOnly != nil && *assignedOnly {
		viewerID, storePath := "", ""
		if storedViewerID != nil {
			viewerID, storePath = storedViewerID()
		}
		if viewerID == "" {
			return "assigned_only requires a connected Linear agent: connect the workspace via POST /api/linear/agent/install first (no install found at " + storePath + ")"
		}
	}
	return ""
}
