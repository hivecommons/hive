package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	webhookSecretEnvVar = "GITHUB_WEBHOOK_SECRET"
	webhookSecretPath   = "/data/saas/webhook-secret.key"
	webhookMaxBodyBytes = 256 * 1024
	webhookPushRetries  = 3
)

// webhookRetryDelay is the backoff between spoke config-push retries, and
// webhookPushTimeout bounds each push attempt. Both are vars (not consts) so
// tests can shorten them — a test that forces a connection-level failure would
// otherwise wait the full 30s timeout per retry (minutes total). Production
// keeps the defaults.
var (
	webhookRetryDelay  = 5 * time.Second
	webhookPushTimeout = 30 * time.Second
)

type installationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		AppID int64 `json:"app_id"`
	} `json:"installation"`
	Repositories []struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	} `json:"repositories"`
	RepositoriesAdded []struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	} `json:"repositories_added"`
}

func (s *HubServer) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		http.Error(w, `{"error":"missing X-GitHub-Event header"}`, http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes))
	if err != nil {
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}

	if secret := loadWebhookSecret(); secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifyWebhookSignature(body, sig, secret) {
			s.logger.Warn("webhook signature verification failed")
			http.Error(w, `{"error":"invalid signature"}`, http.StatusUnauthorized)
			return
		}
	}

	switch event {
	case "installation", "installation_repositories":
		s.handleInstallationEvent(body)
	default:
		s.logger.Debug("ignoring webhook event", "event", event)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *HubServer) handleInstallationEvent(body []byte) {
	var evt installationEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		s.logger.Warn("failed to parse installation event", "error", err)
		return
	}

	if evt.Action != "created" && evt.Action != "added" {
		s.logger.Debug("ignoring installation event", "action", evt.Action)
		return
	}

	org := evt.Installation.Account.Login
	installationID := evt.Installation.ID
	appID := evt.Installation.AppID

	repos := make([]string, 0, len(evt.Repositories)+len(evt.RepositoriesAdded))
	for _, r := range evt.Repositories {
		repos = append(repos, r.Name)
	}
	for _, r := range evt.RepositoriesAdded {
		repos = append(repos, r.Name)
	}

	s.logger.Info("github app installation detected",
		"action", evt.Action,
		"org", org,
		"installation_id", installationID,
		"app_id", appID,
		"repos", repos,
	)

	// 1. Check for a spoke that recently signaled it's waiting for a GitHub App install.
	hive := s.findHiveByPendingInstall(org)

	// 2. Fall back to repo-name matching for hives that were already registered.
	if hive == nil {
		hive = s.findHiveByOrgRepos(org, repos)
	}

	// 3. No match yet — store the webhook for deferred heartbeat matching.
	if hive == nil {
		s.storePendingWebhook(org, appID, installationID)
		s.logger.Info("no matching hive found, storing webhook for deferred heartbeat matching",
			"org", org,
			"installation_id", installationID,
		)
		return
	}

	privateKey := s.loadAppPrivateKey(hive)

	s.logger.Info("storing github app config for heartbeat delivery",
		"hive_id", hive.ID,
		"installation_id", installationID,
		"match_method", func() string {
			if hive.PendingGitHubAppInstall {
				return "pending-install-heartbeat"
			}
			return "org-repo-match"
		}(),
	)

	s.storePendingGitHubAppConfig(hive.ID, &HeartbeatGitHubAppConfig{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     privateKey,
	})
}

func (s *HubServer) findHiveByOrgRepos(org string, repos []string) *RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	repoSet := make(map[string]bool, len(repos))
	for _, r := range repos {
		repoSet[strings.ToLower(r)] = true
	}

	var orgMatch *RegistryEntry
	for i := range s.registry.Hives {
		h := &s.registry.Hives[i]
		if !strings.EqualFold(h.Org, org) {
			continue
		}
		// Exact repo match takes priority
		for _, hiveRepo := range h.Repos {
			if repoSet[strings.ToLower(hiveRepo)] {
				return h
			}
		}
		if repoSet[strings.ToLower(h.PrimaryRepo)] {
			return h
		}
		// Track org-only match as fallback
		if orgMatch == nil {
			orgMatch = h
		}
	}
	// Fall back to org-only match when repos list is empty
	if len(repos) == 0 && orgMatch != nil {
		return orgMatch
	}
	return nil
}

func (s *HubServer) pushGitHubConfigToSpoke(hive *RegistryEntry, appID, installationID int64) {
	privateKey := s.loadAppPrivateKey(hive)
	if privateKey == "" {
		s.logger.Warn("could not load app private key for spoke push", "hive_id", hive.ID)
	}

	payload := map[string]interface{}{
		"app_id":          appID,
		"installation_id": installationID,
	}
	if privateKey != "" {
		payload["private_key"] = privateKey
	}

	body, err := json.Marshal(payload)
	if err != nil {
		s.logger.Error("failed to marshal config payload", "error", err)
		return
	}

	spokeURL := strings.TrimRight(hive.DashboardURL, "/") + "/api/config/github"
	authToken := s.loadSpokeAuthToken(hive)

	var lastErr error
	for attempt := 1; attempt <= webhookPushRetries; attempt++ {
		if attempt > 1 {
			s.logger.Info("retrying spoke config push", "hive_id", hive.ID, "attempt", attempt)
			time.Sleep(webhookRetryDelay)
		}

		ctx, cancel := context.WithTimeout(context.Background(), webhookPushTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, spokeURL, bytes.NewReader(body))
		if err != nil {
			cancel()
			s.logger.Error("failed to create spoke request", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			s.logger.Warn("spoke config push failed", "hive_id", hive.ID, "attempt", attempt, "error", err)
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()

		if resp.StatusCode >= http.StatusBadRequest {
			s.logger.Error("spoke rejected config push",
				"hive_id", hive.ID,
				"status", resp.StatusCode,
				"body", string(respBody),
			)
			return
		}

		s.logger.Info("github app config pushed to spoke",
			"hive_id", hive.ID,
			"status", resp.StatusCode,
			"response", string(respBody),
		)
		return
	}

	s.logger.Error("spoke config push failed after retries",
		"hive_id", hive.ID,
		"attempts", webhookPushRetries,
		"last_error", lastErr,
	)
}

const sharedKeyNamespace = "hive-shared"
const sharedKeySecretName = "hive-app-key"

func (s *HubServer) loadAppPrivateKey(hive *RegistryEntry) string {
	clusterID := hive.ClusterID
	if clusterID == "" {
		clusterID = "hive-oke"
	}

	cluster, ok := s.clusters[clusterID]
	if !ok {
		s.logger.Warn("cluster not found for key lookup", "cluster_id", clusterID)
		return ""
	}

	// First: check the hive's own secret (already configured hives).
	ns := "hive-hosted-" + hive.ID
	out, err := kubectlForCluster(&cluster, "get", "secret", "hive-secrets", "-n", ns,
		"-o", "jsonpath={.data.gh-app-key\\.pem}").Output()
	if err == nil && len(out) > 0 {
		if decoded, decErr := base64Decode(string(out)); decErr == nil && strings.HasPrefix(strings.TrimSpace(decoded), "-----BEGIN") {
			return decoded
		}
	}

	// Fallback: read from the shared namespace (cluster-scoped, independent of any hive).
	out, err = kubectlForCluster(&cluster, "get", "secret", sharedKeySecretName, "-n", sharedKeyNamespace,
		"-o", "jsonpath={.data.gh-app-key\\.pem}").Output()
	if err != nil || len(out) == 0 {
		s.logger.Warn("app key not found in shared namespace", "cluster_id", clusterID, "namespace", sharedKeyNamespace)
		return ""
	}

	decoded, err := base64Decode(string(out))
	if err != nil {
		s.logger.Warn("failed to decode app key from shared secret", "error", err)
		return ""
	}
	return decoded
}

func (s *HubServer) loadSpokeAuthToken(hive *RegistryEntry) string {
	ns := "hive-hosted-" + hive.ID
	clusterID := hive.ClusterID
	if clusterID == "" {
		clusterID = "hive-oke"
	}

	cluster, ok := s.clusters[clusterID]
	if !ok {
		return ""
	}

	out, err := kubectlForCluster(&cluster, "get", "secret", "hive-secrets", "-n", ns,
		"-o", "jsonpath={.data.dashboard-token}").Output()
	if err != nil || len(out) == 0 {
		return ""
	}

	decoded, err := base64Decode(string(out))
	if err != nil {
		return ""
	}
	return decoded
}

func loadWebhookSecret() string {
	if s := os.Getenv(webhookSecretEnvVar); s != "" {
		return s
	}
	if data, err := os.ReadFile(webhookSecretPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func verifyWebhookSignature(payload []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}

const pendingWebhookExpiry = 5 * time.Minute

type pendingWebhookEntry struct {
	Org            string
	AppID          int64
	InstallationID int64
	ReceivedAt     time.Time
}

func (s *HubServer) storePendingWebhook(org string, appID, installationID int64) {
	s.pendingWebhooksMu.Lock()
	defer s.pendingWebhooksMu.Unlock()
	s.pendingWebhooks[strings.ToLower(org)] = &pendingWebhookEntry{
		Org:            org,
		AppID:          appID,
		InstallationID: installationID,
		ReceivedAt:     time.Now(),
	}
}

func (s *HubServer) findHiveByPendingInstall(org string) *RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.registry.Hives {
		h := &s.registry.Hives[i]
		if !h.PendingGitHubAppInstall {
			continue
		}
		if strings.EqualFold(h.Org, org) {
			return h
		}
	}
	return nil
}

func (s *HubServer) checkPendingWebhookMatch(org, hiveID string) {
	s.pendingWebhooksMu.Lock()
	key := strings.ToLower(org)
	pw, ok := s.pendingWebhooks[key]
	if !ok || time.Since(pw.ReceivedAt) > pendingWebhookExpiry {
		s.pendingWebhooksMu.Unlock()
		return
	}
	delete(s.pendingWebhooks, key)
	s.pendingWebhooksMu.Unlock()

	s.mu.RLock()
	var hive *RegistryEntry
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			hive = &s.registry.Hives[i]
			break
		}
	}
	s.mu.RUnlock()

	if hive == nil {
		s.logger.Warn("pending webhook matched but hive not found",
			"hive_id", hiveID, "org", org)
		return
	}

	privateKey := s.loadAppPrivateKey(hive)

	s.logger.Info("pending webhook matched via heartbeat, storing for heartbeat delivery",
		"hive_id", hiveID,
		"org", org,
		"installation_id", pw.InstallationID,
	)

	s.storePendingGitHubAppConfig(hiveID, &HeartbeatGitHubAppConfig{
		AppID:          pw.AppID,
		InstallationID: pw.InstallationID,
		PrivateKey:     privateKey,
	})
}
