package hub

// behindTarget names what a spoke's "behind" badge is measured against.
//
// A spoke can only ever land on what its Deployment's tag delivers. For a
// branch tag (v4-latest) that is branch HEAD; for a release channel (:stable)
// it is the digest the channel currently carries — which can sit a hundred
// commits behind HEAD for as long as the promotion gate holds it. Measuring a
// channel spoke against HEAD produced "128 behind · Queued for auto-upgrade"
// on a fleet that was, by its own channel, fully current, while the upgrade
// engine (reachableUpgradeTarget, #5994) correctly refused to move it. The
// badge, the queued pill and the health verdict now measure against the same
// target the engine would instruct, so they can no longer disagree with it.
type behindTarget struct {
	// SHA is the short commit the spoke can reach, "" when not known.
	SHA string
	// Ref is the operator-facing name of the target: ":stable" for a channel,
	// "v4 tip" for a branch. It is display text, not an identifier.
	Ref string
	// Channel is true when the spoke tracks a release channel rather than a
	// branch tag.
	Channel bool
	// FloatingSHA is the commit currently carried by the channel's mutable tag,
	// which can lag behind SHA when the immutable per-SHA image exists but the
	// channel retag did not advance.
	FloatingSHA string
	// PendingImageCommits is how many newer commits on the channel's source
	// branch were skipped because their immutable images are not published yet.
	PendingImageCommits int
	// VerificationUnavailable means the hub could not verify per-SHA image
	// existence and fell back to the channel's floating tag.
	VerificationUnavailable bool
}

// behindTargetSuffixBranch is appended to a branch name in a display ref.
const behindTargetSuffixBranch = " tip"

// behindTargetFor resolves the target one registry entry is measured against.
//
// Branch targets are cache-only. Release-channel targets additionally consult
// the bounded per-SHA image target cache: a channel tag can lag behind newer
// immutable commit images, and the dashboard/upgrade engine must choose the
// newest image the spoke can actually pull rather than the older floating tag.
// An empty SHA means "not resolved yet"; the caller renders no count.
//
// trackedChannel is the hub-owned channel intent (MyHiveEntry.TrackedChannel,
// from the SaaS record) — the fallback spokeReleaseChannel uses for spokes too
// old to report an image ref. The registry entry itself carries no channel.
func (s *HubServer) behindTargetFor(e *RegistryEntry, trackedChannel string) behindTarget {
	if e == nil {
		return behindTarget{}
	}
	branch := s.upgradeBranchOrDefault(e.GitBranch)
	channel := spokeReleaseChannel(e.ImageRef, trackedChannel)
	if channel == "" {
		return behindTarget{SHA: getLatestSHAForBranch(branch), Ref: branch + behindTargetSuffixBranch}
	}
	return channelPublishedImageTargetNonBlocking(branch, channel, cachedChannelRevisionSHA(channel), s.logger)
}

// cachedChannelRevisionSHA returns the last commit a channel resolved to,
// without a network round-trip. Unlike channelRevisionSHA it does not refresh
// on TTL expiry: it is for read paths that must never block, and it relies on
// the reconcile tick to keep the entry fresh. Returns "" when the channel has
// never resolved.
func cachedChannelRevisionSHA(channel string) string {
	channelRevisionMu.RLock()
	defer channelRevisionMu.RUnlock()
	return channelRevisionCache[channel].sha
}
