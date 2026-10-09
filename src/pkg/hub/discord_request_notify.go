package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	discordRequestsWebhookEnv        = "HIVE_HUB_DISCORD_REQUESTS_WEBHOOK"
	discordRequestWebhookTimeout     = 10 * time.Second
	discordRequestWebhookMaxAttempts = 3
	discordRequestWebhookBackoffBase = 200 * time.Millisecond
	discordContentLimit              = 2000
	discordEmbedDescriptionLimit     = 4096
	discordEmbedFieldValueLimit      = 1024
	discordRequestNotesLimit         = 900
	discordRequestHTTPErrorBodyLimit = 512
)

var (
	discordRequestHTTPClient = &http.Client{Timeout: discordRequestWebhookTimeout}
	discordRequestBackoff    = func(attempt int) time.Duration {
		if attempt <= 1 {
			return 0
		}
		return time.Duration(attempt-1) * discordRequestWebhookBackoffBase
	}
)

type discordRequestNotificationState struct {
	mu                 sync.RWMutex
	SuccessCount       int64     `json:"success_count"`
	FailureCount       int64     `json:"failure_count"`
	LastSuccess        time.Time `json:"last_success,omitempty"`
	LastError          time.Time `json:"last_error,omitempty"`
	LastErrorMessage   string    `json:"last_error_message,omitempty"`
	LastRequestID      string    `json:"last_request_id,omitempty"`
	LastErrorRequestID string    `json:"last_error_request_id,omitempty"`
}

type discordWebhookPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	URL         string              `json:"url,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

func (s *HubServer) notifyDiscordNewProvisionRequest(pr *ProvisionRequest) {
	if s == nil || pr == nil {
		return
	}
	webhookURL := strings.TrimSpace(s.discordRequestsWebhookURL())
	if webhookURL == "" {
		return
	}
	payload, err := buildDiscordRequestPayload(pr)
	if err != nil {
		s.hubLogger().Warn("discord request notification skipped: payload build failed",
			"request_id", pr.Username, "error", err)
		return
	}
	requestID := provisionRequestID(pr)
	go s.deliverDiscordRequestNotification(webhookURL, requestID, payload)
}

func (s *HubServer) discordRequestsWebhookURL() string {
	if v := strings.TrimSpace(os.Getenv(discordRequestsWebhookEnv)); v != "" {
		return v
	}
	if s == nil || strings.TrimSpace(s.configPath) == "" {
		return ""
	}
	cfg, err := config.LoadWithDashboardOverlayForHub(s.configPath)
	if err != nil {
		s.hubLogger().Warn("discord request notification config load failed", "error", err)
		return ""
	}
	if v := strings.TrimSpace(cfg.Hub.Notifications.Discord.RequestsWebhookURL); v != "" {
		return v
	}
	if cfg.Notifications.Discord != nil {
		return strings.TrimSpace(cfg.Notifications.Discord.RequestsWebhookURL)
	}
	return ""
}

func (s *HubServer) deliverDiscordRequestNotification(webhookURL, requestID string, payload discordWebhookPayload) {
	var lastErr error
	for attempt := 1; attempt <= discordRequestWebhookMaxAttempts; attempt++ {
		if delay := discordRequestBackoff(attempt); delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}
		ctx, cancel := context.WithTimeout(context.Background(), discordRequestWebhookTimeout)
		err := postDiscordRequestWebhook(ctx, webhookURL, payload)
		cancel()
		if err == nil {
			s.recordDiscordRequestNotificationSuccess(requestID)
			s.hubLogger().Info("discord request notification delivered", "request_id", requestID)
			return
		}
		lastErr = err
	}
	s.recordDiscordRequestNotificationFailure(requestID, lastErr)
	s.hubLogger().Warn("discord request notification failed",
		"request_id", requestID, "attempts", discordRequestWebhookMaxAttempts, "error", lastErr)
}

func postDiscordRequestWebhook(ctx context.Context, webhookURL string, payload discordWebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := discordRequestHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, discordRequestHTTPErrorBodyLimit))
		return fmt.Errorf("discord request webhook returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func buildDiscordRequestPayload(pr *ProvisionRequest) (discordWebhookPayload, error) {
	if pr == nil {
		return discordWebhookPayload{}, fmt.Errorf("nil provision request")
	}
	requester := discordRequesterLabel(pr)
	target := discordRequestTarget(pr)
	requestURL := provisionRequestAdminURL(pr)
	fields := []discordEmbedField{
		{Name: "Requester", Value: truncateDiscordField(requester), Inline: true},
		{Name: "Target", Value: truncateDiscordField(target), Inline: true},
		{Name: "Requested level", Value: fmt.Sprintf("L%d", pr.ACMMLevel), Inline: true},
		{Name: "Request ID", Value: truncateDiscordField(provisionRequestID(pr)), Inline: true},
		{Name: "Status", Value: truncateDiscordField(pr.Status), Inline: true},
	}
	if notes := discordRequestNotes(pr); notes != "" {
		fields = append(fields, discordEmbedField{Name: "Notes", Value: truncateDiscordField(notes)})
	}
	if requestURL != "" {
		fields = append(fields, discordEmbedField{Name: "Review", Value: truncateDiscordField(requestURL)})
	}
	content := truncateDiscordContent("🧺 New hosted-hive request from " + discordPlainRequester(pr))
	embed := discordEmbed{
		Title:       "New hive request",
		Description: truncateDiscordRunes("A new hosted-hive request is ready for admin review.", discordEmbedDescriptionLimit),
		URL:         requestURL,
		Timestamp:   pr.RequestedAt,
		Fields:      fields,
	}
	return discordWebhookPayload{Content: content, Embeds: []discordEmbed{embed}}, nil
}

func discordRequesterLabel(pr *ProvisionRequest) string {
	login := discordPlainRequester(pr)
	if link := githubProfileURL(pr); link != "" {
		return fmt.Sprintf("[%s](%s)", login, link)
	}
	return login
}

func discordPlainRequester(pr *ProvisionRequest) string {
	for _, v := range []string{pr.UserID, strings.TrimPrefix(pr.Username, "github:"), pr.Username} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return "unknown"
}

func githubProfileURL(pr *ProvisionRequest) string {
	host := strings.TrimSpace(pr.GitHubHost)
	if host == "" {
		host = "github.com"
	}
	if !strings.EqualFold(host, "github.com") && !strings.HasPrefix(strings.ToLower(host), "github.") {
		return ""
	}
	login := strings.TrimSpace(pr.UserID)
	if login == "" && strings.HasPrefix(pr.Username, "github:") {
		login = strings.TrimPrefix(pr.Username, "github:")
	}
	if login == "" || strings.ContainsAny(login, " /:@") {
		return ""
	}
	return "https://" + host + "/" + login
}

func discordRequestTarget(pr *ProvisionRequest) string {
	org := strings.TrimSpace(pr.Org)
	repo := strings.TrimSpace(pr.PrimaryRepo)
	if repo == "" {
		repo = firstCSVValue(pr.Repos)
	}
	if org != "" && repo != "" {
		return org + "/" + repo
	}
	if org != "" {
		return org
	}
	if repo != "" {
		return repo
	}
	return "hosted hive"
}

func discordRequestNotes(pr *ProvisionRequest) string {
	parts := []string{}
	if name := strings.TrimSpace(pr.FullName); name != "" {
		parts = append(parts, "Name: "+name)
	}
	if slack := strings.TrimSpace(pr.SlackID); slack != "" {
		parts = append(parts, "Slack: "+slack)
	}
	if country := strings.TrimSpace(pr.Country); country != "" {
		parts = append(parts, "Country: "+country)
	}
	if auth := strings.TrimSpace(pr.AuthMethod); auth != "" {
		parts = append(parts, "Auth: "+auth)
	}
	if host := strings.TrimSpace(pr.GitHubHost); host != "" {
		parts = append(parts, "Forge: "+host)
	}
	return truncateDiscordRunes(strings.Join(parts, "\n"), discordRequestNotesLimit)
}

func provisionRequestID(pr *ProvisionRequest) string {
	if pr == nil || strings.TrimSpace(pr.Username) == "" {
		return "unknown"
	}
	return pr.Username
}

func provisionRequestAdminURL(pr *ProvisionRequest) string {
	if pr == nil {
		return ""
	}
	return hubDashboardBaseURL() + "/dashboard"
}

func firstCSVValue(in string) string {
	for _, part := range strings.Split(in, ",") {
		if v := strings.TrimSpace(part); v != "" {
			return v
		}
	}
	return ""
}

func truncateDiscordContent(in string) string {
	return truncateDiscordRunes(in, discordContentLimit)
}

func truncateDiscordField(in string) string {
	return truncateDiscordRunes(in, discordEmbedFieldValueLimit)
}

func truncateDiscordRunes(in string, limit int) string {
	runes := []rune(in)
	if limit <= 0 || len(runes) <= limit {
		return in
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func (s *HubServer) recordDiscordRequestNotificationSuccess(requestID string) {
	if s == nil {
		return
	}
	s.discordRequests.mu.Lock()
	defer s.discordRequests.mu.Unlock()
	s.discordRequests.SuccessCount++
	s.discordRequests.LastSuccess = time.Now().UTC()
	s.discordRequests.LastRequestID = requestID
}

func (s *HubServer) recordDiscordRequestNotificationFailure(requestID string, err error) {
	if s == nil {
		return
	}
	s.discordRequests.mu.Lock()
	defer s.discordRequests.mu.Unlock()
	s.discordRequests.FailureCount++
	s.discordRequests.LastError = time.Now().UTC()
	s.discordRequests.LastErrorRequestID = requestID
	if err != nil {
		s.discordRequests.LastErrorMessage = err.Error()
	}
}

func (s *HubServer) discordRequestsNotificationSnapshot() map[string]any {
	if s == nil {
		return nil
	}
	s.discordRequests.mu.RLock()
	defer s.discordRequests.mu.RUnlock()
	if s.discordRequests.SuccessCount == 0 && s.discordRequests.FailureCount == 0 {
		return nil
	}
	return map[string]any{
		"success_count":         s.discordRequests.SuccessCount,
		"failure_count":         s.discordRequests.FailureCount,
		"last_success":          s.discordRequests.LastSuccess,
		"last_error":            s.discordRequests.LastError,
		"last_error_message":    s.discordRequests.LastErrorMessage,
		"last_request_id":       s.discordRequests.LastRequestID,
		"last_error_request_id": s.discordRequests.LastErrorRequestID,
	}
}

func (s *HubServer) hubLogger() *slog.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}
	return slog.Default()
}
