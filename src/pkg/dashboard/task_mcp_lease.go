package dashboard

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/config"
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

// taskMCPURL is the task MCP endpoint a remote contributor is given in
// task_assign, or "" when this hive has no public address of its own. The
// relay sends the lease bearer there, so only an address of THIS hive
// qualifies: dashboard.public_url, or hub.dashboard_url while the hub is
// enabled. A disabled hub leaves a stale one behind, which is why
// resolveSpokeDashboardURL (cmd/hive) has the same condition since #10282.
// hub.url is the hub itself and has a default on every hive, so it is never
// an answer, and neither is that resolver's last resort, the local port.
func (s *Server) taskMCPURL() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	cfg := s.deps.Config
	candidates := []string{cfg.Dashboard.PublicURL}
	if cfg.Hub.Enabled {
		candidates = append(candidates, cfg.Hub.DashboardURL)
	}
	for _, raw := range candidates {
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

// taskMCPLeaseTokenMaxTTL is the hard cap on a remote contributor's task MCP
// lease token, measured from assignment. It is deliberately NOT the task lease
// window: the lease is renewed on every accepted task_progress (renewLease)
// while the token is minted once and never re-sent, so a token that expired
// with the first window cut a healthy task off from its task MCP at minute 30.
// Whether the token is accepted before the cap is decided by the server-side
// lease record alone (authenticateTaskMCPLease), so the token never outlives
// its lease; the cap only bounds how long one minted token can be presented at
// all, however long the lease is kept alive. The value is the launch-token
// default; the launch-token TTL setting configures hub launches only.
const taskMCPLeaseTokenMaxTTL = config.DefaultTaskMCPLaunchTokenTTL

// mintTaskMCPForAssignment mints the lease token for one assignment and binds
// it, by id and hash, to the lease recorded for that task. expiresAt is the
// token's own hard cap - assignment plus taskMCPLeaseTokenMaxTTL - not the end
// of the lease window.
func (h *ContributeWSHub) mintTaskMCPForAssignment(identity string, assign *WSTaskAssign, expiresAt time.Time, contributor string) *WSTaskMCP {
	if h == nil || h.server == nil || assign == nil || !h.server.taskMCPRemoteEnabled() {
		return nil
	}
	secret := h.server.taskMCPLeaseSecret()
	if len(secret) == 0 {
		return nil
	}
	endpoint := h.server.taskMCPURL()
	if endpoint == "" {
		h.logger.Info("[task-mcp] task_assign carries no mcp block: this hive has no public address",
			"task", assign.TaskID, "repo", assign.Repo, "hint", "set dashboard.public_url to an address contributors can reach")
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

// authenticateTaskMCPLease accepts a remote contributor's lease token only
// while the server-side lease it was minted for still backs it: a record for
// the token's {identity, task} exists, has not expired, and still carries this
// token's id and hash (plus repo, number and stage). The token's own expiry is
// only the hard cap, so these checks are what revokes it - on release, lease
// expiry, a lease recorded again, or a newer token minted for the same lease.
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
	token := bearerHeaderToken(r)
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get(taskmcp.TokenQueryParam))
	}
	return token
}

func bearerHeaderToken(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func (s *Server) isQueryDashboardToken(r *http.Request) bool {
	if s == nil || strings.TrimSpace(s.authToken) == "" {
		return false
	}
	return secureCompare(strings.TrimSpace(r.URL.Query().Get(taskmcp.TokenQueryParam)), s.authToken)
}
