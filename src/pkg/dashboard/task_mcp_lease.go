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
	Claims    taskmcp.LeaseTokenClaims
	TokenHash string
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
	token, claims, err := taskmcp.MintLeaseToken(secret, taskmcp.LeaseTokenClaims{
		TaskID: assign.TaskID, Identity: identity, Repo: assign.Repo, Number: assign.Number,
		ExpiresAt: expiresAt, Contributor: contributor,
	}, time.Now())
	if err != nil {
		h.logger.Warn("[task-mcp] failed to mint lease token", "task", assign.TaskID, "repo", assign.Repo, "error", err)
		return nil
	}
	h.leaseMu.Lock()
	if l := h.leaseForLocked(identity, assign.TaskID); l != nil {
		l.mcpTokenID = claims.ID
		l.mcpTokenHash = taskmcp.HashLeaseToken(token)
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
	h.leaseMu.Lock()
	l := h.leaseForLocked(claims.Identity, claims.TaskID)
	ok := l != nil && !time.Now().After(l.expiresAt) && l.mcpTokenID == claims.ID &&
		subtle.ConstantTimeCompare([]byte(l.mcpTokenHash), []byte(hash)) == 1 &&
		l.repo == claims.Repo && l.number == claims.Number
	h.leaseMu.Unlock()
	if !ok {
		return r, false
	}
	ctx := context.WithValue(r.Context(), taskMCPLeaseContextKey{}, taskMCPLeaseContext{Claims: claims, TokenHash: hash})
	return r.WithContext(ctx), true
}

func taskMCPLeaseFromRequest(r *http.Request) (taskMCPLeaseContext, bool) {
	v, ok := r.Context().Value(taskMCPLeaseContextKey{}).(taskMCPLeaseContext)
	return v, ok
}

func bearerToken(r *http.Request) string {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return token
}
