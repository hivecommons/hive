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
	if resp.StatusCode == http.StatusConflict {
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
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
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
		if err != nil {
			m.logger.Warn("shared agent pool lease renewal failed; evicting CLI session", "agent", name, "error", err)
			m.evictSharedPoolSession(name, leaseID)
			return
		}
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
