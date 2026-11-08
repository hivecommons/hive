package github

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HeldRunApprovals is the process-wide record of the contributor lane's
// passes over GitHub's "approve and run workflows" gate: one entry per PR,
// for the head the lane last tried to approve runs on.
//
// It lives on the long-lived Client because the automerge sweeps run on a
// one-shot engine per tick (automerge.SweepContributorLaneAutoMerges): a
// once-per-head map on the engine was empty again on the next tick, so a
// head whose approvals GitHub refused with 403 was POSTed every sweep —
// 3 runs × 2 PRs every ~80 s on projectbluefin, ~270 charged calls an hour
// that could never succeed.
//
// A pass is taken once per head. A refusal is remembered with the runs
// GitHub refused and the App's `actions` permission at the time, so the
// lane keeps reporting the blocker without calling GitHub again until the
// head changes or the installation gains `actions: write`.
type HeldRunApprovals struct {
	mu      sync.Mutex
	entries map[string]heldRunApprovalEntry
}

type heldRunApprovalEntry struct {
	head        string
	refused     bool
	refusedRuns []int64
	grant       string
	grantKnown  bool
	seen        time.Time
}

// heldRunApprovalTTL bounds the ledger: an entry no pass has touched for
// this long belongs to a merged, closed or superseded PR.
const heldRunApprovalTTL = 24 * time.Hour

func heldRunApprovalKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// lookupLocked returns the PR's entry when it covers head. An entry for
// another head is left for the next write (Claim) to replace, so a read
// with a stale head never wipes the current one.
func (l *HeldRunApprovals) lookupLocked(key, head string, now time.Time) (heldRunApprovalEntry, bool) {
	if l.entries == nil {
		l.entries = map[string]heldRunApprovalEntry{}
	}
	for other, entry := range l.entries {
		if now.Sub(entry.seen) >= heldRunApprovalTTL {
			delete(l.entries, other)
		}
	}
	entry, ok := l.entries[key]
	if !ok {
		return heldRunApprovalEntry{}, false
	}
	if !strings.EqualFold(entry.head, head) {
		return heldRunApprovalEntry{}, false
	}
	entry.seen = now
	l.entries[key] = entry
	return entry, true
}

// Attempted reports whether a pass already ran on this PR head.
func (l *HeldRunApprovals) Attempted(repo string, number int, head string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.lookupLocked(heldRunApprovalKey(repo, number), head, time.Now())
	return ok
}

// Claim takes the head's one pass and reports whether it is the first. A
// new head replaces the PR's entry, so a push re-arms the pass.
func (l *HeldRunApprovals) Claim(repo string, number int, head string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	key := heldRunApprovalKey(repo, number)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.lookupLocked(key, head, now); ok {
		return false
	}
	l.entries[key] = heldRunApprovalEntry{head: head, seen: now}
	return true
}

// RecordRefusal notes that GitHub refused the approvals on this head and
// what the App's `actions` permission read at the time (grantKnown false
// when it could not be read).
func (l *HeldRunApprovals) RecordRefusal(repo string, number int, head string, runIDs []int64, grant string, grantKnown bool) {
	if l == nil {
		return
	}
	key := heldRunApprovalKey(repo, number)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, _ := l.lookupLocked(key, head, time.Now())
	entry.head, entry.seen = head, time.Now()
	entry.refused = true
	entry.refusedRuns = append([]int64(nil), runIDs...)
	entry.grant, entry.grantKnown = grant, grantKnown
	l.entries[key] = entry
}

// Refused reports whether GitHub refused the approvals on this head, and
// which runs it refused.
func (l *HeldRunApprovals) Refused(repo string, number int, head string) ([]int64, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.lookupLocked(heldRunApprovalKey(repo, number), head, time.Now())
	if !ok || !entry.refused {
		return nil, false
	}
	return append([]int64(nil), entry.refusedRuns...), true
}

// ReleaseOnGrant gives a refused head its pass back when the installation
// now grants `actions: write` and did not (or could not be read) when
// GitHub refused. It reports whether it released; the caller then makes
// one more pass, and a second refusal is recorded with the new grant.
func (l *HeldRunApprovals) ReleaseOnGrant(repo string, number int, head, grant string, grantKnown bool) bool {
	if l == nil || !grantKnown || !permissionAllowsWrite(grant) {
		return false
	}
	key := heldRunApprovalKey(repo, number)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.lookupLocked(key, head, time.Now())
	if !ok || !entry.refused || (entry.grantKnown && permissionAllowsWrite(entry.grant)) {
		return false
	}
	delete(l.entries, key)
	return true
}

func permissionAllowsWrite(level string) bool {
	level = strings.ToLower(strings.TrimSpace(level))
	return level == "write" || level == "admin"
}

// HeldRunApprovals returns the Client's process-wide held-run ledger.
func (c *Client) HeldRunApprovals() *HeldRunApprovals {
	if c == nil {
		return nil
	}
	return &c.heldRunApprovals
}

// installationPermissionCache holds the installation's granted permission
// map for workflowsGrantTTL.
type installationPermissionCache struct {
	mu    sync.Mutex
	at    time.Time
	perms map[string]string
}

// InstallationPermission returns the level ("read", "write", "admin" or
// "" for none) the App installation grants for permission name, read at
// most once per workflowsGrantTTL. known is false for a token-authenticated
// client or a failed read.
func (c *Client) InstallationPermission(ctx context.Context, name string) (level string, known bool) {
	if c == nil || c.appAuth == nil {
		return "", false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	cache := &c.installationPerms
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !cache.at.IsZero() && time.Since(cache.at) < workflowsGrantTTL {
		return cache.perms[name], true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := installationProbe(ctx, c.appAuth)
	if err != nil || info == nil {
		return "", false
	}
	cache.at, cache.perms = time.Now(), info.Permissions
	return cache.perms[name], true
}
