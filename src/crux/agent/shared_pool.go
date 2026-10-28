package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const sharedSessionPoolPath = "/api/shared-agent-pool"
const sharedPoolIdleEvictionDelay = 5 * time.Minute

// sharedPoolRenewInterval is the spoke's lease renewal cadence;
// sharedPoolRenewGrace is how long renewals may keep failing before the CLI
// session is evicted. Both sit well inside the Hub's 90 s default TTL, so a
// transient Hub blip or a hub roll never destroys an in-flight turn.
const (
	sharedPoolRenewInterval = 20 * time.Second
	sharedPoolRenewGrace    = 60 * time.Second
)

var errSharedSessionPoolBusy = fmt.Errorf("shared session pool is busy")

// errSharedSessionLeaseNotFound means the Hub no longer knows the lease: the
// slot may already be handed to another tenant, so eviction must not wait out
// the renewal grace window.
var errSharedSessionLeaseNotFound = fmt.Errorf("shared session pool lease not found")

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
	if resp.StatusCode == http.StatusConflict {
		return sharedPoolResponse{}, errSharedSessionPoolBusy
	}
	if resp.StatusCode == http.StatusNotFound && method != http.MethodPost {
		return sharedPoolResponse{}, errSharedSessionLeaseNotFound
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
	m.watchSharedPoolLeaseEvery(ctx, name, leaseID, sharedPoolRenewInterval, sharedPoolRenewGrace)
}

func (m *Manager) watchSharedPoolLeaseEvery(ctx context.Context, name, leaseID string, interval, grace time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastRenewed := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.RLock()
		a := m.agents[name]
		if a == nil || a.poolLeaseID != leaseID {
			m.mu.RUnlock()
			return
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
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := m.sharedPool.Renew(ctx, leaseID)
		cancel()
		if err == nil {
			lastRenewed = time.Now()
			continue
		}
		// Tolerate transient renewal failures: the Hub TTL outlasts several
		// missed renewals, so only a lease that is definitively gone or one
		// that has been unrenewed for the whole grace window evicts the CLI.
		sinceRenew := time.Since(lastRenewed)
		if !errors.Is(err, errSharedSessionLeaseNotFound) && sinceRenew < grace {
			m.logger.Warn("shared agent pool lease renewal failed; retrying before eviction",
				"agent", name, "error", err, "since_last_renew", sinceRenew.Round(time.Second))
			continue
		}
		m.logger.Warn("shared agent pool lease renewal failed; evicting CLI session",
			"agent", name, "error", err, "since_last_renew", sinceRenew.Round(time.Second))
		m.evictSharedPoolSession(name, leaseID)
		return
	}
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

// acquireSharedPoolLeaseUnlocked obtains a pool lease for a relaunch path
// (restart, missing-session recovery) that bypasses Start. It returns "" when
// no pool is configured or the agent already holds a lease. Callers MUST NOT
// hold m.mu: Acquire blocks on the Hub.
func (m *Manager) acquireSharedPoolLeaseUnlocked(ctx context.Context, name string) (string, error) {
	if m.sharedPool == nil {
		return "", nil
	}
	m.mu.RLock()
	a := m.agents[name]
	held := a != nil && a.poolLeaseID != ""
	m.mu.RUnlock()
	if held {
		return "", nil
	}
	leaseID, err := m.sharedPool.Acquire(ctx, name)
	if err != nil {
		return "", fmt.Errorf("acquire shared CLI session slot: %w", err)
	}
	return leaseID, nil
}

// launchWithSharedPoolLeaseLocked relaunches agent via launchInTmux and binds a
// lease obtained by acquireSharedPoolLeaseUnlocked to the live session. On
// launch failure the lease is handed back asynchronously (the release must not
// block under m.mu). Caller holds m.mu.
func (m *Manager) launchWithSharedPoolLeaseLocked(ctx context.Context, agent *AgentProcess, name, leaseID string) error {
	if leaseID == "" {
		return m.launchInTmux(ctx, agent)
	}
	agent.poolLeaseID = leaseID
	err := m.launchInTmux(ctx, agent)
	if err != nil {
		agent.poolLeaseID = ""
		go m.releaseSharedPoolLease(name, leaseID)
		return err
	}
	m.startSharedPoolLeaseWatcherLocked(agent, name, leaseID)
	return nil
}

// discardSharedPoolLeaseLocked hands back a lease that was acquired for a
// relaunch that never happened (agent removed, or another path won the launch
// race). The Hub returns the same lease for the same hive+agent, so a lease the
// racing winner already adopted is left alone. Caller holds m.mu; the Hub
// release runs on its own goroutine.
func (m *Manager) discardSharedPoolLeaseLocked(name, leaseID string) {
	if leaseID == "" {
		return
	}
	if cur := m.agents[name]; cur != nil && cur.poolLeaseID == leaseID {
		return
	}
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 8*time.Second)
		defer done()
		if err := m.sharedPool.Release(ctx, leaseID); err != nil {
			m.logger.Warn("shared agent pool lease release failed", "agent", name, "error", err)
		}
	}()
}
