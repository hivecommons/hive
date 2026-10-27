package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	testDiscordBotToken   = "bot-token-super-secret-value"
	testFactoryWebhook    = "https://discord.com/api/webhooks/111/factory-secret"
	testRequestsWebhook   = "https://discord.com/api/webhooks/222/requests-secret"
	notificationsSettings = "/api/config/governor/notifications"
)

func getNotificationsPayload(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	rec := doOwnerGet(s, "/api/governor/notifications")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload, rec.Body.String()
}

func TestNotificationsSettingsDiscordBotAndGitHubActivityRoundTrip(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Notifications = config.NotificationsConfig{}
	s.deps.Config.Project.Org = "acme"

	rec := doPut(s, notificationsSettings, map[string]any{
		"discordBotToken":           testDiscordBotToken,
		"discordChannelId":          " 1234567890 ",
		"discordRfcChannelId":       " 1540404487099777136 ",
		"discordAllowedUsers":       []string{" 42 ", "", "42", "77"},
		"discordFactoryWebhook":     testFactoryWebhook,
		"discordRequestsWebhookUrl": testRequestsWebhook,
		"githubActivity": map[string]any{
			"enabled":          true,
			"org":              "hivecommons",
			"apiUrl":           "https://ghe.example.com/api/v3",
			"pollIntervalS":    120,
			"repos":            []string{"hivecommons/hive", "hivecommons/hive"},
			"events":           []string{"issue_opened"},
			"allowAuthors":     []string{"alice"},
			"denyAuthors":      []string{"mallory"},
			"filterBots":       false,
			"filterDependabot": true,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	d := s.deps.Config.Notifications.Discord
	if d == nil {
		t.Fatal("discord config not created")
	}
	if d.BotToken != testDiscordBotToken || d.ChannelID != "1234567890" || d.RFCChannelID != "1540404487099777136" ||
		d.FactoryWebhook != testFactoryWebhook || d.RequestsWebhookURL != testRequestsWebhook {
		t.Fatalf("discord config = %+v", d)
	}
	if strings.Join(d.AllowedUsers, ",") != "42,77" {
		t.Fatalf("allowed users = %v, want [42 77]", d.AllowedUsers)
	}
	ga := s.deps.Config.Notifications.GitHubActivity
	if ga == nil || !ga.Enabled || ga.Org != "hivecommons" || ga.APIURL != "https://ghe.example.com/api/v3" || ga.PollIntervalS != 120 {
		t.Fatalf("github activity = %+v", ga)
	}
	if strings.Join(ga.Repos, ",") != "hivecommons/hive" || strings.Join(ga.Events, ",") != "issue_opened" ||
		strings.Join(ga.AllowAuthors, ",") != "alice" || strings.Join(ga.DenyAuthors, ",") != "mallory" {
		t.Fatalf("github activity lists = %+v", ga)
	}
	if ga.BotsFiltered() || !ga.DependabotFiltered() {
		t.Fatalf("github activity filters = bots:%v dependabot:%v", ga.BotsFiltered(), ga.DependabotFiltered())
	}

	payload, raw := getNotificationsPayload(t, s)
	for _, secret := range []string{testDiscordBotToken, testFactoryWebhook, testRequestsWebhook} {
		if strings.Contains(raw, secret) {
			t.Fatalf("notifications payload echoes a secret: %s", raw)
		}
	}
	for _, flag := range []string{"hasDiscordBotToken", "hasDiscordFactoryWebhook", "hasDiscordRequestsWebhook"} {
		if payload[flag] != true {
			t.Errorf("%s = %v, want true", flag, payload[flag])
		}
	}
	if payload["discordChannelId"] != "1234567890" {
		t.Errorf("discordChannelId = %v", payload["discordChannelId"])
	}
	if payload["discordRfcChannelId"] != "1540404487099777136" {
		t.Errorf("discordRfcChannelId = %v", payload["discordRfcChannelId"])
	}
	gaPayload, ok := payload["githubActivity"].(map[string]any)
	if !ok || gaPayload["enabled"] != true || gaPayload["org"] != "hivecommons" || gaPayload["filterBots"] != false {
		t.Fatalf("githubActivity payload = %#v", payload["githubActivity"])
	}
}

func TestNotificationsSettingsMaskedEchoKeepsAndEmptyClearsSecrets(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Notifications = config.NotificationsConfig{Discord: &config.DiscordConfig{
		BotToken:           testDiscordBotToken,
		FactoryWebhook:     testFactoryWebhook,
		RequestsWebhookURL: testRequestsWebhook,
	}}

	rec := doPut(s, notificationsSettings, map[string]any{
		"discordBotToken":       "••••••••",
		"discordFactoryWebhook": maskSecret(testFactoryWebhook),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	d := s.deps.Config.Notifications.Discord
	if d.BotToken != testDiscordBotToken || d.FactoryWebhook != testFactoryWebhook || d.RequestsWebhookURL != testRequestsWebhook {
		t.Fatalf("masked echo changed secrets: %+v", d)
	}

	rec = doPut(s, notificationsSettings, map[string]any{
		"discordBotToken":           "",
		"discordRequestsWebhookUrl": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if d := s.deps.Config.Notifications.Discord; d.BotToken != "" || d.RequestsWebhookURL != "" || d.FactoryWebhook != testFactoryWebhook {
		t.Fatalf("explicit clear = %+v, want token and requests webhook cleared only", d)
	}
}

func TestNotificationsSettingsRejectsInvalidValuesWithoutPartialApply(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"activity without factory webhook", map[string]any{
			"discordChannelId": "999",
			"githubActivity":   map[string]any{"enabled": true, "org": "acme"},
		}},
		{"http factory webhook", map[string]any{"discordFactoryWebhook": "http://discord.invalid/hook"}},
		{"private requests webhook", map[string]any{"discordRequestsWebhookUrl": "https://10.0.0.1/hook"}},
		{"negative poll interval", map[string]any{"githubActivity": map[string]any{"pollIntervalS": -1}}},
		{"private api url", map[string]any{"githubActivity": map[string]any{"apiUrl": "https://127.0.0.1/api"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := apiServer(t)
			s.deps.Config.Notifications = config.NotificationsConfig{}
			rec := doPut(s, notificationsSettings, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if s.deps.Config.Notifications.Discord != nil || s.deps.Config.Notifications.GitHubActivity != nil {
				t.Fatalf("rejected save mutated config: %+v", s.deps.Config.Notifications)
			}
		})
	}
}

func TestNotificationsSettingsActivityOnlyLeavesDiscordUnset(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Notifications = config.NotificationsConfig{}
	rec := doPut(s, notificationsSettings, map[string]any{
		"githubActivity": map[string]any{"org": "acme", "filterDependabot": false},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Notifications.Discord != nil {
		t.Fatalf("discord block created by an activity-only save: %+v", s.deps.Config.Notifications.Discord)
	}
	ga := s.deps.Config.Notifications.GitHubActivity
	if ga == nil || ga.Enabled || ga.Org != "acme" || ga.DependabotFiltered() || !ga.BotsFiltered() {
		t.Fatalf("github activity = %+v", ga)
	}
}
