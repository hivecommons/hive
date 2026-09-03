package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	httpTimeoutSeconds      = 10
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
// class changes or the window expires.
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
		n.logger.Warn("ntfy errors suppressed",
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
		n.logger.Warn("slack send failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
}

func (n *Notifier) sendDiscordWebhook(title, message string) {
	if !strings.HasPrefix(n.cfg.Discord.Webhook, "http://") && !strings.HasPrefix(n.cfg.Discord.Webhook, "https://") {
		return
	}
	payload := map[string]string{
		"content": fmt.Sprintf("**%s**\n%s", title, message),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		n.logger.Warn("discord payload marshal failed", "error", err)
		return
	}
	resp, err := n.client.Post(n.cfg.Discord.Webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("discord webhook send failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
}
