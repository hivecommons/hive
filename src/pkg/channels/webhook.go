package channels

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	webhookMaxBodyBytes    = 10 * 1024 * 1024
	webhookSignatureHeader = "X-Hub-Signature-256"
	webhookEventHeader     = "X-GitHub-Event"
	webhookSecretEnvVar    = "HIVE_WEBHOOK_SECRET"
)

// WebhookReceiver handles GitHub webhook events and routes them to agents.
type WebhookReceiver struct {
	mgr *Manager
}

// NewWebhookReceiver creates a new webhook receiver.
func NewWebhookReceiver(mgr *Manager) *WebhookReceiver {
	return &WebhookReceiver{mgr: mgr}
}

func (w *WebhookReceiver) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes))
	if err != nil {
		http.Error(rw, "failed to read body", http.StatusBadRequest)
		return
	}

	secret := os.Getenv(webhookSecretEnvVar)
	if secret == "" {
		// HIVE_WEBHOOK_SECRET is required. Accepting unsigned webhooks would allow
		// any network-accessible client to trigger agent kicks. Fail closed.
		w.mgr.logger.Warn("channels: webhook rejected because HIVE_WEBHOOK_SECRET is not configured")
		http.Error(rw, "webhook secret not configured; set HIVE_WEBHOOK_SECRET to the GitHub webhook secret", http.StatusUnauthorized)
		return
	}

	sig := r.Header.Get(webhookSignatureHeader)
	if !verifyHMAC(body, sig, secret) {
		http.Error(rw, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get(webhookEventHeader)
	if eventType == "" {
		http.Error(rw, "missing X-GitHub-Event header", http.StatusBadRequest)
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(rw, "invalid JSON", http.StatusBadRequest)
		return
	}

	action, _ := payload["action"].(string)
	fullEvent := eventType
	if action != "" {
		fullEvent = eventType + "." + action
	}

	repoName := ""
	if repo, ok := payload["repository"].(map[string]interface{}); ok {
		repoName, _ = repo["name"].(string)
	}

	agents := w.mgr.getAgents()
	triggered := 0
	for agentName, agentCfg := range agents {
		for _, ch := range agentCfg.ChannelsOfType("webhook") {
			if !ch.IsEnabled() {
				continue
			}
			if !matchesEvent(ch.Events, eventType, fullEvent) {
				continue
			}
			if len(ch.Repos) > 0 && !containsStr(ch.Repos, repoName) {
				continue
			}
			ctx := TriggerContext{
				ChannelType: "webhook",
				Event:       fullEvent,
				Payload: map[string]string{
					"event":  eventType,
					"action": action,
					"repo":   repoName,
				},
			}
			go w.mgr.triggerKick(agentName, ctx)
			triggered++
		}
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(rw, `{"ok":true,"event":%q,"triggered":%d}`, fullEvent, triggered) // best-effort; client may already be gone
}

func matchesEvent(events []string, eventType, fullEvent string) bool {
	for _, e := range events {
		if e == eventType || e == fullEvent {
			return true
		}
	}
	return false
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func verifyHMAC(body []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sigBytes, err := hex.DecodeString(signature[len("sha256="):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(sigBytes, expected)
}
