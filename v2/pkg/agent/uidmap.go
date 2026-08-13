package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	UIDMapPath = "/var/run/hive/uid-map.json"
	baseAgentUID = 2001
	proxyUserUID = 1001
)

type UIDMap struct {
	Agents         map[string]int `json:"agents"`
	ProxyUID       int            `json:"proxy_uid"`
	BaseUID        int            `json:"base_uid"`
	IptablesActive bool           `json:"iptables_active"`

	mu sync.RWMutex
}

func NewUIDMap() *UIDMap {
	return &UIDMap{
		Agents:   make(map[string]int),
		ProxyUID: proxyUserUID,
		BaseUID:  baseAgentUID,
	}
}

// AllocateUIDs assigns UIDs to agent names in alphabetical order,
// starting from BaseUID. Existing allocations are preserved.
func (u *UIDMap) AllocateUIDs(names []string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	for i, name := range sorted {
		if _, exists := u.Agents[name]; !exists {
			u.Agents[name] = u.BaseUID + i
		}
	}
}

// AllocateUID assigns a UID to a single agent, using the next available
// UID above all existing allocations. For runtime agent additions.
func (u *UIDMap) AllocateUID(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()

	if uid, exists := u.Agents[name]; exists {
		return uid
	}

	maxUID := u.BaseUID - 1
	for _, uid := range u.Agents {
		if uid > maxUID {
			maxUID = uid
		}
	}
	uid := maxUID + 1
	u.Agents[name] = uid
	return uid
}

// LookupByUID returns the agent name for a given UID, or empty string if not found.
func (u *UIDMap) LookupByUID(uid int) string {
	u.mu.RLock()
	defer u.mu.RUnlock()

	for name, agentUID := range u.Agents {
		if agentUID == uid {
			return name
		}
	}
	return ""
}

// IsInternalUID reports whether uid belongs to the hive's OWN control plane
// rather than to a sandboxed agent — the hive process itself (which runs as
// root) or the MITM proxy user.
//
// WHY THIS EXISTS (proxy request blocked / github_auth 403). v4 forces ALL
// egress through the MITM proxy, including the hive's own control-plane calls.
// The proxy attributes a connection to an agent by looking its UID up in
// Agents; the hive process is not an agent, so the lookup missed, agentName
// came back "", and identifyAgentFromConn's caller fell back to ADVISORY and
// blocked the write. That silently broke every App installation-token mint
// (POST /app/installations/{id}/access_tokens) on v4 spokes — surfacing as a
// bogus GitHub "403" and paused agents fleet-wide.
//
// SECURITY. This must NEVER match an agent UID. Agents are >= BaseUID (2001)
// and are dynamically allocated upward, so the internal set is exactly {0, the
// proxy UID} and is checked against Agents as a belt-and-braces guard: if an
// agent were ever provisioned at 0 or at ProxyUID, this returns false rather
// than handing that agent an exemption. Verified on a live v4 spoke that an
// agent UID cannot reach 0: su-exec is 4750 root:hive-launch and agents are not
// in hive-launch (Permission denied), /usr/bin/su fails (root is password
// locked, "*"), and mount is refused (no CAP_SYS_ADMIN, CapBnd 15fb).
func (u *UIDMap) IsInternalUID(uid int) bool {
	if uid != 0 && uid != u.ProxyUID {
		return false
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, agentUID := range u.Agents {
		if agentUID == uid {
			// An agent occupies this UID — never treat it as internal.
			return false
		}
	}
	return true
}

// LookupByName returns the UID for a given agent name, or 0 if not found.
func (u *UIDMap) LookupByName(name string) int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.Agents[name]
}

// Save writes the UID map to the given path as JSON.
func (u *UIDMap) Save(path string) error {
	u.mu.RLock()
	defer u.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create uid-map dir: %w", err)
	}
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal uid-map: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write uid-map: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadUIDMap reads a UID map from the given path.
func LoadUIDMap(path string) (*UIDMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read uid-map: %w", err)
	}
	u := &UIDMap{}
	if err := json.Unmarshal(data, u); err != nil {
		return nil, fmt.Errorf("unmarshal uid-map: %w", err)
	}
	if u.Agents == nil {
		u.Agents = make(map[string]int)
	}
	return u, nil
}
