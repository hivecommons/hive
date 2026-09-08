package hub

import "time"

// switchResendInterval is the minimum gap between two deliveries of the SAME
// switch tag to one hive. It is the stale-upgrade window on purpose: an
// upgrade that has not landed in staleUpgradeTimeout is already treated as
// stale everywhere else in the hub, so that is also the point at which a
// switch is worth re-sending in case the spoke's PATCH failed.
//
// Why a gap at all: the hub keeps the tag armed until the spoke REPORTS the
// new image, and it is the old pod that keeps reporting — on the old image —
// while the new pod pulls and initializes. Re-sending on every beat had the
// old pod re-patch the Deployment each time; even with the image unchanged the
// fresh restart-at annotation is a new pod template, so the ReplicaSet
// controller killed the initializing pod and started over. A spoke whose pod
// needs longer than one heartbeat interval to become Ready never converged
// (a-ks-wec2 n0sv, 2026-09-08: 18 ReplicaSets in 8 minutes, none Ready).
// SwitchImageSelf is now idempotent too, but that only helps once the spoke
// runs a build that has it; this gap protects the fleet that does not yet.
const switchResendInterval = staleUpgradeTimeout

// switchSend is one delivery of a switch tag to a hive.
type switchSend struct {
	Tag string
	At  time.Time
}

// switchRecentlySent reports whether tag was already delivered to hiveID
// within switchResendInterval of now. A different tag never counts as
// recent: a new switch is always delivered immediately.
func (s *HubServer) switchRecentlySent(hiveID, tag string, now time.Time) (switchSend, bool) {
	s.mu.RLock()
	sent, ok := s.heartbeatSwitchSent[hiveID]
	s.mu.RUnlock()
	if !ok || sent.Tag != tag {
		return switchSend{}, false
	}
	return sent, now.Sub(sent.At) < switchResendInterval
}
