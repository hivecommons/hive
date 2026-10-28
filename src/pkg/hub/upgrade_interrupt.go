package hub

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// upgradeInterruptTTL bounds how long an "Upgrade now (interrupt turns)"
// request stays armed. The spoke collects it on its next heartbeat; if the
// upgrade has not even been delivered after this long the drain it was meant
// to cut is long gone and a stale interrupt must not cut a FUTURE upgrade's
// turns.
const upgradeInterruptTTL = 2 * time.Hour

// upgradeInterruptReq is one operator request to skip the self-upgrade drain.
type upgradeInterruptReq struct {
	Target string
	By     string
	At     time.Time
}

// upgradeInterrupts remembers, per hive, that the owner asked the pending
// upgrade to interrupt in-flight agent turns instead of draining (#14950).
// Leaf mutex; in-memory only — a hub restart drops the request, which is
// the safe failure (the spoke falls back to draining).
type upgradeInterrupts struct {
	mu   sync.Mutex
	byID map[string]upgradeInterruptReq
}

func newUpgradeInterrupts() *upgradeInterrupts {
	return &upgradeInterrupts{byID: make(map[string]upgradeInterruptReq)}
}

func (u *upgradeInterrupts) arm(hiveID, target, by string, now time.Time) {
	if u == nil || hiveID == "" {
		return
	}
	u.mu.Lock()
	u.byID[hiveID] = upgradeInterruptReq{Target: target, By: by, At: now}
	u.mu.Unlock()
}

// pending reports whether an unexpired interrupt is armed for hiveID and
// the upgrade target it was requested for.
func (u *upgradeInterrupts) pending(hiveID string, now time.Time) (upgradeInterruptReq, bool) {
	if u == nil {
		return upgradeInterruptReq{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	req, ok := u.byID[hiveID]
	if !ok {
		return upgradeInterruptReq{}, false
	}
	if now.Sub(req.At) > upgradeInterruptTTL {
		delete(u.byID, hiveID)
		return upgradeInterruptReq{}, false
	}
	return req, true
}

// clear drops the request once the upgrade it belonged to is over.
func (u *upgradeInterrupts) clear(hiveID string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	delete(u.byID, hiveID)
	u.mu.Unlock()
}

// appliesTo reports whether an armed interrupt should ride along with an
// upgrade instruction for target: the request was for this target, or was
// made before the target was known (empty) and is still fresh.
func (r upgradeInterruptReq) appliesTo(target string) bool {
	return r.Target == "" || target == "" || sameCommit(r.Target, target) || r.Target == target
}

// upgradeInterruptRequested reads the interrupt flag off an upgrade request:
// `?interrupt=1` or a JSON body `{"interrupt":true}`.
func upgradeInterruptRequested(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("interrupt"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
