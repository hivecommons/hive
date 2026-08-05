package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// assignStuckResetTimeout is how long a placeholder may sit at
// Status=statusAssigned && !ClaimDelivered — measured from AssignedAt — before
// the self-heal sweep returns it to the available pool automatically. A claim
// normally completes within a heartbeat or two (tens of seconds); anything
// still unclaimed after this window has a spoke that will never report the
// project back (the classic case: a heartbeat-only cluster whose spoke is
// frozen on an old image), so the slot is a dead-end until it is reset.
//
// 15 minutes is deliberately far longer than a healthy claim takes, so a merely
// slow-to-reconcile spoke is never reset out from under a claim in progress.
const assignStuckResetTimeout = 15 * time.Minute

// syncRegistryProvStatus updates the in-memory registry entry's ProvStatus for
// hiveID under the registry lock, and requests a save. The dashboard's My Hives
// enrichment re-copies ProvStatus from meta.json on every read, but mirroring it
// here means the change is visible on the very next render without waiting for
// that pass — the same immediacy the assign paths rely on.
func (s *HubServer) syncRegistryProvStatus(hiveID, provStatus string) {
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].ProvStatus = provStatus
			break
		}
	}
	s.mu.Unlock()
	s.requestSave()
}

// availablePlaceholderProjectName builds the display name an unclaimed
// placeholder carries in My Hives — "Available slot (private) <id>" for a
// private pool slot, "Available slot <id>" otherwise. It mirrors the shape a
// freshly seeded pool slot shows so a reset row is indistinguishable from one
// that was never claimed.
func availablePlaceholderProjectName(h *SaaSHive) string {
	if h.IsPublic {
		return "Available slot " + h.ID
	}
	return "Available slot (private) " + h.ID
}

// resetHiveToAvailablePlaceholder rewrites a claimed-but-undelivered placeholder
// back to the AVAILABLE-placeholder shape — the exact inverse of what the assign
// paths (handleApproveProvision / handleAssignHive) wrote — so the slot can be
// re-armed and re-assigned cleanly. It mutates h in place; the caller persists.
//
// The transitions mirror an unclaimed pool slot: admin-owned, statusAvailable,
// an "available-"-prefixed org (the prefix isPlaceholderEntry/drift/fleet-stats
// all key on), empty repos/primary, the placeholder default ACMM level, and the
// claim/level/forge handshakes re-armed to false so a future assignment delivers
// cleanly. ClaimDelivered is asserted false (a delivered claim must never reach
// here — the endpoint and sweep both guard on it).
func resetHiveToAvailablePlaceholder(h *SaaSHive) {
	h.Status = statusAvailable
	h.Owner = hubAdminUsername
	// The "available-" org prefix is the reliable placeholder signal
	// (placeholderOrgPrefix); derive it from the immutable hive ID so the slot
	// reads as available inventory again. The prior tenant's real org is dropped
	// on purpose — that is the whole point of returning the slot to the pool.
	h.Org = placeholderOrgPrefix + h.ID
	h.ProjectName = availablePlaceholderProjectName(h)
	h.Repos = nil
	h.PrimaryRepo = ""
	// Back to the placeholder default level — the level an unclaimed slot shows
	// and the level the assign paths fall back to when a claim omits one.
	h.ACMMLevel = defaultAssignACMMLevel
	h.RequestedACMMLevel = 0
	// Re-arm every delivery handshake so a subsequent assignment pushes its
	// payload rather than adopting the reset slot's stale state.
	h.ClaimDelivered = false
	h.ACMMDelivered = false
	h.ForgeDelivered = false
	h.RequestedGitHubHost = ""
	// Drop the claimed-project markers so read paths stop advertising the prior
	// tenant's vanity host and error.
	h.VanityURL = ""
	h.Error = ""
	// No longer in the assigned state, so the assigned-at stamp no longer applies.
	h.AssignedAt = ""
}

// clearAssignedRequestForHive finds a provision request whose AssignedHive points
// at hiveID and returns it to pending so the admin can cleanly re-approve after a
// reset — without it the request would be orphaned in the "approved" state while
// its hive is back in the pool, and re-approve (which requires a PENDING request)
// would 404. Returns the affected username, or "" if no request was linked.
func clearAssignedRequestForHive(hiveID string) string {
	for _, pr := range listProvisionRequests() {
		if pr.AssignedHive != hiveID {
			continue
		}
		// Only a request the assignment actually fulfilled (approved) should be
		// re-opened; a denied/pending record that merely names the hive is left
		// alone.
		if pr.Status != provisionStatusApproved {
			continue
		}
		reopened := pr // copy — loadProvisionRequest returns a fresh value per call
		reopened.Status = provisionStatusPending
		reopened.AssignedHive = ""
		reopened.DecidedBy = ""
		reopened.DecidedAt = ""
		if err := saveProvisionRequest(&reopened); err != nil {
			// Best effort: report the linkage even if the flip failed, so the
			// operator knows a manual re-open is needed.
			return reopened.Username
		}
		return reopened.Username
	}
	return ""
}

// handleResetAssignment returns an assigned-but-unclaimed placeholder to the
// AVAILABLE pool (admin-only). It is the escape hatch for a placeholder wedged
// between the two claim paths: handleApproveProvision requires a PENDING request
// (this one is already approved) and handleAssignHive requires statusAvailable
// (this one is statusAssigned), so an assignment whose spoke never reports the
// project back — ClaimDelivered stuck false — has no other way out.
//
// GUARD: it only ever touches a hive at Status=statusAssigned && !ClaimDelivered.
// A delivered claim (ClaimDelivered==true) is a LIVE hive and is refused (409):
// resetting it would disrupt a working tenant. An already-available slot is a
// no-op (409). Both are the conservative choice — reset is strictly for the
// wedged middle state.
func (s *HubServer) handleResetAssignment(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	admin := s.getAuthUser(r) // requireAdmin-gated

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Status == statusAvailable {
		http.Error(w, `{"error":"hive is already an available placeholder — nothing to reset"}`, http.StatusConflict)
		return
	}
	if h.ClaimDelivered {
		http.Error(w, `{"error":"hive claim already delivered — reset would disrupt a live hive; deprovision instead"}`, http.StatusConflict)
		return
	}
	if h.Status != statusAssigned {
		http.Error(w, `{"error":"only an assigned-but-unclaimed placeholder can be reset"}`, http.StatusConflict)
		return
	}

	prevOwner := h.Owner
	prevOrg := h.Org
	resetHiveToAvailablePlaceholder(h)
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to reset placeholder assignment"}`, http.StatusInternalServerError)
		return
	}
	// Mirror the reset into the in-memory registry so the dashboard reflects the
	// available slot immediately, not only after the next enrich pass.
	s.syncRegistryProvStatus(hiveID, statusAvailable)

	// Re-open any provision request this assignment fulfilled so re-approve works
	// end to end (re-approve requires a PENDING request).
	reopened := clearAssignedRequestForHive(hiveID)

	s.logger.Info("audit: assignment reset — placeholder returned to available pool",
		"hive_id", hiveID,
		"by", admin,
		"previous_owner", prevOwner,
		"previous_org", prevOrg,
		"reopened_request_for", reopened,
	)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("assignment reset by %s — slot returned to the available pool (was %s)", admin, prevOwner),
		admin)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                   true,
		"status":               statusAvailable,
		"id":                   hiveID,
		"reopened_request_for": reopened,
	})
}

// sweepStuckAssignments auto-resets any placeholder stuck at
// Status=statusAssigned && !ClaimDelivered for longer than
// assignStuckResetTimeout, so an assigned-but-unclaimed slot can never dead-end
// silently. It is the automatic counterpart to handleResetAssignment and applies
// the identical guard and field transitions. Every reset is logged.
func (s *HubServer) sweepStuckAssignments() {
	now := time.Now()

	type reset struct {
		id, owner, org string
		age            time.Duration
		reopened       string
	}
	var done []reset

	for _, sh := range listSaaSHives() {
		if sh.Status != statusAssigned || sh.ClaimDelivered {
			continue
		}
		// A hive with no AssignedAt stamp predates the stamp (or was written by an
		// older path); its age is unknowable, so leave it for the operator's manual
		// reset rather than reset it on a guessed age.
		if sh.AssignedAt == "" {
			continue
		}
		assignedAt, err := time.Parse(time.RFC3339, sh.AssignedAt)
		if err != nil {
			continue
		}
		age := now.Sub(assignedAt)
		if age < assignStuckResetTimeout {
			continue
		}

		h := loadSaaSHive(sh.ID) // reload to mutate the authoritative record
		if h == nil || h.Status != statusAssigned || h.ClaimDelivered {
			continue
		}
		prevOwner := h.Owner
		prevOrg := h.Org
		resetHiveToAvailablePlaceholder(h)
		if err := saveSaaSHive(h); err != nil {
			s.logger.Warn("stuck-assignment sweep: failed to reset placeholder",
				"hive_id", h.ID, "error", err)
			continue
		}
		s.syncRegistryProvStatus(h.ID, statusAvailable)
		reopened := clearAssignedRequestForHive(h.ID)
		done = append(done, reset{id: h.ID, owner: prevOwner, org: prevOrg, age: age, reopened: reopened})
	}

	for _, c := range done {
		s.logger.Warn("stuck-assignment sweep: auto-reset a placeholder that never completed its claim",
			"hive_id", c.id,
			"previous_owner", c.owner,
			"previous_org", c.org,
			"stuck_for", roundedDuration(c.age),
			"reopened_request_for", c.reopened,
		)
		s.recordTimeline(c.id, TimelineOwnership,
			"assignment reset automatically after "+roundedDuration(c.age)+
				" unclaimed — slot returned to the available pool (was "+c.owner+")",
			"stuck-assignment-sweep")
	}
}
