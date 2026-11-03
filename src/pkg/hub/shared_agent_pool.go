package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var sharedAgentPoolPath = "/data/saas/shared-agent-pool.json"

const defaultSharedAgentLeaseTTL = 90 * time.Second

// A waiter that has not retried within this window is treated as gone so a
// crashed spoke cannot starve the fleet.
const sharedPoolWaiterStaleAfter = 15 * time.Second

type sharedAgentLease struct {
	ID        string    `json:"id"`
	HiveID    string    `json:"hive_id"`
	AgentName string    `json:"agent_name"`
	ExpiresAt time.Time `json:"expires_at"`
}

type sharedAgentPoolState struct {
	Version int                `json:"version"`
	Leases  []sharedAgentLease `json:"leases"`
}

type sharedAgentPool struct {
	mu   sync.Mutex
	path string
	max  int
	// maxPerHive caps the leases a single tenant may hold so one Hive cannot
	// take every slot and renew them indefinitely. 0 means no cap below max.
	maxPerHive   int
	ttl          time.Duration
	leases       map[string]sharedAgentLease
	waitingHives map[string]time.Time // hive_id -> first 409 (FIFO position)
	lastSeen     map[string]time.Time // hive_id -> last retry
	loadError    error
}

func newSharedAgentPool() *sharedAgentPool {
	max, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("HIVE_SHARED_AGENT_POOL_MAX_SESSIONS")))
	maxPerHive, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("HIVE_SHARED_AGENT_POOL_MAX_PER_HIVE")))
	if maxPerHive < 0 {
		maxPerHive = 0
	}
	ttl := defaultSharedAgentLeaseTTL
	if seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HIVE_SHARED_AGENT_POOL_LEASE_SECONDS"))); err == nil && seconds >= 30 {
		ttl = time.Duration(seconds) * time.Second
	}
	p := &sharedAgentPool{
		path:         sharedAgentPoolPath,
		max:          max,
		maxPerHive:   maxPerHive,
		ttl:          ttl,
		leases:       make(map[string]sharedAgentLease),
		waitingHives: make(map[string]time.Time),
		lastSeen:     make(map[string]time.Time),
	}
	p.load()
	return p
}

func sharedPoolOwnerKey(hiveID, agentName string) string { return hiveID + "\x00" + agentName }

func (p *sharedAgentPool) load() {
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		p.loadError = err
		return
	}
	var state sharedAgentPoolState
	if err := json.Unmarshal(data, &state); err != nil || state.Version != 1 {
		p.loadError = fmt.Errorf("invalid shared agent pool state")
		return
	}
	now := time.Now()
	for _, lease := range state.Leases {
		if lease.ID == "" || lease.HiveID == "" || lease.AgentName == "" || !lease.ExpiresAt.After(now) {
			continue
		}
		p.leases[sharedPoolOwnerKey(lease.HiveID, lease.AgentName)] = lease
	}
}

func (p *sharedAgentPool) saveLocked() error {
	state := sharedAgentPoolState{Version: 1, Leases: make([]sharedAgentLease, 0, len(p.leases))}
	for _, lease := range p.leases {
		state.Leases = append(state.Leases, lease)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	tmpPath := p.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, p.path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// hiveAtLeaseCap reports whether hiveID already holds its per-tenant share.
func (p *sharedAgentPool) hiveAtLeaseCap(hiveID string) bool {
	if p.maxPerHive < 1 {
		return false
	}
	held := 0
	for _, lease := range p.leases {
		if lease.HiveID == hiveID {
			held++
		}
	}
	return held >= p.maxPerHive
}

// hasEarlierWaitingHive reports whether another hive has been waiting longer
// than hiveID (which may not be waiting yet). Stale waiters are pruned first.
func (p *sharedAgentPool) hasEarlierWaitingHive(hiveID string, now time.Time) bool {
	if p.waitingHives == nil {
		p.waitingHives = make(map[string]time.Time)
	}
	if p.lastSeen == nil {
		p.lastSeen = make(map[string]time.Time)
	}
	for h := range p.waitingHives {
		if now.Sub(p.lastSeen[h]) > sharedPoolWaiterStaleAfter {
			delete(p.waitingHives, h)
			delete(p.lastSeen, h)
		}
	}
	mySince, waiting := p.waitingHives[hiveID]
	if !waiting {
		mySince = now
	}
	for h, since := range p.waitingHives {
		if h != hiveID && since.Before(mySince) {
			return true
		}
	}
	return false
}

func (p *sharedAgentPool) handle(w http.ResponseWriter, r *http.Request, s *HubServer) {
	if p == nil || p.max < 1 {
		http.Error(w, "shared agent pool is disabled", http.StatusServiceUnavailable)
		return
	}
	if p.loadError != nil {
		s.logger.Error("shared agent pool state is untrusted; refusing leases", "error", p.loadError)
		http.Error(w, "shared agent pool state unavailable", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		HiveID     string `json:"hive_id"`
		AgentName  string `json:"agent_name"`
		LeaseID    string `json:"lease_id"`
		CancelWait bool   `json:"cancel_wait"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.HiveID == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.verifyHeartbeatBearer(strings.TrimPrefix(auth, "Bearer "), req.HiveID) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, lease := range p.leases {
		if !lease.ExpiresAt.After(now) {
			delete(p.leases, key)
		}
	}
	switch r.Method {
	case http.MethodPost:
		if req.AgentName == "" || len(req.AgentName) > 128 {
			http.Error(w, "invalid agent name", http.StatusBadRequest)
			return
		}
		key := sharedPoolOwnerKey(req.HiveID, req.AgentName)
		if lease, ok := p.leases[key]; ok {
			lease.ExpiresAt = now.Add(p.ttl)
			p.leases[key] = lease
			delete(p.waitingHives, req.HiveID)
			delete(p.lastSeen, req.HiveID)
			if err := p.saveLocked(); err != nil {
				http.Error(w, "shared agent pool persistence failed", http.StatusServiceUnavailable)
				return
			}
			writeSharedPoolLease(w, lease.ID)
			return
		}
		if p.hiveAtLeaseCap(req.HiveID) {
			// A tenant at its own cap is not waiting on fleet capacity, so it
			// must not hold a FIFO position that blocks other Hives.
			delete(p.waitingHives, req.HiveID)
			delete(p.lastSeen, req.HiveID)
			w.Header().Set("Retry-After", "3")
			http.Error(w, "shared agent pool per-hive limit reached", http.StatusConflict)
			return
		}
		if len(p.leases) >= p.max || p.hasEarlierWaitingHive(req.HiveID, now) {
			if p.waitingHives == nil {
				p.waitingHives = make(map[string]time.Time)
			}
			if p.lastSeen == nil {
				p.lastSeen = make(map[string]time.Time)
			}
			if _, ok := p.waitingHives[req.HiveID]; !ok {
				p.waitingHives[req.HiveID] = now
			}
			p.lastSeen[req.HiveID] = now
			w.Header().Set("Retry-After", "3")
			http.Error(w, "shared agent pool is busy", http.StatusConflict)
			return
		}
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			http.Error(w, "lease generation failed", http.StatusInternalServerError)
			return
		}
		lease := sharedAgentLease{ID: hex.EncodeToString(raw[:]), HiveID: req.HiveID, AgentName: req.AgentName, ExpiresAt: now.Add(p.ttl)}
		p.leases[key] = lease
		delete(p.waitingHives, req.HiveID)
		delete(p.lastSeen, req.HiveID)
		if err := p.saveLocked(); err != nil {
			delete(p.leases, key)
			http.Error(w, "shared agent pool persistence failed", http.StatusServiceUnavailable)
			return
		}
		writeSharedPoolLease(w, lease.ID)
	case http.MethodPut, http.MethodDelete:
		if r.Method == http.MethodDelete && req.CancelWait {
			delete(p.waitingHives, req.HiveID)
			delete(p.lastSeen, req.HiveID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if req.LeaseID == "" {
			http.Error(w, "lease ID required", http.StatusBadRequest)
			return
		}
		var key string
		for candidate, lease := range p.leases {
			if lease.ID == req.LeaseID && lease.HiveID == req.HiveID {
				key = candidate
				break
			}
		}
		if key == "" {
			http.Error(w, "lease not found", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut {
			lease := p.leases[key]
			lease.ExpiresAt = now.Add(p.ttl)
			p.leases[key] = lease
		} else {
			delete(p.leases, key)
		}
		if err := p.saveLocked(); err != nil {
			http.Error(w, "shared agent pool persistence failed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeSharedPoolLease(w http.ResponseWriter, leaseID string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"lease_id": leaseID})
}
