package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// assignmentInFlight reports whether a hive is mid-claim: it has been ASSIGNED
// to an owner but the spoke has not yet reported the project back
// (ClaimDelivered still false). During this window the placeholder is being
// wired up — org/repos/ACMM are being delivered to the spoke over successive
// heartbeats — and an auto-upgrade that rolls the spoke pod partway through can
// wedge it: the pod restarts onto a new image before the claim completes, the
// in-flight delivery is lost, and the slot dead-ends assigned-but-unclaimed.
// This was a root cause of the EPM wedge (task #94). triggerAutoUpgrades uses
// it as a LATCH to DEFER (never cancel) an auto-upgrade until the claim lands.
//
// SELF-LIMITING — this latch cannot hold forever. It releases the instant the
// claim completes (ClaimDelivered flips true; see saas.go where org/repos/
// primary match) OR the slot is returned to the pool. The slot cannot stay
// wedged either: sweepStuckAssignments auto-resets any placeholder still at
// Status=statusAssigned && !ClaimDelivered after assignStuckResetTimeout
// (measured from AssignedAt), which clears Status back to statusAvailable and
// so also clears this predicate. Every hive this returns true for is therefore
// on a bounded path to false.
func assignmentInFlight(h *SaaSHive) bool {
	return h.Status == statusAssigned && !h.ClaimDelivered
}

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

// hiveHasAssignedIdentity reports whether a placeholder has been handed a REAL
// tenant identity — a non-placeholder org or any primary_repo. A clean available
// slot carries only the synthetic "available-<id>" org (placeholderOrgPrefix) and
// an empty primary_repo, so it returns false. This is the exact mirror of the
// enrichFromSaaSMeta / dashboard AssignedUnclaimed gate, kept here so the handler
// and the self-heal sweep accept precisely the states the UI offers Reset for.
func hiveHasAssignedIdentity(h *SaaSHive) bool {
	hasRealOrg := h.Org != "" && !strings.HasPrefix(h.Org, placeholderOrgPrefix)
	return hasRealOrg || h.PrimaryRepo != ""
}

// isResettableWedge reports whether a placeholder is in the assigned-but-unclaimed
// wedge that Reset assignment targets: NOT a delivered claim (a delivered claim is
// a LIVE hive) AND NOT a clean available slot, but either explicitly
// Status=statusAssigned OR carrying a real assigned identity (org/primary_repo)
// under any/no status. It broadens the original Status==statusAssigned gate to
// also cover the null-Status-but-org-set legacy wedge (the live vllmd-13 class),
// while still refusing a delivered claim and a genuinely-clean available slot.
func isResettableWedge(h *SaaSHive) bool {
	if h.ClaimDelivered {
		return false
	}
	hasIdentity := hiveHasAssignedIdentity(h)
	isCleanAvailable := h.Status == statusAvailable && !hasIdentity
	if isCleanAvailable {
		return false
	}
	return h.Status == statusAssigned || hasIdentity
}

// handleResetAssignment returns an assigned-but-unclaimed placeholder to the
// AVAILABLE pool (admin-only). It is the escape hatch for a placeholder wedged
// between the two claim paths: handleApproveProvision requires a PENDING request
// (this one is already approved) and handleAssignHive requires statusAvailable
// (this one is statusAssigned), so an assignment whose spoke never reports the
// project back — ClaimDelivered stuck false — has no other way out.
//
// GUARD: it accepts a hive in the assigned-but-unclaimed wedge (isResettableWedge)
// — Status=statusAssigned OR a real org/primary_repo written under any/no status
// (the null-Status legacy wedge, e.g. vllmd-13), and always !ClaimDelivered. A
// delivered claim (ClaimDelivered==true) is a LIVE hive and is refused (409):
// resetting it would disrupt a working tenant. A genuinely-clean available slot
// (no real identity) is a no-op (409). Both refusals are the conservative choice —
// reset is strictly for the wedged middle state.
func (s *HubServer) handleResetAssignment(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	admin := s.getAuthUser(r) // requireAdmin-gated

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.ClaimDelivered {
		http.Error(w, `{"error":"hive claim already delivered — reset would disrupt a live hive; deprovision instead"}`, http.StatusConflict)
		return
	}
	// A clean available slot (statusAvailable with no real assigned identity) has
	// nothing to reset. A hive that is neither the wedge nor clean-available (e.g.
	// provisioning/error with no identity) is likewise not a reset target.
	if !isResettableWedge(h) {
		if h.Status == statusAvailable {
			http.Error(w, `{"error":"hive is already an available placeholder — nothing to reset"}`, http.StatusConflict)
			return
		}
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":                   true,
		"status":               statusAvailable,
		"id":                   hiveID,
		"reopened_request_for": reopened,
	})
}

// sweepStuckAssignments auto-resets any placeholder stuck in the
// assigned-but-unclaimed wedge (isResettableWedge && !ClaimDelivered) for longer
// than assignStuckResetTimeout, so an assigned-but-unclaimed slot can never
// dead-end silently. It is the automatic counterpart to handleResetAssignment and
// applies the identical guard and field transitions. Every reset is logged.
//
// AGE SOURCE. A proper assign stamps AssignedAt, so its age is the time since that
// stamp. The null-Status legacy wedge (real org/repo, Status=="", the vllmd-13
// class) typically has NO AssignedAt — its age is genuinely unknowable. Rather
// than reset it on a guessed age (which risks yanking a brand-new,
// legitimately-mid-assignment slot before its first heartbeat), the sweep uses
// CreatedAt as a conservative floor for such identity-bearing wedges: only a slot
// created longer ago than the timeout is auto-reset, and a fresh assign always
// carries an AssignedAt anyway so it takes the precise path. A wedge with neither
// stamp is left for the operator's manual Reset (still reachable via the UI/API).
// stuckAssignmentSweepInterval throttles sweepStuckAssignments. A wedge is
// only actionable once it is older than assignStuckResetTimeout (tens of
// minutes), so sweeping every 2-min poller tick buys nothing but a full fleet
// scan; a wedge is still reset well within one timeout of becoming eligible.
// 7 minutes is deliberately co-prime with the sibling throttled lanes so the
// sweeps drift apart instead of stacking onto the same poller tick.
const stuckAssignmentSweepInterval = 7 * time.Minute

// sweepStuckAssignmentsIfDue runs the stuck-assignment sweep only if at least
// stuckAssignmentSweepInterval has elapsed since the last run. Safe to call
// from the poller loop every tick; same throttle pattern and same guarding
// mutex as reconcileNetAdminIfDue.
func (s *HubServer) sweepStuckAssignmentsIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastStuckAssignmentSweep.IsZero() ||
		time.Since(s.lastStuckAssignmentSweep) >= stuckAssignmentSweepInterval
	if due {
		s.lastStuckAssignmentSweep = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.sweepStuckAssignments()
}

func (s *HubServer) sweepStuckAssignments() {
	now := time.Now()

	type reset struct {
		id, owner, org string
		age            time.Duration
		reopened       string
	}
	var done []reset

	for _, sh := range listSaaSHives() {
		shCopy := sh
		if !isResettableWedge(&shCopy) {
			continue
		}
		// Determine the wedge's age. Prefer AssignedAt (stamped by the current
		// assign paths); for a null-Status identity wedge that predates the stamp,
		// fall back to CreatedAt as a conservative floor. A wedge with neither stamp
		// has an unknowable age, so leave it for the operator's manual reset rather
		// than reset it on a guessed age.
		ageStamp := sh.AssignedAt
		if ageStamp == "" {
			ageStamp = sh.CreatedAt
		}
		if ageStamp == "" {
			continue
		}
		assignedAt, err := time.Parse(time.RFC3339, ageStamp)
		if err != nil {
			continue
		}
		age := now.Sub(assignedAt)
		if age < assignStuckResetTimeout {
			continue
		}

		h := loadSaaSHive(sh.ID) // reload to mutate the authoritative record
		if h == nil || !isResettableWedge(h) {
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
