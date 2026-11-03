package dashboard

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// notificationsExtraBody carries the Settings controls for the
// notifications.discord bot/hub keys and notifications.github_activity.*.
// Every field is optional: nil means "leave unchanged". Write-only secrets
// (bot token, factory and requests webhooks) also treat a masked "•…" echo
// as unchanged; an explicit empty string clears them.
type notificationsExtraBody struct {
	DiscordBotToken           *string                     `json:"discordBotToken"`
	DiscordChannelID          *string                     `json:"discordChannelId"`
	DiscordRFCChannelID       *string                     `json:"discordRfcChannelId"`
	DiscordAllowedUsers       []string                    `json:"discordAllowedUsers"`
	DiscordFactoryWebhook     *string                     `json:"discordFactoryWebhook"`
	DiscordRequestsWebhookURL *string                     `json:"discordRequestsWebhookUrl"`
	GitHubActivity            *githubActivitySettingsBody `json:"githubActivity"`
}

type githubActivitySettingsBody struct {
	Enabled          *bool    `json:"enabled"`
	Org              *string  `json:"org"`
	APIURL           *string  `json:"apiUrl"`
	PollIntervalS    *int     `json:"pollIntervalS"`
	Repos            []string `json:"repos"`
	Events           []string `json:"events"`
	AllowAuthors     []string `json:"allowAuthors"`
	DenyAuthors      []string `json:"denyAuthors"`
	FilterBots       *bool    `json:"filterBots"`
	FilterDependabot *bool    `json:"filterDependabot"`
}

func (b notificationsExtraBody) empty() bool {
	return b.DiscordBotToken == nil && b.DiscordChannelID == nil && b.DiscordRFCChannelID == nil && b.DiscordAllowedUsers == nil &&
		b.DiscordFactoryWebhook == nil && b.DiscordRequestsWebhookURL == nil && b.GitHubActivity == nil
}

func isMaskedSecretEcho(v string) bool { return strings.HasPrefix(v, "•") }

// applyWriteOnly updates dst from a write-only control value.
func applyWriteOnly(dst *string, v *string) {
	if v == nil || isMaskedSecretEcho(*v) {
		return
	}
	*dst = strings.TrimSpace(*v)
}

func cleanSettingsList(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// applyNotificationsExtra validates the extra notification settings against
// a copy of the current config and commits them only when the result is
// valid, so a rejected save never leaves a half-applied section behind.
func applyNotificationsExtra(cfg *config.Config, body notificationsExtraBody) error {
	if body.empty() {
		return nil
	}
	for _, f := range []struct {
		v    *string
		name string
	}{
		{body.DiscordFactoryWebhook, "discordFactoryWebhook"},
		{body.DiscordRequestsWebhookURL, "discordRequestsWebhookUrl"},
	} {
		if f.v != nil {
			if err := validateNotificationURL(strings.TrimSpace(*f.v), f.name); err != nil {
				return err
			}
		}
	}

	discord := config.DiscordConfig{}
	if cfg.Notifications.Discord != nil {
		discord = *cfg.Notifications.Discord
		discord.AllowedUsers = append([]string(nil), discord.AllowedUsers...)
	}
	applyWriteOnly(&discord.BotToken, body.DiscordBotToken)
	applyWriteOnly(&discord.FactoryWebhook, body.DiscordFactoryWebhook)
	applyWriteOnly(&discord.RequestsWebhookURL, body.DiscordRequestsWebhookURL)
	if body.DiscordChannelID != nil {
		discord.ChannelID = strings.TrimSpace(*body.DiscordChannelID)
	}
	if body.DiscordRFCChannelID != nil {
		discord.RFCChannelID = strings.TrimSpace(*body.DiscordRFCChannelID)
	}
	if body.DiscordAllowedUsers != nil {
		discord.AllowedUsers = cleanSettingsList(body.DiscordAllowedUsers)
	}

	var activity *config.GitHubActivityConfig
	if cfg.Notifications.GitHubActivity != nil {
		cp := *cfg.Notifications.GitHubActivity
		activity = &cp
	}
	if ga := body.GitHubActivity; ga != nil {
		if activity == nil {
			activity = &config.GitHubActivityConfig{}
		}
		if ga.Enabled != nil {
			activity.Enabled = *ga.Enabled
		}
		if ga.Org != nil {
			activity.Org = strings.TrimSpace(*ga.Org)
		}
		if ga.APIURL != nil {
			apiURL := strings.TrimSpace(*ga.APIURL)
			if err := validateNotificationURL(apiURL, "githubActivity.apiUrl"); err != nil {
				return err
			}
			activity.APIURL = apiURL
		}
		if ga.PollIntervalS != nil {
			if *ga.PollIntervalS < 0 {
				return fmt.Errorf("githubActivity.pollIntervalS must be non-negative")
			}
			activity.PollIntervalS = *ga.PollIntervalS
		}
		if ga.Repos != nil {
			activity.Repos = cleanSettingsList(ga.Repos)
		}
		if ga.Events != nil {
			activity.Events = cleanSettingsList(ga.Events)
		}
		if ga.AllowAuthors != nil {
			activity.AllowAuthors = cleanSettingsList(ga.AllowAuthors)
		}
		if ga.DenyAuthors != nil {
			activity.DenyAuthors = cleanSettingsList(ga.DenyAuthors)
		}
		if ga.FilterBots != nil {
			v := *ga.FilterBots
			activity.FilterBots = &v
		}
		if ga.FilterDependabot != nil {
			v := *ga.FilterDependabot
			activity.FilterDependabot = &v
		}
	}
	if activity != nil && activity.Enabled {
		if discord.FactoryWebhook == "" {
			return fmt.Errorf("the GitHub activity feed requires a Discord factory webhook")
		}
		if activity.EffectiveOrg(cfg.Project.Org) == "" {
			return fmt.Errorf("the GitHub activity feed requires an org (or project.org)")
		}
	}

	discordTouched := body.DiscordBotToken != nil || body.DiscordChannelID != nil || body.DiscordRFCChannelID != nil || body.DiscordAllowedUsers != nil ||
		body.DiscordFactoryWebhook != nil || body.DiscordRequestsWebhookURL != nil
	if cfg.Notifications.Discord != nil || discordTouched {
		cfg.Notifications.Discord = &discord
	}
	cfg.Notifications.GitHubActivity = activity
	return nil
}

// notificationsExtraPayload adds the extra notification settings to the
// Settings payload. Secrets are reported only as configured/not configured
// (factory webhook as a masked tail); their values are never echoed.
func notificationsExtraPayload(cfg *config.Config, out map[string]any) {
	out["discordChannelId"] = ""
	out["discordRfcChannelId"] = ""
	out["discordAllowedUsers"] = []string{}
	out["discordFactoryWebhook"] = ""
	out["hasDiscordBotToken"] = false
	out["hasDiscordFactoryWebhook"] = false
	out["hasDiscordRequestsWebhook"] = false
	if d := cfg.Notifications.Discord; d != nil {
		out["discordChannelId"] = d.ChannelID
		out["discordRfcChannelId"] = d.RFCChannelID
		out["discordAllowedUsers"] = append([]string{}, d.AllowedUsers...)
		out["discordFactoryWebhook"] = maskSecret(d.FactoryWebhook)
		out["hasDiscordBotToken"] = d.BotToken != ""
		out["hasDiscordFactoryWebhook"] = d.FactoryWebhook != ""
		out["hasDiscordRequestsWebhook"] = d.RequestsWebhookURL != ""
	}
	ga := config.GitHubActivityConfig{}
	if cfg.Notifications.GitHubActivity != nil {
		ga = *cfg.Notifications.GitHubActivity
	}
	out["githubActivity"] = map[string]any{
		"enabled":          ga.Enabled,
		"org":              ga.Org,
		"effectiveOrg":     ga.EffectiveOrg(cfg.Project.Org),
		"apiUrl":           ga.APIURL,
		"pollIntervalS":    ga.PollIntervalS,
		"repos":            append([]string{}, ga.Repos...),
		"events":           append([]string{}, ga.Events...),
		"allowAuthors":     append([]string{}, ga.AllowAuthors...),
		"denyAuthors":      append([]string{}, ga.DenyAuthors...),
		"filterBots":       ga.BotsFiltered(),
		"filterDependabot": ga.DependabotFiltered(),
	}
}
