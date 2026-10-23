package dashboard

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// featuresRunsBody is the part of the Features PUT that covers the long-running
// runs block: runs.checkpoints.*, runs.wait_*, the scalar runs.triage keys, the
// scalar runs.spektacular.recheck keys and the runs.external bindings' endpoint,
// workflow versions and the vibe-kanban host. Every field is a pointer so an
// absent key leaves the config untouched.
//
// runs.external.vibe_kanban.mcp_command is deliberately read-only (returned by
// GET, never accepted on PUT): it names a local command the hive executes. List
// keys (triage labels, recheck sources and egress allow-list) stay yaml-only.
type featuresRunsBody struct {
	RunsCheckpointSpec      *bool   `json:"runsCheckpointSpec"`
	RunsCheckpointPlan      *bool   `json:"runsCheckpointPlan"`
	RunsCheckpointImplement *bool   `json:"runsCheckpointImplement"`
	RunsWaitTimeoutSeconds  *int    `json:"runsWaitTimeoutSeconds"`
	RunsWaitSeverity        *string `json:"runsWaitSeverity"`

	RunsTriageEnabled        *bool `json:"runsTriageEnabled"`
	RunsTriageMinBodyChars   *int  `json:"runsTriageMinBodyChars"`
	RunsTriageClarifyComment *bool `json:"runsTriageClarifyComment"`

	RunsTriageClarityGateEnabled   *bool `json:"runsTriageClarityGateEnabled"`
	RunsTriageClarityGateThreshold *int  `json:"runsTriageClarityGateThreshold"`

	RunsTriagePriorArtEnabled   *bool `json:"runsTriagePriorArtEnabled"`
	RunsTriagePriorArtThreshold *int  `json:"runsTriagePriorArtThreshold"`

	RecheckEnabled         *bool   `json:"spektacularRecheckEnabled"`
	RecheckDefaultInterval *string `json:"spektacularRecheckDefaultInterval"`
	RecheckMaxDeltaTasks   *int    `json:"spektacularRecheckMaxDeltaTasks"`
	RecheckDiscoveryProxy  *string `json:"spektacularRecheckDiscoveryProxy"`
	DiscoveryEnabled       *bool   `json:"spektacularDiscoveryEnabled"`
	DiscoveryTimeout       *string `json:"spektacularDiscoveryTimeout"`
	DiscoveryMaxTotalItems *int    `json:"spektacularDiscoveryMaxTotalItems"`

	ExtFlueEndpoint        *string `json:"extFlueEndpoint"`
	ExtFlueWorkflowVersion *string `json:"extFlueWorkflowVersion"`
	ExtOMPWorkflowVersion  *string `json:"extOmpWorkflowVersion"`

	ExtVibeKanbanEnabled         *bool   `json:"extVibeKanbanEnabled"`
	ExtVibeKanbanMode            *string `json:"extVibeKanbanMode"`
	ExtVibeKanbanProjectID       *string `json:"extVibeKanbanProjectId"`
	ExtVibeKanbanStateDir        *string `json:"extVibeKanbanStateDir"`
	ExtVibeKanbanWorkflowVersion *string `json:"extVibeKanbanWorkflowVersion"`
}

// runsWaitSeverities mirrors pkg/escalate's catalogued severities; the
// dashboard import ratchet keeps this package off pkg/escalate.
var runsWaitSeverities = []string{"decision", "info"}

// maxRunsSettingLen bounds the free-text runs settings (workflow versions and
// the vibe-kanban project id).
const maxRunsSettingLen = 200

// prepare validates every field the request carried against cur and returns
// the mutation to apply. Nothing is changed when an error is returned.
func (b featuresRunsBody) prepare(cur *config.Config) (func(*config.Config), error) {
	var steps []func(*config.Config)

	for _, f := range []struct {
		v   *bool
		dst func(*config.Config) **bool
	}{
		{b.RunsCheckpointSpec, func(c *config.Config) **bool { return &c.Runs.Checkpoints.Spec }},
		{b.RunsCheckpointPlan, func(c *config.Config) **bool { return &c.Runs.Checkpoints.Plan }},
		{b.RunsCheckpointImplement, func(c *config.Config) **bool { return &c.Runs.Checkpoints.Implement }},
		{b.RunsTriageClarifyComment, func(c *config.Config) **bool { return &c.Runs.Triage.ClarifyComment }},
		{b.RunsTriagePriorArtEnabled, func(c *config.Config) **bool { return &c.Runs.Triage.PriorArt.Enabled }},
	} {
		if f.v == nil {
			continue
		}
		v := *f.v
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = &v })
	}
	for _, f := range []struct {
		v   *bool
		dst func(*config.Config) *bool
	}{
		{b.RunsTriageEnabled, func(c *config.Config) *bool { return &c.Runs.Triage.Enabled }},
		{b.RunsTriageClarityGateEnabled, func(c *config.Config) *bool { return &c.Runs.Triage.ClarityGate.Enabled }},
		{b.RecheckEnabled, func(c *config.Config) *bool { return &c.Runs.Spektacular.Recheck.Enabled }},
		{b.DiscoveryEnabled, func(c *config.Config) *bool { return &c.Runs.Spektacular.Recheck.Discovery.Enabled }},
		{b.ExtVibeKanbanEnabled, func(c *config.Config) *bool { return &c.Runs.External.VibeKanban.Enabled }},
	} {
		if f.v == nil {
			continue
		}
		v := *f.v
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = v })
	}

	for _, f := range []struct {
		v    *int
		name string
		dst  func(*config.Config) *int
	}{
		{b.RunsWaitTimeoutSeconds, "runs wait_timeout_seconds", func(c *config.Config) *int { return &c.Runs.WaitTimeoutSeconds }},
		{b.RunsTriageMinBodyChars, "runs triage min_body_chars", func(c *config.Config) *int { return &c.Runs.Triage.MinBodyChars }},
		{b.RecheckMaxDeltaTasks, "runs spektacular recheck max_delta_tasks", func(c *config.Config) *int { return &c.Runs.Spektacular.Recheck.MaxDeltaTasks }},
		{b.DiscoveryMaxTotalItems, "runs spektacular recheck discovery max_total_items", func(c *config.Config) *int { return &c.Runs.Spektacular.Recheck.Discovery.MaxTotalItems }},
	} {
		if f.v == nil {
			continue
		}
		if *f.v < 0 {
			return nil, fmt.Errorf("%s must be 0 (default) or greater", f.name)
		}
		v := *f.v
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = v })
	}

	if b.RunsTriageClarityGateThreshold != nil {
		v := *b.RunsTriageClarityGateThreshold
		if v < 0 || v > 100 {
			return nil, fmt.Errorf("runs triage clarity_gate threshold must be 0 (default) to 100")
		}
		steps = append(steps, func(c *config.Config) { c.Runs.Triage.ClarityGate.Threshold = v })
	}
	if b.RunsTriagePriorArtThreshold != nil {
		v := *b.RunsTriagePriorArtThreshold
		if v < 0 || v > 100 {
			return nil, fmt.Errorf("runs triage prior_art threshold must be 0 (default) to 100")
		}
		steps = append(steps, func(c *config.Config) { c.Runs.Triage.PriorArt.Threshold = v })
	}

	if b.RunsWaitSeverity != nil {
		v := strings.ToLower(strings.TrimSpace(*b.RunsWaitSeverity))
		valid := v == ""
		for _, s := range runsWaitSeverities {
			valid = valid || v == s
		}
		if !valid {
			return nil, fmt.Errorf("runs wait_severity must be empty (default) or one of %s", strings.Join(runsWaitSeverities, ", "))
		}
		steps = append(steps, func(c *config.Config) { c.Runs.WaitSeverity = v })
	}

	if b.RecheckDefaultInterval != nil {
		d, err := parseSettingsDuration("runs spektacular recheck default_interval", *b.RecheckDefaultInterval)
		if err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Runs.Spektacular.Recheck.DefaultInterval = d })
	}
	if b.DiscoveryTimeout != nil {
		d, err := parseSettingsDuration("runs spektacular recheck discovery timeout", *b.DiscoveryTimeout)
		if err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Runs.Spektacular.Recheck.Discovery.Timeout = d })
	}
	if b.RecheckDiscoveryProxy != nil {
		proxy := strings.TrimSpace(*b.RecheckDiscoveryProxy)
		if proxy != "" {
			u, err := url.Parse(proxy)
			if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
				return nil, fmt.Errorf("runs spektacular recheck discovery_proxy must be empty or the HTTP(S) relay proxy without credentials, path, query or fragment")
			}
		}
		// With exact sources configured the proxy is mandatory; reuse the
		// config validator so the dialog cannot save a policy boot rejects.
		recheck := cur.Runs.Spektacular.Recheck
		recheck.DiscoveryProxy = proxy
		if err := recheck.ValidateDiscovery(); err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Runs.Spektacular.Recheck.DiscoveryProxy = proxy })
	}

	if b.ExtFlueEndpoint != nil {
		v := strings.TrimRight(strings.TrimSpace(*b.ExtFlueEndpoint), "/")
		if v != "" {
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, fmt.Errorf("runs external flue endpoint must be empty or an http(s) URL without credentials, query or fragment")
			}
		}
		steps = append(steps, func(c *config.Config) { c.Runs.External.Flue.Endpoint = v })
	}

	if b.ExtVibeKanbanMode != nil && !config.ValidExternalBindingMode(*b.ExtVibeKanbanMode) {
		return nil, fmt.Errorf("extVibeKanbanMode must be one of %s", strings.Join(config.ExternalBindingModes(), ", "))
	}
	if b.ExtVibeKanbanMode != nil {
		v := strings.TrimSpace(*b.ExtVibeKanbanMode)
		steps = append(steps, func(c *config.Config) { c.Runs.External.VibeKanban.Mode = v })
	}
	if b.ExtVibeKanbanStateDir != nil {
		p, err := settingsAbsPath("runs external vibe_kanban state_dir", *b.ExtVibeKanbanStateDir, true)
		if err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Runs.External.VibeKanban.StateDir = p })
	}

	for _, f := range []struct {
		raw  *string
		name string
		dst  func(*config.Config) *string
	}{
		{b.ExtFlueWorkflowVersion, "runs external flue workflow_version", func(c *config.Config) *string { return &c.Runs.External.Flue.WorkflowVersion }},
		{b.ExtOMPWorkflowVersion, "runs external omp workflow_version", func(c *config.Config) *string { return &c.Runs.External.OMP.WorkflowVersion }},
		{b.ExtVibeKanbanWorkflowVersion, "runs external vibe_kanban workflow_version", func(c *config.Config) *string { return &c.Runs.External.VibeKanban.WorkflowVersion }},
		{b.ExtVibeKanbanProjectID, "runs external vibe_kanban project_id", func(c *config.Config) *string { return &c.Runs.External.VibeKanban.ProjectID }},
	} {
		if f.raw == nil {
			continue
		}
		v := sanitizeString(*f.raw)
		if len(v) > maxRunsSettingLen || strings.ContainsAny(v, " \t\r\n") {
			return nil, fmt.Errorf("%s must be at most %d characters with no whitespace", f.name, maxRunsSettingLen)
		}
		dst := f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = v })
	}

	return func(c *config.Config) {
		for _, step := range steps {
			step(c)
		}
	}, nil
}

// featuresRunsResponse is the GET side of featuresRunsBody. Checkpoints and
// clarify_comment report their effective value (an unset key means true).
// The extFlueModeSetting/extOmpModeSetting/extVibeKanbanModeSetting values are
// the raw configured modes, so the mode selects show what hive.yaml holds
// rather than the effective "off" of a disabled binding.
func featuresRunsResponse(cfg *config.Config) map[string]interface{} {
	r := cfg.Runs
	vk := r.External.VibeKanban
	return map[string]interface{}{
		"runsCheckpointSpec":      r.CheckpointBlocks("spec"),
		"runsCheckpointPlan":      r.CheckpointBlocks("plan"),
		"runsCheckpointImplement": r.CheckpointBlocks("implement"),
		"runsWaitTimeoutSeconds":  r.WaitTimeoutSeconds,
		"runsWaitSeverity":        r.WaitSeverity,
		"runsWaitSeverities":      runsWaitSeverities,

		"runsTriageEnabled":        r.Triage.Enabled,
		"runsTriageMinBodyChars":   r.Triage.MinBodyChars,
		"runsTriageClarifyComment": r.Triage.ShouldClarifyComment(),

		"runsTriageClarityGateEnabled":   r.Triage.ClarityGate.Enabled,
		"runsTriageClarityGateThreshold": r.Triage.ClarityGate.Threshold,

		"runsTriagePriorArtEnabled":   r.Triage.PriorArt.IsEnabled(),
		"runsTriagePriorArtThreshold": r.Triage.PriorArt.Threshold,

		"spektacularRecheckEnabled":         r.Spektacular.Recheck.Enabled,
		"spektacularRecheckDefaultInterval": settingsDurationString(r.Spektacular.Recheck.DefaultInterval),
		"spektacularRecheckMaxDeltaTasks":   r.Spektacular.Recheck.MaxDeltaTasks,
		"spektacularRecheckDiscoveryProxy":  r.Spektacular.Recheck.DiscoveryProxy,
		"spektacularDiscoveryEnabled":       r.Spektacular.Recheck.Discovery.Enabled,
		"spektacularDiscoveryTimeout":       settingsDurationString(r.Spektacular.Recheck.Discovery.Timeout),
		"spektacularDiscoveryMaxTotalItems": r.Spektacular.Recheck.Discovery.MaxTotalItems,

		"extFlueModeSetting":     r.External.Flue.Mode,
		"extFlueEndpoint":        r.External.Flue.Endpoint,
		"extFlueWorkflowVersion": r.External.Flue.WorkflowVersion,
		"extOmpModeSetting":      r.External.OMP.Mode,
		"extOmpWorkflowVersion":  r.External.OMP.WorkflowVersion,

		"extVibeKanbanEnabled":         vk.Enabled,
		"extVibeKanbanMode":            cfg.VibeKanbanBindingMode(),
		"extVibeKanbanModeSetting":     vk.Mode,
		"extVibeKanbanMcpCommand":      vk.MCPCommand,
		"extVibeKanbanProjectId":       vk.ProjectID,
		"extVibeKanbanStateDir":        vk.StateDir,
		"extVibeKanbanWorkflowVersion": vk.WorkflowVersion,

		"runsDefaults": map[string]interface{}{
			"runsWaitTimeoutSeconds":            config.DefaultRunsWaitTimeoutSeconds,
			"runsWaitSeverity":                  config.DefaultRunsWaitSeverity,
			"runsTriageMinBodyChars":            config.DefaultTriageMinBodyChars,
			"runsTriageClarityGateThreshold":    config.DefaultClarityGateThreshold,
			"runsTriagePriorArtThreshold":       config.DefaultPriorArtGateThreshold,
			"spektacularRecheckDefaultInterval": config.DefaultSpektacularRecheckInterval.String(),
			"spektacularRecheckMaxDeltaTasks":   config.DefaultSpektacularMaxDeltaTasks,
			"spektacularDiscoveryTimeout":       fmt.Sprintf("%ds", config.DefaultSpektacularDiscoveryTimeoutS),
			"spektacularDiscoveryMaxTotalItems": config.DefaultSpektacularDiscoveryMaxTotalItems,
			"extFlueMode":                       config.DefaultFlueBindingMode,
			"implementCheckpointMinAcmm":        config.RunImplementCheckpointMinACMM,
		},
	}
}
