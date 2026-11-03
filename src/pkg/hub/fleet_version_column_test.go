package hub

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// The live fleet on 2026-10-12: :stable auto-promoted to 0d399a6, v5 tip
// 28 commits further on with newer per-SHA images published. Walking stable
// forward to the newest branch image bypasses the soak gate — the hub
// instructed :stable spokes to 3910932/9c1a585/6eed1b9 while the channel sat
// at 0d399a6. A promotion-gated channel's target is the channel's own commit.
func TestChannelPublishedImageTargetStableIsPromotionGated(t *testing.T) {
	var walked int32
	origList := listChannelBranchCommits
	listChannelBranchCommits = func(string, *slog.Logger) []branchSHAInfo {
		atomic.AddInt32(&walked, 1)
		return []branchSHAInfo{{SHA: "3d0503c"}, {SHA: "3910932"}, {SHA: "0d399a6"}}
	}
	origExists := channelSpokeImageTagExists
	channelSpokeImageTagExists = func(string, *slog.Logger) (bool, bool) { return true, true }
	t.Cleanup(func() {
		listChannelBranchCommits = origList
		channelSpokeImageTagExists = origExists
	})

	got := channelPublishedImageTarget("v5", ReleaseChannelStable, "0d399a6", targetingLogger())
	if got.SHA != "0d399a6" || got.FloatingSHA != "0d399a6" {
		t.Fatalf("stable target = %+v, want the channel's own commit 0d399a6, never the newer branch image", got)
	}
	if !got.PromotionGated || !got.Channel || got.Ref != ":stable" {
		t.Errorf("stable target = %+v, want PromotionGated on a :stable channel ref", got)
	}
	if got.VerificationUnavailable || got.PendingImageCommits != 0 {
		t.Errorf("stable target = %+v, want fully verified with nothing pending — the channel tag IS the published image", got)
	}
	if n := atomic.LoadInt32(&walked); n != 0 {
		t.Errorf("stable must not walk the branch at all, but listed commits %d time(s)", n)
	}
}

// The non-blocking read path (my-hives) agrees: no cache miss, no refresh
// goroutine, no "verification unavailable" note for a stable spoke.
func TestChannelPublishedImageTargetNonBlockingStableNeedsNoCache(t *testing.T) {
	channelImageTargetMu.Lock()
	savedDisabled := channelImageTargetRefreshDisabled
	channelImageTargetRefreshDisabled = true
	channelImageTargetMu.Unlock()
	t.Cleanup(func() {
		channelImageTargetMu.Lock()
		channelImageTargetRefreshDisabled = savedDisabled
		channelImageTargetMu.Unlock()
	})

	got := channelPublishedImageTargetNonBlocking("v5", ReleaseChannelStable, "0d399a6", targetingLogger())
	if got.SHA != "0d399a6" || got.VerificationUnavailable || !got.PromotionGated {
		t.Fatalf("non-blocking stable target = %+v, want 0d399a6 verified and promotion-gated", got)
	}
	// Tip-tracking channels keep the cache-miss behaviour.
	cand := channelPublishedImageTargetNonBlocking("v5", ReleaseChannelCandidate, "0d399a6", targetingLogger())
	if !cand.VerificationUnavailable || cand.PromotionGated {
		t.Errorf("candidate on a cold cache = %+v, want VerificationUnavailable and not promotion-gated", cand)
	}
}

// The upgrade engine must honour the gate too: a :stable spoke's reachable
// target is the channel commit even when newer v5 images exist.
func TestReachableUpgradeTargetStableDoesNotFallForwardPastPromotion(t *testing.T) {
	stubBranchHead(t, "v5", "3d0503c")
	stubChannelRevisions(t, map[string]string{"stable": "0d399a6"})
	stubChannelImageSelection(t, []branchSHAInfo{{SHA: "3d0503c"}, {SHA: "3910932"}, {SHA: "0d399a6"}},
		map[string]bool{"3d0503c": true, "3910932": true, "0d399a6": true}, true)
	s := &HubServer{logger: targetingLogger()}

	got := s.reachableUpgradeTarget("v5", "ghcr.io/hivecommons/hive:stable", ReleaseChannelStable)
	if !got.Resolved || got.SHA != "0d399a6" || got.Channel != ReleaseChannelStable {
		t.Fatalf("reachable target = %+v, want the promoted stable commit 0d399a6", got)
	}
}

func TestChannelFollowsBranchTip(t *testing.T) {
	if channelFollowsBranchTip(ReleaseChannelStable) {
		t.Error("stable must be promotion-gated")
	}
	for _, ch := range []string{ReleaseChannelCandidate, ReleaseChannelEdge} {
		if !channelFollowsBranchTip(ch) {
			t.Errorf("%s must follow its branch tip", ch)
		}
	}
}

// Live state: stable → 0d399a6 (no branch: it is no tracked tip), candidate →
// v5, edge → v6. The stable line is v5 — the branch stable is promoted from —
// not v6, which is what the highest-numbered fallback produced and what made
// every stable hive read "796 behind".
func TestActiveReleaseLine_StablePromotedOffTipFollowsUpstreamChannel(t *testing.T) {
	channels := []ChannelTarget{
		{Channel: ReleaseChannelStable, SHA: "0d399a6", Digest: "sha256:aaa"},
		{Channel: ReleaseChannelCandidate, Branch: "v5", SHA: "3d0503c", Digest: "sha256:bbb"},
		{Channel: ReleaseChannelEdge, Branch: "v6", SHA: "bbebcb3", Digest: "sha256:ccc"},
	}
	shas := map[string]string{"v4": "7cc8355", "v5": "3d0503c", "v6": "bbebcb3", "dd": "ea23fe8"}
	if got := activeReleaseLine(channels, shas); got != "v5" {
		t.Errorf("want v5 (candidate's branch, which stable is promoted from), got %q", got)
	}
	// With candidate unresolved too, edge is the next upstream.
	channels[1].Branch = ""
	if got := activeReleaseLine(channels, shas); got != "v6" {
		t.Errorf("want v6 via edge when candidate has no branch, got %q", got)
	}
	// Stable entirely unresolved keeps the historical fallback — do not guess
	// from candidate when the hub cannot see stable at all.
	unresolved := []ChannelTarget{
		{Channel: ReleaseChannelStable},
		{Channel: ReleaseChannelCandidate, Branch: "v5", SHA: "3d0503c"},
	}
	if got := activeReleaseLine(unresolved, map[string]string{"v4": "7cc8355", "v5": "3d0503c"}); got != "v5" {
		t.Errorf("want v5 (highest resolved line) when stable is unresolved, got %q", got)
	}
}

func TestVersionMissingReason(t *testing.T) {
	cases := []struct {
		name          string
		lastHeartbeat string
		pullOnly      bool
		want          string
	}{
		{"never heard, reachable cluster", "", false, versionMissingNoHeartbeat},
		{"never heard, push-only cluster", "", true, versionMissingNoHeartbeatPushOnly},
		{"heard but no version", "2026-10-12T17:02:21Z", false, versionMissingNotReported},
		{"heard but no version, push-only", "2026-10-12T17:02:21Z", true, versionMissingNotReported},
	}
	for _, tc := range cases {
		if got := versionMissingReason(tc.lastHeartbeat, tc.pullOnly); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClusterIsPullOnly(t *testing.T) {
	s := &HubServer{clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}, "vllm-d": {ID: "vllm-d", PullOnly: true}}}
	if !s.clusterIsPullOnly("vllm-d") {
		t.Error("vllm-d is configured push-only")
	}
	if s.clusterIsPullOnly("hive-oke") || s.clusterIsPullOnly("unknown") || s.clusterIsPullOnly("") {
		t.Error("reachable and unknown clusters are not push-only")
	}
	var nilServer *HubServer
	if nilServer.clusterIsPullOnly("vllm-d") {
		t.Error("nil server must not panic or claim push-only")
	}
}

// UI contract for the three fleet-page defects: a promotion-gated stable row
// is not "N behind · building", the version cell never renders a bare "—",
// and the upgrade control is emitted through one helper that disables itself
// (with a reason) while the target image is still building.
func TestFleetVersionColumnRendering(t *testing.T) {
	body := funcBody(t, "var buildRow = function(")
	for _, want := range []string{
		"var promotionGated = !!h.behindTargetPromotionGated || h.behindTargetRef === ':' + RELEASE_CHANNEL_STABLE;",
		"var channelImagePending = !!(isCurrent && !promotionGated &&",
		"var targetBuildingTitle = 'image for ' + (branchLatest || '') + ' still building';",
		"var upgradeNowAttrs = function(title, hover) {",
		"aria-disabled=\"true\"",
		">Upgrade now</span>",
		"'\">↑ Upgrade available → ' + esc(latestShort) + '</span>'",
		"stableLagNote",
		"var vmReason = h.versionMissingReason || (h.lastHeartbeat ? 'heartbeat carries no version' : 'no heartbeat yet');",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fleet Version column is missing %q", want)
		}
	}
	for _, want := range []string{
		"var RELEASE_CHANNEL_STABLE = 'stable'",
		`.upgrade-ctl[aria-disabled="true"] { cursor: not-allowed !important; opacity: 0.5;`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboard.html is missing %q", want)
		}
	}
}
