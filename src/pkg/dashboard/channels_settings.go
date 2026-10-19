package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Settings → Channels (hivecommons/hive#12984): one tab for every inbound
// trigger surface that used to be yaml-only — GitHub @-mentions and the
// Actions trigger, the chat bots (Discord, Slack, Matrix, Telegram, Teams),
// the escalation email/push sinks, plus read-only status for dashboard chat
// and the admin MCP endpoint, which have no hive.yaml knobs of their own.
//
// Contract, shared with the other settings writers: every field is a pointer
// or nil-able slice, so an absent key leaves that setting untouched. Secrets
// are write-only — GET reports only has*/configured flags, and a PUT whose
// value is the masked "•…" echo (or absent) keeps the stored secret; an
// explicit empty string clears it. The whole body is validated against a copy
// of the live config and committed only when the result is valid, so a 400
// never leaves a half-applied section behind.

const (
	channelsSettingsPath   = "/api/config/channels"
	channelsMaxPollMinutes = 24 * 60
)

// githubActionKnownCommands is the closed command vocabulary the Actions
// trigger accepts; it mirrors GitHubActionsConfig.Validate.
var githubActionKnownCommands = []string{"status", "review", "kick"}

type channelsSettingsBody struct {
	GitHubMentions  *channelsMentionsBody `json:"githubMentions"`
	GitHubActions   *channelsActionsBody  `json:"githubActions"`
	Discord         *channelsDiscordBody  `json:"discord"`
	Slack           *channelsSlackBody    `json:"slack"`
	Matrix          *channelsMatrixBody   `json:"matrix"`
	Telegram        *channelsTelegramBody `json:"telegram"`
	MSTeams         *channelsTeamsBody    `json:"msteams"`
	EscalationEmail *channelsEmailBody    `json:"escalationEmail"`
	EscalationPush  *channelsPushBody     `json:"escalationPush"`
}

type channelsMentionsBody struct {
	Enabled          *bool    `json:"enabled"`
	PollInterval     *string  `json:"pollInterval"`
	DefaultAgent     *string  `json:"defaultAgent"`
	Summoners        []string `json:"summoners"`
	MinRole          *string  `json:"minRole"`
	PerUserPerHour   *int     `json:"perUserPerHour"`
	PerRepoPerHour   *int     `json:"perRepoPerHour"`
	PerThreadMax     *int     `json:"perThreadMax"`
	AckReaction      *string  `json:"ackReaction"`
	WebhookEnabled   *bool    `json:"webhookEnabled"`
	WebhookSecretEnv *string  `json:"webhookSecretEnv"`
	WebhookMinGap    *string  `json:"webhookMinGap"`
}

type channelsActionsBody struct {
	Enabled               *bool             `json:"enabled"`
	SourceLabel           *string           `json:"sourceLabel"`
	AllowedCommands       []string          `json:"allowedCommands"`
	AllowApply            *bool             `json:"allowApply"`
	TrustedCommentAuthors []string          `json:"trustedCommentAuthors"`
	IdentityMap           map[string]string `json:"identityMap"`
	OIDCEnabled           *bool             `json:"oidcEnabled"`
	OIDCAudience          *string           `json:"oidcAudience"`
	OIDCJWKSURL           *string           `json:"oidcJwksUrl"`
	OIDCMaxSkew           *string           `json:"oidcMaxSkew"`
}

type channelsDiscordBody struct {
	BotToken     *string  `json:"botToken"`
	ChannelID    *string  `json:"channelId"`
	AllowedUsers []string `json:"allowedUsers"`
}

type channelsSlackBody struct {
	Enabled      *bool    `json:"enabled"`
	AppToken     *string  `json:"appToken"`
	BotToken     *string  `json:"botToken"`
	ChannelID    *string  `json:"channelId"`
	AllowedUsers []string `json:"allowedUsers"`
}

type channelsMatrixBody struct {
	Enabled       *bool    `json:"enabled"`
	HomeserverURL *string  `json:"homeserverUrl"`
	AccessToken   *string  `json:"accessToken"`
	RoomID        *string  `json:"roomId"`
	AllowedUsers  []string `json:"allowedUsers"`
}

type channelsTelegramBody struct {
	Enabled      *bool    `json:"enabled"`
	BotToken     *string  `json:"botToken"`
	ChatID       *string  `json:"chatId"`
	AllowedUsers []string `json:"allowedUsers"`
}

type channelsTeamsBody struct {
	Enabled      *bool    `json:"enabled"`
	TenantID     *string  `json:"tenantId"`
	ClientID     *string  `json:"clientId"`
	ClientSecret *string  `json:"clientSecret"`
	TeamID       *string  `json:"teamId"`
	ChannelID    *string  `json:"channelId"`
	WebhookURL   *string  `json:"webhookUrl"`
	AllowedUsers []string `json:"allowedUsers"`
}

type channelsEmailBody struct {
	Enabled      *bool    `json:"enabled"`
	SMTPHost     *string  `json:"smtpHost"`
	SMTPPort     *int     `json:"smtpPort"`
	SMTPUsername *string  `json:"smtpUsername"`
	SMTPPassword *string  `json:"smtpPassword"`
	From         *string  `json:"from"`
	To           []string `json:"to"`
	DigestTo     []string `json:"digestTo"`
	DigestAt     *string  `json:"digestAt"`
}

type channelsPushBody struct {
	Enabled             *bool   `json:"enabled"`
	MinSeverity         *string `json:"minSeverity"`
	NtfyURL             *string `json:"ntfyUrl"`
	NtfyToken           *string `json:"ntfyToken"`
	PushoverAppToken    *string `json:"pushoverAppToken"`
	PushoverUserKey     *string `json:"pushoverUserKey"`
	PagerDutyRoutingKey *string `json:"pagerdutyRoutingKey"`
}

func (s *Server) handleChannelsSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, channelsSettingsPayload(s.deps.Config))
}

func (s *Server) handleChannelsSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body channelsSettingsBody
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := applyChannelsSettings(s.deps.Config, body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after channels update", "error", err)
	}
	s.auditFromRequest(r, "config_channels", auditDetail("section", "channels"), "")
	if s.deps.ChannelsConfigApplied != nil {
		s.deps.ChannelsConfigApplied()
	}
	s.refreshAndPersist()
	jsonResponse(w, channelsSettingsPayload(s.deps.Config))
}

// applyChannelsSettings validates body against a copy of cfg and commits the
// copy's channel sections only when every validator passes.
func applyChannelsSettings(cfg *config.Config, body channelsSettingsBody) error {
	next := *cfg
	next.Notifications = cloneNotificationsForChannels(cfg.Notifications)

	if err := applyChannelsMentions(&next, body.GitHubMentions); err != nil {
		return err
	}
	if err := applyChannelsActions(&next, body.GitHubActions); err != nil {
		return err
	}
	if err := applyChannelsChatSurfaces(&next, body); err != nil {
		return err
	}
	if err := applyChannelsEscalation(&next, body.EscalationEmail, body.EscalationPush); err != nil {
		return err
	}

	cfg.GitHub.Mentions = next.GitHub.Mentions
	cfg.GitHub.Actions = next.GitHub.Actions
	cfg.Notifications = next.Notifications
	cfg.Escalation = next.Escalation
	return nil
}

// cloneNotificationsForChannels deep-copies the chat-surface pointers so a
// rejected save never mutates the live config through a shared pointer.
func cloneNotificationsForChannels(n config.NotificationsConfig) config.NotificationsConfig {
	out := n
	if n.Discord != nil {
		cp := *n.Discord
		cp.AllowedUsers = append([]string(nil), n.Discord.AllowedUsers...)
		out.Discord = &cp
	}
	if n.Slack != nil {
		cp := *n.Slack
		cp.AllowedUsers = append([]string(nil), n.Slack.AllowedUsers...)
		out.Slack = &cp
	}
	if n.Matrix != nil {
		cp := *n.Matrix
		cp.AllowedUsers = append([]string(nil), n.Matrix.AllowedUsers...)
		out.Matrix = &cp
	}
	if n.Telegram != nil {
		cp := *n.Telegram
		cp.AllowedUsers = append([]string(nil), n.Telegram.AllowedUsers...)
		out.Telegram = &cp
	}
	if n.MSTeams != nil {
		cp := *n.MSTeams
		cp.AllowedUsers = append([]string(nil), n.MSTeams.AllowedUsers...)
		out.MSTeams = &cp
	}
	return out
}

func applyTrimmed(dst *string, v *string) {
	if v != nil {
		*dst = strings.TrimSpace(*v)
	}
}

func applyBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

// parseChannelsDuration accepts a Go duration ("5m", "90s", "1h30m") or a
// bare number of seconds; empty restores the default (zero).
func parseChannelsDuration(field, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		var secs int
		if _, scanErr := fmt.Sscanf(raw, "%d", &secs); scanErr != nil || fmt.Sprint(secs) != raw {
			return 0, fmt.Errorf("%s must be a duration such as 5m or 90s", field)
		}
		d = time.Duration(secs) * time.Second
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must be non-negative", field)
	}
	if d > time.Duration(channelsMaxPollMinutes)*time.Minute {
		return 0, fmt.Errorf("%s must be at most 24h", field)
	}
	return d, nil
}

func cleanGitHubLogins(field string, in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		login := strings.TrimPrefix(strings.TrimSpace(raw), "@")
		if login == "" {
			continue
		}
		if !config.ValidGitHubLogin(login) {
			return nil, fmt.Errorf("%s: %q is not a valid GitHub login", field, login)
		}
		key := strings.ToLower(login)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, login)
	}
	return out, nil
}

func applyChannelsMentions(next *config.Config, b *channelsMentionsBody) error {
	if b == nil {
		return nil
	}
	m := &next.GitHub.Mentions
	applyBool(&m.Enabled, b.Enabled)
	if b.PollInterval != nil {
		d, err := parseChannelsDuration("githubMentions.pollInterval", *b.PollInterval)
		if err != nil {
			return err
		}
		m.PollInterval = d
	}
	if b.DefaultAgent != nil {
		agent := strings.TrimSpace(*b.DefaultAgent)
		if agent != "" {
			if _, ok := next.Agents[agent]; !ok {
				return fmt.Errorf("githubMentions.defaultAgent: no agent named %q", agent)
			}
		}
		m.DefaultAgent = agent
	}
	if b.Summoners != nil {
		summoners, err := cleanGitHubLogins("githubMentions.summoners", b.Summoners)
		if err != nil {
			return err
		}
		m.Summoners = summoners
	}
	if b.MinRole != nil {
		role := strings.TrimSpace(*b.MinRole)
		if role != "" && !config.ValidRole(role) {
			return fmt.Errorf("githubMentions.minRole %q is invalid (must be read, read-write, merger, or owner)", role)
		}
		m.MinRole = role
	}
	for _, f := range []struct {
		dst  *int
		v    *int
		name string
	}{
		{&m.PerUserPerHour, b.PerUserPerHour, "perUserPerHour"},
		{&m.PerRepoPerHour, b.PerRepoPerHour, "perRepoPerHour"},
		{&m.PerThreadMax, b.PerThreadMax, "perThreadMax"},
	} {
		if f.v == nil {
			continue
		}
		if *f.v < 0 {
			return fmt.Errorf("githubMentions.%s must be non-negative", f.name)
		}
		*f.dst = *f.v
	}
	if b.AckReaction != nil {
		// An explicit empty string is a real value (no reaction) and must
		// stay distinguishable from "unset = default eyes".
		v := strings.TrimSpace(*b.AckReaction)
		if v == config.DefaultMentionAckReaction {
			m.AckReaction = nil
		} else {
			m.AckReaction = &v
		}
	}
	applyBool(&m.WebhookEnabled, b.WebhookEnabled)
	applyTrimmed(&m.WebhookSecretEnv, b.WebhookSecretEnv)
	if b.WebhookMinGap != nil {
		d, err := parseChannelsDuration("githubMentions.webhookMinGap", *b.WebhookMinGap)
		if err != nil {
			return err
		}
		m.WebhookMinGap = d
	}
	return m.Validate()
}

func applyChannelsActions(next *config.Config, b *channelsActionsBody) error {
	if b == nil {
		return nil
	}
	a := &next.GitHub.Actions
	applyBool(&a.Enabled, b.Enabled)
	applyTrimmed(&a.SourceLabel, b.SourceLabel)
	if b.AllowedCommands != nil {
		known := map[string]bool{}
		for _, cmd := range githubActionKnownCommands {
			known[cmd] = true
		}
		cmds := []string{}
		seen := map[string]bool{}
		for _, raw := range b.AllowedCommands {
			cmd := strings.ToLower(strings.TrimSpace(raw))
			if cmd == "" || seen[cmd] {
				continue
			}
			if !known[cmd] {
				return fmt.Errorf("githubActions.allowedCommands: unknown command %q (known: %s)", cmd, strings.Join(githubActionKnownCommands, ", "))
			}
			seen[cmd] = true
			cmds = append(cmds, cmd)
		}
		a.AllowedCommands = cmds
	}
	applyBool(&a.AllowApply, b.AllowApply)
	if b.TrustedCommentAuthors != nil {
		a.TrustedCommentAuthors = cleanSettingsList(b.TrustedCommentAuthors)
	}
	if b.IdentityMap != nil {
		im := map[string]string{}
		for k, v := range b.IdentityMap {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k == "" || v == "" {
				continue
			}
			im[k] = v
		}
		a.IdentityMap = im
	}
	applyBool(&a.OIDC.Enabled, b.OIDCEnabled)
	applyTrimmed(&a.OIDC.Audience, b.OIDCAudience)
	if b.OIDCJWKSURL != nil {
		u := strings.TrimSpace(*b.OIDCJWKSURL)
		if u != "" {
			if err := validateNotificationURL(u, "githubActions.oidcJwksUrl"); err != nil {
				return err
			}
		}
		a.OIDC.JWKSURL = u
	}
	if b.OIDCMaxSkew != nil {
		d, err := parseChannelsDuration("githubActions.oidcMaxSkew", *b.OIDCMaxSkew)
		if err != nil {
			return err
		}
		a.OIDC.MaxSkew = d
	}
	return a.Validate()
}

func applyChannelsChatSurfaces(next *config.Config, body channelsSettingsBody) error {
	n := &next.Notifications
	if b := body.Discord; b != nil {
		if n.Discord == nil {
			n.Discord = &config.DiscordConfig{}
		}
		applyWriteOnly(&n.Discord.BotToken, b.BotToken)
		applyTrimmed(&n.Discord.ChannelID, b.ChannelID)
		if b.AllowedUsers != nil {
			n.Discord.AllowedUsers = cleanSettingsList(b.AllowedUsers)
		}
	}
	if b := body.Slack; b != nil {
		if n.Slack == nil {
			n.Slack = &config.SlackConfig{}
		}
		applyBool(&n.Slack.Enabled, b.Enabled)
		applyWriteOnly(&n.Slack.AppToken, b.AppToken)
		applyWriteOnly(&n.Slack.BotToken, b.BotToken)
		applyTrimmed(&n.Slack.ChannelID, b.ChannelID)
		if b.AllowedUsers != nil {
			n.Slack.AllowedUsers = cleanSettingsList(b.AllowedUsers)
		}
		if n.Slack.Enabled && (n.Slack.AppToken == "" || n.Slack.BotToken == "" || n.Slack.ChannelID == "") {
			return fmt.Errorf("slack: the app token, bot token, and channel id are required when the Slack bot is enabled")
		}
	}
	if b := body.Matrix; b != nil {
		if n.Matrix == nil {
			n.Matrix = &config.MatrixConfig{}
		}
		applyBool(&n.Matrix.Enabled, b.Enabled)
		if b.HomeserverURL != nil {
			u := strings.TrimSpace(*b.HomeserverURL)
			if u != "" {
				if err := validateNotificationURL(u, "matrix.homeserverUrl"); err != nil {
					return err
				}
			}
			n.Matrix.HomeserverURL = u
		}
		applyWriteOnly(&n.Matrix.AccessToken, b.AccessToken)
		applyTrimmed(&n.Matrix.RoomID, b.RoomID)
		if b.AllowedUsers != nil {
			n.Matrix.AllowedUsers = cleanSettingsList(b.AllowedUsers)
		}
		if n.Matrix.Enabled && (n.Matrix.HomeserverURL == "" || n.Matrix.AccessToken == "" || n.Matrix.RoomID == "") {
			return fmt.Errorf("matrix: the homeserver URL, access token, and room id are required when the Matrix bot is enabled")
		}
	}
	if b := body.Telegram; b != nil {
		if n.Telegram == nil {
			n.Telegram = &config.TelegramConfig{}
		}
		applyBool(&n.Telegram.Enabled, b.Enabled)
		applyWriteOnly(&n.Telegram.BotToken, b.BotToken)
		applyTrimmed(&n.Telegram.ChatID, b.ChatID)
		if b.AllowedUsers != nil {
			n.Telegram.AllowedUsers = cleanSettingsList(b.AllowedUsers)
		}
		if n.Telegram.Enabled && (n.Telegram.BotToken == "" || n.Telegram.ChatID == "") {
			return fmt.Errorf("telegram: the bot token and chat id are required when the Telegram bot is enabled")
		}
	}
	if b := body.MSTeams; b != nil {
		if n.MSTeams == nil {
			n.MSTeams = &config.MSTeamsConfig{}
		}
		applyBool(&n.MSTeams.Enabled, b.Enabled)
		applyTrimmed(&n.MSTeams.TenantID, b.TenantID)
		applyTrimmed(&n.MSTeams.ClientID, b.ClientID)
		applyWriteOnly(&n.MSTeams.ClientSecret, b.ClientSecret)
		applyTrimmed(&n.MSTeams.TeamID, b.TeamID)
		applyTrimmed(&n.MSTeams.ChannelID, b.ChannelID)
		if b.WebhookURL != nil && !isMaskedSecretEcho(*b.WebhookURL) {
			u := strings.TrimSpace(*b.WebhookURL)
			if u != "" {
				if err := validateNotificationURL(u, "msteams.webhookUrl"); err != nil {
					return err
				}
			}
			n.MSTeams.WebhookURL = u
		}
		if b.AllowedUsers != nil {
			n.MSTeams.AllowedUsers = cleanSettingsList(b.AllowedUsers)
		}
		if n.MSTeams.Enabled && (n.MSTeams.TenantID == "" || n.MSTeams.ClientID == "" || n.MSTeams.ClientSecret == "" || n.MSTeams.TeamID == "" || n.MSTeams.ChannelID == "") {
			return fmt.Errorf("msteams: the tenant, client id, client secret, team id, and channel id are required when the Teams bot is enabled")
		}
	}
	return nil
}

func applyChannelsEscalation(next *config.Config, email *channelsEmailBody, push *channelsPushBody) error {
	if email == nil && push == nil {
		return nil
	}
	e := &next.Escalation
	if b := email; b != nil {
		applyBool(&e.Email.Enabled, b.Enabled)
		applyTrimmed(&e.Email.SMTP.Host, b.SMTPHost)
		if b.SMTPPort != nil {
			if *b.SMTPPort < 0 || *b.SMTPPort > 65535 {
				return fmt.Errorf("escalationEmail.smtpPort must be a valid TCP port")
			}
			e.Email.SMTP.Port = *b.SMTPPort
		}
		applyTrimmed(&e.Email.SMTP.Username, b.SMTPUsername)
		applyWriteOnly(&e.Email.SMTP.Password, b.SMTPPassword)
		applyTrimmed(&e.Email.From, b.From)
		if b.To != nil {
			e.Email.To = cleanSettingsList(b.To)
		}
		if b.DigestTo != nil {
			e.Email.Digest.To = cleanSettingsList(b.DigestTo)
		}
		applyTrimmed(&e.Email.Digest.At, b.DigestAt)
	}
	if b := push; b != nil {
		applyBool(&e.Push.Enabled, b.Enabled)
		applyTrimmed(&e.Push.MinSeverity, b.MinSeverity)
		if b.NtfyURL != nil {
			u := strings.TrimSpace(*b.NtfyURL)
			if u != "" {
				if err := validateNotificationURL(u, "escalationPush.ntfyUrl"); err != nil {
					return err
				}
			}
			e.Push.Ntfy.URL = u
		}
		applyWriteOnly(&e.Push.Ntfy.Token, b.NtfyToken)
		applyWriteOnly(&e.Push.Pushover.AppToken, b.PushoverAppToken)
		applyWriteOnly(&e.Push.Pushover.UserKey, b.PushoverUserKey)
		applyWriteOnly(&e.Push.PagerDuty.RoutingKey, b.PagerDutyRoutingKey)
	}
	return e.ValidateSurfaces()
}

func durationOrEmpty(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func channelsSettingsPayload(cfg *config.Config) map[string]any {
	m := cfg.GitHub.Mentions
	a := cfg.GitHub.Actions
	out := map[string]any{
		"githubMentions": map[string]any{
			"enabled":                 m.Enabled,
			"appUsable":               cfg.GitHub.HasUsableApp(),
			"pollInterval":            durationOrEmpty(m.PollInterval),
			"effectivePollInterval":   m.PollIntervalEffective().String(),
			"defaultAgent":            m.DefaultAgent,
			"summoners":               append([]string{}, m.Summoners...),
			"minRole":                 m.MinRole,
			"effectiveMinRole":        m.MinRoleEffective(),
			"perUserPerHour":          m.PerUserPerHour,
			"perRepoPerHour":          m.PerRepoPerHour,
			"perThreadMax":            m.PerThreadMax,
			"effectivePerUserPerHour": m.PerUserPerHourEffective(),
			"effectivePerRepoPerHour": m.PerRepoPerHourEffective(),
			"effectivePerThreadMax":   m.PerThreadMaxEffective(),
			"ackReaction":             m.AckReactionEffective(),
			"webhookEnabled":          m.WebhookEnabled,
			"webhookSecretEnv":        m.WebhookSecretEnv,
			"hasWebhookSecret":        m.WebhookSecretEffective() != "",
			"webhookMinGap":           durationOrEmpty(m.WebhookMinGap),
			"effectiveWebhookMinGap":  m.WebhookMinGapEffective().String(),
		},
		"githubActions": map[string]any{
			"enabled":               a.Enabled,
			"sourceLabel":           a.SourceLabel,
			"effectiveSourceLabel":  a.SourceLabelEffective(),
			"allowedCommands":       a.AllowedCommandsEffective(),
			"knownCommands":         append([]string{}, githubActionKnownCommands...),
			"allowApply":            a.AllowApply,
			"trustedCommentAuthors": a.TrustedCommentAuthorsEffective(),
			"identityMap":           identityMapOrEmpty(a.IdentityMap),
			"oidcEnabled":           a.OIDC.Enabled,
			"oidcAudience":          a.OIDC.Audience,
			"oidcJwksUrl":           a.OIDC.JWKSURL,
			"effectiveOidcJwksUrl":  a.OIDC.JWKSURLEffective(),
			"oidcMaxSkew":           durationOrEmpty(a.OIDC.MaxSkew),
			"effectiveOidcMaxSkew":  a.OIDC.MaxSkewEffective().String(),
		},
		"dashboardChat": map[string]any{
			"endpoint":        "/api/chat",
			"authorizedUsers": len(cfg.Dashboard.AuthorizedUsers),
		},
		"adminMcp": map[string]any{
			"endpoint":      adminMCPEndpointPath,
			"writesEnabled": adminMCPWritesEnabled(),
			"writesEnv":     adminMCPWritesEnvName,
		},
		"mentionAgents": channelsMentionAgents(cfg),
	}

	discord := map[string]any{"hasBotToken": false, "channelId": "", "allowedUsers": []string{}}
	if d := cfg.Notifications.Discord; d != nil {
		discord["hasBotToken"] = d.BotToken != ""
		discord["channelId"] = d.ChannelID
		discord["allowedUsers"] = append([]string{}, d.AllowedUsers...)
	}
	out["discord"] = discord

	slack := map[string]any{"enabled": false, "hasAppToken": false, "hasBotToken": false, "channelId": "", "allowedUsers": []string{}}
	if sl := cfg.Notifications.Slack; sl != nil {
		slack["enabled"] = sl.Enabled
		slack["hasAppToken"] = sl.AppToken != ""
		slack["hasBotToken"] = sl.BotToken != ""
		slack["channelId"] = sl.ChannelID
		slack["allowedUsers"] = append([]string{}, sl.AllowedUsers...)
	}
	out["slack"] = slack

	matrix := map[string]any{"enabled": false, "homeserverUrl": "", "hasAccessToken": false, "roomId": "", "allowedUsers": []string{}}
	if mx := cfg.Notifications.Matrix; mx != nil {
		matrix["enabled"] = mx.Enabled
		matrix["homeserverUrl"] = mx.HomeserverURL
		matrix["hasAccessToken"] = mx.AccessToken != ""
		matrix["roomId"] = mx.RoomID
		matrix["allowedUsers"] = append([]string{}, mx.AllowedUsers...)
	}
	out["matrix"] = matrix

	telegram := map[string]any{"enabled": false, "hasBotToken": false, "chatId": "", "allowedUsers": []string{}}
	if tg := cfg.Notifications.Telegram; tg != nil {
		telegram["enabled"] = tg.Enabled
		telegram["hasBotToken"] = tg.BotToken != ""
		telegram["chatId"] = tg.ChatID
		telegram["allowedUsers"] = append([]string{}, tg.AllowedUsers...)
	}
	out["telegram"] = telegram

	teams := map[string]any{"enabled": false, "tenantId": "", "clientId": "", "hasClientSecret": false, "teamId": "", "channelId": "", "hasWebhookUrl": false, "allowedUsers": []string{}}
	if ms := cfg.Notifications.MSTeams; ms != nil {
		teams["enabled"] = ms.Enabled
		teams["tenantId"] = ms.TenantID
		teams["clientId"] = ms.ClientID
		teams["hasClientSecret"] = ms.ClientSecret != ""
		teams["teamId"] = ms.TeamID
		teams["channelId"] = ms.ChannelID
		teams["hasWebhookUrl"] = ms.WebhookURL != ""
		teams["allowedUsers"] = append([]string{}, ms.AllowedUsers...)
	}
	out["msteams"] = teams

	e := cfg.Escalation
	out["escalationEmail"] = map[string]any{
		"enabled":         e.Email.Enabled,
		"smtpHost":        e.Email.SMTP.Host,
		"smtpPort":        e.Email.SMTP.Port,
		"smtpUsername":    e.Email.SMTP.Username,
		"hasSmtpPassword": e.Email.SMTP.Password != "",
		"from":            e.Email.From,
		"to":              append([]string{}, e.Email.To...),
		"digestTo":        append([]string{}, e.Email.Digest.To...),
		"digestAt":        e.Email.Digest.At,
	}
	out["escalationPush"] = map[string]any{
		"enabled":                e.Push.Enabled,
		"minSeverity":            e.Push.MinSeverity,
		"ntfyUrl":                e.Push.Ntfy.URL,
		"hasNtfyToken":           e.Push.Ntfy.Token != "",
		"hasPushoverAppToken":    e.Push.Pushover.AppToken != "",
		"hasPushoverUserKey":     e.Push.Pushover.UserKey != "",
		"hasPagerdutyRoutingKey": e.Push.PagerDuty.RoutingKey != "",
	}
	return out
}

func identityMapOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// channelsMentionAgents lists every agent with its @-mention eligibility, so
// the tab can show which agents a mention can reach and deep-link to the
// agent dialog's Channels tab for the rest. The eligibility rule mirrors the
// mention handler's resolveAgent (enabled, converse, mention channel, and
// governor-kickable).
func channelsMentionAgents(cfg *config.Config) []map[string]any {
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		ac := cfg.Agents[name]
		converse := ac.Converse != nil && *ac.Converse
		mention := ac.HasEnabledChannel(config.ChannelTypeMention)
		kick := ac.UsesGovernorKick()
		out = append(out, map[string]any{
			"name":         name,
			"enabled":      ac.Enabled,
			"converse":     converse,
			"mention":      mention,
			"governorKick": kick,
			"eligible":     ac.Enabled && converse && mention && kick,
		})
	}
	return out
}
