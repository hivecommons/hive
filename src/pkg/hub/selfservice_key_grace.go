package hub

// Hub registration protocol v1, rollout step 2: key rotation with a grace
// window (hivecommons/hive#19235, #19056).
//
// Rotation has always kept the previous key live for the fixed
// selfServiceKeyRotationOverlap. That is too short for an owner who redeploys
// a spoke by hand, and a second rotation inside the overlap made the previous
// key the one minted a moment earlier, so the key the spoke was still running
// stopped working at once: a rotation could cause an outage.
//
// With HIVE_HUB_SELFSERVICE_KEY_GRACE on:
//   - the old key keeps authenticating for HIVE_HUB_SELFSERVICE_KEY_GRACE_WINDOW
//     (default selfServiceKeyGraceWindowDefault) after a rotation;
//   - a rotation while a grace window is open keeps the old key (the one the
//     spoke still runs) and replaces the pending new key, whose hash is
//     dropped, and the window restarts from this rotation.
//
// Off (the default), rotation behaves exactly as before. Nothing here changes
// the stored record shape, so existing registrations keep working with no data
// change, and nothing here logs or returns a hash.

import (
	"os"
	"strings"
	"time"
)

const (
	// selfServiceKeyGraceEnv turns the configurable grace window on. Off by
	// default: a hub keeps the fixed overlap until its operator enables it.
	selfServiceKeyGraceEnv = "HIVE_HUB_SELFSERVICE_KEY_GRACE"
	// selfServiceKeyGraceWindowEnv is how long the old key keeps working after
	// a rotation while the grace flag is on.
	selfServiceKeyGraceWindowEnv = "HIVE_HUB_SELFSERVICE_KEY_GRACE_WINDOW"
	// selfServiceKeyGraceWindowDefault is the grace window when the env is
	// unset or invalid.
	selfServiceKeyGraceWindowDefault = time.Hour
	// selfServiceKeyGraceWindowCeiling clamps a configured window so a typo
	// cannot keep a rotated-away key alive for months.
	selfServiceKeyGraceWindowCeiling = 7 * 24 * time.Hour
)

// selfServiceKeyRotationPolicy is how one rotation treats the previous key.
type selfServiceKeyRotationPolicy struct {
	// window is how long the old key keeps authenticating after the rotation.
	window time.Duration
	// replacePending keeps a still-live old key across a second rotation and
	// replaces the pending key instead of making it the old one.
	replacePending bool
}

// selfServiceKeyGraceEnabled reads the flag per request, like the other
// self-service toggles.
func selfServiceKeyGraceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(selfServiceKeyGraceEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// currentSelfServiceKeyRotationPolicy is the policy rotation uses now: the
// legacy fixed overlap while the flag is off, the configured grace window
// otherwise.
func currentSelfServiceKeyRotationPolicy() selfServiceKeyRotationPolicy {
	if !selfServiceKeyGraceEnabled() {
		return selfServiceKeyRotationPolicy{window: selfServiceKeyRotationOverlap}
	}
	window := min(durationEnv(selfServiceKeyGraceWindowEnv, selfServiceKeyGraceWindowDefault), selfServiceKeyGraceWindowCeiling)
	return selfServiceKeyRotationPolicy{window: window, replacePending: true}
}

// selfServiceRotation describes a completed rotation for the audit line and
// the response. It carries no key material.
type selfServiceRotation struct {
	// replacedPending is true when the rotation replaced a pending key inside
	// an open grace window rather than retiring the current one.
	replacedPending bool
	// oldKeyExpiresAt is when the old key stops authenticating.
	oldKeyExpiresAt time.Time
}

// rotateWithPolicy mints a new key for id under policy and returns it ONCE.
// ifMatch follows update's rules; a mismatch returns errSelfServiceStale.
func (st *selfServiceStore) rotateWithPolicy(id, ifMatch string, policy selfServiceKeyRotationPolicy) (string, selfServiceRotation, error) {
	if st == nil {
		return "", selfServiceRotation{}, errSelfServiceNilStore
	}
	raw, hash, err := mintSelfServiceKey()
	if err != nil {
		return "", selfServiceRotation{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	h, ok := st.hives[id]
	if !ok {
		return "", selfServiceRotation{}, errSelfServiceNotFound
	}
	if !selfServiceETagMatches(*h, ifMatch) {
		return "", selfServiceRotation{}, errSelfServiceStale
	}
	now := st.clock().UTC()
	before := *h
	var info selfServiceRotation
	if policy.replacePending && h.PrevKeyHash != "" && now.Before(h.PrevKeyExpiresAt) {
		// The old key is still the one the spoke may be running: keep it and
		// drop the pending key, which was never deployed long enough to matter.
		info.replacedPending = true
	} else {
		h.PrevKeyHash = h.KeyHash
	}
	h.PrevKeyExpiresAt = now.Add(policy.window)
	h.KeyHash = hash
	h.Revision++
	if err := st.saveLocked(); err != nil {
		*h = before
		return "", selfServiceRotation{}, err
	}
	info.oldKeyExpiresAt = h.PrevKeyExpiresAt
	return raw, info, nil
}

// auditSelfServiceRotated writes the selfservice_hive_rotated line. It names
// the window and whether a pending key was replaced, never a key or a hash.
func (s *HubServer) auditSelfServiceRotated(login, hiveID string, policy selfServiceKeyRotationPolicy, rotation selfServiceRotation) {
	s.logger.Info("audit: selfservice_hive_rotated", "user", login, "hive", hiveID,
		"overlap", policy.window.String(), "grace", policy.replacePending,
		"replaced_pending", rotation.replacedPending,
		"old_key_expires_at", rotation.oldKeyExpiresAt.Format(time.RFC3339))
}
