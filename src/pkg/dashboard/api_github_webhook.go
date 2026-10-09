package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

const (
	// githubWebhookPath is the PUBLIC path GitHub (or the hub relay) posts App
	// webhooks to. Not actually open: the handler fails closed without
	// GITHUB_WEBHOOK_SECRET and verifies X-Hub-Signature-256 over the raw body.
	githubWebhookPath = "/api/webhook/github"

	githubWebhookSecretEnvVar = "GITHUB_WEBHOOK_SECRET"
	githubWebhookMaxBodyBytes = 1 << 20
)

func (s *Server) registerGitHubWebhookRoutes() {
	s.mux.HandleFunc("POST "+githubWebhookPath, s.handleGitHubWebhook)
	s.registerReviewEventDispatchRoutes()
}

// handleGitHubWebhook applies GitHub App webhook deliveries to the PR caches
// (hivecommons/hive#11177): PR, review, check, status, PR-comment and push
// events invalidate the matching cached PR state and mark it dirty for the
// next eval cycle, and every delivery for a managed repo refreshes webhook
// health. Deliveries for repositories this hive does not manage are ignored.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		jsonError(w, "missing X-GitHub-Event header", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, githubWebhookMaxBodyBytes))
	if err != nil {
		jsonError(w, "failed to read body", http.StatusBadRequest)
		return
	}
	secret := strings.TrimSpace(os.Getenv(githubWebhookSecretEnvVar))
	if secret == "" {
		s.webhookLogger().Warn("github webhook rejected: no webhook secret configured (set GITHUB_WEBHOOK_SECRET to enable signed webhooks)")
		jsonError(w, "webhooks disabled: no secret configured", http.StatusServiceUnavailable)
		return
	}
	if !verifyGitHubWebhookSignature(body, r.Header.Get("X-Hub-Signature-256"), secret) {
		s.webhookLogger().Warn("github webhook signature verification failed", "event", event)
		jsonError(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonResponse(w, map[string]any{"status": "ignored", "reason": "github client not configured"})
		return
	}
	res, err := s.deps.GHClient.HandleWebhookEvent(event, body)
	if err != nil {
		jsonError(w, "invalid webhook payload", http.StatusBadRequest)
		return
	}
	if !res.KnownRepo {
		jsonResponse(w, map[string]any{"status": "ignored", "reason": "unknown repository", "event": event})
		return
	}
	resp := map[string]any{"status": "ok", "event": event, "repo": res.Repo, "invalidated": res.Invalidated}
	if outcome := s.enqueueReviewEvent(event, body); outcome != "" {
		resp["review_dispatch"] = outcome
	}
	jsonResponse(w, resp)
}

func (s *Server) webhookLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// verifyGitHubWebhookSignature is the same HMAC-SHA256 check the hub applies
// to GitHub App deliveries (pkg/hub verifyWebhookSignature).
func verifyGitHubWebhookSignature(payload []byte, signature, secret string) bool {
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
