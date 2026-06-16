package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"
)

const (
	heartbeatTimeout = 10 * time.Second
	staleThreshold   = 15 * time.Minute
)

type AgentSummary struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type GovernorSummary struct {
	Mode   string `json:"mode"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
}

type ContributorSummary struct {
	Active     int `json:"active"`
	Registered int `json:"registered"`
}

type LeaderboardEntry struct {
	GitHubUsername  string `json:"github_username"`
	AvatarURL      string `json:"avatar_url"`
	TrustTier      string `json:"trust_tier"`
	TasksCompleted int    `json:"tasks_completed"`
	TasksFailed    int    `json:"tasks_failed"`
	Active         bool   `json:"active"`
	CurrentTask    string `json:"current_task,omitempty"`
	HiveName       string `json:"hive_name,omitempty"`
}

type HeartbeatPayload struct {
	HiveID       string             `json:"hive_id"`
	Org          string             `json:"org"`
	Repos        []string           `json:"repos"`
	PrimaryRepo  string             `json:"primary_repo"`
	ACMMLevel    int                `json:"acmm_level"`
	Agents       []AgentSummary     `json:"agents"`
	Governor     GovernorSummary    `json:"governor"`
	Tokens24h    int64              `json:"tokens_24h"`
	Contributors ContributorSummary `json:"contributors"`
	Leaderboard  []LeaderboardEntry `json:"leaderboard"`
	Health       map[string]any     `json:"health"`
	DashboardURL string             `json:"dashboard_url"`
	SnapshotURL  string             `json:"snapshot_url"`
	Owner        string             `json:"owner,omitempty"`
	IsPublic     bool               `json:"is_public"`
	Version      string             `json:"version"`
	GitHash      string             `json:"git_hash"`
	GitBranch    string             `json:"git_branch,omitempty"`
	Timestamp          string         `json:"timestamp"`
	GitHubAppRequired  bool           `json:"github_app_required,omitempty"`
}

type StatusCollector func() *HeartbeatPayload

func StartHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, interval time.Duration, logger *slog.Logger) {
	if hubURL == "" {
		logger.Info("hub heartbeat disabled (no HIVE_HUB_URL)")
		return
	}

	logger.Info("hub heartbeat enabled", "url", hubURL, "interval", interval)

	waitForReady(ctx, logger)

	sendHeartbeat(ctx, hubURL, collect, logger)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("hub heartbeat stopped")
			return
		case <-ticker.C:
			sendHeartbeat(ctx, hubURL, collect, logger)
		}
	}
}

func waitForReady(ctx context.Context, logger *slog.Logger) {
	const healthURL = "http://localhost:3001/api/health"
	const pollInterval = 5 * time.Second
	const maxWait = 3 * time.Minute
	deadline := time.After(maxWait)
	logger.Info("heartbeat waiting for dashboard readiness")
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			logger.Warn("heartbeat readiness wait timed out, starting anyway")
			return
		default:
			reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, healthURL, nil)
			if err == nil {
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						cancel()
						logger.Info("dashboard ready, starting heartbeats")
						return
					}
				}
			}
			cancel()
			time.Sleep(pollInterval)
		}
	}
}

var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func sendHeartbeat(ctx context.Context, hubURL string, collect StatusCollector, logger *slog.Logger) {
	payload := collect()
	if payload == nil {
		return
	}
	payload.Timestamp = time.Now().UTC().Format(time.RFC3339)

	filtered := payload.Leaderboard[:0]
	for _, lb := range payload.Leaderboard {
		if lb.GitHubUsername != "" && validNamePattern.MatchString(lb.GitHubUsername) {
			filtered = append(filtered, lb)
		}
	}
	payload.Leaderboard = filtered

	filteredAgents := payload.Agents[:0]
	for _, a := range payload.Agents {
		if a.Name != "" && validNamePattern.MatchString(a.Name) {
			filteredAgents = append(filteredAgents, a)
		}
	}
	payload.Agents = filteredAgents

	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("hub heartbeat marshal failed", "error", err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+"/api/heartbeat", bytes.NewReader(body))
	if err != nil {
		logger.Warn("hub heartbeat request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := os.Getenv("HIVE_HUB_SECRET"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("hub heartbeat unreachable", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Warn("hub heartbeat rejected", "status", resp.StatusCode, "body", string(respBody))
	}
}

const taskPushInterval = 30 * time.Second

type TaskStatusPayload struct {
	HiveID       string             `json:"hive_id"`
	Leaderboard  []LeaderboardEntry `json:"leaderboard"`
	Contributors ContributorSummary `json:"contributors"`
}

type TaskStatusCollector func() *TaskStatusPayload

func StartTaskStatusPush(ctx context.Context, hubURL string, collect TaskStatusCollector, logger *slog.Logger) {
	if hubURL == "" {
		return
	}

	logger.Info("hub task status push enabled", "url", hubURL, "interval", taskPushInterval)
	ticker := time.NewTicker(taskPushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			payload := collect()
			if payload == nil {
				continue
			}
			body, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			reqCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hubURL+"/api/task-status", bytes.NewReader(body))
			if err != nil {
				cancel()
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			if secret := os.Getenv("HIVE_HUB_SECRET"); secret != "" {
				req.Header.Set("Authorization", "Bearer "+secret)
			}
			resp, err := http.DefaultClient.Do(req)
			cancel()
			if err == nil {
				resp.Body.Close()
			}
		}
	}
}
