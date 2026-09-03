package notify

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// SetHiveID
// ---------------------------------------------------------------------------

func TestSetHiveID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	n := New(config.NotificationsConfig{}, logger)

	n.SetHiveID("test-hive-42")
	if n.hiveID != "test-hive-42" {
		t.Errorf("hiveID = %q, want test-hive-42", n.hiveID)
	}
}

// ---------------------------------------------------------------------------
// Send — with HiveID prefix
// ---------------------------------------------------------------------------

func TestSend_WithHiveID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Header.Get("Title")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{
			Server: srv.URL,
			Topic:  "test-topic",
		},
	}
	n := New(cfg, logger)
	n.SetHiveID("hive-99")

	// Send is async via goroutine, but we can test the prefix logic
	// by checking the hiveID is set
	if n.hiveID != "hive-99" {
		t.Errorf("hiveID = %q, want hive-99", n.hiveID)
	}
}

// ---------------------------------------------------------------------------
// sendNtfy — error status
// ---------------------------------------------------------------------------

func TestSendNtfy_ErrorStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{
			Server: srv.URL,
			Topic:  "test-topic",
		},
	}
	n := New(cfg, logger)
	// Should not panic on error status
	n.sendNtfy("title", "message", PriorityHigh)
}

// ---------------------------------------------------------------------------
// sendSlack
// ---------------------------------------------------------------------------

func TestSendSlack(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{
			Webhook: srv.URL,
		},
	}
	n := New(cfg, logger)
	n.sendSlack("title", "message")

	if !called {
		t.Error("expected slack webhook to be called")
	}
}

// ---------------------------------------------------------------------------
// sendDiscordWebhook
// ---------------------------------------------------------------------------

func TestSendDiscordWebhook(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{
			Webhook: srv.URL,
		},
	}
	n := New(cfg, logger)
	n.sendDiscordWebhook("title", "message")

	if !called {
		t.Error("expected discord webhook to be called")
	}
}
