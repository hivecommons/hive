package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Channel-aware upgrade targeting (#5994).
//
// A spoke's Deployment tracks ONE image tag, and that tag is the entire set of
// builds the spoke can reach. Rolling the pod re-pulls the tag; nothing the hub
// says can make a restart land on a digest the tag does not carry.
//
// Until the stable soak policy landed, "the newest merged SHA on the release
// branch" and "the digest :stable carries" were the same image, so the hub's
// habit of targeting branch HEAD happened to be reachable for every spoke. The
// soak broke that identity on purpose: per-merge publishes now move :candidate
// only, and :stable advances hourly, 24h behind. A hub that keeps targeting
// branch HEAD is asking a :stable spoke to reach a digest its own tag is
// deliberately withholding.
//
// Measured 2026-09-04: 41 spokes in UPGRADE FAILED, 90 of 97 tracking :stable.
// The loop is exact — the hub instructs 526ef71, the spoke rolls, re-pulls
// :stable (a different digest), reports the SHA it started on, and the hub
// re-sends the identical instruction. The spoke's retry budget cannot rescue
// it: a target the tag withholds is not a transient failure.
//
// So the target has to be resolved THROUGH the tag, not around it. For a
// channel-tracking spoke the reachable target is whatever the channel tag
// currently resolves to; for everything else it is the branch's image-verified
// latest, exactly as before.

// revisionLabel is the OCI label .github/workflows/docker.yml stamps onto every
// published image with the git SHA it was built from. It is the only thing that
// survives a retag: promote-stable.sh moves :stable by re-pointing the tag at
// an existing candidate digest, so the tag NAME says nothing about the commit
// while this label continues to name it exactly. src/scripts/promote-stable.sh
// reads the same label for the same reason.
const revisionLabel = "org.opencontainers.image.revision"

// channelRevisionStaleGrace bounds how long a channel→SHA answer may be served
// after a refresh has failed.
//
// The alternative — suppressing every channel spoke's upgrade the moment GHCR
// blips — trades a targeting bug for a fleet-wide stall, and the cached answer
// is very likely still correct: promotion runs hourly, so a channel moves at
// most once an hour while this grace is measured in minutes. Past the grace we
// do suppress, because an answer that old could genuinely predate a promotion,
// and a wrong target is the thing this file exists to prevent.
const channelRevisionStaleGrace = 4 * channelDigestTTL

// ghcrManifestMaxBytes / ghcrConfigMaxBytes bound the two registry documents
// decoded below. Both are small by construction (a manifest lists a handful of
// descriptors; an image config is labels plus history), and the registry is an
// external service — decoding an unbounded body from one is how a single bad
// response becomes hub memory pressure.
const (
	ghcrManifestMaxBytes = 4 << 20
	ghcrConfigMaxBytes   = 4 << 20
)

type channelRevisionEntry struct {
	sha string
	at  time.Time
}

var (
	channelRevisionMu    sync.RWMutex
	channelRevisionCache = map[string]channelRevisionEntry{}
)

// ghcrManifest is the subset of an OCI image index / image manifest this file
// reads. An index carries Manifests (per-platform descriptors) and no Config; a
// platform manifest carries Config and no Manifests.
type ghcrManifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

// platformDigest picks the linux/amd64 descriptor out of an index.
//
// Buildx attaches provenance/SBOM descriptors to the same index with platform
// "unknown/unknown"; those carry no image config and therefore no revision
// label, so selecting by platform rather than by position is required, not
// tidiness.
func (m *ghcrManifest) platformDigest() string {
	for _, d := range m.Manifests {
		if d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
			return d.Digest
		}
	}
	return ""
}

// ghcrManifestBody GETs repo:reference and returns the decoded manifest. Shared
// by the index hop and the platform-manifest hop, which differ only in what
// they read out of the same document shape.
func ghcrManifestBody(client *http.Client, token, repo, reference string, logger *slog.Logger) *ghcrManifest {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, repo, reference)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("channel revision: GHCR manifest GET failed", "repo", repo, "ref", reference, "error", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("channel revision: GHCR manifest GET returned non-OK",
			"repo", repo, "ref", reference, "status", resp.StatusCode)
		return nil
	}
	var m ghcrManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, ghcrManifestMaxBytes)).Decode(&m); err != nil {
		logger.Warn("channel revision: GHCR manifest was not decodable",
			"repo", repo, "ref", reference, "error", err)
		return nil
	}
	return &m
}

// ghcrTagRevision returns the short git SHA that repo:tag was built from, or ""
// when it cannot be determined.
//
// A var so tests can answer without a network round-trip. Production walks the
// registry: tag → image index → linux/amd64 manifest → config blob → labels.
// The walk is the point — a digest cannot be turned back into a commit, and a
// channel tag's name never names one.
var ghcrTagRevision = func(repo, tag string, logger *slog.Logger) string {
	client := &http.Client{Timeout: channelResolveTimeout}
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + repo + ":pull")
	if err != nil {
		logger.Warn("channel revision: GHCR token request failed", "repo", repo, "error", err)
		return ""
	}
	var tok struct {
		Token string `json:"token"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(tokenResp.Body, ghcrManifestMaxBytes)).Decode(&tok)
	_ = tokenResp.Body.Close()
	if decodeErr != nil {
		logger.Warn("channel revision: GHCR token response was not decodable",
			"repo", repo, "tag", tag, "status", tokenResp.StatusCode, "error", decodeErr)
		return ""
	}

	m := ghcrManifestBody(client, tok.Token, repo, tag, logger)
	if m == nil {
		return ""
	}
	if len(m.Manifests) > 0 {
		d := m.platformDigest()
		if d == "" {
			logger.Warn("channel revision: image index carries no linux/amd64 manifest",
				"repo", repo, "tag", tag)
			return ""
		}
		if m = ghcrManifestBody(client, tok.Token, repo, d, logger); m == nil {
			return ""
		}
	}
	if m.Config.Digest == "" {
		logger.Warn("channel revision: manifest has no config descriptor", "repo", repo, "tag", tag)
		return ""
	}

	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", ghcrBase, repo, m.Config.Digest)
	req, err := http.NewRequest("GET", blobURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("channel revision: GHCR config blob GET failed", "repo", repo, "tag", tag, "error", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("channel revision: GHCR config blob GET returned non-OK",
			"repo", repo, "tag", tag, "status", resp.StatusCode)
		return ""
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, ghcrConfigMaxBytes)).Decode(&cfg); err != nil {
		logger.Warn("channel revision: GHCR config blob was not decodable",
			"repo", repo, "tag", tag, "error", err)
		return ""
	}
	rev := cfg.Config.Labels[revisionLabel]
	if len(rev) < StandardSHALen {
		// An image published without the label, or with a truncated one. Say so:
		// returning "" silently reads downstream as "GHCR is unreachable", which
		// points an operator at the wrong system entirely.
		logger.Warn("channel revision: image carries no usable revision label",
			"repo", repo, "tag", tag, "label", revisionLabel, "value", rev)
		return ""
	}
	return shortSHA(rev)
}

// channelRevisionSHA returns the short SHA that release channel `channel`
// currently resolves to, cached for channelDigestTTL.
//
// The cache matters more than the round-trips it saves: this is consulted once
// per hive per reconcile tick, so an uncached implementation would issue three
// registry requests per spoke per cycle against a ~100-spoke fleet.
func channelRevisionSHA(channel string, logger *slog.Logger) string {
	channelRevisionMu.RLock()
	prev, had := channelRevisionCache[channel]
	channelRevisionMu.RUnlock()
	if had && time.Since(prev.at) < channelDigestTTL {
		return prev.sha
	}

	sha := ghcrTagRevision(ghcrRepoSpoke, channel, logger)
	if sha == "" {
		// Refresh failed. Serve the last good answer while it is young enough to
		// still be trustworthy — see channelRevisionStaleGrace — and do NOT
		// overwrite the cache, so the very next call retries instead of latching
		// the failure for a whole TTL.
		if had && prev.sha != "" && time.Since(prev.at) < channelRevisionStaleGrace {
			logger.Warn("channel revision: refresh failed — serving the previous answer",
				"channel", channel, "sha", prev.sha, "age", time.Since(prev.at).Round(time.Second))
			return prev.sha
		}
		logger.Warn("channel revision: channel did not resolve to a commit",
			"channel", channel, "repo", ghcrRepoSpoke)
		return ""
	}

	channelRevisionMu.Lock()
	channelRevisionCache[channel] = channelRevisionEntry{sha: sha, at: time.Now()}
	channelRevisionMu.Unlock()
	return sha
}

// spokeReleaseChannel returns the release channel a spoke's Deployment actually
// follows, or "" when it tracks a branch tag or a pin.
//
// The REPORTED image ref leads, because it is what the kubelet will pull; the
// hive record's tracked_channel is intent, and the two disagree exactly while a
// channel switch is still on the wire. Targeting on intent during that window
// would aim at a channel the spoke is not on yet — the same unreachable-target
// mistake, one step removed. tracked_channel remains the fallback for spokes
// too old to report an image ref at all.
func spokeReleaseChannel(imageRef, trackedChannel string) string {
	if tag := imageTagOf(sanitizeImageRef(imageRef)); isReleaseChannel(tag) {
		return tag
	}
	if imageRef == "" && isReleaseChannel(trackedChannel) {
		return trackedChannel
	}
	return ""
}

// upgradeReachability answers "what is the newest build this spoke can actually
// land on?".
type upgradeReachability struct {
	// SHA is the short commit SHA to target, or "" when none is known.
	SHA string
	// Channel names the release channel the spoke tracks, "" for a branch tag
	// or a pin. Carried for logging: "held because :stable has not moved" and
	// "held because the branch has not moved" are different operator stories.
	Channel string
	// Resolved is false ONLY when the spoke tracks a channel that would not
	// resolve. The caller must instruct nothing in that case: it knows the
	// spoke's reachable set is not what branch HEAD says, and does not know
	// what it is. Branch-tracking spokes are always Resolved — an empty SHA
	// there is the pre-existing "no image verified yet" state, not a failure.
	Resolved bool
}

// reachableUpgradeTarget resolves the upgrade target for one spoke, through
// whatever tag its Deployment tracks.
func (s *HubServer) reachableUpgradeTarget(branch, imageRef, trackedChannel string) upgradeReachability {
	channel := spokeReleaseChannel(imageRef, trackedChannel)
	if channel == "" {
		return upgradeReachability{SHA: getLatestSHAForBranch(branch), Resolved: true}
	}
	sha := channelRevisionSHA(channel, s.logger)
	if sha == "" {
		return upgradeReachability{Channel: channel}
	}
	return upgradeReachability{SHA: sha, Channel: channel, Resolved: true}
}
