package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	githubWebhookSecretEnvVar = "GITHUB_WEBHOOK_SECRET"
	githubWebhookSecretPath   = "/data/saas/webhook-secret.key"
	githubWebhookMaxBodyBytes = 256 * 1024
)

func (s *Server) handleGitHubWebhookInvalidation(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		http.Error(w, `{"error":"missing X-GitHub-Event header"}`, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, githubWebhookMaxBodyBytes))
	if err != nil {
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}
	secret := loadDashboardGitHubWebhookSecret()
	if secret == "" {
		http.Error(w, `{"error":"webhooks disabled: no secret configured"}`, http.StatusServiceUnavailable)
		return
	}
	if !verifyDashboardGitHubWebhookSignature(body, r.Header.Get("X-Hub-Signature-256"), secret) {
		http.Error(w, `{"error":"invalid signature"}`, http.StatusUnauthorized)
		return
	}
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		jsonResponse(w, map[string]any{"status": "ignored", "reason": "no github client"})
		return
	}
	res, err := s.deps.GHClient.HandleWebhookInvalidation(r.Context(), event, body)
	if err != nil {
		http.Error(w, `{"error":"invalid webhook payload"}`, http.StatusBadRequest)
		return
	}
	status := "ok"
	if res.Ignored {
		status = "ignored"
	}
	jsonResponse(w, map[string]any{"status": status, "repo": res.Repo, "invalidations": res.Invalidations})
}

func loadDashboardGitHubWebhookSecret() string {
	if s := os.Getenv(githubWebhookSecretEnvVar); s != "" {
		return s
	}
	if data, err := os.ReadFile(githubWebhookSecretPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func verifyDashboardGitHubWebhookSignature(payload []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hmac.Equal(sig, mac.Sum(nil))
}
