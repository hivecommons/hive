package hub

// One roll per upgrade for a release-channel follower (#17978).
//
// The hub and the spoke agree on one rule: pin to the target's SHA tag only
// when the channel tag lags the target. When the tag already carries the
// target (or newer), UpgradeChannelTag tells the spoke so and it rolls once on
// the channel tag. When it lags, the spoke pins (#10719) and the hub leaves
// that pin alone rather than switching straight back to a channel that is
// still behind; the next upgrade returns the spoke to its channel tag in the
// same roll.

// noteChannelUpgradeSent records that target is going on the wire as UpgradeTo
// for hiveID and returns the UpgradeChannelTag to send with it.
func (s *HubServer) noteChannelUpgradeSent(hiveID string, reach upgradeReachability, target string) string {
	s.mu.Lock()
	if reach.Channel == "" || target == "" {
		delete(s.heartbeatChannelPinSent, hiveID)
	} else {
		if s.heartbeatChannelPinSent == nil {
			s.heartbeatChannelPinSent = make(map[string]string)
		}
		// Recorded whatever the hint says: a spoke too old to read the hint
		// pins anyway, and must not be switched straight back either.
		s.heartbeatChannelPinSent[hiveID] = target
	}
	s.mu.Unlock()
	return channelTagCarryingTarget(reach, target, s.logger)
}

// channelPinSentMatches reports whether reportedTag is the SHA tag of the
// target last sent to this channel follower, i.e. a pin the hub asked for.
// A follower reporting its channel tag again drops the record.
func (s *HubServer) channelPinSentMatches(hiveID, trackedChannel, reportedTag string) bool {
	if reportedTag == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sent, ok := s.heartbeatChannelPinSent[hiveID]
	if !ok {
		return false
	}
	if reportedTag == trackedChannel {
		delete(s.heartbeatChannelPinSent, hiveID)
		return false
	}
	return imageTagSHAPattern.MatchString(reportedTag) && sameCommit(reportedTag, sent)
}
