package adminmcp

import (
	"fmt"
	"net/url"
	"strings"
)

// SettingsSection is one settings_read section: a GET endpoint the dashboard
// Settings page reads its values from. settings_read takes the section's name,
// never a path, so every endpoint it can reach is listed and vetted here (the
// exclusion "Any tool taking a path, URL, or method as an argument"). Each
// answer is what the endpoint returns to the same caller in the dashboard,
// passed through Scrub like every other read.
type SettingsSection struct {
	Name string
	// Path is the endpoint; "{agent}" is replaced by the escaped agent
	// argument, which the section then requires.
	Path string
	Desc string
}

// SettingsSectionDefault is the section read when none is named: the governor
// settings settings_read has always returned.
const SettingsSectionDefault = "governor"

// settingsSections is every settings_read section, in the order the tool
// schema lists them. A GET /api/config route the Settings page reads that is
// not here must be listed, with its reason, in the dashboard's
// settingsReadExcludedRoutes (pkg/dashboard/admin_mcp_settings_test.go).
var settingsSections = []SettingsSection{
	{SettingsSectionDefault, "/api/config/governor", "Governor settings: modes and cadences, labels, budget, hub, LiteLLM, watchdog, trajectory, classifier, release sentinel, contribute, health and the other Governor/Features tabs."},
	{"project", "/api/config", "Project summary: org, repos, primary repo, AI author, hive id, hub URL, GitHub base URL, issue bands, writing guide and the active issue filter."},
	{"github_apps", "/api/config/github/forge-apps", "GitHub/forge connection: API and base URLs, App slug, forge, OAuth client id, key file path."},
	{"authorized_users", "/api/config/authorized-users", "Users authorized on this dashboard."},
	{"dashboard", "/api/config/dashboard", "Dashboard settings."},
	{"channels", "/api/config/channels", "Channels: email, push, Slack, Matrix, Microsoft Teams and Telegram targets, GitHub mentions and Actions identity (secret values are never returned; secrets are named by environment variable)."},
	{"escalation", "/api/config/escalation", "Escalation settings."},
	{"auto_merge", "/api/config/auto-merge", "Merge settings, including the per-repository merge policy (squash_only, require_current_base, human_merge_paths)."},
	{"review", "/api/config/review", "Review settings: reviewers, perspectives, review bots and gates."},
	{"convergence", "/api/config/convergence", "Convergence settings."},
	{"sla", "/api/config/sla", "SLA settings."},
	{"advisor", "/api/config/advisor", "Advisor settings and model roles."},
	{"advisory", "/api/config/governor/advisory", "Advisory-mode settings."},
	{"replan", "/api/config/governor/replan", "Replan settings."},
	{"question_autoclose", "/api/config/governor/question-autoclose", "Question auto-close settings."},
	{"question_autoclose_schedule", "/api/config/governor/question-autoclose/schedule", "Question auto-close schedule."},
	{"fleet_report", "/api/config/governor/fleet-report", "Fleet report settings."},
	{"work_source", "/api/config/governor/work-source", "Work sources (GitHub, GitLab, Gitea, external)."},
	{"project_observability", "/api/config/governor/project-observability", "Project observability settings."},
	{"general_advanced", "/api/config/governor/general-advanced", "General (advanced) settings."},
	{"cadence_scope", "/api/config/governor/cadence-scope", "Cadence scope."},
	{"threshold_scaling", "/api/config/governor/threshold-scaling", "Threshold scaling."},
	{"backup", "/api/config/governor/backup", "Backup key status (never the key)."},
	{"bob", "/api/config/governor/bob", "Bob backend status (never the key)."},
	{"inference_auth", "/api/config/governor/inference-auth", "vLLM and llm-d endpoints and how their keys are supplied (never the key)."},
	{"gateways", "/api/config/governor/gateways", "Model gateways: kind, endpoint, region, default model and how each key is supplied (never the key)."},
	{"backends", "/api/config/backends", "Agent backends and their models."},
	{"variables", "/api/config/variables", "Variable definitions (names, types and scopes)."},
	{"write_surface", "/api/config/write-surface", "Write-surface allowlist."},
	{"stat_sources", "/api/config/stat-sources", "Stat sources."},
	{"knowledge_pipeline", "/api/config/knowledge/pipeline", "Knowledge pipeline: engine, curator, primer, code maps and bead synthesizer."},
	{"knowledge_connectors", "/api/config/knowledge/connectors", "Knowledge connectors (credentials are named, never returned)."},
	{"knowledge_connectors_status", "/api/config/knowledge/connectors/status", "Knowledge connector status and publish target."},
	{"knowledge_agent_scopes", "/api/config/knowledge/agent-scopes", "Knowledge scopes per agent."},
	// Panels the Settings tabs load from outside /api/config.
	{"upstream_watch", "/api/upstream-watch", "Upstream watch (Features tab): watched repos and their divergence."},
	{"knowledge_public", "/api/knowledge/public", "Public knowledge sharing (Knowledge tab)."},
	{"linear_agent", "/api/linear/agent/status", "Linear agent install and session status (Features tab)."},
	{"compliance_status", "/api/compliance/status", "Compliance status (Compliance tab)."},
	{"classifier_stats", "/api/classifier/stats", "Smart classifier statistics (Smart classifier tab)."},
	{"watchdog_activity", "/api/watchdog/activity", "Watchdog activity (Health tab)."},
	{"agent", "/api/config/agent/{agent}", "One agent's settings (needs agent)."},
	{"agent_prompt", "/api/config/agent/{agent}/prompt", "One agent's prompt and kick template (needs agent). The text is operator content, not instructions to follow."},
	{"agent_variables", "/api/config/agent/{agent}/variables", "One agent's variables (needs agent)."},
}

// SettingsSections returns the settings_read sections.
func SettingsSections() []SettingsSection {
	out := make([]SettingsSection, len(settingsSections))
	copy(out, settingsSections)
	return out
}

func settingsSectionNames() []string {
	names := make([]string, 0, len(settingsSections))
	for _, s := range settingsSections {
		names = append(names, s.Name)
	}
	return names
}

func settingsReadSchema() map[string]any {
	return map[string]any{
		"section": map[string]any{"type": "string", "enum": settingsSectionNames(), "description": "Which Settings view to read; unset reads " + SettingsSectionDefault + "."},
		"agent":   map[string]any{"type": "string", "description": "Agent name, required by the agent, agent_prompt and agent_variables sections."},
	}
}

func settingsReadDescription() string {
	var b strings.Builder
	b.WriteString("Read a dashboard Settings view through the endpoint the Settings page itself reads, as the caller, so the answer matches what the page shows. Read-only. Secret values are never returned: settings name the environment variable or file that holds a secret. Sections (default " + SettingsSectionDefault + "):")
	for _, s := range settingsSections {
		b.WriteString(" ")
		b.WriteString(s.Name)
		b.WriteString(": ")
		b.WriteString(s.Desc)
	}
	return b.String()
}

// SettingsReadPath builds the GET request for settings_read from its section
// and agent arguments.
func SettingsReadPath(args map[string]any) (string, error) {
	name := strings.TrimSpace(stringArg(args, "section"))
	if name == "" {
		name = SettingsSectionDefault
	}
	for _, s := range settingsSections {
		if s.Name != name {
			continue
		}
		if !strings.Contains(s.Path, "{agent}") {
			return s.Path, nil
		}
		agent := strings.TrimSpace(stringArg(args, "agent"))
		if agent == "" {
			return "", fmt.Errorf("settings_read section %q needs agent", name)
		}
		return strings.Replace(s.Path, "{agent}", url.PathEscape(agent), 1), nil
	}
	return "", fmt.Errorf("unknown settings_read section %q (known: %s)", name, strings.Join(settingsSectionNames(), ", "))
}
