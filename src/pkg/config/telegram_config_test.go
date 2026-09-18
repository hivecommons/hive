package config

import (
	"strings"
	"testing"
)

func TestTelegramConfigValidation(t *testing.T) {
	tests := []struct {
		name     string
		telegram *TelegramConfig
		want     string
	}{
		{name: "nil ok"},
		{name: "disabled empty ok", telegram: &TelegramConfig{}},
		{name: "enabled bot token required", telegram: &TelegramConfig{Enabled: true, ChatID: "42"}, want: "bot_token"},
		{name: "enabled chat id required", telegram: &TelegramConfig{Enabled: true, BotToken: "123:abc"}, want: "chat_id"},
		{name: "enabled complete ok", telegram: &TelegramConfig{Enabled: true, BotToken: "123:abc", ChatID: "42", AllowedUsers: []string{"7"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validSlackConfig()
			cfg.Notifications.Telegram = tt.telegram
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

func TestTelegramTokenEnvExpansion(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN_TEST", "123:expanded")
	got := expandEnvVars("bot_token: ${TELEGRAM_BOT_TOKEN_TEST}")
	if !strings.Contains(got, "123:expanded") {
		t.Fatalf("expanded telegram token = %q", got)
	}
}
