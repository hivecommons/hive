package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/timeline"
)

const runRetiredAbandoned = "abandoned"

type runAbandonRequest struct {
	Reason string `json:"reason"`
}

// retireRun atomically removes all stage leases for the run and records a
// non-expiring admission tombstone. A stale owner snapshot cannot retire a
// newer generation, and a failed write leaves the old leases usable.
func (h *ContributeWSHub) retireRun(held taskLease, disposition string) error {
	h.leaseMu.Lock()
	var released []*taskLease
	defer func() {
		h.leaseMu.Unlock()
		for _, lease := range released {
			h.releaseClaimForLease(lease.identity, leaseClaimKey(lease), "run retired")
		}
	}()
	cur := h.leaseForLocked(held.identity, held.taskID)
	if cur == nil {
		return errLeaseNotFound
	}
	if cur.gen != held.gen {
		return errLeaseGenStale
	}
	key := runKeyOfLease(leaseWorkKey(cur), cur.repo)
	removed := make(map[string]*taskLease)
	for k, l := range h.leases {
		if l != nil && l.stage != "" && runKeyOfLease(leaseWorkKey(l), l.repo) == key {
			removed[k] = l
			delete(h.leases, k)
		}
	}
	if h.retiredRuns == nil {
		h.retiredRuns = make(map[string]string)
	}
	previous, existed := h.retiredRuns[key]
	h.retiredRuns[key] = disposition
	if err := h.saveLeasesLocked(); err != nil {
		for k, l := range removed {
			h.leases[k] = l
		}
		if existed {
			h.retiredRuns[key] = previous
		} else {
			delete(h.retiredRuns, key)
		}
		return fmt.Errorf("persisting run retirement: %w", err)
	}
	for _, lease := range removed {
		released = append(released, lease)
	}
	return nil
}

func (s *Server) runRetirement(key string) string {
	if s == nil || s.contributeHub == nil {
		return ""
	}
	h := s.contributeHub
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	return h.retiredRuns[key]
}

// RunAbandoned is used by automatic admission to avoid converting a terminal
// owner decision back into a fresh run (or falling through to direct-fix work).
func (s *Server) RunAbandoned(key string) bool {
	return s.runRetirement(key) == runRetiredAbandoned
}

func (s *Server) handleRunAbandon(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if decoded, err := url.PathUnescape(key); err == nil {
		key = strings.TrimSpace(decoded)
	}
	if key == "" {
		jsonError(w, "run key required", http.StatusBadRequest)
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "run lease registry unavailable", http.StatusServiceUnavailable)
		return
	}
	var body runAbandonRequest
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}
	held, ok := s.contributeHub.runLeaseHolder(key, time.Now())
	if !ok {
		jsonError(w, "run not found", http.StatusNotFound)
		return
	}
	if err := s.contributeHub.retireRun(held, runRetiredAbandoned); err != nil {
		jsonError(w, err.Error(), runResetErrorStatus(err))
		return
	}
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: s.canonicalRunKey(held.repo, held.number, runKeyOfLease(leaseWorkKey(&held), held.repo), held.runKey()), Kind: timeline.KindStageCompleted, Agent: held.identity,
		Attrs: map[string]string{"stage_from": held.stage, "stage_to": runRetiredAbandoned,
			"gen": strconv.FormatUint(held.gen, 10), "reason": body.Reason, "abandoned": "true"},
	})
	s.auditFromRequest(r, "run_abandoned", auditDetail("run", key, "stage_from", held.stage, "reason", body.Reason), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "key": key, "state": runRetiredAbandoned, "reason": body.Reason})
}
