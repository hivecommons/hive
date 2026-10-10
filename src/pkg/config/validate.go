package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	dashboardtheme "github.com/hivecommons/hive/pkg/dashboard/theme"
)

type ValidateOptions struct {
	RequireAgents bool
}

func (c *Config) Validate() error {
	return c.ValidateWithOptions(ValidateOptions{RequireAgents: true})
}

func (c *Config) validateSpektacularRecheckDiscovery() error {
	d := c.Runs.Spektacular.Recheck.Discovery
	if !d.Enabled && len(d.Sources) == 0 {
		return nil
	}
	allowedKinds := map[string]bool{
		"upstream_release": true,
		"repo_activity":    true,
		"standards_feed":   true,
		"landscape":        true,
	}
	for i, src := range d.Sources {
		kind := strings.TrimSpace(src.Kind)
		if !allowedKinds[kind] {
			return fmt.Errorf("runs.spektacular.recheck.discovery.sources[%d]: invalid kind %q (must be upstream_release, repo_activity, standards_feed, or landscape)", i, src.Kind)
		}
		if strings.TrimSpace(src.Name) == "" {
			return fmt.Errorf("runs.spektacular.recheck.discovery.sources[%d]: name is required", i)
		}
		if kind != "landscape" && strings.TrimSpace(src.URLOrRepo) == "" {
			return fmt.Errorf("runs.spektacular.recheck.discovery.sources[%d]: url_or_repo is required", i)
		}
		for _, host := range discoverySourceHosts(kind, src.URLOrRepo) {
			if !hostAllowedByHTTPList(host, c.Variables.Security.HTTPAllowlist) {
				return fmt.Errorf("runs.spektacular.recheck.discovery.sources[%d]: host %q is not covered by variables.security.http_allowlist", i, host)
			}
		}
	}
	return nil
}

func discoverySourceHosts(kind, value string) []string {
	value = strings.TrimSpace(value)
	switch kind {
	case "upstream_release", "repo_activity":
		return []string{"api.github.com"}
	case "standards_feed":
		u, err := url.Parse(value)
		if err != nil || u.Hostname() == "" {
			return []string{value}
		}
		return []string{strings.ToLower(u.Hostname())}
	case "landscape":
		return []string{"api.github.com"}
	default:
		return nil
	}
}

func hostAllowedByHTTPList(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, allowed := range allowlist {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == host || allowed == "*" {
			return true
		}
	}
	return false
}

func (c *Config) ValidateWithOptions(opts ValidateOptions) error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if opts.RequireAgents && len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	if err := ValidateWritingGuide(c.Project.WritingGuide); err != nil {
		return err
	}
	for _, rp := range c.Project.RepoPolicies {
		if err := ValidateRepoMergeStrategy(rp.Repo, rp.MergeStrategy); err != nil {
			return err
		}
	}
	// Deliberately a bare zero-test, NOT HasApp(): PlaceholderAppID exists
	// precisely so a hive awaiting its real App can satisfy this check and boot
	// into dashboard-only mode. Everywhere else, use HasApp().
	// A hive described by `forge:` alone satisfies this too: ResolvedAppID()
	// derives a real App ID from a known forge, so the identity is present even
	// though app_id is not written down. Without this, the end state of this
	// design — one field naming the forge, the rest derived — fails validation
	// and the spoke will not boot.
	// An EXPLICIT `forge:` satisfies this too: ResolvedAppID() derives a real
	// App ID from a known forge, so the identity is present even though app_id
	// is not written down. Deliberately keyed on Forge_ (the raw field) and not
	// Forge() — Forge() INFERS public for a blank config, which would make an
	// empty github block validate and silently boot a hive with no credentials
	// at all. Only a forge the operator actually wrote counts.
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 &&
		(strings.TrimSpace(c.GitHub.Forge_) == "" || c.GitHub.ResolvedAppID() == 0) {
		return fmt.Errorf("github.token, github.app_id or github.forge is required")
	}
	if err := c.GitHub.Mentions.Validate(); err != nil {
		return err
	}
	if err := c.GitHub.Actions.Validate(); err != nil {
		return err
	}
	if c.TaskMCP.LeaseRateLimitPerMinute < 0 {
		return fmt.Errorf("task_mcp.lease_rate_limit_per_minute must be >= 0")
	}
	if c.TaskMCP.LaunchTokenTTL < 0 {
		return fmt.Errorf("task_mcp.launch_token_ttl must be >= 0")
	}
	if c.GitHub.GraphQLPRBatchPageSize < 0 || c.GitHub.GraphQLPRBatchPageSize > 100 {
		return fmt.Errorf("github.graphql_pr_batch_page_size must be between 1 and 100, or 0 for the default")
	}
	if _, err := HeartbeatOmitClasses(c.Hub.HeartbeatOmit); err != nil {
		return err
	}
	if err := c.Governor.LiteLLM.Validate(); err != nil {
		return err
	}
	if err := c.Classifier.Validate(); err != nil {
		return err
	}
	if err := c.Publication.Validate(); err != nil {
		return err
	}
	if err := c.Runs.ValidateEngine(); err != nil {
		return err
	}
	if err := c.Runs.Spektacular.Validate(); err != nil {
		return err
	}
	if err := c.Jev.Validate(); err != nil {
		return err
	}
	if err := c.validateAdvisorLane(); err != nil {
		return err
	}
	if err := c.Hub.ValidateNPS(); err != nil {
		return err
	}
	if err := c.Review.ValidateReviewEventDispatch(); err != nil {
		return err
	}
	if err := c.validateGitHubActivityNotifications(); err != nil {
		return err
	}
	if err := c.Runs.Spektacular.Recheck.ValidateDiscovery(); err != nil {
		return err
	}
	if err := c.validateUpstreamWatch(); err != nil {
		return err
	}
	if err := c.ValidateRecheckDiscovery(); err != nil {
		return err
	}
	if err := ValidateKnowledgeConnectors(c.Knowledge.Connectors); err != nil {
		return err
	}
	if err := ValidateKnowledgePublish(c.Knowledge.Publish); err != nil {
		return err
	}
	if err := ValidateKnowledgeAgentScopes(c.Knowledge.AgentScopes); err != nil {
		return err
	}
	if err := c.AutoMerge.TrustedAuthors.Validate(); err != nil {
		return err
	}
	if err := c.Compliance.Validate(); err != nil {
		return err
	}
	if err := c.Review.Severity.Validate(); err != nil {
		return err
	}
	if err := c.Review.Backlog.Validate(); err != nil {
		return err
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err != nil {
		return err
	} else {
		c.Dashboard.SnapshotFrameAncestors = normalized
	}

	if normalized, err := ValidateDashboardPublicURL(c.Dashboard.PublicURL); err != nil {
		return err
	} else {
		c.Dashboard.PublicURL = normalized
	}
	if err := dashboardtheme.ValidateSelection(c.Dashboard.Theme, c.Dashboard.ThemeOverrides); err != nil {
		return err
	}
	if normalized, err := NormalizeContributeHelpLinks(c.Contribute.HelpLinks); err != nil {
		return err
	} else {
		c.Contribute.HelpLinks = normalized
	}
	if !ValidateThresholdScaling(c.Governor.ThresholdScaling) {
		return fmt.Errorf("governor: invalid threshold_scaling %q (must be linear, sqrt, or none)", c.Governor.ThresholdScaling)
	}
	if !ValidateCadenceScope(c.Governor.CadenceScope) {
		return fmt.Errorf("governor: invalid cadence_scope %q (must be aggregate or per_repo)", c.Governor.CadenceScope)
	}
	if err := ValidateBobSessionLabel("governor.bob.session_prefix", strings.TrimSpace(c.Governor.Bob.SessionPrefix)); err != nil {
		return err
	}
	apiReserve := c.GitHub.APIReserve
	if apiReserve == 0 {
		apiReserve = defaultGitHubAPIReserve
	}
	apiCritical := c.GitHub.APICritical
	if apiCritical == 0 {
		apiCritical = defaultGitHubAPICritical
	}
	if apiCritical >= apiReserve {
		return fmt.Errorf("github.api_critical must be less than github.api_reserve")
	}
	if !ValidateCoverageTarget(c.Governor.CoverageTarget) {
		return fmt.Errorf("governor.coverage_target must be between 1 and 100 (got %d)", c.Governor.CoverageTarget)
	}
	if c.Governor.EvalIntervalMaxS > 0 && c.Governor.EvalIntervalS > 0 && c.Governor.EvalIntervalMaxS < c.Governor.EvalIntervalS {
		return fmt.Errorf("governor.eval_interval_max_s must be greater than or equal to governor.eval_interval_s")
	}
	if c.Governor.EvalIntervalWebhookS < 0 {
		return fmt.Errorf("governor.eval_interval_webhook_s must be at least 0")
	}
	if c.Governor.ConserveIntervalMultiplier < 0 {
		return fmt.Errorf("governor.conserve_interval_multiplier must be at least 1")
	}
	if c.Governor.OptionalSweepEveryNCycles < 0 {
		return fmt.Errorf("governor.optional_sweep_every_n_cycles must be at least 1")
	}
	if c.Governor.Budget.USD < 0 {
		return fmt.Errorf("governor.budget.usd must be non-negative")
	}
	for backend, coin := range c.Governor.Budget.Coins {
		name := strings.TrimSpace(backend)
		if name == "" {
			return fmt.Errorf("governor.budget.coins: backend name is required")
		}
		if coin.TokensPerCoin <= 0 {
			return fmt.Errorf("governor.budget.coins.%s.tokens_per_coin must be positive", name)
		}
		if coin.USDPerCoin <= 0 {
			return fmt.Errorf("governor.budget.coins.%s.usd_per_coin must be positive", name)
		}
		if coin.Budget < 0 {
			return fmt.Errorf("governor.budget.coins.%s.budget must be non-negative", name)
		}
	}
	for modeName, mode := range c.Governor.Modes {
		for agentName, cadence := range mode.Cadences {
			if err := cadence.Validate(); err != nil {
				return fmt.Errorf("governor mode %s cadence for %s: %w", modeName, agentName, err)
			}
		}
	}
	// The hive-wide default goes through the SAME gate as the per-agent field.
	// Without this a bad value here is silently normalized to off by
	// ResolveExplainModeDefault, so an operator who typed "verbose" in the
	// dashboard would see explanation stay off with nothing saying why.
	if !ValidateExplainMode(strings.TrimSpace(c.Governor.ExplainMode)) {
		return fmt.Errorf("governor: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", c.Governor.ExplainMode, ExplainModeEnvVar)
	}
	if err := c.Governor.WorkSource.Wavefront.Validate(); err != nil {
		return fmt.Errorf("governor: %w", err)
	}
	if err := c.Governor.WorkSource.Validate(); err != nil {
		return fmt.Errorf("governor: %w", err)
	}
	if !ValidateACMMIssueTracker(strings.TrimSpace(c.Governor.ACMM.IssueTracker)) {
		return fmt.Errorf("governor: invalid acmm.issue_tracker %q (must be %s or %s, or empty for %s)", c.Governor.ACMM.IssueTracker, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource, ACMMIssueTrackerGitHub)
	}
	if err := c.Escalation.ValidateSurfaces(); err != nil {
		return err
	}
	if c.Notifications.Slack != nil && c.Notifications.Slack.Enabled {
		if strings.TrimSpace(c.Notifications.Slack.AppToken) == "" {
			return fmt.Errorf("notifications.slack.app_token is required when slack.enabled is true")
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Notifications.Slack.AppToken), "xapp-") {
			return fmt.Errorf("notifications.slack.app_token must start with xapp- when slack.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Slack.BotToken) == "" {
			return fmt.Errorf("notifications.slack.bot_token is required when slack.enabled is true")
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Notifications.Slack.BotToken), "xoxb-") {
			return fmt.Errorf("notifications.slack.bot_token must start with xoxb- when slack.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Slack.ChannelID) == "" {
			return fmt.Errorf("notifications.slack.channel_id is required when slack.enabled is true")
		}
	}
	if c.Notifications.Telegram != nil && c.Notifications.Telegram.Enabled {
		if strings.TrimSpace(c.Notifications.Telegram.BotToken) == "" {
			return fmt.Errorf("notifications.telegram.bot_token is required when telegram.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Telegram.ChatID) == "" {
			return fmt.Errorf("notifications.telegram.chat_id is required when telegram.enabled is true")
		}
	}
	if c.Notifications.Matrix != nil && c.Notifications.Matrix.Enabled {
		if strings.TrimSpace(c.Notifications.Matrix.HomeserverURL) == "" {
			return fmt.Errorf("notifications.matrix.homeserver_url is required when matrix.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Matrix.AccessToken) == "" {
			return fmt.Errorf("notifications.matrix.access_token is required when matrix.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Matrix.RoomID) == "" {
			return fmt.Errorf("notifications.matrix.room_id is required when matrix.enabled is true")
		}
	}
	if c.Notifications.MSTeams != nil && c.Notifications.MSTeams.Enabled {
		if strings.TrimSpace(c.Notifications.MSTeams.TenantID) == "" {
			return fmt.Errorf("notifications.msteams.tenant_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.ClientID) == "" {
			return fmt.Errorf("notifications.msteams.client_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.ClientSecret) == "" {
			return fmt.Errorf("notifications.msteams.client_secret is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.TeamID) == "" {
			return fmt.Errorf("notifications.msteams.team_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.ChannelID) == "" {
			return fmt.Errorf("notifications.msteams.channel_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.WebhookURL) == "" {
			return fmt.Errorf("notifications.msteams.webhook_url is required when msteams.enabled is true")
		}
	}
	for name, agent := range c.Agents {
		// One gate, shared with the config write path (dashboard agent-config
		// save) and agreeing with what the launcher can actually dispatch. A
		// configured gateway name is valid too: naming a gateway as the backend
		// routes that agent through it, matched case-insensitively to mirror
		// ResolveGateway.
		label := agentSourceLabel(name, agent.sourceFile)
		if err := c.Governor.ValidateBackend(agent.Backend); err != nil {
			return fmt.Errorf("agent %s: %w", label, err)
		}
		if agent.ReviewModels.Configured() {
			if !ValidateReviewModelsFallback(agent.ReviewModels.Fallback) {
				return fmt.Errorf("agent %s: invalid review_models.fallback %q (must be pinned, skip, or requires_human)", label, agent.ReviewModels.Fallback)
			}
			for i, entry := range agent.ReviewModels.Pool {
				if strings.TrimSpace(entry.Backend) == "" {
					return fmt.Errorf("agent %s: review_models.pool[%d]: backend is required", label, i)
				}
				if err := c.Governor.ValidateBackend(strings.TrimSpace(entry.Backend)); err != nil {
					return fmt.Errorf("agent %s: review_models.pool[%d]: %w", label, i, err)
				}
				if strings.TrimSpace(entry.Model) == "" {
					return fmt.Errorf("agent %s: review_models.pool[%d]: model is required", label, i)
				}
			}
		}
		if err := c.Governor.ValidateLaunchCmdBackend(agent.Backend, agent.LaunchCmd); err != nil {
			return fmt.Errorf("agent %s: %w", label, err)
		}
		if !ValidateCavemanMode(agent.CavemanMode) {
			return fmt.Errorf("agent %s: invalid caveman_mode %q (must be lite, full, ultra, or wenyan)", label, agent.CavemanMode)
		}
		if !ValidateJevMode(agent.JevMode) {
			return fmt.Errorf("agent %s: invalid jev_mode %q (must be off or assist)", label, agent.JevMode)
		}
		if !ValidateExplainMode(agent.ExplainMode) {
			return fmt.Errorf("agent %s: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", name, agent.ExplainMode, ExplainModeEnvVar)
		}
		if !ValidateCadenceScope(agent.CadenceScope) {
			return fmt.Errorf("agent %s: invalid cadence_scope %q (must be aggregate or per_repo)", name, agent.CadenceScope)
		}
		if agent.ContinuousCooldown < 0 {
			return fmt.Errorf("agent %s: continuous_cooldown must be positive", name)
		}
		if agent.ContinuousBudgetPct < 0 || agent.ContinuousBudgetPct > 100 {
			return fmt.Errorf("agent %s: continuous_budget_pct must be 0 (default) or between 1 and 100", name)
		}
		if err := ValidateKickTemplateName(agent.KickTemplate); err != nil {
			return fmt.Errorf("agent %s: %w", agentSourceLabel(name, agent.sourceFile), err)
		}
		if err := ValidateBobDisplayName(agent.BobDisplayName); err != nil {
			return fmt.Errorf("agent %s: %w", agentSourceLabel(name, agent.sourceFile), err)
		}
		if err := ValidateBobSessionLabel("bob.session_label", strings.TrimSpace(agent.Bob.SessionLabel)); err != nil {
			return fmt.Errorf("agent %s: %w", agentSourceLabel(name, agent.sourceFile), err)
		}
		bobSessionLabel := strings.TrimSpace(agent.Bob.SessionLabel)
		if bobSessionLabel == "" {
			bobSessionLabel = strings.TrimSpace(agent.BobDisplayName)
		}
		if bobSessionLabel == "" && strings.TrimSpace(c.Governor.Bob.SessionPrefix) != "" {
			bobSessionLabel = strings.TrimSpace(c.Governor.Bob.SessionPrefix) + name
		}
		if len(bobSessionLabel) > MaxBobSessionLabelLen {
			return fmt.Errorf("agent %s: bob session label %q is longer than %d characters", agentSourceLabel(name, agent.sourceFile), bobSessionLabel, MaxBobSessionLabelLen)
		}
		if err := validateChannels(name, agent.Channels); err != nil {
			return err
		}
		if err := validateTools(name, agent.Tools); err != nil {
			return err
		}
		if err := validateConnections(name, agent.Connections); err != nil {
			return err
		}
		if err := validateAgentVariables(name, agent.Variables); err != nil {
			return err
		}
		if err := validateAgentSpecRef(name, agent.AgentSpec); err != nil {
			return err
		}
	}
	// Standby (RFC #7629 S2). Its own pass rather than a block inside the
	// agent loop: two of its rules (an enabled lane needs a non-empty approved
	// list; the tier map must not contradict itself) are relations between the
	// agent and the hub blocks, not properties of a single agent.
	if err := c.validateStandby(); err != nil {
		return err
	}
	return nil
}

func validateAgentSpecRef(agentName, ref string) error {
	if strings.ContainsRune(ref, '\x00') {
		return fmt.Errorf("agent %s: agent_spec contains a NUL byte", agentName)
	}
	return nil
}

func (e EscalationConfig) ValidateSurfaces() error {
	if e.Email.Enabled {
		if strings.TrimSpace(e.Email.SMTP.Host) == "" {
			return fmt.Errorf("escalation.email.smtp.host is required when escalation.email.enabled is true")
		}
		if e.Email.SMTP.Port < 0 || e.Email.SMTP.Port > 65535 {
			return fmt.Errorf("escalation.email.smtp.port must be a valid TCP port")
		}
		if strings.TrimSpace(e.Email.From) == "" {
			return fmt.Errorf("escalation.email.from is required when escalation.email.enabled is true")
		}
		if countNonEmpty(e.Email.To) == 0 {
			return fmt.Errorf("escalation.email.to is required when escalation.email.enabled is true")
		}
		if e.Email.Digest.At != "" {
			if _, err := time.Parse("15:04", strings.TrimSpace(e.Email.Digest.At)); err != nil {
				return fmt.Errorf("escalation.email.digest.at must be HH:MM")
			}
		}
	}
	if e.Push.Enabled {
		min := strings.TrimSpace(e.Push.MinSeverity)
		if min == "" {
			min = "page"
		}
		if min != "decision" && min != "page" {
			return fmt.Errorf("escalation.push.min_severity must be decision or page")
		}
		providers := 0
		if strings.TrimSpace(e.Push.Ntfy.URL) != "" {
			providers++
		}
		if strings.TrimSpace(e.Push.Pushover.AppToken) != "" || strings.TrimSpace(e.Push.Pushover.UserKey) != "" {
			if strings.TrimSpace(e.Push.Pushover.AppToken) == "" || strings.TrimSpace(e.Push.Pushover.UserKey) == "" {
				return fmt.Errorf("escalation.push.pushover.app_token and user_key are required together")
			}
			providers++
		}
		if strings.TrimSpace(e.Push.PagerDuty.RoutingKey) != "" {
			providers++
		}
		if providers == 0 {
			return fmt.Errorf("escalation.push requires at least one provider when enabled")
		}
	}
	return nil
}

func (c *Config) validateGitHubActivityNotifications() error {
	if c.Notifications.Discord != nil && strings.TrimSpace(c.Notifications.Discord.FactoryWebhook) != "" {
		if err := validateWebhookURL(c.Notifications.Discord.FactoryWebhook); err != nil {
			return fmt.Errorf("notifications.discord.factory_webhook: %w", err)
		}
	}
	if c.Notifications.GitHubActivity == nil || !c.Notifications.GitHubActivity.Enabled {
		return nil
	}
	if c.Notifications.Discord == nil || strings.TrimSpace(c.Notifications.Discord.FactoryWebhook) == "" {
		return fmt.Errorf("notifications.github_activity requires notifications.discord.factory_webhook")
	}
	if c.Notifications.GitHubActivity.EffectiveOrg(c.Project.Org) == "" {
		return fmt.Errorf("notifications.github_activity.org or project.org is required")
	}
	if c.Notifications.GitHubActivity.PollIntervalS < 0 {
		return fmt.Errorf("notifications.github_activity.poll_interval_s must be non-negative")
	}
	if strings.TrimSpace(c.Notifications.GitHubActivity.APIURL) != "" {
		if err := validateAPIURL(c.Notifications.GitHubActivity.APIURL); err != nil {
			return fmt.Errorf("notifications.github_activity.api_url: %w", err)
		}
	}
	return nil
}

func countNonEmpty(in []string) int {
	count := 0
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			count++
		}
	}
	return count
}

func validateWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host")
	}
	return nil
}

func validateAPIURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host")
	}
	return nil
}

func (c *Config) validate() error {
	return c.validateWithOptions(ValidateOptions{RequireAgents: true})
}

func (c *Config) validateWithOptions(opts ValidateOptions) error {
	return c.ValidateWithOptions(opts)
}

func validateChannels(agentName string, channels []ChannelConfig) error {
	return ValidateChannels(agentName, channels)
}

// ValidateChannels rejects any channel declaration whose type has no trigger
// runtime, and rejects mention-only configs that would suppress governor kicks
// while waiting for an inbound mention. Exported so config writers such as the
// dashboard channels endpoint can fail fast before persisting.
func ValidateChannels(agentName string, channels []ChannelConfig) error {
	hasKick := false
	for _, ch := range channels {
		if ch.Type == ChannelTypeKick {
			hasKick = true
			break
		}
	}
	for i, ch := range channels {
		if ch.Type != ChannelTypeKick && ch.Type != ChannelTypeMention {
			return fmt.Errorf("agent %s: channel[%d]: type %q has no trigger runtime (only %q and %q are supported; the webhook/discord/schedule/bead runtime was removed, see #5591) — declaring it would leave the agent permanently unkicked", agentName, i, ch.Type, ChannelTypeKick, ChannelTypeMention)
		}
		if ch.Type == ChannelTypeMention && !hasKick {
			return fmt.Errorf("agent %s: channel[%d]: type %q must be paired with a %q channel; mention-only agents have no governor kick runtime and would remain permanently dormant", agentName, i, ch.Type, ChannelTypeKick)
		}
	}
	return nil
}

func validateTools(agentName string, tools *ToolsConfig) error {
	if tools == nil {
		return nil
	}
	validPresets := map[string]bool{"": true, "advisory": true, "issues-only": true, "issues-prs": true, "full": true}
	if !validPresets[tools.Preset] {
		return fmt.Errorf("agent %s: tools.preset %q is invalid (must be advisory, issues-only, issues-prs, or full)", agentName, tools.Preset)
	}
	validActions := map[string]bool{"allow": true, "deny": true}
	for i, rule := range tools.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("agent %s: tools.rules[%d]: pattern is required", agentName, i)
		}
		if !validActions[rule.Action] {
			return fmt.Errorf("agent %s: tools.rules[%d]: action must be allow or deny, got %q", agentName, i, rule.Action)
		}
	}
	return nil
}

func validateConnections(agentName string, conns []ConnectionConfig) error {
	validTypes := map[string]bool{"mcp": true, "api": true, "knowledge": true}
	seen := map[string]bool{}
	for i, conn := range conns {
		if conn.Name == "" {
			return fmt.Errorf("agent %s: connections[%d]: name is required", agentName, i)
		}
		if seen[conn.Name] {
			return fmt.Errorf("agent %s: connections[%d]: duplicate name %q", agentName, i, conn.Name)
		}
		seen[conn.Name] = true
		if !validTypes[conn.Type] {
			return fmt.Errorf("agent %s: connections[%d]: invalid type %q (must be mcp, api, or knowledge)", agentName, i, conn.Type)
		}
		if (conn.Type == "mcp" || conn.Type == "api") && conn.URI == "" {
			return fmt.Errorf("agent %s: connections[%d]: %s requires a uri", agentName, i, conn.Type)
		}
		if conn.Auth != nil {
			validAuthTypes := map[string]bool{"env": true, "file": true}
			if !validAuthTypes[conn.Auth.Type] {
				return fmt.Errorf("agent %s: connections[%d]: auth.type must be env or file, got %q", agentName, i, conn.Auth.Type)
			}
			if conn.Auth.Type == "env" && conn.Auth.EnvVar == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.env_var is required when auth.type is env", agentName, i)
			}
			if conn.Auth.Type == "file" && conn.Auth.File == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.file is required when auth.type is file", agentName, i)
			}
		}
	}
	return nil
}

// variableNamePattern is the ${NAME} identifier shape a variable name must
// have: letters, digits and underscore, not starting with a digit. The
// dashboard's variable endpoints enforce the same shape.
var variableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidVariableName reports whether name is usable as a ${NAME} variable.
func ValidVariableName(name string) bool {
	return variableNamePattern.MatchString(name)
}

// validateAgentVariables checks an agent's per-agent `variables:` block. Only
// static and env variables are allowed (script/http and the security policy
// are hive-level and seed-only), and the scope must be template or both — a
// per-agent variable only ever feeds that agent's kick prompt, never the
// config-load expansion, so scope "config" would silently do nothing.
func validateAgentVariables(agentName string, vars map[string]VarDef) error {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := vars[name]
		if !ValidVariableName(name) {
			return fmt.Errorf("agent %s: variables.%s: invalid name (use letters, digits, underscore; not starting with a digit)", agentName, name)
		}
		if !AgentVarTypeAllowed(def.Type) {
			return fmt.Errorf("agent %s: variables.%s: type %q is not allowed per agent (only static or env; script/http are hive-level variables.defs in the seed config)", agentName, name, def.Type)
		}
		switch def.Scope {
		case "", "template", "both":
		default:
			return fmt.Errorf("agent %s: variables.%s: scope %q is invalid for a per-agent variable (must be template or both)", agentName, name, def.Scope)
		}
	}
	return nil
}
