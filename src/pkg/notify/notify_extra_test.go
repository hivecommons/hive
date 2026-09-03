package notify

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestLogNtfyErrorDedupe(t *testing.T) {
	n := &Notifier{
		logger: slog.Default(),
		client: &http.Client{},
	}

	n.logNtfyError("first error", "details1")
	n.logNtfyError("first error", "details1")
	n.logNtfyError("first error", "details1")

	if n.ntfyErrCount != 2 {
		t.Errorf("suppressed count = %d, want 2", n.ntfyErrCount)
	}

	n.logNtfyError("different error", "details2")
	if n.ntfyErrCount != 0 {
		t.Errorf("count after flush = %d, want 0", n.ntfyErrCount)
	}
}

func TestSendSlackSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{
		cfg:    config.NotificationsConfig{Slack: &config.SlackConfig{Webhook: srv.URL}},
		logger: slog.Default(),
		client: srv.Client(),
	}
	n.sendSlack("test title", "test message")
}

func TestSendDiscordWebhookSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := &Notifier{
		cfg:    config.NotificationsConfig{Discord: &config.DiscordConfig{Webhook: srv.URL}},
		logger: slog.Default(),
		client: srv.Client(),
	}
	n.sendDiscordWebhook("test title", "test message")
}

func TestSendAllChannelsWithHiveID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(config.NotificationsConfig{
		Ntfy:    &config.NtfyConfig{Server: srv.URL, Topic: "topic"},
		Slack:   &config.SlackConfig{Webhook: srv.URL},
		Discord: &config.DiscordConfig{Webhook: srv.URL},
	}, slog.Default())
	n.SetHiveID("my-hive")
	n.Send("title", "body", PriorityDefault)
}

func TestSendSlackInvalidScheme(t *testing.T) {
	n := &Notifier{
		cfg:    config.NotificationsConfig{Slack: &config.SlackConfig{Webhook: "ftp://invalid"}},
		logger: slog.Default(),
		client: &http.Client{},
	}
	n.sendSlack("test", "test")
}

func TestSendDiscordInvalidScheme(t *testing.T) {
	n := &Notifier{
		cfg:    config.NotificationsConfig{Discord: &config.DiscordConfig{Webhook: "ftp://invalid"}},
		logger: slog.Default(),
		client: &http.Client{},
	}
	n.sendDiscordWebhook("test", "test")
}
