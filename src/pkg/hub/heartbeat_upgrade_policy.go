package hub

// heartbeatUpgradePolicy builds the descriptive HeartbeatUpgradePolicy for one
// spoke (#7262). It is a pure projection of the state the heartbeat handler
// already resolved for its UpgradeTo decision — the SaaS record, the spoke's
// own auto_upgrade flag, the kill switch and the reachable target — so the
// spoke's dashboard cannot disagree with the hub card about who upgrades the
// hive, on what cadence, or how far behind it is.
//
// Returns nil when the hub has no upgrade opinion about the hive: no SaaS
// record and the spoke does not self-upgrade. That keeps the wire format
// unchanged for bare spokes and lets the spoke fall back to its local view.
func (s *HubServer) heartbeatUpgradePolicy(payload *HeartbeatPayload, saasHive *SaaSHive, spokeManaged, paused bool, branch, armedTarget string) *HeartbeatUpgradePolicy {
	if saasHive == nil && !spokeManaged {
		return nil
	}
	hubManaged := saasHive != nil && saasHive.AutoUpgrade
	trackedChannel := ""
	schedule := ""
	if saasHive != nil {
		trackedChannel = saasHive.TrackedChannel
		if hubManaged {
			schedule = normalizeAutoUpgradeMode(saasHive.AutoUpgradeMode)
		}
	}
	if spokeManaged && schedule == "" {
		// A self-upgrading spoke acts on the first UpgradeTo it sees.
		schedule = AutoUpgradeModeInstant
	}
	reach := s.reachableUpgradeTarget(branch, payload.ImageRef, trackedChannel)
	return &HeartbeatUpgradePolicy{
		HubManaged:     hubManaged,
		SpokeManaged:   spokeManaged,
		Schedule:       schedule,
		Paused:         paused,
		Branch:         branch,
		Channel:        reach.Channel,
		TargetSHA:      reach.SHA,
		TargetResolved: reach.Resolved,
		ArmedTarget:    armedTarget,
	}
}
