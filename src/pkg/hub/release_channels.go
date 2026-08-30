package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Release channels are MOVING container tags published by
// .github/workflows/docker.yml as retags of the same digest as the release
// branch's "<branch>-latest" tag. They exist so an operator can point a hive at
// a promotion track ("give me whatever is currently blessed") instead of at a
// branch name or a frozen SHA.
//
// IMPORTANT: nothing here hardcodes WHICH branch a channel currently tracks.
// Today all three resolve to the same v4 digest, but promotion policy will
// diverge them later, and a hardcoded mapping would then silently lie on the
// dashboard. The association is derived by resolving each channel tag to its
// registry digest and matching that digest against the digests of the tracked
// branches' "<branch>-latest" tags. When CI re-points a channel, the badge
// follows on the next cache refresh with no hub code change.
const (
	// ReleaseChannelStable is the most conservative track: the newest build
	// that has been promoted as generally safe.
	ReleaseChannelStable = "stable"

	// ReleaseChannelCandidate is the pre-promotion track — a build believed
	// good and awaiting soak before it becomes stable.
	ReleaseChannelCandidate = "candidate"

	// ReleaseChannelEdge is the newest good build, promoted with no soak.
	ReleaseChannelEdge = "edge"
)

// releaseChannels is the ordered channel list, most-stable first. Order is
// meaningful: it is the order the dashboard renders the channel rows and the
// order the per-hive upgrade dropdown offers them, so the least surprising
// choice reads first.
var releaseChannels = []string{
	ReleaseChannelStable,
	ReleaseChannelCandidate,
	ReleaseChannelEdge,
}

// ReleaseChannels returns the ordered release-channel names. A copy, so a
// caller (e.g. the dashboard JSON encoder) cannot mutate the package list.
func ReleaseChannels() []string {
	return append([]string(nil), releaseChannels...)
}

// isReleaseChannel reports whether tag names a release channel. Used to accept
// a channel as a branch-switch target without it having to exist as a git
// branch — "stable" is a tag, not a ref, so the git-branch existence check
// would reject it.
func isReleaseChannel(tag string) bool {
	for _, c := range releaseChannels {
		if c == tag {
			return true
		}
	}
	return false
}

// upgradeTargetTag maps a branch-switch target to the GHCR tag to pin. A
// release channel is ALREADY a moving tag, so it is used verbatim; a branch's
// moving tag is "<branch>-latest". Appending "-latest" to a channel would ask
// for "stable-latest", which CI never publishes — the switch would be refused
// by the publish check, or worse, land the hive on ImagePullBackOff.
func upgradeTargetTag(target string) string {
	if isReleaseChannel(target) {
		return target
	}
	return branchToTag(target) + "-latest"
}

// ChannelTarget is one row of the dashboard's channel block: the channel, and
// the image it CURRENTLY resolves to.
type ChannelTarget struct {
	// Channel is the moving tag name ("stable", "candidate", "edge").
	Channel string `json:"channel"`

	// Branch is the tracked branch whose "<branch>-latest" tag resolves to the
	// same digest as this channel, or "" when the channel points at a digest
	// that is not any tracked branch's latest (mid-promotion, a pinned older
	// build, or a branch the hub does not track). An empty Branch must render
	// honestly — the SHA alone, or "unknown" — never as a guessed branch.
	Branch string `json:"branch,omitempty"`

	// SHA is the short commit SHA displayed for this channel. It comes from the
	// matched branch's resolved SHA when Branch is set; otherwise it is empty
	// and the UI falls back to showing the digest.
	SHA string `json:"sha,omitempty"`

	// Digest is the registry digest the channel tag resolves to, or "" when the
	// channel could not be resolved at all (tag absent, or GHCR unreachable).
	Digest string `json:"digest,omitempty"`
}

// channelDigestTTL bounds how stale a channel→digest association may be. A
// channel re-point must become visible on the dashboard without an operator
// restarting the hub, but resolving costs one GHCR round-trip per channel plus
// one per tracked branch, which must not ride on every dashboard poll (the
// My Hives page polls every few seconds). Five minutes matches
// imageBranchCacheTTL, the existing registry-derived cache next door.
const channelDigestTTL = 5 * time.Minute

// channelResolveTimeout bounds the whole refresh. GHCR being slow must degrade
// to "serve the previous answer", never to a hung dashboard request.
const channelResolveTimeout = 8 * time.Second

var (
	channelTargetMu       sync.RWMutex
	channelTargetCache    []ChannelTarget
	channelTargetCachedAt time.Time
)

// ghcrTagDigest returns the registry digest that repo:tag currently resolves
// to, or "" if the tag does not exist or the registry could not be reached.
//
// A var so tests can supply digests without a network round-trip. Production
// issues a HEAD against the manifest endpoint and reads Docker-Content-Digest,
// which is exactly what a `docker pull` would resolve — so two tags reporting
// the same digest ARE the same image, which is the property the channel→branch
// match depends on.
var ghcrTagDigest = func(repo, tag string, logger *slog.Logger) string {
	client := &http.Client{Timeout: channelResolveTimeout}
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + repo + ":pull")
	if err != nil {
		logger.Warn("channel resolve: GHCR token request failed", "repo", repo, "error", err)
		return ""
	}
	defer func() { _ = tokenResp.Body.Close() }()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		// Previously silent. A malformed token response makes every subsequent
		// manifest HEAD unauthorized, so the whole channel block renders
		// "unknown" with nothing in the log to explain why.
		logger.Warn("channel resolve: GHCR token response was not decodable",
			"repo", repo, "tag", tag, "status", tokenResp.StatusCode, "error", err)
		return ""
	}
	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, repo, tag), nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("channel resolve: GHCR manifest HEAD failed", "repo", repo, "tag", tag, "error", err)
		return ""
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Previously silent, which is how an auth/permission regression on the
		// package (401/403) or a channel CI never published (404) could blank
		// the dashboard's channel rows with zero diagnostic trace.
		logger.Warn("channel resolve: GHCR manifest HEAD returned non-OK",
			"repo", repo, "tag", tag, "status", resp.StatusCode)
		return ""
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		// A 200 with no digest header is a registry contract violation; without
		// this the tag silently behaves as if it did not exist.
		logger.Warn("channel resolve: GHCR returned 200 without Docker-Content-Digest",
			"repo", repo, "tag", tag)
	}
	return digest
}

// resolveChannelTargets builds the channel→image association from digests
// alone. branchSHAs is branch→short-SHA (what the dashboard already displays,
// i.e. getDisplaySHAs()); each branch is matched via its "<branch>-latest" tag.
//
// Pure with respect to the network: every registry read goes through the
// ghcrTagDigest hook, so a test can express "channel X points at branch Y" by
// handing back digests, and a hardcoded branch mapping cannot pass.
func resolveChannelTargets(branchSHAs map[string]string, logger *slog.Logger) []ChannelTarget {
	// Resolve each tracked branch's moving tag ONCE, then invert: digest →
	// branch. Iterating branches per channel would multiply round-trips by the
	// channel count for no extra information.
	branchByDigest := make(map[string]string, len(branchSHAs))
	// Deterministic branch order so a digest carrying two branch tags (two
	// branches at the identical commit) always attributes to the same one
	// instead of flapping between dashboard polls with Go's random map order.
	branchNames := make([]string, 0, len(branchSHAs))
	for b := range branchSHAs {
		branchNames = append(branchNames, b)
	}
	sort.Strings(branchNames)
	for _, b := range branchNames {
		d := ghcrTagDigest(ghcrRepoSpoke, branchToTag(b)+"-latest", logger)
		if d == "" {
			continue
		}
		if _, taken := branchByDigest[d]; !taken {
			branchByDigest[d] = b
		}
	}

	out := make([]ChannelTarget, 0, len(releaseChannels))
	for _, ch := range releaseChannels {
		t := ChannelTarget{Channel: ch}
		t.Digest = ghcrTagDigest(ghcrRepoSpoke, ch, logger)
		if t.Digest == "" {
			// The channel row will render "unknown". Say so once, at the level
			// that knows it is a CHANNEL failing rather than an anonymous tag
			// lookup, so an operator seeing "unknown" on the dashboard can find
			// the reason without attaching a debugger.
			logger.Warn("channel resolve: channel did not resolve to any image — the dashboard will render it as \"unknown\"",
				"channel", ch, "repo", ghcrRepoSpoke)
		} else {
			// Branch stays empty when the digest matches nothing tracked. That
			// is a real state (a channel pinned to an older build), and the UI
			// renders it as such rather than inventing an attribution.
			if b, ok := branchByDigest[t.Digest]; ok {
				t.Branch = b
				t.SHA = branchSHAs[b]
			}
		}
		out = append(out, t)
	}
	return out
}

// getChannelTargets returns the cached channel→image association, refreshing it
// when older than channelDigestTTL. On a failed refresh the previous answer is
// kept: a transient GHCR blip must not blank the channel rows.
func getChannelTargets(branchSHAs map[string]string, logger *slog.Logger) []ChannelTarget {
	channelTargetMu.RLock()
	if channelTargetCache != nil && time.Since(channelTargetCachedAt) < channelDigestTTL {
		cp := append([]ChannelTarget(nil), channelTargetCache...)
		channelTargetMu.RUnlock()
		return cp
	}
	stale := append([]ChannelTarget(nil), channelTargetCache...)
	channelTargetMu.RUnlock()

	fresh := resolveChannelTargets(branchSHAs, logger)
	// "Resolved nothing at all" means the registry was unreachable, not that
	// the channels vanished — keep the last good answer in that case.
	resolvedAny := false
	for _, t := range fresh {
		if t.Digest != "" {
			resolvedAny = true
			break
		}
	}
	if !resolvedAny {
		// Nothing resolved at all — the registry was unreachable, not "the
		// channels vanished".
		//
		// Serve the previous good answer when there is one. When there is NOT
		// (cold cache: the hub started while GHCR was unreachable), return the
		// empty rows WITHOUT caching them. Caching here latched "unknown" for
		// the full channelDigestTTL, so a hub that lost one refresh showed no
		// channel association for five minutes and no amount of reloading the
		// dashboard would fix it. Leaving the cache untouched means the very
		// next poll retries.
		logger.Warn("channel resolve: no channel resolved to an image — serving the previous answer if one exists, and retrying on the next poll",
			"had_previous", len(stale) > 0)
		if len(stale) > 0 {
			return stale
		}
		return fresh
	}

	channelTargetMu.Lock()
	channelTargetCache = fresh
	channelTargetCachedAt = time.Now()
	channelTargetMu.Unlock()
	return append([]ChannelTarget(nil), fresh...)
}
