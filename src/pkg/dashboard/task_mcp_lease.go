package dashboard

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

type taskMCPLeaseContextKey struct{}

type taskMCPLeaseContext struct {
	Claims       taskmcp.LeaseTokenClaims
	TokenHash    string
	CurrentStage string
}

func (s *Server) taskMCPRemoteEnabled() bool {
	return s != nil && s.deps != nil && s.deps.Config != nil && s.deps.Config.TaskMCP.RemoteEnabled
}

func (s *Server) taskMCPLeaseSecret() []byte {
	if s == nil || strings.TrimSpace(s.authToken) == "" {
		return nil
	}
	return []byte(s.authToken)
}

func (s *Server) taskMCPURL() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	cfg := s.deps.Config
	for _, raw := range []string{cfg.Dashboard.PublicURL, cfg.Hub.DashboardURL, cfg.Hub.URL} {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		return raw + taskmcp.EndpointPath
	}
	return ""
}

func (h *ContributeWSHub) taskMCPRateLimitPerMinute() int {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return config.DefaultTaskMCPLeaseRateLimitPerMinute
	}
	return h.server.deps.Config.TaskMCP.LeaseRateLimitPerMinuteOrDefault()
}

func (h *ContributeWSHub) allowTaskMCPCall(leaseID string, now time.Time) bool {
	limit := h.taskMCPRateLimitPerMinute()
	if limit <= 0 || leaseID == "" {
		return true
	}
	cutoff := now.Add(-time.Minute)
	h.mcpRateMu.Lock()
	defer h.mcpRateMu.Unlock()
	if h.mcpCallTimes == nil {
		h.mcpCallTimes = make(map[string][]time.Time)
	}
	kept := h.mcpCallTimes[leaseID][:0]
	for _, ts := range h.mcpCallTimes[leaseID] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= limit {
		h.mcpCallTimes[leaseID] = kept
		return false
	}
	kept = append(kept, now)
	h.mcpCallTimes[leaseID] = kept
	return true
}

func (h *ContributeWSHub) mintTaskMCPForAssignment(identity string, assign *WSTaskAssign, expiresAt time.Time, contributor string) *WSTaskMCP {
	if h == nil || h.server == nil || assign == nil || !h.server.taskMCPRemoteEnabled() {
		return nil
	}
	endpoint := h.server.taskMCPURL()
	secret := h.server.taskMCPLeaseSecret()
	if endpoint == "" || len(secret) == 0 {
		return nil
	}
	stage := ""
	h.leaseMu.Lock()
	if l := h.leaseForLocked(identity, assign.TaskID); l != nil {
		stage = l.stage
	}
	h.leaseMu.Unlock()
	token, claims, err := taskmcp.MintLeaseToken(secret, taskmcp.LeaseTokenClaims{
		TaskID: assign.TaskID, Identity: identity, Repo: assign.Repo, Number: assign.Number,
		Stage: stage, ExpiresAt: expiresAt, Contributor: contributor,
	}, time.Now())
	if err != nil {
		h.logger.Warn("[task-mcp] failed to mint lease token", "task", assign.TaskID, "repo", assign.Repo, "error", err)
		return nil
	}
	h.leaseMu.Lock()
	if l := h.leaseForLocked(identity, assign.TaskID); l != nil {
		l.mcpTokenID = claims.ID
		l.mcpTokenHash = taskmcp.HashLeaseToken(token)
		l.mcpTokenStage = claims.Stage
		h.saveLeasesLocked()
	}
	h.leaseMu.Unlock()
	return &WSTaskMCP{URL: endpoint, Token: token, ExpiresAt: expiresAt.UTC().Format(time.RFC3339)}
}

func (s *Server) authenticateTaskMCPLease(r *http.Request) (*http.Request, bool) {
	if !s.taskMCPRemoteEnabled() {
		return r, false
	}
	token := bearerToken(r)
	if token == "" {
		return r, false
	}
	claims, err := taskmcp.VerifyLeaseToken(s.taskMCPLeaseSecret(), token, time.Now())
	if err != nil || s.contributeHub == nil {
		return r, false
	}
	hash := taskmcp.HashLeaseToken(token)
	h := s.contributeHub
	currentStage := ""
	h.leaseMu.Lock()
	l := h.leaseForLocked(claims.Identity, claims.TaskID)
	if l != nil {
		currentStage = l.stage
	}
	ok := l != nil && !time.Now().After(l.expiresAt) && l.mcpTokenID == claims.ID &&
		subtle.ConstantTimeCompare([]byte(l.mcpTokenHash), []byte(hash)) == 1 &&
		l.repo == claims.Repo && l.number == claims.Number &&
		(l.mcpTokenStage == "" || l.mcpTokenStage == claims.Stage)
	h.leaseMu.Unlock()
	if !ok {
		return r, false
	}
	ctx := context.WithValue(r.Context(), taskMCPLeaseContextKey{}, taskMCPLeaseContext{Claims: claims, TokenHash: hash, CurrentStage: currentStage})
	return r.WithContext(ctx), true
}

// authenticateTaskMCPLaunch accepts the per-launch token the boot mints for
// hub-launched agents (#8348). The signature proves the hub issued it; the
// claims must then name a launch the agent manager still reports as active
// (same task id, repo and agent identity), so the token dies with the launch
// - a relaunch changes the generation baked into the task id and the old
// token stops matching. Unlike remote leases this path does not require
// task_mcp.remote_enabled: hub launches exist on every hive.
func (s *Server) authenticateTaskMCPLaunch(r *http.Request) (*http.Request, bool) {
	if s == nil {
		return r, false
	}
	token := bearerToken(r)
	if token == "" {
		return r, false
	}
	claims, err := taskmcp.VerifyLeaseToken(s.taskMCPLeaseSecret(), token, time.Now())
	if err != nil {
		return r, false
	}
	lookup := dashboardTaskMCPProvider{server: s}.activeLaunchLookup()
	if lookup == nil {
		return r, false
	}
	matched := false
	for _, launch := range lookup.ActiveLaunches() {
		if launch.Agent == claims.Identity && launchMatches(launch, claims.TaskID, claims.Repo, claims.Number) {
			matched = true
			break
		}
	}
	if !matched {
		return r, false
	}
	ctx := context.WithValue(r.Context(), taskMCPLeaseContextKey{}, taskMCPLeaseContext{Claims: claims, TokenHash: taskmcp.HashLeaseToken(token)})
	return r.WithContext(ctx), true
}

func taskMCPLeaseFromRequest(r *http.Request) (taskMCPLeaseContext, bool) {
	v, ok := r.Context().Value(taskMCPLeaseContextKey{}).(taskMCPLeaseContext)
	return v, ok
}

func bearerToken(r *http.Request) string {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get(taskmcp.TokenQueryParam))
	}
	return token
}
