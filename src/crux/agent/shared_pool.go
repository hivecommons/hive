package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const sharedSessionPoolPath = "/api/shared-agent-pool"
const sharedPoolIdleEvictionDelay = 5 * time.Minute

// sharedPoolRenewInterval is the lease heartbeat cadence; sharedPoolRenewGrace
// bounds how long renewals may keep failing before the spoke assumes the Hub
// has expired the lease (default TTL 90s) and evicts the CLI session. A single
// Hub blip or rolling restart must never tear down an in-flight turn.
const (
	sharedPoolRenewInterval = 20 * time.Second
	sharedPoolRenewGrace    = 60 * time.Second
)

var errSharedSessionPoolBusy = fmt.Errorf("shared session pool is busy")

// SharedSessionPool coordinates heavyweight CLI sessions across Hive tenants.
// Each tenant keeps its own tmux session, UID, credentials, and workspace; only
// the right to have a live CLI session is shared.
type SharedSessionPool interface {
	Acquire(ctx context.Context, agentName string) (string, error)
	Renew(ctx context.Context, leaseID string) error
	Release(ctx context.Context, leaseID string) error
}

type sharedSessionPoolClient struct {
	baseURL string
	hiveID  string
	token   string
	client  *http.Client
}

// NewSharedSessionPoolClient creates an authenticated client for the Hub's
// tenant-scoped session-slot API. The token must be the per-Hive heartbeat key,
// never the fleet master secret.
func NewSharedSessionPoolClient(baseURL, hiveID, token string) (SharedSessionPool, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || hiveID == "" || token == "" {
		return nil, fmt.Errorf("shared session pool requires Hub URL, Hive ID, and per-Hive token")
	}
	return &sharedSessionPoolClient{
		baseURL: baseURL,
		hiveID:  hiveID,
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

type sharedPoolRequest struct {
	HiveID     string `json:"hive_id"`
	AgentName  string `json:"agent_name,omitempty"`
	LeaseID    string `json:"lease_id,omitempty"`
	CancelWait bool   `json:"cancel_wait,omitempty"`
}

type sharedPoolResponse struct {
	LeaseID string `json:"lease_id"`
}

func (c *sharedSessionPoolClient) request(ctx context.Context, method, path string, body sharedPoolRequest) (sharedPoolResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return sharedPoolResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+sharedSessionPoolPath+path, bytes.NewReader(payload))
	if err != nil {
		return sharedPoolResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return sharedPoolResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusTooManyRequests {
		return sharedPoolResponse{}, errSharedSessionPoolBusy
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return sharedPoolResponse{}, fmt.Errorf("shared session pool returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if resp.ContentLength == 0 {
		return sharedPoolResponse{}, nil
	}
	var result sharedPoolResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil && err != io.EOF {
		return sharedPoolResponse{}, err
	}
	return result, nil
}

func (c *sharedSessionPoolClient) Acquire(ctx context.Context, agentName string) (string, error) {
	deadlineCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		deadlineCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	for {
		result, err := c.request(deadlineCtx, http.MethodPost, "/leases", sharedPoolRequest{HiveID: c.hiveID, AgentName: agentName})
		if err == nil {
			if result.LeaseID == "" {
				return "", fmt.Errorf("shared session pool returned an empty lease ID")
			}
			return result.LeaseID, nil
		}
		if err != errSharedSessionPoolBusy {
			return "", err
		}
		timer := time.NewTimer(1 * time.Second)
		select {
		case <-deadlineCtx.Done():
			timer.Stop()
			cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = c.request(cancelCtx, http.MethodDelete, "/leases", sharedPoolRequest{HiveID: c.hiveID, CancelWait: true})
			cancel()
			return "", fmt.Errorf("shared session pool is busy: %w", deadlineCtx.Err())
		case <-timer.C:
		}
	}
}

func (c *sharedSessionPoolClient) Renew(ctx context.Context, leaseID string) error {
	_, err := c.request(ctx, http.MethodPut, "/leases", sharedPoolRequest{HiveID: c.hiveID, LeaseID: leaseID})
	return err
}

func (c *sharedSessionPoolClient) Release(ctx context.Context, leaseID string) error {
	_, err := c.request(ctx, http.MethodDelete, "/leases", sharedPoolRequest{HiveID: c.hiveID, LeaseID: leaseID})
	return err
}

func (m *Manager) watchSharedPoolLease(ctx context.Context, name, leaseID string) {
	ticker := time.NewTicker(sharedPoolRenewInterval)
	defer ticker.Stop()
	lastRenewed := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if done := m.sharedPoolLeaseTick(name, leaseID, &lastRenewed); done {
			return
		}
	}
}

// sharedPoolLeaseTick runs one watcher iteration: evict an idle session, else
// renew the lease. lastRenewed is advanced on success; a failed renewal only
// evicts once the grace window since the last success has elapsed, so a Hub
// blip never destroys an in-flight turn. Returns true when the watcher should
// stop (lease gone or session evicted).
func (m *Manager) sharedPoolLeaseTick(name, leaseID string, lastRenewed *time.Time) bool {
	m.mu.RLock()
	a := m.agents[name]
	if a == nil || a.poolLeaseID != leaseID {
		m.mu.RUnlock()
		return true
	}
	state := a.State
	idle := a.LastKick == nil || a.KickOutcome.Settled(a.LastKick)
	delivering := a.kickDelivering.Load()
	idleSince := time.Time{}
	if a.StartedAt != nil {
		idleSince = *a.StartedAt
	}
	if a.KickOutcome.At.After(idleSince) {
		idleSince = a.KickOutcome.At
	}
	m.mu.RUnlock()
	if state == StateRunning && idle && !delivering &&
		time.Since(idleSince) >= sharedPoolIdleEvictionDelay &&
		paneShowsInputPrompt(m.captureVisiblePaneForAgent(a)) {
		m.evictSharedPoolSession(name, leaseID)
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	err := m.sharedPool.Renew(ctx, leaseID)
	cancel()
	if err == nil {
		*lastRenewed = time.Now()
		return false
	}
	if since := time.Since(*lastRenewed); since < sharedPoolRenewGrace {
		m.logger.Warn("shared agent pool lease renewal failed; retrying before lease expiry",
			"agent", name, "error", err, "since_last_renew", since.Round(time.Second))
		return false
	}
	m.logger.Warn("shared agent pool lease renewal failed past grace window; evicting CLI session",
		"agent", name, "error", err, "since_last_renew", time.Since(*lastRenewed).Round(time.Second))
	m.evictSharedPoolSession(name, leaseID)
	return true
}

// sharedPoolAutoStartEligibleLocked reports whether a kick may transparently
// relaunch this agent: pooled agents are parked in StateStopped between turns
// by eviction, so a kick to one is a request for a new session, not an error.
// Operator stops and pauses still win. Caller holds m.mu.
func (m *Manager) sharedPoolAutoStartEligibleLocked(agent *AgentProcess) bool {
	return m.sharedPool != nil && agent.State == StateStopped && !agent.Paused && !agent.OperatorStopped
}

// acquireSharedPoolLeaseForRelaunchUnlocked obtains a pool lease for a restart
// path that relaunches via launchInTmux directly (and so bypasses Start's
// acquire). Returns "" when the pool is disabled or the agent already holds a
// lease. MUST be called with m.mu released: Acquire blocks on the Hub.
func (m *Manager) acquireSharedPoolLeaseForRelaunchUnlocked(ctx context.Context, agent *AgentProcess, name string) (string, error) {
	if m.sharedPool == nil {
		return "", nil
	}
	m.mu.RLock()
	held := agent.poolLeaseID
	m.mu.RUnlock()
	if held != "" {
		return "", nil
	}
	leaseID, err := m.sharedPool.Acquire(ctx, name)
	if err != nil {
		return "", fmt.Errorf("acquire shared CLI session slot: %w", err)
	}
	return leaseID, nil
}

// relaunchWithSharedPoolLeaseLocked binds leaseID (from
// acquireSharedPoolLeaseForRelaunchUnlocked; "" when none was needed) to the
// agent, arms the renewal watcher, and relaunches. A failed launch hands the
// slot straight back so the global cap is not consumed by a dead session.
// Caller holds m.mu.
func (m *Manager) relaunchWithSharedPoolLeaseLocked(ctx context.Context, agent *AgentProcess, leaseID string) error {
	if leaseID != "" {
		agent.poolLeaseID = leaseID
		m.startSharedPoolLeaseWatcherLocked(agent, agent.Name, leaseID)
	}
	err := m.launchInTmux(ctx, agent)
	if err != nil && leaseID != "" {
		m.discardSharedPoolLeaseLocked(agent, leaseID)
	}
	return err
}

// discardSharedPoolLeaseLocked detaches leaseID from the agent (if bound) and
// returns it to the Hub off the lock. Caller holds m.mu.
func (m *Manager) discardSharedPoolLeaseLocked(agent *AgentProcess, leaseID string) {
	if leaseID == "" {
		return
	}
	if agent != nil && agent.poolLeaseID == leaseID {
		agent.poolLeaseID = ""
		if cancel := agent.poolLeaseCancel; cancel != nil {
			agent.poolLeaseCancel = nil
			cancel()
		}
	}
	name := ""
	if agent != nil {
		name = agent.Name
	}
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 8*time.Second)
		defer done()
		if err := m.sharedPool.Release(ctx, leaseID); err != nil {
			m.logger.Warn("shared agent pool lease release failed", "agent", name, "error", err)
		}
	}()
}

func (m *Manager) evictSharedPoolSession(name, leaseID string) {
	m.evictSharedPoolSessionIf(name, leaseID, nil)
}

func (m *Manager) evictSharedPoolSessionAfterTurn(name, leaseID string, outcome KickOutcome) {
	m.evictSharedPoolSessionIf(name, leaseID, &outcome)
}

func (m *Manager) evictSharedPoolSessionIf(name, leaseID string, expected *KickOutcome) {
	m.mu.Lock()
	a := m.agents[name]
	if a == nil || a.poolLeaseID != leaseID {
		m.mu.Unlock()
		return
	}
	if expected != nil &&
		(!a.KickOutcome.At.Equal(expected.At) ||
			!a.KickOutcome.KickAt.Equal(expected.KickAt) ||
			a.KickOutcome.Kind != expected.Kind ||
			a.KickOutcome.Reason != expected.Reason ||
			!a.KickOutcome.Settled(a.LastKick) ||
			a.kickDelivering.Load()) {
		m.mu.Unlock()
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	m.archiveKickLogLocked(a, "shared-pool-eviction")
	_ = m.tmuxCmd(a, "kill-session", "-t", a.tmuxSession).Run()
	_ = m.reapAgentCLI(a)
	a.State = StateStopped
	a.poolLeaseID = ""
	cancel := a.poolLeaseCancel
	a.poolLeaseCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	ctx, done := context.WithTimeout(context.Background(), 8*time.Second)
	if err := m.sharedPool.Release(ctx, leaseID); err != nil {
		m.logger.Warn("shared agent pool lease release failed", "agent", name, "error", err)
	}
	done()
	m.logger.Info("shared agent pool evicted idle CLI session", "agent", name)
}

func (m *Manager) releaseSharedPoolLease(name, leaseID string) {
	m.mu.Lock()
	a := m.agents[name]
	if a != nil && a.poolLeaseID == leaseID {
		a.poolLeaseID = ""
		cancel := a.poolLeaseCancel
		a.poolLeaseCancel = nil
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	} else {
		m.mu.Unlock()
	}
	ctx, done := context.WithTimeout(context.Background(), 8*time.Second)
	defer done()
	if err := m.sharedPool.Release(ctx, leaseID); err != nil {
		m.logger.Warn("shared agent pool lease release failed", "agent", name, "error", err)
	}
}
