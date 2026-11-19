package dashboard

import (
	"sync"
	"time"
)

// proxyProofWarnInterval is the minimum gap between WARN logs for the same
// rejection reason and path. A hub/spoke token mismatch rejects every request
// the hub proxies, which flooded the log (99 in 10m, #18377).
const proxyProofWarnInterval = time.Minute

// proxyProofWarnGate dedupes the proxy-proof rejection WARN. The 403 itself is
// never throttled; only the log line is.
var proxyProofWarnGate = &warnGate{interval: proxyProofWarnInterval}

type warnGate struct {
	mu         sync.Mutex
	interval   time.Duration
	last       map[string]time.Time
	suppressed map[string]int
}

// allow reports whether key may log now, and how many logs for key were
// suppressed since the last allowed one.
func (g *warnGate) allow(key string, now time.Time) (suppressed int, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]time.Time{}
		g.suppressed = map[string]int{}
	}
	if t, seen := g.last[key]; seen && now.Sub(t) < g.interval {
		g.suppressed[key]++
		return 0, false
	}
	if len(g.last) >= 256 { // bound memory against forged paths
		g.last = map[string]time.Time{}
		g.suppressed = map[string]int{}
	}
	n := g.suppressed[key]
	delete(g.suppressed, key)
	g.last[key] = now
	return n, true
}
