package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// journeyBannerFor decides whether this hive is due a journey nudge right now
// and, if so, returns the banner to deliver on the heartbeat response.
//
// It returns nil when: the hive is snoozed by an admin, no stage is
// outstanding, the required signal was never reported, or the nudge was
// already sent recently enough that re-sending would be spam.
//
// Callers must NOT hold hubBannersMu.
func (s *HubServer) journeyBannerFor(hiveID string, now time.Time) *HubBanner {
	if s == nil || s.journey == nil {
		return nil
	}

	entry := s.registryEntryCopy(hiveID)
	if entry == nil {
		return nil
	}

	st := s.journey.get(hiveID)
	// An admin exemption suppresses every journey nudge for this hive. Admin
	// banners sent by hand are unaffected — this only gates journey traffic.
	if st.snoozeActive(now) {
		return nil
	}

	n := nextNudge(entry, now)
	if !shouldSend(n, st, now) {
		return nil
	}
	// Defence in depth for the product's hardest rule: ACMM is a suggestion,
	// never a threat. Even if a future edit to nextNudge got this wrong, no
	// de-provision warning can leave the hub on an ACMM nudge.
	if n.Stage == StageACMMLevel && n.Severity == SeverityDeprovisionWarning {
		s.logger.Error("journey: refusing to send a de-provision warning for ACMM level",
			"hive_id", hiveID)
		return nil
	}

	s.journey.recordSent(hiveID, n, entry.ACMMLevel, now)
	if err := s.journey.save(); err != nil {
		// A failed save means the nudge may repeat after a restart. That is
		// strictly better than dropping the nudge, so continue.
		s.logger.Error("failed to persist journey nudge state", "error", err, "hive_id", hiveID)
	}

	s.logger.Info("journey: sending nudge",
		"hive_id", hiveID,
		"stage", n.Stage.String(),
		"severity", n.Severity.String(),
		"stalled_for", n.StalledFor.String(),
	)

	return &HubBanner{
		ID:      journeyBannerID(n, entry.ACMMLevel),
		Message: n.Message,
		Color:   n.Severity.bannerColor(),
	}
}

// registryEntryCopy returns a copy of one registry entry under the registry
// lock, so journey computation never races with a concurrent heartbeat write.
func (s *HubServer) registryEntryCopy(hiveID string) *RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			cp := s.registry.Hives[i]
			return &cp
		}
	}
	return nil
}

// attachJourneyStatus fills in the Journey field on each entry of a hive slice
// so the My Hives table can show where every hive is stalled. Computed on read
// — journey status is derived data and is never persisted on the registry.
func (s *HubServer) attachJourneyStatus(hives []RegistryEntry, now time.Time) {
	if s == nil || s.journey == nil {
		return
	}
	for i := range hives {
		st := s.journey.get(hives[i].ID)
		status := JourneyStatusFor(&hives[i], st, now)
		hives[i].Journey = &status
	}
}

// ── Admin snooze API ────────────────────────────────────────────────────────

// handleJourneySnooze sets or clears an admin exemption for one hive. Some
// hives are legitimately exempt (vendor pilots, demo spokes, hives parked
// between events) and must not be nagged or threatened.
//
// POST /api/saas/admin/journey-snooze
//
//	{"hive_id": "...", "duration": "720h", "reason": "vendor pilot"}
//
// An empty or zero duration clears the snooze.
func (s *HubServer) handleJourneySnooze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HiveID   string `json:"hive_id"`
		Duration string `json:"duration"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	hiveID := strings.TrimSpace(body.HiveID)
	if hiveID == "" {
		http.Error(w, `{"error":"hive_id is required"}`, http.StatusBadRequest)
		return
	}
	// maxSnoozeReasonLen keeps an audit note readable and bounds what is
	// written to the PVC; the reason is operator-facing prose, not data.
	const maxSnoozeReasonLen = 200
	reason := strings.TrimSpace(body.Reason)
	if runes := []rune(reason); len(runes) > maxSnoozeReasonLen {
		reason = string(runes[:maxSnoozeReasonLen])
	}

	now := time.Now()
	until, err := parseSnoozeUntil(body.Duration, now)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	username := s.getAuthUser(r)
	s.journey.setSnooze(hiveID, until, username, reason)
	if err := s.journey.save(); err != nil {
		s.logger.Error("failed to persist journey snooze", "error", err, "hive_id", hiveID)
		writeJSONError(w, http.StatusInternalServerError, "failed to persist snooze")
		return
	}

	if until.IsZero() {
		s.logger.Info("journey: snooze cleared", "hive_id", hiveID, "by", username)
	} else {
		s.logger.Info("journey: snooze set",
			"hive_id", hiveID, "until", until.UTC().Format(time.RFC3339),
			"reason", reason, "by", username)
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"ok": true, "hive_id": hiveID}
	if until.IsZero() {
		resp["snoozed"] = false
	} else {
		resp["snoozed"] = true
		resp["snoozed_until"] = until.UTC().Format(time.RFC3339)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleJourneyStatus returns the journey status of every hive, for the admin
// view of who is stalled where.
func (s *HubServer) handleJourneyStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	s.mu.RLock()
	hives := make([]RegistryEntry, len(s.registry.Hives))
	copy(hives, s.registry.Hives)
	s.mu.RUnlock()

	s.attachJourneyStatus(hives, now)

	type row struct {
		HiveID string         `json:"hive_id"`
		Name   string         `json:"name"`
		Owner  string         `json:"owner,omitempty"`
		Status *JourneyStatus `json:"status"`
	}
	rows := make([]row, 0, len(hives))
	for i := range hives {
		rows = append(rows, row{
			HiveID: hives[i].ID,
			Name:   hives[i].Name,
			Owner:  hives[i].Owner,
			Status: hives[i].Journey,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"hives": rows})
}

// writeJSONError emits a JSON error body with the given status code.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
