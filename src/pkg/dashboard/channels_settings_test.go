package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	testSlackBotToken   = "xoxb-slack-bot-secret"
	testSlackAppToken   = "xapp-slack-app-secret"
	testMatrixToken     = "syt_matrix_access_secret"
	testTelegramToken   = "123456:telegram-bot-secret"
	testSMTPPassword    = "smtp-password-secret"
	testNtfyToken       = "tk_ntfy_secret"
	testPagerDutyKey    = "pd-routing-key-secret"
	testTeamsSecret     = "teams-client-secret-value"
	testTeamsWebhookURL = "https://prod.westus.logic.azure.com/workflows/teams-secret"
)

func getChannelsPayload(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	rec := doOwnerGet(s, channelsSettingsPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload, rec.Body.String()
}

func channelsSection(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()
	sec, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("payload[%q] = %#v, want object", key, payload[key])
	}
	return sec
}

func TestChannelsSettingsGitHubTriggersRoundTrip(t *testing.T) {
	s, _ := apiServer(t)
	converse := true
	s.deps.Config.Agents = map[string]config.AgentConfig{
		"scout": {Enabled: true, Converse: &converse, Channels: []config.ChannelConfig{{Type: config.ChannelTypeKick}, {Type: config.ChannelTypeMention}}},
		"quiet": {Enabled: true},
	}
	applied := 0
	s.deps.ChannelsConfigApplied = func() { applied++ }

	rec := doPut(s, channelsSettingsPath, map[string]any{
		"githubMentions": map[string]any{
			"enabled":        true,
			"pollInterval":   "90s",
			"defaultAgent":   "scout",
			"summoners":      []string{"@Alice", " bob ", "alice", ""},
			"minRole":        "merger",
			"perUserPerHour": 3,
			"ackReaction":    "rocket",
		},
		"githubActions": map[string]any{
			"enabled":               true,
			"allowedCommands":       []string{"Status", "kick", "status"},
			"trustedCommentAuthors": []string{"github-actions[bot]", "ci-bot"},
			"identityMap":           map[string]string{" ci-bot ": "alice", "": "x"},
			"oidcEnabled":           true,
			"oidcAudience":          "hive",
			"oidcMaxSkew":           "3m",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if applied != 1 {
		t.Fatalf("ChannelsConfigApplied called %d times, want 1", applied)
	}

	m := s.deps.Config.GitHub.Mentions
	if !m.Enabled || m.PollInterval != 90*time.Second || m.DefaultAgent != "scout" || m.MinRole != "merger" || m.PerUserPerHour != 3 {
		t.Fatalf("mentions = %+v", m)
	}
	if strings.Join(m.Summoners, ",") != "Alice,bob" {
		t.Fatalf("summoners = %v, want [Alice bob]", m.Summoners)
	}
	if m.AckReactionEffective() != "rocket" {
		t.Fatalf("ack reaction = %q", m.AckReactionEffective())
	}
	a := s.deps.Config.GitHub.Actions
	if !a.Enabled || strings.Join(a.AllowedCommands, ",") != "status,kick" || !a.OIDC.Enabled || a.OIDC.Audience != "hive" || a.OIDC.MaxSkew != 3*time.Minute {
		t.Fatalf("actions = %+v", a)
	}
	if a.IdentityMap["ci-bot"] != "alice" || len(a.IdentityMap) != 1 {
		t.Fatalf("identity map = %v", a.IdentityMap)
	}

	payload, _ := getChannelsPayload(t, s)
	gm := channelsSection(t, payload, "githubMentions")
	if gm["enabled"] != true || gm["pollInterval"] != "1m30s" || gm["defaultAgent"] != "scout" || gm["ackReaction"] != "rocket" {
		t.Fatalf("githubMentions payload = %#v", gm)
	}
	ga := channelsSection(t, payload, "githubActions")
	if ga["enabled"] != true || ga["oidcAudience"] != "hive" || ga["effectiveOidcMaxSkew"] != "3m0s" {
		t.Fatalf("githubActions payload = %#v", ga)
	}
	agents, ok := payload["mentionAgents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("mentionAgents = %#v", payload["mentionAgents"])
	}
	first := agents[0].(map[string]any)
	second := agents[1].(map[string]any)
	if first["name"] != "quiet" || first["eligible"] != false || second["name"] != "scout" || second["eligible"] != true {
		t.Fatalf("mentionAgents = %#v", agents)
	}
}

func TestChannelsSettingsRejectsInvalidGitHubTriggerValuesWithoutPartialApply(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Agents = map[string]config.AgentConfig{"scout": {Enabled: true}}
	s.deps.Config.GitHub.Mentions = config.GitHubMentionsConfig{Enabled: false, PollInterval: time.Minute}
	s.deps.ChannelsConfigApplied = func() { t.Fatal("ChannelsConfigApplied must not run on a rejected save") }

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown command", map[string]any{"githubActions": map[string]any{"allowedCommands": []string{"deploy"}}}, "unknown command"},
		{"bad duration", map[string]any{"githubMentions": map[string]any{"pollInterval": "soon"}}, "duration"},
		{"bad summoner", map[string]any{"githubMentions": map[string]any{"summoners": []string{"not a login!"}}}, "not a valid GitHub login"},
		{"unknown agent", map[string]any{"githubMentions": map[string]any{"defaultAgent": "ghost"}}, "no agent named"},
		{"bad role", map[string]any{"githubMentions": map[string]any{"minRole": "admin"}}, "invalid"},
		{"negative limit", map[string]any{"githubMentions": map[string]any{"perRepoPerHour": -1}}, "non-negative"},
		{"oidc without audience", map[string]any{"githubActions": map[string]any{"oidcEnabled": true}}, "audience"},
		{"webhook without secret env", map[string]any{"githubMentions": map[string]any{"enabled": true, "webhookEnabled": true}}, "webhook_secret_env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPut(s, channelsSettingsPath, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("error %q does not mention %q", rec.Body.String(), tc.want)
			}
		})
	}
	// A rejected save that also carried a valid field must not apply it.
	rec := doPut(s, channelsSettingsPath, map[string]any{
		"githubMentions": map[string]any{"enabled": true, "pollInterval": "nope"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if m := s.deps.Config.GitHub.Mentions; m.Enabled || m.PollInterval != time.Minute {
		t.Fatalf("rejected save partially applied: %+v", m)
	}
}

func TestChannelsSettingsChatSurfacesMaskSecretsAndKeepMaskedEcho(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Notifications = config.NotificationsConfig{}

	rec := doPut(s, channelsSettingsPath, map[string]any{
		"discord":  map[string]any{"botToken": testDiscordBotToken, "channelId": " 42 ", "allowedUsers": []string{"7", "7", "9"}},
		"slack":    map[string]any{"enabled": true, "appToken": testSlackAppToken, "botToken": testSlackBotToken, "channelId": "C123", "allowedUsers": []string{"U1:owner"}},
		"matrix":   map[string]any{"enabled": true, "homeserverUrl": "https://matrix.example.org", "accessToken": testMatrixToken, "roomId": "!room:example.org"},
		"telegram": map[string]any{"enabled": true, "botToken": testTelegramToken, "chatId": "-100123"},
		"msteams":  map[string]any{"tenantId": "t", "clientId": "c", "clientSecret": testTeamsSecret, "teamId": "team", "channelId": "chan", "webhookUrl": testTeamsWebhookURL},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	n := s.deps.Config.Notifications
	if n.Discord == nil || n.Discord.BotToken != testDiscordBotToken || n.Discord.ChannelID != "42" || strings.Join(n.Discord.AllowedUsers, ",") != "7,9" {
		t.Fatalf("discord = %+v", n.Discord)
	}
	if n.Slack == nil || !n.Slack.Enabled || n.Slack.AppToken != testSlackAppToken || n.Slack.BotToken != testSlackBotToken || n.Slack.ChannelID != "C123" {
		t.Fatalf("slack = %+v", n.Slack)
	}
	if n.Matrix == nil || !n.Matrix.Enabled || n.Matrix.AccessToken != testMatrixToken || n.Matrix.RoomID != "!room:example.org" {
		t.Fatalf("matrix = %+v", n.Matrix)
	}
	if n.Telegram == nil || !n.Telegram.Enabled || n.Telegram.BotToken != testTelegramToken || n.Telegram.ChatID != "-100123" {
		t.Fatalf("telegram = %+v", n.Telegram)
	}
	if n.MSTeams == nil || n.MSTeams.Enabled || n.MSTeams.ClientSecret != testTeamsSecret || n.MSTeams.WebhookURL != testTeamsWebhookURL {
		t.Fatalf("msteams = %+v", n.MSTeams)
	}

	payload, raw := getChannelsPayload(t, s)
	for _, secret := range []string{testDiscordBotToken, testSlackAppToken, testSlackBotToken, testMatrixToken, testTelegramToken, testTeamsSecret, testTeamsWebhookURL} {
		if strings.Contains(raw, secret) {
			t.Fatalf("channels payload echoes a secret %q: %s", secret, raw)
		}
	}
	if channelsSection(t, payload, "discord")["hasBotToken"] != true ||
		channelsSection(t, payload, "slack")["hasBotToken"] != true ||
		channelsSection(t, payload, "slack")["hasAppToken"] != true ||
		channelsSection(t, payload, "matrix")["hasAccessToken"] != true ||
		channelsSection(t, payload, "telegram")["hasBotToken"] != true ||
		channelsSection(t, payload, "msteams")["hasClientSecret"] != true ||
		channelsSection(t, payload, "msteams")["hasWebhookUrl"] != true {
		t.Fatalf("configured flags missing: %s", raw)
	}

	// Masked echo keeps every secret; explicit empty clears.
	rec = doPut(s, channelsSettingsPath, map[string]any{
		"discord":  map[string]any{"botToken": "••••••••"},
		"slack":    map[string]any{"appToken": "••••••••", "botToken": "••••••••"},
		"telegram": map[string]any{"botToken": "••••••••", "enabled": false},
		"matrix":   map[string]any{"accessToken": "", "enabled": false},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	n = s.deps.Config.Notifications
	if n.Discord.BotToken != testDiscordBotToken || n.Slack.AppToken != testSlackAppToken || n.Slack.BotToken != testSlackBotToken || n.Telegram.BotToken != testTelegramToken {
		t.Fatalf("masked echo changed a secret: discord=%q slack=%q/%q telegram=%q", n.Discord.BotToken, n.Slack.AppToken, n.Slack.BotToken, n.Telegram.BotToken)
	}
	if n.Matrix.AccessToken != "" || n.Matrix.Enabled || n.Telegram.Enabled {
		t.Fatalf("explicit clear/disable not applied: matrix=%+v telegram=%+v", n.Matrix, n.Telegram)
	}
}

func TestChannelsSettingsEnabledChatSurfaceRequiresCredentials(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Notifications = config.NotificationsConfig{}
	for name, body := range map[string]map[string]any{
		"slack":    {"slack": map[string]any{"enabled": true, "channelId": "C1"}},
		"telegram": {"telegram": map[string]any{"enabled": true}},
		"matrix":   {"matrix": map[string]any{"enabled": true, "homeserverUrl": "http://insecure.example"}},
		"msteams":  {"msteams": map[string]any{"enabled": true}},
	} {
		rec := doPut(s, channelsSettingsPath, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	if n := s.deps.Config.Notifications; n.Slack != nil || n.Telegram != nil || n.Matrix != nil || n.MSTeams != nil {
		t.Fatalf("rejected saves created chat sections: %+v", n)
	}
}

func TestChannelsSettingsEscalationSinksRoundTripAndValidate(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Escalation = config.EscalationConfig{}

	rec := doPut(s, channelsSettingsPath, map[string]any{
		"escalationEmail": map[string]any{
			"enabled": true, "smtpHost": "smtp.example.org", "smtpPort": 587, "smtpUsername": "hive",
			"smtpPassword": testSMTPPassword, "from": "hive@example.org", "to": []string{"ops@example.org", "ops@example.org"},
			"digestTo": []string{"lead@example.org"}, "digestAt": "09:00",
		},
		"escalationPush": map[string]any{
			"enabled": true, "minSeverity": "decision", "ntfyUrl": "https://ntfy.example.org/hive", "ntfyToken": testNtfyToken,
			"pagerdutyRoutingKey": testPagerDutyKey,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	e := s.deps.Config.Escalation
	if !e.Email.Enabled || e.Email.SMTP.Host != "smtp.example.org" || e.Email.SMTP.Port != 587 || e.Email.SMTP.Password != testSMTPPassword ||
		strings.Join(e.Email.To, ",") != "ops@example.org" || e.Email.Digest.At != "09:00" {
		t.Fatalf("email = %+v", e.Email)
	}
	if !e.Push.Enabled || e.Push.MinSeverity != "decision" || e.Push.Ntfy.Token != testNtfyToken || e.Push.PagerDuty.RoutingKey != testPagerDutyKey {
		t.Fatalf("push = %+v", e.Push)
	}

	payload, raw := getChannelsPayload(t, s)
	for _, secret := range []string{testSMTPPassword, testNtfyToken, testPagerDutyKey} {
		if strings.Contains(raw, secret) {
			t.Fatalf("channels payload echoes a secret %q", secret)
		}
	}
	email := channelsSection(t, payload, "escalationEmail")
	push := channelsSection(t, payload, "escalationPush")
	if email["hasSmtpPassword"] != true || email["smtpHost"] != "smtp.example.org" || push["hasNtfyToken"] != true || push["hasPagerdutyRoutingKey"] != true {
		t.Fatalf("escalation payload = %#v / %#v", email, push)
	}

	for name, body := range map[string]map[string]any{
		"digest time":   {"escalationEmail": map[string]any{"digestAt": "9am"}},
		"port":          {"escalationEmail": map[string]any{"smtpPort": 70000}},
		"severity":      {"escalationPush": map[string]any{"minSeverity": "loud"}},
		"pushover half": {"escalationPush": map[string]any{"pushoverAppToken": "app-only"}},
		"no provider":   {"escalationPush": map[string]any{"ntfyUrl": "", "pagerdutyRoutingKey": ""}},
	} {
		rec := doPut(s, channelsSettingsPath, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	if got := s.deps.Config.Escalation; got.Email.Digest.At != "09:00" || got.Push.MinSeverity != "decision" || got.Push.Ntfy.URL == "" {
		t.Fatalf("rejected saves mutated escalation: %+v", got)
	}
}

func TestChannelsSettingsReadOnlyStatusAndOwnerGate(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"alice", "bob:read"}
	t.Setenv(adminMCPWritesEnvName, "true")

	payload, _ := getChannelsPayload(t, s)
	mcp := channelsSection(t, payload, "adminMcp")
	if mcp["endpoint"] != adminMCPEndpointPath || mcp["writesEnabled"] != true || mcp["writesEnv"] != adminMCPWritesEnvName {
		t.Fatalf("adminMcp = %#v", mcp)
	}
	if chat := channelsSection(t, payload, "dashboardChat"); chat["authorizedUsers"] != float64(2) {
		t.Fatalf("dashboardChat = %#v", chat)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, channelsSettingsPath, strings.NewReader(`{"githubMentions":{"enabled":true}}`))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("PUT without owner role succeeded")
	}
	if s.deps.Config.GitHub.Mentions.Enabled {
		t.Fatal("unauthorized PUT mutated config")
	}
}
