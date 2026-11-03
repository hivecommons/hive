package hub

import "github.com/hivecommons/hive/pkg/hub/spoke"

// heartbeatUpgradeAnnounceInput is what decides whether a heartbeat
// response tells the spoke its pod is about to be rolled by the hub
// (#15788). Pure inputs so the rule is testable without a hub.
type heartbeatUpgradeAnnounceInput struct {
	// HubManaged: the hub rolls this pod itself (kubectl), so the spoke's
	// own self-upgrade machinery never opens its drain latch for it.
	HubManaged bool
	// LatchOpen/Target mirror the registry entry's Upgrading/UpgradeTarget.
	LatchOpen bool
	Target    string
	// Current is the SHA the spoke reported in this beat.
	Current string
	// SpokeReportedUpgrading: the spoke already knows (self-upgrade), so
	// there is nothing to announce.
	SpokeReportedUpgrading bool
	// Restart: this response carries RestartSpoke.
	Restart bool
}

// heartbeatUpgradeAnnounce returns the announce the hub should attach to a
// heartbeat response, or nil. A hub-managed hive whose latch is open onto a
// target it is not yet running is about to be (or is being) rolled; a
// pending restart is a roll with no new build. Both show the spoke
// dashboard's upgrade animation before the kill rather than after the SSE
// stream drops.
func heartbeatUpgradeAnnounce(in heartbeatUpgradeAnnounceInput) *spoke.UpgradeAnnounce {
	if in.Restart {
		return &spoke.UpgradeAnnounce{Source: spoke.UpgradeSourceHub, Reason: "hub restart"}
	}
	if !in.HubManaged || !in.LatchOpen || in.SpokeReportedUpgrading {
		return nil
	}
	if in.Target == "" || (!isReleaseChannel(in.Target) && sameCommit(in.Current, in.Target)) {
		return nil
	}
	return &spoke.UpgradeAnnounce{Target: in.Target, Source: spoke.UpgradeSourceHub, Reason: "hub-managed upgrade"}
}
