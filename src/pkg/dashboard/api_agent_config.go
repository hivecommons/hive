package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/resolve"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

func (s *Server) handleAgentConfigGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	proc, err := s.deps.AgentMgr.GetStatusFast(name)

	cli := agentCfg.Backend
	if err == nil && proc.BackendOverride != "" {
		cli = proc.BackendOverride
	}
	model := agentCfg.Model
	if err == nil && proc.ModelOverride != "" {
		model = proc.ModelOverride
	}

	// Use configured launch command if available; otherwise construct one
	launchCmd := agentCfg.LaunchCmd
	if launchCmd == "" {
		launchCmd = fmt.Sprintf("%s --model %s", cli, model)
		switch cli {
		case "claude":
			launchCmd = fmt.Sprintf("claude --model %s --dangerously-skip-permissions", model)
		case "copilot":
			launchCmd = fmt.Sprintf("/usr/bin/copilot --allow-all --model %s", model)
		}
	}

	// Use configured display name if available
	displayName := agentCfg.DisplayName
	if displayName == "" {
		displayName = ""
	}

	// Stale timeout from config, default to 28800 (8 hours).
	// Must exceed the longest cadence interval to avoid false stale flags.
	const defaultStaleTimeoutS = 28800
	staleTimeout := agentCfg.StaleTimeout
	if staleTimeout == 0 {
		staleTimeout = defaultStaleTimeoutS
	}

	// Restart strategy from config, default to "immediate"
	restartStrategy := agentCfg.RestartStrategy
	if restartStrategy == "" {
		restartStrategy = "immediate"
	}

	// Cadences as seconds (int) — frontend expects numbers, not duration strings
	cadences := map[string]any{}
	for modeName, modeCfg := range s.deps.Config.Governor.Modes {
		if c, ok := modeCfg.Cadences[name]; ok {
			if c.Mode() != config.CadenceModeInterval {
				cadences[modeName] = c
			} else if c.Interval() == "pause" || c.Interval() == "off" || c.Interval() == "0" {
				cadences[modeName] = 0
			} else {
				d := parseCadenceDuration(c.Interval())
				cadences[modeName] = int64(d.Seconds())
			}
		}
	}

	// Per-mode models (empty strings = inherit from general)
	models := map[string]string{}
	for modeName := range s.deps.Config.Governor.Modes {
		models[modeName] = ""
	}

	lastPrompt := ""
	pinnedCLI := agentCfg.CLIPinned
	if err == nil {
		lastPrompt = proc.LastKickMessage
		pinnedCLI = pinnedCLI || proc.PinnedCLI != ""
	}

	// Read restrictions from agent work dir files
	restrictions := s.loadAgentRestrictions(name)

	// Read prompt template from agent policy file
	promptTemplate := s.loadPromptTemplate(name)

	// Read stat sources from config
	stats := s.loadAgentStats(name)

	pipeline := s.getAgentPipeline(name)
	hooks := s.getAgentHooks(name)

	includeRepos := true
	if agentCfg.IncludeRepos != nil {
		includeRepos = *agentCfg.IncludeRepos
	}

	// general.jevReady tells the settings panel whether the Jev toggle can be
	// turned on: a Jev key (JEV_API_KEY or the connected OpenRouter gateway)
	// must resolve first, else the toggle renders disabled with a hint
	// (hivecommons/hive#8939). The flag only — never the key itself.
	jsonResponse(w, map[string]interface{}{
		"general": map[string]interface{}{
			"enabled":             agentCfg.Enabled,
			"launchCmd":           launchCmd,
			"displayName":         displayName,
			"description":         agentCfg.Description,
			"cliPinned":           pinnedCLI,
			"cliPinValue":         cli,
			"modelOwner":          agentCfg.ModelOwner,
			"backendOwner":        agentCfg.BackendOwner,
			"staleTimeout":        staleTimeout,
			"restartStrategy":     restartStrategy,
			"model":               model,
			"clearOnKick":         agentCfg.ClearOnKick,
			"onDemand":            agentCfg.OnDemand,
			"continuous":          agentCfg.Continuous,
			"continuousCooldown":  int64(agentCfg.EffectiveContinuousCooldown().Seconds()),
			"continuousBudgetPct": agentCfg.EffectiveContinuousBudgetPct(),
			"emoji":               agentCfg.Emoji,
			"color":               agentCfg.Color,
			"sortOrder":           agentCfg.SortOrder,
			"beadRole":            agentCfg.BeadRole,
			"role":                agentCfg.Role,
			"kickTemplate":        agentCfg.KickTemplate,
			"promptSource":        agentCfg.PromptSource,
			"definitionSource":    agentCfg.DefinitionSource,
			"mode":                agentCfg.Mode,
			"includeRepos":        includeRepos,
			// The agent's repository scope (#6204), plus the hive's own repo
			// list so the dialog can offer real choices instead of a free-text
			// field the operator has to spell from memory.
			"repos":            agentCfg.Repos,
			"projectRepos":     s.deps.Config.Project.Repos,
			"laneKeywords":     agentCfg.LaneKeywords,
			"detectKeywords":   agentCfg.DetectKeywords,
			"aliases":          agentCfg.Aliases,
			"cavemanMode":      agentCfg.CavemanMode,
			"jevMode":          agentCfg.JevMode,
			"jevReady":         s.deps.Config.JevReady(),
			"explainMode":      agentCfg.ExplainMode,
			"sandboxEnabled":   agentCfg.Sandbox != nil && agentCfg.Sandbox.Enabled != nil && *agentCfg.Sandbox.Enabled,
			"sandboxEffective": agentCfg.SandboxEnabled(s.deps.Config.AgentSandbox),
			"replicas":         agentCfg.Replicas,
		},
		"cadences":     cadences,
		"models":       models,
		"pipeline":     pipeline,
		"hooks":        hooks,
		"restrictions": restrictions,
		"stats":        stats,
		// The Stats tab offers only sources that apply to THIS agent (#7411):
		// an ADVISORY/on-demand agent is never offered the CI health strip.
		"statSources":    map[string]any{"sources": statSourcesFor(&agentCfg), "styles": statStyles},
		"prompt":         lastPrompt,
		"promptTemplate": promptTemplate,
		"channels":       agentCfg.Channels,
		"tools":          agentCfg.Tools,
		"connections":    maskConnectionAuth(agentCfg.Connections),
	})
}

type restriction struct {
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

func (s *Server) loadAgentRestrictions(name string) map[string]interface{} {
	result := map[string]interface{}{
		"agent":  []any{},
		"global": []any{},
		"policy": []any{},
	}

	// Read global restrictions from /data/restrictions.conf (one pattern per line)
	globalRestrictions := []any{}
	if data, err := os.ReadFile("/data/restrictions.conf"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			r := restriction{Pattern: parts[0], Source: "global"}
			if len(parts) > 1 {
				r.Reason = parts[1]
			}
			globalRestrictions = append(globalRestrictions, r)
		}
	}
	result["global"] = globalRestrictions

	// Read agent-specific restrictions
	agentRestrictions := []any{}
	agentRestFile := fmt.Sprintf("/data/agents/%s/restrictions.conf", name)
	if data, err := os.ReadFile(agentRestFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			r := restriction{Pattern: parts[0], Source: "agent"}
			if len(parts) > 1 {
				r.Reason = parts[1]
			}
			agentRestrictions = append(agentRestrictions, r)
		}
	}
	result["agent"] = agentRestrictions

	// Read policy restrictions from agent policy file
	// Old hive extracts lines containing policy-relevant keywords, including
	// markdown-formatted lines with ** bold markers and numbered list items.
	policyRestrictions := []any{}
	policyContent := s.loadPromptTemplate(name)
	if policyContent != "" {
		content := policyContent
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			// Strip leading markdown list markers (-, *, numbered)
			stripped := line
			if len(stripped) > 2 && (stripped[0] == '-' || stripped[0] == '*') && stripped[1] == ' ' {
				stripped = strings.TrimSpace(stripped[2:])
			}
			// Strip bold markers for pattern matching
			plain := strings.ReplaceAll(stripped, "**", "")

			if strings.HasPrefix(plain, "NEVER ") ||
				strings.HasPrefix(plain, "Do not ") ||
				strings.HasPrefix(plain, "Do NOT ") ||
				strings.HasPrefix(plain, "ALWAYS ") ||
				strings.HasPrefix(plain, "Never ") ||
				strings.Contains(plain, "HARD RULE") ||
				strings.Contains(plain, "LANE BOUNDARY") ||
				strings.Contains(plain, "DO NOT") {
				// Truncate very long lines to keep the response manageable
				const maxPolicyLen = 200
				entry := stripped
				if runes := []rune(entry); len(runes) > maxPolicyLen {
					entry = string(runes[:maxPolicyLen]) + "..."
				}
				policyRestrictions = append(policyRestrictions, restriction{
					Pattern: entry,
					Source:  "policy",
				})
			}
		}
	}
	result["policy"] = policyRestrictions

	return result
}

func (s *Server) loadPromptTemplate(name string) string {
	templateName := ""
	if s.deps != nil && s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
			templateName = ac.KickTemplate
		}
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	paths := []string{
		fmt.Sprintf("/data/policies/examples/kubestellar/agents/%s", templateName),
	}
	if s.deps != nil && s.deps.Config != nil {
		policyDir := s.deps.Config.Policies.LocalDir
		if policyDir != "" {
			paths = append(paths,
				fmt.Sprintf("%s/examples/kubestellar/agents/%s", policyDir, templateName),
				fmt.Sprintf("%s/%s%s", policyDir, s.deps.Config.Policies.Path, templateName),
			)
		}
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return s.substituteTemplateVars(string(data), name)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return s.substituteTemplateVars(string(data), name)
	}
	return ""
}

func (s *Server) substituteTemplateVars(template, agentName string) string {
	if s.deps == nil || s.deps.Config == nil {
		return template
	}
	cfg := s.deps.Config
	org := cfg.Project.Org
	primaryRepo := cfg.Project.PrimaryRepo

	// Build full primary repo path (org/repo) if not already qualified
	fullPrimaryRepo := primaryRepo
	if org != "" && !strings.Contains(primaryRepo, "/") {
		fullPrimaryRepo = fmt.Sprintf("%s/%s", org, primaryRepo)
	}

	reposList := strings.Join(cfg.Project.Repos, ", ")

	displayName := agentName
	if ac, ok := cfg.Agents[agentName]; ok && ac.DisplayName != "" {
		displayName = ac.DisplayName
	}

	// The dashboard preview substitutes the config-only subset of kick-template
	// variables (it has no live GitHub context). Route it through the same
	// resolve engine as the scheduler so operator-defined ${VAR}s (from the
	// config `variables:` block) render in previews too, and the two paths can
	// no longer drift. Built-ins win; with no operator variables the output
	// matches the previous strings.NewReplacer exactly.
	lit := func(v string) func() string { return func() string { return v } }
	rt := &resolve.RuntimeContext{Vars: map[string]func() string{
		"AGENT_NAME":           lit(agentName),
		"AGENT_DISPLAY_NAME":   lit(displayName),
		"PROJECT_NAME":         lit(cfg.Project.Name),
		"PROJECT_ORG":          lit(org),
		"PROJECT_PRIMARY_REPO": lit(fullPrimaryRepo),
		"PROJECT_AI_AUTHOR":    lit(cfg.EffectiveAIAuthor()),
		"PROJECT_REPOS_LIST":   lit(reposList),
		"HIVE_REPO":            lit(fmt.Sprintf("%s/hive", org)),
		"HIVE_ID":              lit(cfg.HiveID),
		// project.writing_guide is config-only, so the prompt editor's preview
		// can show it exactly where a kick will place it — which is how an
		// owner sees that the setting took (hivecommons/hive#7667).
		"WRITING_GUIDE": lit(cfg.Project.WritingGuideSection()),
	}}
	return cfg.ResolveRegistry(s.deps.Logger).Expand(context.Background(), template, resolve.ScopeTemplate, rt)
}

func (s *Server) loadAgentStats(name string) []any {
	return loadStatsConfig(name)
}

func (s *Server) handleAgentConfigGeneral(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateAgentGeneralInput(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := s.deps.Config.Agents[name]
	// Captured before the field writes below, because leaving on-demand is a
	// TRANSITION rather than a state: acting on it needs the previous value.
	prevOnDemand := agentCfg.OnDemand
	if v, ok := body["enabled"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.Enabled = b
		}
	}
	if v, ok := body["clearOnKick"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.ClearOnKick = b
		}
	}
	// On-demand is a real per-agent field that had no control: it was only ever
	// readable as a badge, so an operator could see "on demand" but not leave it
	// (hivecommons/hive#7446). Turning it off is necessary but not sufficient —
	// the agent also needs a cadence in the active mode before it is kicked.
	if v, ok := body["onDemand"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.OnDemand = b
			// An explicit operator choice; ApplyPack must not reconcile it
			// back to the pack's value on the next restart.
			agentCfg.OnDemandOwner = config.FieldOwnerOperator
		}
	}
	if v, ok := body["continuous"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.Continuous = b
		}
	}
	if v, ok := body["continuousCooldown"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.ContinuousCooldown = time.Duration(int64(f)) * time.Second
		}
	}
	if v, ok := body["continuousBudgetPct"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.ContinuousBudgetPct = int(f)
		}
	}
	if v, ok := body["displayName"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.DisplayName = sanitizeString(s)
		}
	}
	if v, ok := body["description"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Description = sanitizeString(s)
		}
	}
	if v, ok := body["launchCmd"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.LaunchCmd = sanitizeString(s)
		}
	}
	if v, ok := body["staleTimeout"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.StaleTimeout = int(f)
		}
	}
	if v, ok := body["replicas"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.Replicas = int(f)
		}
	}
	if v, ok := body["restartStrategy"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.RestartStrategy = sanitizeString(s)
		}
	}
	if v, ok := body["cliPinned"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.CLIPinned = b
		}
	}
	// model / cliPinValue: the dialog's Model and CLI-Pin selects. These were
	// historically dropped here (the handler only read a fixed allowlist), so
	// changes made in the config dialog silently never persisted. Track whether
	// they changed so we can apply them live after the config write, the same
	// way the card dropdowns do (SetModelOverride + restart) — otherwise a stale
	// live override would mask the freshly-saved value and it still wouldn't stick.
	modelChanged, backendChanged := false, false
	// Both are operator edits, so they claim ownership of the field — without
	// the marker the next pack apply (every restart) reconciles them away.
	if v, ok := body["model"]; ok {
		if str, ok := v.(string); ok {
			agentCfg.Model = sanitizeString(str)
			agentCfg.ModelOwner = config.FieldOwnerOperator
			modelChanged = true
		}
	}
	if v, ok := body["cliPinValue"]; ok {
		if str, ok := v.(string); ok && str != "" {
			backend := sanitizeString(str)
			// Validate at set time against the same list the launcher
			// dispatches on — persisting an unsupported backend produces an
			// agent that is accepted now and fails to launch later.
			if err := s.deps.Config.Governor.ValidateBackend(backend); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			agentCfg.Backend = backend
			agentCfg.BackendOwner = config.FieldOwnerOperator
			backendChanged = true
		}
	}
	if v, ok := body["emoji"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Emoji = sanitizeString(s)
		}
	}
	if v, ok := body["color"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Color = sanitizeString(s)
		}
	}
	if v, ok := body["sortOrder"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.SortOrder = int(f)
		}
	}
	if v, ok := body["beadRole"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.BeadRole = sanitizeString(s)
		}
	}
	if v, ok := body["role"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Role = sanitizeString(s)
		}
	}
	if v, ok := body["kickTemplate"]; ok {
		if str, ok := v.(string); ok {
			tpl := sanitizeString(str)
			// Catch a dangling kick_template when it is SET, not never
			// (hivecommons/hive#7390). The shape was already checked by
			// validateAgentGeneralInput (bare .md name); a NEW name must also
			// resolve somewhere the scheduler looks — otherwise the save
			// succeeds, the kick silently falls back, and the prompt editor
			// shows an empty box. An unchanged value is left alone so an
			// already-dangling field cannot block an unrelated edit (display
			// name, model) until it is fixed.
			if tpl != "" && tpl != agentCfg.KickTemplate && s.deps.Scheduler != nil {
				if _, exists := s.deps.Scheduler.TemplateExists(tpl); !exists {
					jsonError(w, fmt.Sprintf("kick_template %q does not exist: it is not shipped in pkg/policies/defaults and no file of that name is in the policy directories. Save the prompt in the Prompt Template tab first (that creates %s), or pick a shipped template", tpl, filepath.Join(promptTemplateSaveDir, tpl)), http.StatusBadRequest)
					return
				}
			}
			agentCfg.KickTemplate = tpl
		}
	}
	if v, ok := body["mode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			if s != "" {
				validModes := map[string]bool{"ADVISORY": true, "ISSUES_ONLY": true, "ISSUES_AND_PRS": true, "ISSUES_PRS_MERGE": true, "NO_GITHUB": true}
				if !validModes[s] {
					jsonError(w, "mode must be one of: ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE, NO_GITHUB", http.StatusBadRequest)
					return
				}
			}
			agentCfg.Mode = s
		}
	}
	if v, ok := body["includeRepos"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.IncludeRepos = &b
		}
	}
	// repos: the agent's repository scope (#6204). An operator edit claims
	// ownership of the field, the same contract model/backend/pause carry — no
	// pack ships a repo scope today, but a field a human chose must not be
	// reconcilable by one that someday does (#5632/#5706).
	if v, ok := body["repos"]; ok {
		if arr, ok := v.([]interface{}); ok {
			repos := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(sanitizeString(s)); s != "" {
						repos = append(repos, s)
					}
				}
			}
			if len(repos) == 0 {
				repos = nil
			}
			agentCfg.Repos = repos
			agentCfg.ReposOwner = config.FieldOwnerOperator
		}
	}
	if v, ok := body["laneKeywords"]; ok {
		if arr, ok := v.([]interface{}); ok {
			kw := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					kw = append(kw, sanitizeString(s))
				}
			}
			agentCfg.LaneKeywords = kw
		}
	}
	if v, ok := body["detectKeywords"]; ok {
		if arr, ok := v.([]interface{}); ok {
			kw := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					kw = append(kw, sanitizeString(s))
				}
			}
			agentCfg.DetectKeywords = kw
		}
	}
	if v, ok := body["aliases"]; ok {
		if arr, ok := v.([]interface{}); ok {
			a := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					a = append(a, s)
				}
			}
			agentCfg.Aliases = a
		}
	}
	if v, ok := body["sandboxEnabled"]; ok {
		if b, ok := v.(bool); ok {
			if agentCfg.Sandbox == nil {
				agentCfg.Sandbox = &config.AgentSandboxOverride{}
			}
			bb := b
			agentCfg.Sandbox.Enabled = &bb
		}
	}
	if v, ok := body["cavemanMode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			// Same gate as config.Validate, so the write path cannot persist a
			// value that would fail the next config load.
			if !config.ValidateCavemanMode(s) {
				jsonError(w, "caveman_mode must be one of: lite, full, ultra, wenyan (or empty to disable)", http.StatusBadRequest)
				return
			}
			agentCfg.CavemanMode = s
		}
	}
	// jev_mode is applied at launch (skill install + HIVE_JEV_MODE env in
	// launchInTmux), so a change here restarts the agent below, the same way
	// model/backend do — otherwise the running CLI keeps neither the skill nor
	// the env until something else relaunches it (hivecommons/hive#8939).
	jevModeChanged := false
	if v, ok := body["jevMode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			// Same gate as config.Validate, so the write path cannot persist a
			// value that would fail the next config load.
			if !config.ValidateJevMode(s) {
				jsonError(w, "jev_mode must be one of: off, assist (or empty to disable)", http.StatusBadRequest)
				return
			}
			jevModeChanged = agentCfg.JevEnabled() != (config.AgentConfig{JevMode: s}).JevEnabled()
			agentCfg.JevMode = s
		}
	}
	if v, ok := body["explainMode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			// Same gate as config.Validate, so the write path cannot persist a
			// value that would fail the next config load.
			if !config.ValidateExplainMode(s) {
				jsonError(w, "explain_mode must be one of: off, brief, full (or empty to inherit the hive default)", http.StatusBadRequest)
				return
			}
			agentCfg.ExplainMode = s
		}
	}
	// promptSource: a nested {owner,repo,path,ref} object (or explicit null to
	// clear). When set, validate against the seed-only allowlist and re-bake so
	// the agent has a fresh last-known-good prompt on disk.
	if v, ok := body["promptSource"]; ok {
		if v == nil {
			agentCfg.PromptSource = nil
		} else if m, ok := v.(map[string]interface{}); ok {
			ps := &config.PromptSourceConfig{
				Type:  "github",
				Owner: sanitizeString(stringField(m, "owner")),
				Repo:  sanitizeString(stringField(m, "repo")),
				Path:  sanitizeString(stringField(m, "path")),
				Ref:   sanitizeString(stringField(m, "ref")),
			}
			if !ps.IsSet() {
				// Partially filled → treat as cleared rather than erroring, so an
				// operator can blank the fields to disable the source.
				agentCfg.PromptSource = nil
			} else {
				agentCfg.PromptSource = ps
				if err := s.bakePromptSource(r.Context(), name, &agentCfg); err != nil {
					jsonError(w, "prompt source: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
		}
	}
	// definitionSource: a nested {owner,repo,path,ref,url} object (or explicit
	// null to clear) keeping the whole agent linked to a repo. When set, validate
	// against the seed-only allowlist so a non-allowlisted repo is rejected here
	// rather than silently ignored at reload. The actual re-apply happens on the
	// reload path (resolveDefinitionSources); this handler only persists the pointer.
	if v, ok := body["definitionSource"]; ok {
		if v == nil {
			agentCfg.DefinitionSource = nil
		} else if m, ok := v.(map[string]interface{}); ok {
			ds := &config.DefinitionSourceConfig{
				Type:  "github",
				Owner: sanitizeString(stringField(m, "owner")),
				Repo:  sanitizeString(stringField(m, "repo")),
				Path:  sanitizeString(stringField(m, "path")),
				Ref:   sanitizeString(stringField(m, "ref")),
				URL:   sanitizeString(stringField(m, "url")),
			}
			if !ds.IsSet() {
				agentCfg.DefinitionSource = nil
			} else if !s.deps.Config.GitHubDefinitionAllowed(ds.Slug()) {
				jsonError(w, fmt.Sprintf("definition source: repo %q is not on the GitHub definition allowlist", ds.Slug()), http.StatusBadRequest)
				return
			} else {
				agentCfg.DefinitionSource = ds
			}
		}
	}

	// Refuse to persist a backend/launch_cmd contradiction (#5921): saved
	// silently, it produces an agent launched as one CLI but health-checked
	// and diagnosed as another, relaunched as "hung" forever. Checked here —
	// after every field edit above — so a save changing either half (or both)
	// is judged on the final combination.
	if err := s.deps.Config.Governor.ValidateLaunchCmdBackend(agentCfg.Backend, agentCfg.LaunchCmd); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	prevAgents := make(map[string]config.AgentConfig, len(s.deps.Config.Agents))
	for k, v := range s.deps.Config.Agents {
		prevAgents[k] = v
	}
	s.deps.Config.Agents[name] = agentCfg
	if err := s.deps.Config.ExpandAgentReplicas(); err != nil {
		s.deps.Config.Agents = prevAgents
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentCfg = s.deps.Config.Agents[name]
	addedAgents := s.deps.AgentMgr.ReconcileAgents(s.deps.Config.EnabledAgents())
	for _, added := range addedAgents {
		if ac, ok := s.deps.Config.Agents[added]; ok && !ac.OnDemand {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, added); err != nil {
				s.logger.Warn("failed to start reconciled agent", "agent", added, "error", err)
			}
		}
	}
	// An on_demand TRANSITION has to move the process, not just rewrite config.
	// Both directions were no-ops before, for different reasons:
	//
	//   leaving on-demand — ReconcileAgents above reports only agents that were
	//     just ADDED to the fleet. An agent an operator toggles already exists,
	//     so it is never in that list, and it was skipped at launch precisely
	//     BECAUSE it was on-demand ("skipping on-demand agent at startup"). It
	//     therefore has no process, no pane, and no tmux session to attach to,
	//     and the toggle appears to do nothing until the pod restarts.
	//
	//   becoming on-demand — nothing stopped the running process. The governor
	//     does stop KICKING it (it skips OnDemand agents), so the agent lingers
	//     as a live CLI and pane that will never be scheduled again — which is
	//     not what "on demand" means.
	//
	// (#7446)
	switch onDemandTransition(prevOnDemand, agentCfg.OnDemand, agentCfg.Enabled) {
	case onDemandStart:
		startedByReconcile := false
		for _, added := range addedAgents {
			if added == name {
				startedByReconcile = true
				break
			}
		}
		if !startedByReconcile {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, name); err != nil {
				// "already running" is an expected, benign outcome here.
				s.logger.Warn("could not start agent after it left on-demand", "agent", name, "error", err)
			} else {
				s.logger.Info("started agent after it left on-demand", "agent", name)
			}
		}
	case onDemandStop:
		// Stop already returns nil for an agent that is not running, so this is
		// safe regardless of the agent's current state.
		if err := s.deps.AgentMgr.Stop(name); err != nil {
			s.logger.Warn("could not stop agent after it became on-demand", "agent", name, "error", err)
		} else {
			s.logger.Info("stopped agent after it became on-demand", "agent", name)
		}
	}
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateAgents(s.deps.Config.EnabledAgents())
	}

	// Sync the updated config into the agent process so that status builders
	// (which read from AgentProcess.Config, not the global config map) reflect
	// changes like display_name immediately.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent update", "agent", name, "error", err)
	}

	s.deps.AgentMgr.SyncModeFiles(s.deps.AgentMgr.GetACMMLevel())
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after update", "agent", name, "error", err)
		}
	}
	// Apply model/backend live so the change takes effect immediately and isn't
	// masked by a pre-existing override from a card dropdown. SetModelOverride
	// with the saved value (or "" to clear back to the launch-command default)
	// keeps the dialog and the card in sync; a single restart applies both.
	if modelChanged {
		if err := s.deps.AgentMgr.SetModelOverride(name, agentCfg.Model); err != nil {
			s.logger.Warn("failed to apply model from config dialog", "agent", name, "error", err)
		}
	}
	if backendChanged {
		if err := s.deps.AgentMgr.SetBackendOverride(name, agentCfg.Backend); err != nil {
			s.logger.Warn("failed to apply backend from config dialog", "agent", name, "error", err)
		}
	}
	if modelChanged || backendChanged || jevModeChanged {
		if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
			s.logger.Warn("restart after config-dialog model/backend/jev_mode change failed", "agent", name, "error", err)
		}
	}

	s.auditFromRequest(r, "config_agent_general", auditDetail("section", "general"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigCadences(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]json.RawMessage
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	for modeName, raw := range body {
		mode, ok := s.deps.Config.Governor.Modes[modeName]
		if !ok {
			continue
		}
		if mode.Cadences == nil {
			mode.Cadences = make(map[string]config.Cadence)
		}
		var seconds int64
		var cadence config.Cadence
		if err := json.Unmarshal(raw, &seconds); err == nil {
			if seconds <= 0 {
				cadence = "pause"
			} else {
				cadence = config.Cadence(formatCadenceDuration(seconds))
			}
		} else if err := json.Unmarshal(raw, &cadence); err != nil {
			jsonError(w, "invalid cadence for "+modeName+": must be seconds, interval string, or schedule object", http.StatusBadRequest)
			return
		}
		if err := cadence.Validate(); err != nil {
			jsonError(w, "invalid cadence for "+modeName+": "+err.Error(), http.StatusBadRequest)
			return
		}
		if cadence.Mode() == config.CadenceModeInterval && cadence.IsPaused() {
			mode.Cadences[name] = "pause"
		} else {
			mode.Cadences[name] = cadence
		}
		s.deps.Config.Governor.Modes[modeName] = mode
		// Operator edits claim ownership so the pack apply that runs on every
		// restart (and on steady-state re-applies) cannot reconcile the cadence
		// back to the pack default — the same contract model/backend edits
		// already have (#5632).
		s.deps.Config.Governor.ClaimCadenceOwnership(modeName, name)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after cadence update", "agent", name, "error", err)
	}
	s.auditFromRequest(r, "config_agent_cadences", auditDetail("section", "cadences"), name)
	// The rebuild kicked here is asynchronous, so the browser's post-save
	// GET /api/status can be served the CACHED pre-mutation snapshot and
	// repaint the OLD cadence — the operator then waits for a later broadcast
	// to see their own write (#5492). minStatusSeq is the lowest StatusSeq
	// guaranteed to reflect this mutation; the dashboard raises its
	// stale-snapshot floor to it and drops anything built earlier (#4348).
	floor := s.refreshAndPersistSeq()
	// jsonResponse rather than okResponse: the latter is typed map[string]string
	// and cannot carry the numeric floor. "ok" is preserved for callers.
	jsonResponse(w, map[string]any{"ok": true, "status": "updated", "agent": name, "minStatusSeq": floor})
}

func (s *Server) handleAgentConfigModels(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	var body struct {
		Backend string `json:"backend"`
		Model   string `json:"model"`
		// A pointer so "clear the effort" (explicit "") and "leave it
		// unchanged" (field absent) stay distinguishable — Backend/Model
		// above treat "" as unchanged, but empty is a meaningful effort
		// value (the backend's own default).
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	// What the agent LAUNCHES with is not agentCfg alone: launchInTmux
	// prefers a live ModelOverride/BackendOverride (set by the card
	// dropdowns via /api/model and /api/switch, by a pin, or by the
	// governor's auto-selection) over Config.Model/Backend. Compare the
	// request against those effective values so a change is detected — and
	// applied — even when the config field already matched (#7374).
	prevModel, prevBackend := agentCfg.Model, agentCfg.Backend
	if proc, err := s.deps.AgentMgr.GetStatus(name); err == nil && proc != nil {
		if proc.ModelOverride != "" {
			prevModel = proc.ModelOverride
		}
		if proc.BackendOverride != "" {
			prevBackend = proc.BackendOverride
		}
	}
	effectiveBackend, effectiveModel := prevBackend, prevModel
	modelChanged, backendChanged, effortChanged := false, false, false

	// Operator edits claim ownership so the pack apply that runs on every
	// restart cannot reconcile the choice back to the pack default.
	if body.Backend != "" {
		backend := sanitizeString(body.Backend)
		// Refuse an unsupported backend HERE, at set time, with a message that
		// names what is valid. Without this the value is persisted happily and
		// the failure surfaces hours later as "unknown backend: <x>" on the
		// kick path, with the agent silently never launching.
		if err := s.deps.Config.Governor.ValidateBackend(backend); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		agentCfg.Backend = backend
		agentCfg.BackendOwner = config.FieldOwnerOperator
		effectiveBackend = agentCfg.Backend
		backendChanged = backend != prevBackend
	}
	if body.Model != "" {
		agentCfg.Model = sanitizeString(body.Model)
		agentCfg.ModelOwner = config.FieldOwnerOperator
		effectiveModel = agentCfg.Model
		modelChanged = agentCfg.Model != prevModel
	}
	if body.ReasoningEffort != nil {
		effort := sanitizeString(*body.ReasoningEffort)
		// Same set-time rejection rationale as ValidateBackend above: an
		// unsupported effort persisted happily would surface hours later as
		// a broken launch command. Validated against the backend/model the
		// agent will actually launch with, including values set in this
		// same request. omp is per-model, not backend-wide.
		if err := s.validateAgentReasoningEffort(effectiveBackend, effectiveModel, effort); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		effortChanged = effort != agentCfg.ReasoningEffort
		agentCfg.ReasoningEffort = effort
	}
	s.deps.Config.Agents[name] = agentCfg

	// Sync updated backend/model into the agent process so status builders
	// reflect the change without requiring a restart.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after model update", "agent", name, "error", err)
	}
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after model update", "agent", name, "error", err)
		}
	}
	s.auditFromRequest(r, "config_agent_models", auditDetail("backend", agentCfg.Backend, "model", agentCfg.Model, "reasoning_effort", agentCfg.ReasoningEffort), name)

	// Apply the change to the LIVE launch configuration, exactly as
	// handleAgentConfigGeneral and the card dropdowns (/api/model, /api/switch)
	// do (#7374). UpdateConfig above only refreshed agent.Config; a stale
	// ModelOverride left by an earlier /api/model or the governor still won
	// at launch, so "persist, then restart" produced a real respawn on the
	// OLD model with a 200 and a correct-looking overlay file. Setting the
	// override to the saved value keeps config, status and the next launch
	// in agreement; the effort lives in agent.Config and is already synced.
	// Model, backend and effort are all launch-time flags, so the change is
	// applied by restarting — once, for however many of them moved.
	if modelChanged {
		if err := s.deps.AgentMgr.SetModelOverride(name, agentCfg.Model); err != nil {
			s.logger.Warn("failed to apply model from config endpoint", "agent", name, "error", err)
		}
	}
	if backendChanged {
		if err := s.deps.AgentMgr.SetBackendOverride(name, agentCfg.Backend); err != nil {
			s.logger.Warn("failed to apply backend from config endpoint", "agent", name, "error", err)
		}
	}
	resp := map[string]any{"ok": true, "status": "updated", "agent": name, "applied": true, "restarted": false}
	if modelChanged || backendChanged || effortChanged {
		if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
			// Persisted and applied to the launch configuration, but the
			// running session (if any) is still on the old flags. Say so in
			// the response rather than returning a bare "updated" — the
			// silent success is what made this bug invisible.
			s.logger.Warn("restart after config-endpoint model/backend/effort change failed", "agent", name, "error", err)
			resp["status"] = "updated; applied to the launch configuration but the restart failed — restart the agent to pick it up"
			resp["restart_error"] = err.Error()
		} else {
			resp["restarted"] = true
			s.deps.Logger.Info("audit: agent restarted to apply config change", "agent", name,
				"model_changed", modelChanged, "backend_changed", backendChanged, "effort_changed", effortChanged, "trigger", "dashboard-api")
		}
	}
	resp["minStatusSeq"] = s.refreshAndPersistSeq()
	jsonResponse(w, resp)
}

func (s *Server) handleAgentConfigPipeline(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]bool
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.pipelineMu.Lock()
	s.agentPipelines[name] = body
	s.pipelineMu.Unlock()

	s.auditFromRequest(r, "config_agent_pipeline", auditDetail("section", "pipeline"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigHooks(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string][]any
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.hooksMu.Lock()
	s.agentHooks[name] = body
	s.hooksMu.Unlock()

	s.auditFromRequest(r, "config_agent_hooks", auditDetail("section", "hooks"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigRestrictions(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Agent []restriction `json:"agent"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	restFile := fmt.Sprintf("/data/agents/%s/restrictions.conf", name)
	_ = os.MkdirAll(fmt.Sprintf("/data/agents/%s", name), 0o755)

	var lines []string
	for _, r := range body.Agent {
		pattern := strings.ReplaceAll(r.Pattern, "\n", "")
		pattern = strings.ReplaceAll(pattern, "\r", "")
		reason := strings.ReplaceAll(r.Reason, "\n", "")
		reason = strings.ReplaceAll(reason, "\r", "")
		line := pattern
		if reason != "" {
			line += "|" + reason
		}
		lines = append(lines, line)
	}
	tmpRest := restFile + ".tmp"
	if err := os.WriteFile(tmpRest, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		s.logger.Warn("rest file write failed", "error", err)
	} else if err := os.Rename(tmpRest, restFile); err != nil {
		s.logger.Warn("rest file rename failed", "error", err)
	}

	s.auditFromRequest(r, "config_agent_restrictions", auditDetail("section", "restrictions"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigStats(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Stats []any `json:"stats"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	statsFile := agentStatsPath(name)
	_ = os.MkdirAll(filepath.Dir(statsFile), 0o755)

	data, err := json.Marshal(body)
	if err == nil {
		tmpStats := statsFile + ".tmp"
		if err := os.WriteFile(tmpStats, data, 0o644); err != nil {
			s.logger.Warn("stats file write failed", "error", err)
		} else if err := os.Rename(tmpStats, statsFile); err != nil {
			s.logger.Warn("stats file rename failed", "error", err)
		}
	}

	s.auditFromRequest(r, "config_agent_stats", auditDetail("section", "stats"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigChannels(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Channels []config.ChannelConfig `json:"channels"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// Fail fast on channel types with no trigger runtime (#5591): persisting
	// them would validate a config that silently never kicks the agent.
	if err := config.ValidateChannels(name, body.Channels); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg.Channels = body.Channels
	s.deps.Config.Agents[name] = agentCfg
	s.auditFromRequest(r, "config_agent_channels", auditDetail("section", "channels"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigTools(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body config.ToolsConfig
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg.Tools = &body
	s.deps.Config.Agents[name] = agentCfg
	s.auditFromRequest(r, "config_agent_tools", auditDetail("section", "tools"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigConnections(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Connections []config.ConnectionConfig `json:"connections"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg.Connections = body.Connections
	s.deps.Config.Agents[name] = agentCfg
	s.auditFromRequest(r, "config_agent_connections", auditDetail("section", "connections"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

const maskedSecret = "***"

func maskConnectionAuth(conns []config.ConnectionConfig) []config.ConnectionConfig {
	if len(conns) == 0 {
		return conns
	}
	result := make([]config.ConnectionConfig, len(conns))
	copy(result, conns)
	for i, c := range result {
		result[i].URI = uriWithoutTokenQuery(c.URI)
		if c.Auth != nil {
			masked := *c.Auth
			if masked.EnvVar != "" {
				masked.EnvVar = masked.EnvVar + " (" + maskedSecret + ")"
			}
			result[i].Auth = &masked
		}
	}
	return result
}

func uriWithoutTokenQuery(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	q := u.Query()
	if !q.Has(taskmcp.TokenQueryParam) {
		return raw
	}
	q.Del(taskmcp.TokenQueryParam)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) handleAgentPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	rawParam := r.URL.Query().Get("raw")
	var template string
	if rawParam == "1" {
		template = s.loadPromptTemplateRaw(name)
	} else {
		template = s.loadPromptTemplate(name)
	}

	const repoBaseURL = "https://github.com/hivecommons/hive/blob/HEAD/"
	sourceFiles := []map[string]string{}

	templateName := ""
	if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
		templateName = ac.KickTemplate
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	// Only link to a repo path that exists (hivecommons/hive#7390). The
	// editor used to render "src/pkg/policies/defaults/<kick_template>" for
	// whatever name was configured, and the link 404'd for a dangling one —
	// the operator's first clue that anything was wrong, and a misleading
	// one (it read as "the file was removed", not "it never existed").
	_, embeddedErr := policies.DefaultPolicies.ReadFile("defaults/" + templateName)
	src := map[string]string{
		"label": "Kick template",
		"path":  "src/pkg/policies/defaults/" + templateName,
		"note":  "kick_template: " + templateName,
	}
	if embeddedErr == nil {
		src["url"] = repoBaseURL + "src/pkg/policies/defaults/" + templateName
	} else {
		src["note"] = "kick_template: " + templateName + " — not shipped in pkg/policies/defaults (no repo link)"
	}
	sourceFiles = append(sourceFiles, src)

	resp := map[string]interface{}{
		"agent":       name,
		"prompt":      template,
		"sourceFiles": sourceFiles,
	}
	// template: how the configured kick_template actually resolves, from the
	// scheduler's own chain, so the editor can say "template not found:
	// review.md — kicks fall back to <source>" instead of showing an empty
	// box that looks like a lost file and, once typed into and saved,
	// becomes a live override.
	if s.deps.Scheduler != nil {
		resp["template"] = s.deps.Scheduler.ResolveTemplate(name)
	}
	jsonResponse(w, resp)
}

// loadPromptTemplateRaw returns the raw template content without variable substitution.
func (s *Server) loadPromptTemplateRaw(name string) string {
	templateName := ""
	if s.deps != nil && s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
			templateName = ac.KickTemplate
		}
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	paths := []string{
		fmt.Sprintf("/data/policies/examples/kubestellar/agents/%s", templateName),
	}
	if s.deps != nil && s.deps.Config != nil {
		policyDir := s.deps.Config.Policies.LocalDir
		if policyDir != "" {
			paths = append(paths,
				fmt.Sprintf("%s/examples/kubestellar/agents/%s", policyDir, templateName),
				fmt.Sprintf("%s/%s%s", policyDir, s.deps.Config.Policies.Path, templateName),
			)
		}
	}
	// Check the user-saved templates first.
	userPath := filepath.Join(promptTemplateSaveDir, templateName)
	if data, err := os.ReadFile(userPath); err == nil {
		return string(data)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return string(data)
	}
	return ""
}

const defaultPromptTemplateSaveDir = "/data/policies"

// promptTemplateSaveDir is the durable location for operator-saved and baked
// prompt templates. Tests redirect it before any server is constructed so a
// prompt-save exercise cannot overwrite a live template on a hive host.
var promptTemplateSaveDir = defaultPromptTemplateSaveDir

func (s *Server) handleAgentPromptSave(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Template string `json:"template"`
		// PromptSource + KeepLinked let the Prompt Template tab import the kick
		// prompt from a GitHub repo. When PromptSource is set:
		//   - KeepLinked=true  → persist prompt_source (live: resolved at kick time)
		//   - KeepLinked=false → FetchOnce + bake into the template file, and CLEAR
		//     any existing prompt_source so the prompt is a plain, unlinked copy.
		// When PromptSource is nil this behaves exactly as before (save inline text).
		PromptSource *struct {
			Owner string `json:"owner"`
			Repo  string `json:"repo"`
			Path  string `json:"path"`
			Ref   string `json:"ref"`
		} `json:"promptSource"`
		KeepLinked bool `json:"keepLinked"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := s.deps.Config.Agents[name]

	// Repo-sourced prompt import (Prompt Template tab).
	if body.PromptSource != nil {
		ps := &config.PromptSourceConfig{
			Type:  "github",
			Owner: sanitizeString(body.PromptSource.Owner),
			Repo:  sanitizeString(body.PromptSource.Repo),
			Path:  sanitizeString(body.PromptSource.Path),
			Ref:   sanitizeString(body.PromptSource.Ref),
		}
		if !ps.IsSet() {
			jsonError(w, "prompt source: owner, repo, and path are all required", http.StatusBadRequest)
			return
		}
		agentCfg.PromptSource = ps
		// bakePromptSource validates against the seed-only allowlist (rejecting a
		// non-allowlisted repo server-side), fetches once, writes the baked prompt
		// to the template file, and repoints KickTemplate.
		if err := s.bakePromptSource(r.Context(), name, &agentCfg); err != nil {
			jsonError(w, "prompt source: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !body.KeepLinked {
			// Bake-only: the prompt is now a plain copy on disk; drop the live link.
			agentCfg.PromptSource = nil
		}
		s.deps.Config.Agents[name] = agentCfg
		if err := s.saveConfig(); err != nil {
			s.logger.Error("failed to persist config after prompt import", "agent", name, "error", err)
		}
		if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
			if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
				s.logger.Error("failed to persist agent overlay after prompt import", "agent", name, "error", err)
			}
		}
		s.refreshAndPersist()
		s.auditFromRequest(r, "config_agent_prompt", auditDetail("section", "prompt"), name)
		s.logger.Info("prompt imported from repo", "agent", name, "linked", body.KeepLinked)
		okResponse(w, map[string]string{"status": "imported", "agent": name, "kickTemplate": agentCfg.KickTemplate})
		return
	}

	templateFileName := name + ".md"
	if agentCfg.KickTemplate != "" {
		templateFileName = agentCfg.KickTemplate
	}

	if err := os.MkdirAll(promptTemplateSaveDir, 0o755); err != nil {
		jsonError(w, "failed to create policies directory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	savePath := filepath.Join(promptTemplateSaveDir, templateFileName)
	if err := os.WriteFile(savePath, []byte(body.Template), 0o644); err != nil {
		jsonError(w, "failed to save template: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Point the agent's KickTemplate to the saved file
	agentCfg.KickTemplate = templateFileName
	s.deps.Config.Agents[name] = agentCfg

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after template save", "agent", name, "error", err)
	}
	s.refreshAndPersist()

	s.auditFromRequest(r, "config_agent_prompt", auditDetail("section", "prompt"), name)
	s.logger.Info("prompt template saved", "agent", name, "path", savePath)
	okResponse(w, map[string]string{"status": "saved", "path": savePath})
}

const exportAPIVersion = "hive.kubestellar.io/v1"

const exportKind = "AgentDefinition"

func (s *Server) handleAgentExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	rawTemplate := s.loadPromptTemplateRaw(name)

	includeRepos := true
	if agentCfg.IncludeRepos != nil {
		includeRepos = *agentCfg.IncludeRepos
	}

	// Collect cadences from governor modes
	cadences := map[string]string{}
	for modeName, modeCfg := range s.deps.Config.Governor.Modes {
		if c, ok := modeCfg.Cadences[name]; ok {
			cadences[modeName] = c.String()
		}
	}

	yamlContent := s.buildExportYAML(name, agentCfg, cadences, includeRepos, rawTemplate)

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/yaml") || strings.Contains(accept, "application/yaml") {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.yaml", name))
		_, _ = w.Write([]byte(yamlContent))
		return
	}

	jsonResponse(w, map[string]interface{}{
		"name": name,
		"yaml": yamlContent,
	})
}

func (s *Server) buildExportYAML(name string, cfg config.AgentConfig, cadences map[string]string, includeRepos bool, promptTemplate string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("apiVersion: %s\n", exportAPIVersion))
	b.WriteString(fmt.Sprintf("kind: %s\n", exportKind))
	b.WriteString("metadata:\n")
	b.WriteString(fmt.Sprintf("  name: %s\n", name))
	if cfg.DisplayName != "" {
		b.WriteString(fmt.Sprintf("  displayName: %q\n", cfg.DisplayName))
	}
	if cfg.Description != "" {
		b.WriteString(fmt.Sprintf("  description: %q\n", cfg.Description))
	}
	if cfg.Emoji != "" {
		b.WriteString(fmt.Sprintf("  emoji: %q\n", cfg.Emoji))
	}
	if cfg.Color != "" {
		b.WriteString(fmt.Sprintf("  color: %q\n", cfg.Color))
	}
	hasNewFeatures := len(cfg.Channels) > 0 || cfg.Tools != nil || len(cfg.Connections) > 0
	if hasNewFeatures {
		b.WriteString("  specVersion: 1\n")
	}
	b.WriteString("spec:\n")
	b.WriteString(fmt.Sprintf("  backend: %s\n", valueOrDefault(cfg.Backend, "copilot")))
	if cfg.Model != "" {
		b.WriteString(fmt.Sprintf("  model: %s\n", cfg.Model))
	}
	if cfg.Role != "" {
		b.WriteString(fmt.Sprintf("  role: %s\n", cfg.Role))
	}
	if cfg.Mode != "" {
		b.WriteString(fmt.Sprintf("  mode: %s\n", cfg.Mode))
	}
	b.WriteString(fmt.Sprintf("  sortOrder: %d\n", cfg.SortOrder))
	b.WriteString(fmt.Sprintf("  beadRole: %s\n", cfg.GetBeadRole()))
	b.WriteString(fmt.Sprintf("  staleTimeout: %d\n", cfg.StaleTimeout))
	b.WriteString(fmt.Sprintf("  restartStrategy: %s\n", valueOrDefault(cfg.RestartStrategy, "immediate")))
	b.WriteString(fmt.Sprintf("  clearOnKick: %t\n", cfg.ClearOnKick))
	b.WriteString(fmt.Sprintf("  includeRepos: %t\n", includeRepos))

	if len(cfg.LaneKeywords) > 0 {
		b.WriteString("  laneKeywords:\n")
		for _, kw := range cfg.LaneKeywords {
			b.WriteString(fmt.Sprintf("    - %s\n", kw))
		}
	}
	if len(cfg.DetectKeywords) > 0 {
		b.WriteString("  detectKeywords:\n")
		for _, kw := range cfg.DetectKeywords {
			b.WriteString(fmt.Sprintf("    - %s\n", kw))
		}
	}
	if len(cfg.Aliases) > 0 {
		b.WriteString("  aliases:\n")
		for _, a := range cfg.Aliases {
			b.WriteString(fmt.Sprintf("    - %s\n", a))
		}
	}

	if len(cfg.Channels) > 0 {
		b.WriteString("  channels:\n")
		for _, ch := range cfg.Channels {
			b.WriteString(fmt.Sprintf("    - type: %s\n", ch.Type))
			if ch.Enabled != nil {
				b.WriteString(fmt.Sprintf("      enabled: %t\n", *ch.Enabled))
			}
		}
	}

	if cfg.Tools != nil {
		b.WriteString("  tools:\n")
		if cfg.Tools.Preset != "" {
			b.WriteString(fmt.Sprintf("    preset: %s\n", cfg.Tools.Preset))
		}
		if len(cfg.Tools.Rules) > 0 {
			b.WriteString("    rules:\n")
			for _, r := range cfg.Tools.Rules {
				b.WriteString(fmt.Sprintf("      - pattern: %q\n", r.Pattern))
				b.WriteString(fmt.Sprintf("        action: %s\n", r.Action))
				if r.Reason != "" {
					b.WriteString(fmt.Sprintf("        reason: %q\n", r.Reason))
				}
			}
		}
	}

	if len(cfg.Connections) > 0 {
		b.WriteString("  connections:\n")
		for _, c := range maskConnectionAuth(cfg.Connections) {
			b.WriteString(fmt.Sprintf("    - name: %s\n", c.Name))
			b.WriteString(fmt.Sprintf("      type: %s\n", c.Type))
			if c.URI != "" {
				b.WriteString(fmt.Sprintf("      uri: %q\n", c.URI))
			}
			if c.EnvName != "" {
				b.WriteString(fmt.Sprintf("      env_name: %s\n", c.EnvName))
			}
			if c.Auth != nil {
				b.WriteString("      auth:\n")
				b.WriteString(fmt.Sprintf("        type: %s\n", c.Auth.Type))
				if c.Auth.EnvVar != "" {
					b.WriteString(fmt.Sprintf("        env_var: %s\n", c.Auth.EnvVar))
				}
				if c.Auth.File != "" {
					b.WriteString(fmt.Sprintf("        file: %s\n", c.Auth.File))
				}
			}
			if len(c.Options) > 0 {
				b.WriteString("      options:\n")
				for k, v := range c.Options {
					b.WriteString(fmt.Sprintf("        %s: %q\n", k, v))
				}
			}
		}
	}

	if len(cadences) > 0 {
		b.WriteString("  cadences:\n")
		modeOrder := []string{"idle", "quiet", "busy", "surge"}
		for _, mode := range modeOrder {
			if c, ok := cadences[mode]; ok {
				if strings.Contains(c, "\n") {
					b.WriteString(fmt.Sprintf("    %s:\n", mode))
					for _, line := range strings.Split(c, "\n") {
						if strings.TrimSpace(line) == "" {
							continue
						}
						b.WriteString("      " + line + "\n")
					}
				} else {
					b.WriteString(fmt.Sprintf("    %s: %s\n", mode, c))
				}
			}
		}
	}

	if promptTemplate != "" {
		b.WriteString("  promptTemplate: |\n")
		for _, line := range strings.Split(promptTemplate, "\n") {
			b.WriteString("    " + line + "\n")
		}
	}

	return b.String()
}

func valueOrDefault(v, dflt string) string {
	if v != "" {
		return v
	}
	return dflt
}

// statSourcesFor returns the stat sources the Stats tab may offer an agent.
// agentCfg == nil means unscoped (every source).
func statSourcesFor(agentCfg *config.AgentConfig) map[string]any {
	sources := map[string]any{
		"status": map[string]any{
			"label":  "Repo Status",
			"fields": []string{"actionableCount", "openPrCount", "mergeableCount"},
		},
		"agentMetrics": map[string]any{
			"label": "Agent Metrics",
			"fields": []string{
				"stars", "forks", "contributors", "adopters", "acmm",
				"outreachOpen", "outreachMerged", "coverage", "prs", "closed",
			},
		},
		"tokens": map[string]any{
			"label":  "Token Usage",
			"fields": []string{"input", "output", "cacheRead", "cacheCreate", "sessions", "messages"},
		},
	}
	return sources
}

var statStyles = []string{"number", "dot", "pct", "pct-bar", "spark"}

// handleStatSources answers the stat source catalogue. With ?agent=<name> the
// catalogue is scoped to the requested agent when future sources need it.
func (s *Server) handleStatSources(w http.ResponseWriter, r *http.Request) {
	var scope *config.AgentConfig
	if name := r.URL.Query().Get("agent"); name != "" {
		if agentCfg, ok := s.deps.Config.Agents[name]; ok {
			scope = &agentCfg
		}
	}
	jsonResponse(w, map[string]any{"sources": statSourcesFor(scope), "styles": statStyles})
}

var defaultPipelineSteps = map[string]bool{
	"resolve-beads":  true,
	"track-prs":      true,
	"stale-check":    true,
	"repo-scan":      true,
	"coverage-gate":  true,
	"prompt-compose": true,
	"budget-check":   true,
	"api-collect":    true,
	"final-compose":  true,
}

func (s *Server) getAgentPipeline(name string) map[string]bool {
	s.pipelineMu.RLock()
	defer s.pipelineMu.RUnlock()
	if p, ok := s.agentPipelines[name]; ok {
		return p
	}
	result := make(map[string]bool, len(defaultPipelineSteps))
	for k, v := range defaultPipelineSteps {
		result[k] = v
	}
	return result
}

func (s *Server) getAgentHooks(name string) map[string][]any {
	s.hooksMu.RLock()
	defer s.hooksMu.RUnlock()
	if h, ok := s.agentHooks[name]; ok {
		return h
	}
	return map[string][]any{"preKick": {}, "postIdle": {}}
}
