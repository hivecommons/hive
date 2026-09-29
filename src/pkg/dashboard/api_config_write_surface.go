package dashboard

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// The write-surface allowlist editor (#9587 phase 2).
//
// write_surface.allowlist is a top-level Config field (not GovernorConfig). It
// maps a lane (agent) name to the relay operations that lane may ask the hive
// to perform; see docs/github-write-surface.md. Both endpoints are OWNER-ONLY:
// the allowlist decides what agents may write to GitHub.
//
// The editor can only narrow what agents do. An empty allowlist (the default)
// restricts nothing, and a lane with no entry stays unrestricted, so nothing
// here can reduce what a hive does until an operator lists a lane.

// writeSurfaceSectionResponse is the GET/PUT response body.
type writeSurfaceSectionResponse struct {
	// Allowlist is the current allowlist, lane -> operations. Always an
	// object (never null); an empty object means nothing is restricted.
	Allowlist map[string][]string `json:"allowlist"`
	// Ops is the operation vocabulary the editor offers, plus "*".
	Ops []string `json:"ops"`
	// Warnings lists entries that cannot do what they say: an unknown
	// operation (possible in a hand-edited hive.yaml) or a lane naming no
	// configured agent.
	Warnings []string `json:"warnings"`
}

// handleWriteSurfaceGet returns the lane write allowlist.
func (s *Server) handleWriteSurfaceGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, writeSurfaceSection(s.deps.Config))
}

// handleWriteSurfacePut replaces the lane write allowlist. The body must carry
// the whole allowlist ({"allowlist": {...}}); send {"allowlist": {}} to clear
// it. A missing key is refused rather than read as "clear", so a malformed
// client cannot drop every restriction by accident. Validation happens before
// anything is changed: an invalid body leaves the live allowlist untouched.
func (s *Server) handleWriteSurfacePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Allowlist *map[string][]string `json:"allowlist"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Allowlist == nil {
		jsonError(w, `allowlist is required; send {"allowlist": {}} to clear it`, http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	next, err := config.NormalizeWriteSurfaceAllowlist(*body.Allowlist)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	previous := len(cfg.WriteSurfaceAllowlist())
	cfg.SetWriteSurfaceAllowlist(next)
	if err := s.saveConfig(); err != nil && s.logger != nil {
		s.logger.Error("failed to persist config after write surface allowlist update", "error", err)
	}
	s.auditFromRequest(r, "config_write_surface", auditDetail(
		"section", "write_surface",
		"lanes", strconv.Itoa(len(next)),
		"previous_lanes", strconv.Itoa(previous),
		"restricted", strings.Join(sortedLanes(next), "+"),
	), "")
	if s.logger != nil {
		s.logger.Info("write surface allowlist updated via runtime settings",
			"lanes", len(next), "previous_lanes", previous)
	}
	jsonResponse(w, writeSurfaceSection(cfg))
}

// writeSurfaceSection renders the allowlist for the dashboard.
func writeSurfaceSection(cfg *config.Config) writeSurfaceSectionResponse {
	allowlist := cfg.WriteSurfaceAllowlist()
	if allowlist == nil {
		allowlist = map[string][]string{}
	}
	warnings := config.WriteSurfaceWarnings(cfg)
	for _, lane := range sortedLanes(allowlist) {
		if !writeSurfaceLaneConfigured(cfg, lane) {
			warnings = append(warnings, "write_surface.allowlist."+lane+": no agent named "+
				strconv.Quote(lane)+" is configured, so this entry has no effect until one is")
		}
	}
	if warnings == nil {
		warnings = []string{}
	}
	ops := append(append([]string{}, config.KnownWriteOps...), config.WriteSurfaceAllowAll)
	return writeSurfaceSectionResponse{Allowlist: allowlist, Ops: ops, Warnings: warnings}
}

// replicaSuffixPattern matches the "-N" suffix of a replica name ("scanner-2").
var replicaSuffixPattern = regexp.MustCompile(`-[0-9]+$`)

// writeSurfaceLaneConfigured reports whether lane names a configured agent,
// or a replica of one (whose allowlist falls back to its base agent's).
func writeSurfaceLaneConfigured(cfg *config.Config, lane string) bool {
	if cfg == nil {
		return false
	}
	if _, ok := cfg.Agents[lane]; ok {
		return true
	}
	if base := replicaSuffixPattern.ReplaceAllString(lane, ""); base != lane {
		_, ok := cfg.Agents[base]
		return ok
	}
	return false
}

// sortedLanes returns the allowlist's lane names in sorted order.
func sortedLanes(allowlist map[string][]string) []string {
	lanes := make([]string, 0, len(allowlist))
	for lane := range allowlist {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	return lanes
}
