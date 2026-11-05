package notify

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/logscrub"
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

func TestWebhookSendErrorOmitsRequestPath(t *testing.T) {
	const hookPath = "/services/T00000000/B00000000/abcdefghijklmnopqrstuvwx"
	cases := []struct {
		name    string
		handler func(*bytes.Buffer) slog.Handler
	}{
		{
			name: "plain",
			handler: func(buf *bytes.Buffer) slog.Handler {
				return slog.NewJSONHandler(buf, nil)
			},
		},
		{
			name: "wrapped",
			handler: func(buf *bytes.Buffer) slog.Handler {
				return logscrub.NewHandler(slog.NewJSONHandler(buf, nil))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			n := New(config.NotificationsConfig{
				Slack: &config.SlackConfig{Webhook: "http://127.0.0.1:1" + hookPath},
			}, slog.New(tc.handler(&buf)))

			n.sendSlack("Test", "message")

			logged := buf.String()
			if logged == "" {
				t.Fatal("expected a send error log line")
			}
			if strings.Contains(logged, hookPath) || strings.Contains(logged, "abcdefghijklmnopqrstuvwx") {
				t.Fatalf("send error log included request path: %s", logged)
			}
			if !strings.Contains(logged, `"webhook_host"`) {
				t.Fatalf("send error log missing redacted host field: %s", logged)
			}
		})
	}
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

func TestWebhookNonSuccessStatusLogsHostOnly(t *testing.T) {
	const hookPath = "/hooks/abcdefghijklmnop"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	var buf bytes.Buffer
	n := New(config.NotificationsConfig{
		Slack:   &config.SlackConfig{Webhook: server.URL + hookPath},
		Discord: &config.DiscordConfig{Webhook: server.URL + hookPath},
	}, slog.New(slog.NewJSONHandler(&buf, nil)))

	n.sendSlack("Test", "message")
	n.sendDiscordWebhook("Test", "message")

	logged := buf.String()
	for _, want := range []string{"slack send returned status", "discord webhook send returned status", `"status":403`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %q: %s", want, logged)
		}
	}
	if strings.Contains(logged, hookPath) {
		t.Fatalf("log included request path: %s", logged)
	}
}

func TestWebhookLogHelpers(t *testing.T) {
	attrs := webhookLogAttrs("https://hooks.example.test/x/y", errPlain("boom"))
	if len(attrs) != 4 || attrs[3] != "hooks.example.test" {
		t.Fatalf("webhookLogAttrs = %v", attrs)
	}
	if got := webhookHost("://bad"); got != "[redacted]" {
		t.Fatalf("webhookHost(invalid) = %q", got)
	}
	if got := webhookHost("/relative/only"); got != "[redacted]" {
		t.Fatalf("webhookHost(no host) = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("firstNonEmpty(empty) = %q", got)
	}
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
}

type errPlain string

func (e errPlain) Error() string { return string(e) }

func TestLogNtfyErrorDedupeWindowLevel(t *testing.T) {
	// Regression test for #16708: dedupe summary must be INFO level, not WARN.
	// If logged at WARN, the selfreport feed would count it as a repeated error.
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	n := New(config.NotificationsConfig{}, logger)

	// First call logs at WARN
	n.logNtfyError("ntfy returned error", "status=429")
	// Second call within window is suppressed silently
	n.logNtfyError("ntfy returned error", "status=429")
	// Third call with different message flushes the dedupe summary at INFO
	n.logNtfyError("ntfy send failed", "error detail")

	logged := buf.String()
	lines := strings.Split(strings.TrimSpace(logged), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 log lines, got %d: %s", len(lines), logged)
	}

	// Check that "ntfy errors suppressed" line is at INFO level
	var foundSuppressionSummary bool
	var suppressionAtWarnLevel bool
	for _, line := range lines {
		if strings.Contains(line, "ntfy errors suppressed") {
			foundSuppressionSummary = true
			// The level field should be "INFO", not "WARN"
			if strings.Contains(line, `"level":"WARN"`) {
				suppressionAtWarnLevel = true
			}
			if !strings.Contains(line, `"level":"INFO"`) {
				t.Logf("WARNING: suppression summary might not be INFO level: %s", line)
			}
		}
	}

	if !foundSuppressionSummary {
		t.Fatalf("did not find 'ntfy errors suppressed' in logs:\n%s", logged)
	}
	if suppressionAtWarnLevel {
		t.Fatalf("ntfy errors suppressed was logged at WARN level (regression #16708): %s", logged)
	}
}
