package notify

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSendNtfyErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	n := New(config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: server.URL, Topic: "test"},
	}, slog.Default())
	n.sendNtfy("Error Test", "message", PriorityHigh)
}

func TestSendNtfySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Title") == "" {
			t.Error("should set Title header")
		}
		if r.Header.Get("Priority") != "high" {
			t.Errorf("priority = %q, want 'high'", r.Header.Get("Priority"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: server.URL, Topic: "test"},
	}, slog.Default())
	n.sendNtfy("Test Title", "test body", PriorityHigh)
}

func TestSendSlackSuccessMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: server.URL},
	}, slog.Default())
	n.sendSlack("Test", "message")
}

func TestSendSlackInvalidWebhook(t *testing.T) {
	n := New(config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: "not-a-url"},
	}, slog.Default())
	n.sendSlack("Test", "message")
}

func TestSendDiscordSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	n := New(config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: server.URL},
	}, slog.Default())
	n.sendDiscordWebhook("Test", "message")
}

func TestSendDiscordInvalidWebhook(t *testing.T) {
	n := New(config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: "not-a-url"},
	}, slog.Default())
	n.sendDiscordWebhook("Test", "message")
}

func TestLogNtfyErrorDedupeWindow(t *testing.T) {
	n := New(config.NotificationsConfig{}, slog.Default())

	// First call logs immediately
	n.logNtfyError("test error", "detail1")
	// Second call within window is suppressed
	n.logNtfyError("test error", "detail2")
	// Third call within window is suppressed
	n.logNtfyError("test error", "detail3")

	n.mu.Lock()
	count := n.ntfyErrCount
	n.mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 suppressed, got %d", count)
	}
}

func TestLogNtfyErrorDifferentMessage(t *testing.T) {
	n := New(config.NotificationsConfig{}, slog.Default())

	n.logNtfyError("error A", "detail1")
	n.logNtfyError("error B", "detail2")

	n.mu.Lock()
	lastErr := n.lastNtfyErr
	count := n.ntfyErrCount
	n.mu.Unlock()

	if lastErr != "error B" {
		t.Errorf("lastNtfyErr = %q, want 'error B'", lastErr)
	}
	if count != 0 {
		t.Errorf("different message should reset count, got %d", count)
	}
}

func TestSendSlackNetworkError(t *testing.T) {
	n := New(config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: "http://127.0.0.1:1/slack"},
	}, slog.Default())
	n.sendSlack("Test", "message")
}

func TestSendDiscordNetworkError(t *testing.T) {
	n := New(config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: "http://127.0.0.1:1/discord"},
	}, slog.Default())
	n.sendDiscordWebhook("Test", "message")
}

func TestSendNtfyNetworkError(t *testing.T) {
	n := New(config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: "http://127.0.0.1:1", Topic: "test"},
	}, slog.Default())
	n.sendNtfy("Test", "message", PriorityDefault)
}

func TestSendWithHiveID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: server.URL, Topic: "test"},
	}, slog.Default())
	n.SetHiveID("my-hive")
	n.Send("Alert", "body", PriorityDefault)
}
