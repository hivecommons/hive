package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
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
// next eval cycle, queue a targeted re-read of the named PR or issue for the
// activity monitor (webhook_tracker_refresh.go), and every delivery for a
// managed repo refreshes webhook health. A `pull_request` delivery records
// the issues the PR claims (`Fixes #N`, branch name, references) into the
// duplicate-PR claim ledger on the event (hivecommons/hive#14814), so the
// contribute lane stops offering them without waiting for the reconcile
// pass; opened/synchronize/reopened also hands the PR to the sentinel
// (#13379), and the
// organization-scoped `projects_v2_item` delivery patches the GitHub
// Projects work source cache. Deliveries for repositories this hive does not
// manage are ignored.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	// Receipt time anchors webhook.event_to_patch_ms (hivecommons/hive#13380):
	// how long a delivery takes to become a cache patch.
	receivedAt := time.Now()
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
		const msg = "github webhook rejected: no webhook secret configured (set GITHUB_WEBHOOK_SECRET to enable signed webhooks)"
		warned := false
		s.webhookNoSecretOnce.Do(func() {
			warned = true
			s.webhookLogger().Warn(msg)
		})
		if !warned {
			s.webhookLogger().Debug(msg)
		}
		jsonError(w, "webhooks disabled: no secret configured", http.StatusServiceUnavailable)
		return
	}
	if !verifyGitHubWebhookSignature(body, r.Header.Get("X-Hub-Signature-256"), secret) {
		s.webhookLogger().Warn("github webhook signature verification failed", "event", event)
		github.RecordWebhookDeliveryFailure()
		jsonError(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if event == githubWebhookEventProjectsV2Item {
		// Organization-scoped: no repository to resolve, so it never reaches
		// the PR-cache handler. The work source applies it as a patch.
		if s.deps == nil || s.deps.ProjectsV2ItemEvent == nil {
			jsonResponse(w, map[string]any{"status": "ignored", "reason": "no projects v2 work source", "event": event})
			return
		}
		outcome := s.deps.ProjectsV2ItemEvent(body)
		jsonResponse(w, map[string]any{"status": "ok", "event": event, "projects_patch": outcome})
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonResponse(w, map[string]any{"status": "ignored", "reason": "github client not configured"})
		return
	}
	res, err := s.deps.GHClient.HandleWebhookEvent(event, body)
	if err != nil {
		github.RecordWebhookDeliveryFailure()
		jsonError(w, "invalid webhook payload", http.StatusBadRequest)
		return
	}
	if !res.KnownRepo {
		jsonResponse(w, map[string]any{"status": "ignored", "reason": "unknown repository", "event": event})
		return
	}
	if err := s.observeIntegrationWebhook(event, r.Header.Get("X-GitHub-Delivery"), body, receivedAt); err != nil {
		s.webhookLogger().Error("integration observation not durable", "event", event, "error", err)
	}
	// HandleWebhookEvent is the legacy writer: it invalidated the PR cache
	// and dirtied issues for the next cycle. The forgestate writer records
	// its own sample (WriterForgeState) when it applies the item patch.
	forgestate.RecordEventToPatch(r.Context(), res.Repo, forgestate.WriterCacheInvalidate, receivedAt, time.Now())
	resp := map[string]any{"status": "ok", "event": event, "repo": res.Repo, "invalidated": res.Invalidated}
	if len(res.ClaimedIssues) > 0 {
		resp["claimed_issues"] = res.ClaimedIssues
	}
	if n := s.scheduleWebhookTrackerRefresh(res, r.Header.Get("X-GitHub-Delivery")); n > 0 {
		resp["tracker_refresh"] = n
	}
	if outcome := s.enqueueReviewEvent(event, body); outcome != "" {
		resp["review_dispatch"] = outcome
	}
	if moved := s.noteForgeOutcome(event, body); moved {
		resp["outcome_counted"] = true
	}
	if reason, ok := webhookWakeReason(event, res); ok && s.deps.WebhookWake != nil {
		s.deps.WebhookWake(reason)
		resp["wake"] = reason
	}
	if err := s.integrationFailureIdentity(event, body, res.CIFailure); err != nil {
		s.webhookLogger().Warn("integration failure identity unavailable", "error", err)
	}
	if reactions := s.reactToCIFailure(res); len(reactions) > 0 {
		resp["ci_failure"] = reactions
	}
	if n, ok := sentinelPREventTarget(event, body); ok && s.deps.SentinelPREvent != nil {
		s.deps.SentinelPREvent(res.ConfiguredRepo, n)
		resp["sentinel"] = n
	}
	if s.deps.WebhookDelivery != nil {
		s.deps.WebhookDelivery(event, r.Header.Get("X-GitHub-Delivery"), body, res)
	}
	jsonResponse(w, resp)
}

// githubWebhookEventProjectsV2Item is the organization-scoped Projects v2
// item delivery (created/edited/deleted/reordered/archived/restored/
// converted). It is handled ahead of the repository-keyed events because its
// payload names a project, not a repository.
const githubWebhookEventProjectsV2Item = "projects_v2_item"

// sentinelPREventTarget returns the PR number of a `pull_request` delivery
// whose action means the head is new (opened, synchronize, reopened), the
// only deliveries the sentinel evaluates per event.
func sentinelPREventTarget(event string, body []byte) (int, bool) {
	var p struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest *struct {
			Number int `json:"number"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil || !github.SentinelPREventWanted(event, p.Action) {
		return 0, false
	}
	n := p.Number
	if n <= 0 && p.PullRequest != nil {
		n = p.PullRequest.Number
	}
	return n, n > 0
}

// noteForgeOutcome feeds a verified `pull_request closed` (merged) or
// `issues closed` delivery to the forgestate outcome counters so the
// dashboard's merged / closed totals move on the event instead of on a
// per-cycle search (hivecommons/hive#13376). It reports whether a counter
// moved; a body the counters cannot decode is logged and otherwise ignored
// since HandleWebhookEvent already accepted it.
func (s *Server) noteForgeOutcome(event string, body []byte) bool {
	if s == nil || s.deps == nil || s.deps.MetricsCollector == nil {
		return false
	}
	fc := s.deps.MetricsCollector.ForgeCounts()
	if fc == nil {
		return false
	}
	moved, err := fc.NoteWebhook(event, body)
	if err != nil {
		s.webhookLogger().Warn("forgestate outcome counter skipped delivery", "event", event, "error", err)
		return false
	}
	return moved
}

// webhookWakeReason reports whether a verified delivery changed cached state
// the next eval cycle should see now (an invalidated PR, a dirty or reopened
// issue) and names it for the wake log line and /api/status.
func webhookWakeReason(event string, res github.WebhookEventResult) (string, bool) {
	if !res.KnownRepo {
		return "", false
	}
	var items []string
	for _, n := range res.Invalidated {
		items = append(items, "#"+strconv.Itoa(n))
	}
	for _, n := range res.DirtyIssues {
		items = append(items, "issue #"+strconv.Itoa(n))
	}
	if res.ReopenedIssue > 0 {
		items = append(items, "reopened issue #"+strconv.Itoa(res.ReopenedIssue))
	}
	if len(items) == 0 {
		return "", false
	}
	const maxNamed = 3
	named := items
	if len(named) > maxNamed {
		named = append(append([]string{}, items[:maxNamed]...), "+"+strconv.Itoa(len(items)-maxNamed))
	}
	return event + " " + res.Repo + " " + strings.Join(named, ","), true
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
