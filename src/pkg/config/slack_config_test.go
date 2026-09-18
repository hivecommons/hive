package config

import (
	"strings"
	"testing"
)

func validSlackConfig() *Config {
	return &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "ghp_test"},
		Agents:  map[string]AgentConfig{"worker": {Backend: "claude", Enabled: true}},
	}
}

func TestSlackConfigValidation(t *testing.T) {
	tests := []struct {
		name  string
		slack *SlackConfig
		want  string
	}{
		{name: "disabled empty ok", slack: &SlackConfig{}},
		{name: "enabled app token required", slack: &SlackConfig{Enabled: true, BotToken: "xoxb", ChannelID: "C1"}, want: "app_token"},
		{name: "enabled bot token required", slack: &SlackConfig{Enabled: true, AppToken: "xapp", ChannelID: "C1"}, want: "bot_token"},
		{name: "enabled channel required", slack: &SlackConfig{Enabled: true, AppToken: "xapp", BotToken: "xoxb"}, want: "channel_id"},
		{name: "enabled complete ok", slack: &SlackConfig{Enabled: true, AppToken: "xapp", BotToken: "xoxb", ChannelID: "C1", AllowedUsers: []string{"U1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validSlackConfig()
			cfg.Notifications.Slack = tt.slack
			err := cfg.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSlackTokenEnvExpansion(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN_TEST", "xapp-expanded")
	t.Setenv("SLACK_BOT_TOKEN_TEST", "xoxb-expanded")
	got := expandEnvVars("app_token: ${SLACK_APP_TOKEN_TEST}\nbot_token: ${SLACK_BOT_TOKEN_TEST}")
	if !strings.Contains(got, "xapp-expanded") || !strings.Contains(got, "xoxb-expanded") {
		t.Fatalf("expanded slack tokens = %q", got)
	}
}
