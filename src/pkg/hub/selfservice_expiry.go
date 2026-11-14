package hub

import (
	"context"
	"strings"
	"time"
)

// Self-service (byo-) hive lifecycle: expiry sweep and the hub-assigned hive
// type (RFC #18060 section 5, child #18146).

const (
	// SelfServiceHiveType is the hub-assigned RegistryEntry.HiveType for every
	// byo- hive. The hub derives it from the ID prefix; a value the heartbeat
	// body claims is never consulted for this lane, so a self-hoster cannot
	// present their hive as "hosted" in Active Hives or the fleet views.
	SelfServiceHiveType = "self-hosted"

	// selfServiceSweepIntervalEnv sets how often the expiry sweep runs. The
	// window itself is HIVE_HUB_SELFSERVICE_EXPIRY (selfServiceExpiry).
	selfServiceSweepIntervalEnv     = "HIVE_HUB_SELFSERVICE_SWEEP_INTERVAL"
	selfServiceSweepIntervalDefault = 10 * time.Minute

	// selfServiceExpiredBy is the actor recorded on the "registry entry
	// removed" audit line when the sweep, not a user, removes an entry.
	selfServiceExpiredBy = "selfservice-expiry-sweep"

	// selfServiceNeverHeartbeated is the last_heartbeat audit value for a
	// registration that expired without one accepted heartbeat.
	selfServiceNeverHeartbeated = "never"
)

func selfServiceSweepInterval() time.Duration {
	return durationEnv(selfServiceSweepIntervalEnv, selfServiceSweepIntervalDefault)
}

// hubAssignedHiveType is the RegistryEntry.HiveType the heartbeat handler
// stores for hiveID. byo- hives always get SelfServiceHiveType regardless of
// what the payload claims; every other lane keeps the historical rule (a
// claimed type wins, hosted-/saas- IDs default to "hosted", the rest to
// "local").
func hubAssignedHiveType(hiveID, claimed string) string {
	if strings.HasPrefix(hiveID, SelfServiceHiveIDPrefix) {
		return SelfServiceHiveType
	}
	if claimed != "" {
		return sanitizeHeartbeatField(claimed)
	}
	if strings.HasPrefix(hiveID, "hosted-") || strings.HasPrefix(hiveID, "saas-") {
		return "hosted"
	}
	return "local"
}

// selfServiceLastSeen is the instant the expiry window counts from: the last
// accepted heartbeat, or the registration time while there has been none.
func selfServiceLastSeen(h SelfServiceHive) time.Time {
	if !h.LastAcceptedHeartbeatAt.IsZero() {
		return h.LastAcceptedHeartbeatAt
	}
	return h.CreatedAt
}

// selfServiceExpired reports whether h has gone longer than window without an
// accepted heartbeat as of now. A record with no timestamps at all (a hand
// edited store file) is never expired by the sweep; it has no window to count.
func selfServiceExpired(h SelfServiceHive, now time.Time, window time.Duration) bool {
	last := selfServiceLastSeen(h)
	if last.IsZero() {
		return false
	}
	return now.Sub(last) > window
}

// sweepExpiredSelfServiceHives removes every registration past the expiry
// window through the same purge path as the owner DELETE: revoke the key, drop
// the registry entry (s.activity.forget and saveRegistryNow happen inside
// removeRegistryEntry), then one audit line per hive. It returns the removed
// IDs. Nothing here reads or logs key material.
func (s *HubServer) sweepExpiredSelfServiceHives() []string {
	if s == nil || s.selfService == nil {
		return nil
	}
	now := s.selfService.clock().UTC()
	window := selfServiceExpiry()
	var removed []string
	for _, h := range s.selfService.list() {
		if !selfServiceExpired(h, now, window) {
			continue
		}
		if _, err := s.selfService.revoke(h.ID); err != nil {
			s.logger.Error("self-service expiry: store write failed", "hive", h.ID, "error", err)
			continue
		}
		s.removeRegistryEntry(h.ID, selfServiceExpiredBy)
		lastHeartbeat := selfServiceNeverHeartbeated
		if !h.LastAcceptedHeartbeatAt.IsZero() {
			lastHeartbeat = h.LastAcceptedHeartbeatAt.UTC().Format(time.RFC3339)
		}
		s.logger.Info("audit: selfservice_hive_expired", "id", h.ID, "owner", h.Owner, "last_heartbeat", lastHeartbeat, "expiry", window.String())
		removed = append(removed, h.ID)
	}
	return removed
}

// startSelfServiceExpirySweep runs sweepExpiredSelfServiceHives every
// selfServiceSweepInterval until ctx is cancelled. Started from
// StartBackgroundPollers beside the other hub pollers; a hub without a
// self-service store ticks harmlessly (the store's readers are nil-safe).
func (s *HubServer) startSelfServiceExpirySweep(ctx context.Context) {
	ticker := time.NewTicker(selfServiceSweepInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepExpiredSelfServiceHives()
		}
	}
}
