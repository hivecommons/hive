package dashboard

import (
	"net/http"
	"strings"
)

// Merge-hotspot advisor surfaces (pkg/hotspot, wired by cmd/hive): the
// per-file report on GET /api/hotspots and the one-line count block on
// /api/status. The dashboard holds the store behind a small interface so
// it does not import pkg/hotspot (import ratchet).

// HotspotSource is what cmd/hive installs: the cached analysis, its counts
// and the conflict intake the rebase follow-through feeds.
type HotspotSource interface {
	// HotspotsJSON returns the cached hotspots (all repos when repo is "")
	// as a JSON-encodable slice, highest score first.
	HotspotsJSON(repo string) any
	// HotspotCounts summarises the cache: files at or above the thresholds,
	// those with a split recommendation, and those with an issue filed.
	HotspotCounts() (total, actionable, filed int)
	// ObserveConflict records a rebase/merge conflict blamed on files.
	ObserveConflict(repo string, number int, files []string)
}

// HotspotStatus is the /api/status `hotspots` block.
type HotspotStatus struct {
	Count      int `json:"count"`
	Actionable int `json:"actionable"`
	Filed      int `json:"filed"`
}

// SetHotspotSource installs the advisor's store; nil removes it.
func (s *Server) SetHotspotSource(src HotspotSource) {
	if s == nil {
		return
	}
	s.hotspotMu.Lock()
	s.hotspotSource = src
	s.hotspotMu.Unlock()
}

func (s *Server) hotspotSourceRef() HotspotSource {
	if s == nil {
		return nil
	}
	s.hotspotMu.RLock()
	defer s.hotspotMu.RUnlock()
	return s.hotspotSource
}

// HotspotStatusBlock is nil when the advisor is not wired.
func (s *Server) HotspotStatusBlock() *HotspotStatus {
	src := s.hotspotSourceRef()
	if src == nil {
		return nil
	}
	total, actionable, filed := src.HotspotCounts()
	return &HotspotStatus{Count: total, Actionable: actionable, Filed: filed}
}

// handleHotspots serves GET /api/hotspots?repo=owner/name: files repeatedly
// touched by concurrent PRs in the rolling window, with per-file PR count,
// concurrent-pair overlaps, attributed conflicts, line count, the
// recommendation and the modularize issue if one was filed.
func (s *Server) handleHotspots(w http.ResponseWriter, r *http.Request) {
	src := s.hotspotSourceRef()
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	resp := map[string]any{"repo": repo, "hotspots": []any{}}
	if src != nil {
		if hs := src.HotspotsJSON(repo); hs != nil {
			resp["hotspots"] = hs
		}
		total, actionable, filed := src.HotspotCounts()
		resp["count"], resp["actionable"], resp["filed"] = total, actionable, filed
	}
	jsonResponse(w, resp)
}
