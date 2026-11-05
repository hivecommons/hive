package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	httpTimeoutSeconds       = 10
	errorDedupeWindowSeconds = 60
)

type Notifier struct {
	cfg    config.NotificationsConfig
	hiveID string
	client *http.Client
	logger *slog.Logger

	mu              sync.Mutex
	lastNtfyErr     string
	lastNtfyErrTime time.Time
	ntfyErrCount    int
}

func New(cfg config.NotificationsConfig, logger *slog.Logger) *Notifier {
	return &Notifier{
		cfg: cfg,
		client: &http.Client{
			Timeout: httpTimeoutSeconds * time.Second,
		},
		logger: logger,
	}
}

// SetHiveID configures the Hive instance ID to prefix notification titles.
func (n *Notifier) SetHiveID(id string) {
	n.hiveID = id
}

type Priority string

const (
	PriorityHigh    Priority = "high"
	PriorityDefault Priority = "default"
	PriorityLow     Priority = "low"
)

func (n *Notifier) Send(title, message string, priority Priority) {
	if n.hiveID != "" {
		title = fmt.Sprintf("[%s] %s", n.hiveID, title)
	}
	if n.cfg.Ntfy != nil {
		go n.sendNtfy(title, message, priority)
	}
	if n.cfg.Slack != nil {
		go n.sendSlack(title, message)
	}
	if n.cfg.Discord != nil && n.cfg.Discord.Webhook != "" {
		go n.sendDiscordWebhook(title, message)
	}
}

func (n *Notifier) sendNtfy(title, message string, priority Priority) {
	url := fmt.Sprintf("%s/%s", n.cfg.Ntfy.Server, n.cfg.Ntfy.Topic)

	req, err := http.NewRequest("POST", url, bytes.NewBufferString(message))
	if err != nil {
		n.logNtfyError("ntfy request creation failed", err.Error())
		return
	}

	safeTitle := strings.NewReplacer("\r", "", "\n", " ").Replace(title)
	req.Header.Set("Title", safeTitle)
	req.Header.Set("Priority", string(priority))

	resp, err := n.client.Do(req)
	if err != nil {
		n.logNtfyError("ntfy send failed", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		n.logNtfyError("ntfy returned error", fmt.Sprintf("status=%d", resp.StatusCode))
		return
	}

	n.logger.Debug("ntfy sent", "title", title)
}

// logNtfyError deduplicates repeated ntfy errors within a window.
// Logs the first occurrence immediately, then a summary when the error
// class changes or the window expires. The summary is logged at INFO level
// (not WARN) because the first occurrence already warned; a WARN per window
// would itself surface as an unclassified_error_repeated selfreport.
func (n *Notifier) logNtfyError(msg, errDetail string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now()
	windowExpired := now.Sub(n.lastNtfyErrTime) > errorDedupeWindowSeconds*time.Second

	if n.lastNtfyErr == msg && !windowExpired {
		n.ntfyErrCount++
		return
	}

	// Flush previous suppressed count
	if n.ntfyErrCount > 0 {
		n.logger.Info("ntfy errors suppressed",
			"message", n.lastNtfyErr,
			"suppressed_count", n.ntfyErrCount,
			"window_seconds", errorDedupeWindowSeconds,
		)
	}

	n.logger.Warn(msg, "error", errDetail)
	n.lastNtfyErr = msg
	n.lastNtfyErrTime = now
	n.ntfyErrCount = 0
}

func (n *Notifier) sendSlack(title, message string) {
	if !strings.HasPrefix(n.cfg.Slack.Webhook, "http://") && !strings.HasPrefix(n.cfg.Slack.Webhook, "https://") {
		return
	}
	payload := map[string]string{
		"text": fmt.Sprintf("*%s*\n%s", title, message),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		n.logger.Warn("slack payload marshal failed", "error", err)
		return
	}
	resp, err := n.client.Post(n.cfg.Slack.Webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("slack send failed", webhookLogAttrs(n.cfg.Slack.Webhook, err)...)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		n.logger.Warn("slack send returned status", "status", resp.StatusCode, "webhook_host", webhookHost(n.cfg.Slack.Webhook))
	}
}

// DiscordSuppressEmbeds is the Discord message flag (1<<2, SUPPRESS_EMBEDS)
// that stops link previews from unfurling under every notification that
// carries a URL. Alerts are one-liners; the preview card only adds noise.
const DiscordSuppressEmbeds = 1 << 2

func (n *Notifier) sendDiscordWebhook(title, message string) {
	if !strings.HasPrefix(n.cfg.Discord.Webhook, "http://") && !strings.HasPrefix(n.cfg.Discord.Webhook, "https://") {
		return
	}
	payload := map[string]any{
		"content":          fmt.Sprintf("**%s**\n%s", title, message),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"flags":            DiscordSuppressEmbeds,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		n.logger.Warn("discord payload marshal failed", "error", err)
		return
	}
	resp, err := n.client.Post(n.cfg.Discord.Webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("discord webhook send failed", webhookLogAttrs(n.cfg.Discord.Webhook, err)...)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		n.logger.Warn("discord webhook send returned status", "status", resp.StatusCode, "webhook_host", webhookHost(n.cfg.Discord.Webhook))
	}
}

func webhookLogAttrs(raw string, err error) []any {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return []any{
			"op", urlErr.Op,
			"error", urlErr.Err,
			"webhook_host", webhookHost(firstNonEmpty(urlErr.URL, raw)),
		}
	}
	return []any{"error", err, "webhook_host", webhookHost(raw)}
}

func webhookHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "[redacted]"
	}
	return parsed.Host
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
