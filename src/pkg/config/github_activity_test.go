package config

import (
	"strings"
	"testing"
)

func TestGitHubActivityValidation(t *testing.T) {
	base := Config{
		Project: ProjectConfig{Org: "hivecommons"},
		Agents:  map[string]AgentConfig{"worker": {Backend: "claude"}},
		GitHub:  GitHubConfig{Token: "token"},
		Notifications: NotificationsConfig{
			Discord:        &DiscordConfig{FactoryWebhook: "https://discord.com/api/webhooks/one/two"},
			GitHubActivity: &GitHubActivityConfig{Enabled: true},
		},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid github activity config rejected: %v", err)
	}

	badURL := base
	badURL.Notifications.Discord = &DiscordConfig{FactoryWebhook: "ftp://discord.example/hook"}
	if err := badURL.Validate(); err == nil || !strings.Contains(err.Error(), "notifications.discord.factory_webhook") {
		t.Fatalf("bad factory webhook error = %v, want factory_webhook validation", err)
	}

	missingWebhook := base
	missingWebhook.Notifications.Discord = nil
	if err := missingWebhook.Validate(); err == nil || !strings.Contains(err.Error(), "factory_webhook") {
		t.Fatalf("missing factory webhook error = %v, want requirement", err)
	}
}
