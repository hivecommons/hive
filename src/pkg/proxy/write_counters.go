package proxy

import "sync"

// WriteCounts is a snapshot of agent GitHub write traffic seen by the proxy
// since it started (#12175). Tier is the agent's ACMM mode at request time.
// Only the agent's own writes are counted: REST write methods, GraphQL
// mutations and git-receive-pack. Control-plane (internal caller) and Linear
// traffic are excluded, and upstream 403s are not counted as blocked — only
// refusals made by the proxy itself are.
type WriteCounts struct {
	Attempted int64 `json:"attempted"`
	Blocked   int64 `json:"blocked"`
	// BlockedByReason is keyed by the machine-readable DenyReason* code.
	BlockedByReason map[string]int64 `json:"blocked_by_reason"`
}

// writeCounters is zero-value usable; observation never affects gating.
type writeCounters struct {
	mu    sync.Mutex
	tiers map[string]*WriteCounts
}

// record counts one write attempt for tier. A non-empty blockReason also
// counts it as blocked under that reason.
func (c *writeCounters) record(tier, blockReason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tiers == nil {
		c.tiers = make(map[string]*WriteCounts)
	}
	wc := c.tiers[tier]
	if wc == nil {
		wc = &WriteCounts{BlockedByReason: make(map[string]int64)}
		c.tiers[tier] = wc
	}
	wc.Attempted++
	if blockReason != "" {
		wc.Blocked++
		wc.BlockedByReason[blockReason]++
	}
}

func (c *writeCounters) snapshot() map[string]WriteCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]WriteCounts, len(c.tiers))
	for tier, wc := range c.tiers {
		cp := *wc
		cp.BlockedByReason = make(map[string]int64, len(wc.BlockedByReason))
		for k, v := range wc.BlockedByReason {
			cp.BlockedByReason[k] = v
		}
		out[tier] = cp
	}
	return out
}

// WriteCounts returns a copy of the in-memory attempted/blocked write
// counters keyed by tier (ACMM mode).
func (p *GitHubProxy) WriteCounts() map[string]WriteCounts {
	return p.writes.snapshot()
}
