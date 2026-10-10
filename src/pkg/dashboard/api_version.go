package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	imageRef, channel := "", versionChannel
	imageSource := spoke.SelfImageSourceUnknown
	if versionImageSource != nil {
		imageRef = versionImageSource()
		channel = spoke.ImageReleaseChannel(imageRef)
		imageSource = selfDeploymentImageSourceForDashboard()
	}
	tracking := spoke.ImageTrackingMode(imageRef)
	if imageSource == spoke.SelfImageSourcePodmanEnv {
		tracking = spoke.PodmanSelfImageTrackingMode(imageRef)
	}
	resp := map[string]interface{}{
		"version":     "2.0.0",
		"go":          "1.25",
		"hash":        versionHash,
		"short":       versionShort,
		"branch":      upstreamBranch(),
		"tracking":    tracking,
		"imageSource": imageSource,
		"deployment":  s.detectDeployment(),
	}
	if channel != "" {
		resp["channel"] = channel
	}
	if tracking != "unknown" {
		resp["imageRef"] = imageRef
	}
	// autoUpgrade tells the dashboard whether the hub manages this spoke's
	// upgrades. When true the manual spoke Upgrade button is hidden — the hub
	// rolls the upgrade out automatically, so offering a manual button is
	// redundant and confusing.
	if s.deps != nil && s.deps.Config != nil {
		resp["autoUpgrade"] = s.deps.Config.Hub.AutoUpgrade
	}
	// Auto-update health (#6765): the on-PVC upgrade marker survives the
	// restarts an upgrade causes, so its presence at runtime means the last
	// instructed upgrade has NOT landed yet — either retrying or terminally
	// failed. Surfacing it is what lets an operator tell "updated as
	// configured" apart from "auto-update has a problem".
	if m := readUpgradeMarker(); m != nil {
		resp["upgradeMarker"] = m
	}
	// Read the persisted last-LANDED upgrade record once and share it: the
	// auto-update object uses it to answer "when was this hive last updated"
	// (#10038), the release-status view below uses it to classify the last
	// attempt (#7092), and dashboard-initiated upgrades use it to distinguish a
	// floating-tag overshoot that actually landed from a superseded request.
	upgradeOutcomeRec := readUpgradeOutcome()
	dashboardUpgradeRec := readDashboardUpgradeState()

	s.versionMu.RLock()
	cached := s.cachedLatestHash
	cachedMsg := s.cachedLatestMessage
	cacheAge := time.Since(s.cachedLatestAt)
	policy := s.hubUpgradePolicy
	s.versionMu.RUnlock()
	if policy != nil {
		resp["upgradePolicy"] = policy
		if channel == "" && policy.Channel != "" {
			channel = policy.Channel
			resp["channel"] = channel
		}
	}

	if cacheAge > dashboardVersionTipCacheTTL || cached == "" {
		if latest, err := s.fetchLatestRemoteHash(); err == nil && latest != "" {
			msg := s.fetchCommitMessage(latest)
			s.versionMu.Lock()
			s.cachedLatestHash = latest
			s.cachedLatestMessage = msg
			s.cachedLatestAt = time.Now()
			s.versionMu.Unlock()
			cached = latest
			cachedMsg = msg
		}
	}

	// Upgrade target (#7262). Precedence: the hub's heartbeat policy — the
	// commit this spoke can actually land on (its release channel's revision,
	// or its branch head), which is exactly what the hub card measures against
	// — else the tip of the branch this build came from. Never a hard-wired
	// stable branch: a v5 spoke measured against v4 said "35 behind" while its
	// real distance to anything it could reach was 28.
	target := resolveUpgradeTarget(policy, upstreamBranch(), cached)
	deployment := s.detectDeployment()
	if deployment.Runtime == deploymentRuntimePodmanQuadlet || deployment.Runtime == deploymentRuntimeDockerCompose {
		if ref := standaloneTrackedChannelRef(); ref != "" {
			target = resolveStandaloneChannelTarget(ref)
		}
	}

	if cached != "" {
		latestShort := shortSHADashboard(cached)
		// Only report "behind" if the container image exists on GHCR
		containerReady := ghcrTagExistsCached(latestShort)
		resp["latestHash"] = cached
		resp["latestShort"] = latestShort
		if target.SHA == "" {
			if target.Source == upgradeTargetSourceBranch {
				resp["behind"] = containerReady && !sameCommitDashboard(versionHash, cached)
			} else {
				resp["behind"] = false
			}
		}
		if cachedMsg != "" {
			resp["latestMessage"] = cachedMsg
		}
	}
	resp["target"] = target
	manualUpgrade := s.reconcileDashboardUpgradeState(dashboardUpgradeRec, versionHash, time.Now().UTC(), upgradeOutcomeRec, target.SHA)
	if manualUpgrade != nil {
		resp["manualUpgrade"] = manualUpgrade
	}
	if target.SHA != "" {
		// stableV4* are the legacy key names the top bar reads; they now carry
		// the resolved target rather than the v4 tip. Kept so a newer hub UI
		// and an older spoke keep rendering; the semantic lives in resp["target"].
		resp["stableV4Hash"] = target.SHA
		resp["stableV4Short"] = target.Short
		// Distinguish "the tip has no image yet" (a known state: nothing to
		// upgrade to, compare never attempted) from "the compare failed"
		// (genuinely unknown). Without this the frontend renders a yellow
		// "? behind" next to the green ✓ whenever the tip is unbuilt (#4804).
		targetImageReady := target.Resolved && (target.Source == "channel" || ghcrTagExistsCached(target.Short))
		resp["stableV4ImageReady"] = targetImageReady
		if sameCommitDashboard(versionHash, target.SHA) {
			resp["behind"] = false
			resp["commitsBehind"] = 0
		} else if targetImageReady {
			resp["behind"] = true
			if count, ok := s.commitsBehindStableTip(versionHash, target.SHA); ok {
				resp["commitsBehind"] = count
			}
		} else {
			resp["behind"] = false
		}
	}

	// Auto-update status (#6962, #6963, #7262): consolidate what the spoke
	// knows — the hub's policy when it has one (who upgrades this hive, on what
	// schedule, paused or not), else the spoke-local flag and schedule — with
	// the target line, the current commit, how far behind it is, and the on-PVC
	// upgrade marker, into one explicit status object with a hard
	// "unknown/failed is never healthy" invariant.
	enabled := false
	period := ""
	if s.deps != nil && s.deps.Config != nil {
		enabled = s.deps.Config.Hub.AutoUpgrade
		period = s.deps.Config.Hub.AutoUpgradeMode
	}
	var behindPtr *int
	if cb, ok := resp["commitsBehind"].(int); ok {
		behindPtr = &cb
	}
	var marker map[string]any
	if m, ok := resp["upgradeMarker"].(map[string]any); ok {
		marker = m
	}
	resp["autoUpdate"] = buildAutoUpdateStatus(autoUpdateInputs{
		Enabled:       enabled,
		Period:        period,
		Policy:        policy,
		TargetBranch:  target.Branch,
		TargetChannel: target.Channel,
		TargetCommit:  target.Short,
		CurrentCommit: versionShort,
		CommitsBehind: behindPtr,
		Marker:        marker,
		LastUpdate:    upgradeOutcomeRec,
	})

	// Spoke release visibility (#7092): the channel this spoke follows and what
	// happened on the last upgrade attempt — including EXPLICIT success and an
	// explicit "never attempted", which the auto-update object above cannot
	// express (its "up to date" is a commit-count reading, not an attempt
	// outcome, so it cannot tell a hive that succeeded apart from one that never
	// tried). Channel selection is enabled below only when the observed image
	// proves this is a hub-managed release-channel spoke.
	lastBeat, beatOK := spoke.LastHeartbeatAttempt()
	releaseStatus := buildSpokeReleaseStatus(
		imageRef, channel,
		upgradeOutcomeRec, marker, versionHash,
		lastBeat, beatOK, dashboardHeartbeatStaleAfter,
	)
	if !beatOK && spoke.HeartbeatEnabled() {
		// A hub is configured but no beat has been attempted yet: that is
		// genuinely "not reached", unlike a hive with no hub at all.
		unreachable := false
		releaseStatus.HubReachable = &unreachable
	}
	if versionBranch != "" && versionBranch != "unknown" {
		releaseStatus.Channel.Branch = versionBranch
	}
	if marker == nil {
		if manualAttempt := upgradeAttemptFromDashboardState(manualUpgrade); manualAttempt != nil {
			releaseStatus.Attempt = *manualAttempt
		}
	}
	if imageSource != spoke.SelfImageSourcePodmanEnv && s.releaseChannelSelectorAvailable(releaseStatus.Channel) {
		releaseStatus.Channel.SelectorEnabled = true
		releaseStatus.Channel.SelectorDetail = "Choose stable, candidate, or edge. The hub records your intent and the current channel changes only after the Deployment image lands."
		if pending := pendingReleaseChannel(releaseStatus.Channel.Channel); pending != "" && pending != releaseStatus.Channel.Channel {
			releaseStatus.Channel.PendingChannel = pending
		}
	} else {
		releaseStatus.Channel.SelectorEnabled = false
		if imageSource == spoke.SelfImageSourcePodmanEnv {
			releaseStatus.Channel.SelectorReason = "podman-self-hosted"
			releaseStatus.Channel.SelectorDetail = "self-hosted Podman spoke; change Image= in hive.container"
		} else if strings.TrimSpace(imageRef) == "" && deployment.Runtime == deploymentRuntimePodmanQuadlet {
			releaseStatus.Channel.SelectorReason = "podman-self-hosted"
			releaseStatus.Channel.SelectorDetail = "self-hosted Podman spoke; HIVE_SELF_IMAGE is not set in hive.env, so this hive cannot read its own image reference. Rerun bin/hive-podman-setup.sh (or bin/hive-podman-update.sh) to record it, then restart hive.service."
		} else {
			releaseStatus.Channel.SelectorReason = "unsupported"
			releaseStatus.Channel.SelectorDetail = "Release-channel selection is available only for hub-managed spokes already following a release channel; this deployment appears self-hosted, branch-tracking, pinned, or missing hub credentials."
		}
	}
	resp["releaseStatus"] = releaseStatus

	jsonResponse(w, resp)
}

// dashboardHeartbeatStaleAfter bounds how old the most recent heartbeat attempt
// may be before the release-status view warns it may be stale. Three missed
// beats at the fixed 2-minute cadence — long enough to ride a single blip,
// short enough that a partitioned spoke stops presenting stale data as current.
const dashboardHeartbeatStaleAfter = 6 * time.Minute

const dashboardVersionTipCacheTTL = 5 * time.Minute
const dashboardUpgradeInProgressMaxAge = 15 * time.Minute
const dashboardUnpublishedTargetGraceDefault = 20 * time.Minute
const dashboardUnpublishedTargetGraceEnv = "HIVE_UPGRADE_UNPUBLISHED_TARGET_GRACE"

// upgradeTargetSource labels where /api/version's target came from (#7262).
const (
	// upgradeTargetSourceHub — the hub's heartbeat upgrade policy.
	upgradeTargetSourceHub = "hub"
	// upgradeTargetSourceBranch — the tip of the branch this build came from;
	// the fallback for a spoke the hub does not manage (or before its first beat).
	upgradeTargetSourceBranch = "branch"
)

// upgradeTarget is the resolved "what should this spoke be running" answer
// every version surface (top bar, Hub tab, auto-update status) is measured
// against, so they cannot disagree with each other or with the hub card.
type upgradeTarget struct {
	Source  string `json:"source"`
	Branch  string `json:"branch,omitempty"`
	Channel string `json:"channel,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Short   string `json:"short,omitempty"`
	// Ref is the standalone helper's install target. SHA describes that image;
	// it must not replace a channel ref with an immutable commit tag.
	Ref string `json:"ref,omitempty"`
	// Resolved is false when a channel could not resolve to a commit, whether
	// resolved locally or by the hub; the UI must show "unknown", not a tip.
	Resolved bool `json:"resolved"`
	// ManagedBy is "hub", "spoke" or "" (nobody upgrades this hive automatically).
	ManagedBy string `json:"managedBy,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
}

// resolveUpgradeTarget picks the target from the hub policy when one has been
// delivered, else from the upstream branch tip. Pure so tests pin the
// precedence directly.
func resolveUpgradeTarget(policy *spoke.HeartbeatUpgradePolicy, branch, branchTip string) upgradeTarget {
	if policy == nil {
		return upgradeTarget{
			Source:   upgradeTargetSourceBranch,
			Branch:   branch,
			SHA:      branchTip,
			Short:    shortSHADashboard(branchTip),
			Resolved: true,
		}
	}
	t := upgradeTarget{
		Source:   upgradeTargetSourceHub,
		Branch:   policy.Branch,
		Channel:  policy.Channel,
		Resolved: policy.TargetResolved,
		Paused:   policy.Paused,
	}
	if t.Branch == "" {
		t.Branch = branch
	}
	switch {
	case policy.HubManaged:
		t.ManagedBy = upgradeTargetSourceHub
	case policy.SpokeManaged:
		t.ManagedBy = "spoke"
	}
	if policy.TargetResolved && policy.TargetSHA != "" {
		t.SHA = policy.TargetSHA
		t.Short = shortSHADashboard(policy.TargetSHA)
	} else if policy.TargetResolved && policy.Channel == "" {
		// Branch-tracking spoke whose hub has not verified an image yet: the
		// branch tip is the same answer the hub would give.
		t.SHA = branchTip
		t.Short = shortSHADashboard(branchTip)
	}
	return t
}

// SetHubUpgradePolicy records the hub's upgrade posture for this spoke as
// delivered on the heartbeat (#7262). Called from the heartbeat callback; the
// next /api/version measures against it.
// SetHubPushedDashboardURL records that the hub delivered a vanity dashboard
// URL on a heartbeat (#7451). From then on hub.dashboard_url is rendered
// read-only on the Hub tab and a save that tries to change it is refused.
func (s *Server) SetHubPushedDashboardURL(url string) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	s.versionMu.Lock()
	s.hubPushedDashboardURL = url
	s.versionMu.Unlock()
}

// hubOwnsDashboardURL reports whether hub.dashboard_url belongs to the hub
// rather than to this spoke's operator (#7451). True on a hub-proxied
// (hosted) spoke — the hub's ingress terminates the hostname and the hub
// pushes the vanity URL on the heartbeat — and on any spoke that has received
// such a push in this process lifetime (covers hosted spokes reached over an
// OpenShift Route, which are not hub-proxied). A self-hosted spoke that merely
// registered with the hub keeps the field: there hub.dashboard_url is how the
// spoke TELLS the hub where its dashboard is.
func (s *Server) hubOwnsDashboardURL() bool {
	if s.hubProxied() {
		return true
	}
	s.versionMu.RLock()
	defer s.versionMu.RUnlock()
	return s.hubPushedDashboardURL != ""
}

// SetHubUpgradePolicy records the hub's upgrade posture for this spoke as
// delivered on the heartbeat (#7262). Called from the heartbeat callback; the
// next /api/version measures against it.
func (s *Server) SetHubUpgradePolicy(p *spoke.HeartbeatUpgradePolicy) {
	if p == nil {
		return
	}
	cp := *p
	s.versionMu.Lock()
	s.hubUpgradePolicy = &cp
	s.hubUpgradePolicyAt = time.Now()
	s.versionMu.Unlock()
}

// upgradeMarkerPath is where cmd/hive persists its self-upgrade attempt
// bookkeeping (upgradeMarker in cmd/hive/main.go). Var, not const, so tests
// can point it at a fixture instead of the live PVC.
var upgradeMarkerPath = "/data/upgrade-requested"

// dashboardSelfUpgradeMaxAttempts mirrors cmd/hive's selfUpgradeMaxAttempts —
// the retry budget after which the spoke stops attempting an upgrade and
// reports terminal failure. Duplicated (package main cannot be imported); the
// two must stay in step.
const dashboardSelfUpgradeMaxAttempts = 5

// readUpgradeMarker exposes the spoke's persisted self-upgrade attempt state
// for /api/version (#6765). nil when no upgrade is in flight or failing —
// the marker is removed on the boot that lands the new image, so a present
// marker always describes an upgrade that has not landed. "failed" is the
// terminal give-up state (attempt budget exhausted); attempts >= 1 with the
// marker still present means the previous attempt did not change the image
// and the spoke is retrying with backoff.
func readUpgradeMarker() map[string]any {
	data, err := os.ReadFile(upgradeMarkerPath)
	if err != nil {
		return nil
	}
	var m struct {
		TargetSHA   string    `json:"target_sha"`
		CurrentSHA  string    `json:"current_sha"`
		RequestedAt time.Time `json:"requested_at"`
		Attempts    int       `json:"attempts"`
		LastError   string    `json:"last_error"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.TargetSHA == "" {
		return nil
	}
	if m.Attempts < 1 {
		// Legacy marker without attempt bookkeeping — same reading as
		// cmd/hive's parseUpgradeMarker: counts as one prior attempt.
		m.Attempts = 1
	}
	out := map[string]any{
		"target":      m.TargetSHA,
		"current":     m.CurrentSHA,
		"attempts":    m.Attempts,
		"maxAttempts": dashboardSelfUpgradeMaxAttempts,
		"failed":      m.Attempts >= dashboardSelfUpgradeMaxAttempts,
	}
	if !m.RequestedAt.IsZero() {
		out["requestedAt"] = m.RequestedAt.Format(time.RFC3339)
	}
	if m.LastError != "" {
		out["lastError"] = m.LastError
	}
	return out
}

func (s *Server) commitsBehindStableTip(base, head string) (int, bool) {
	base = shortSHADashboard(base)
	head = shortSHADashboard(head)
	if base == "" || head == "" {
		return 0, false
	}
	if sameCommitDashboard(base, head) {
		return 0, true
	}
	key := base + "..." + head
	s.versionMu.RLock()
	if s.commitBehindCache != nil {
		if count, ok := s.commitBehindCache[key]; ok {
			s.versionMu.RUnlock()
			return count, true
		}
	}
	// A pair that failed to compare is not retried on every status build:
	// with the quota exhausted that was a ~1/s stream of doomed requests
	// and warnings against the very limit the hive was waiting out (#7430).
	// The answer is cosmetic (a "behind by N" badge); one retry per
	// commitBehindRetryAfter is plenty.
	if failedAt, ok := s.commitBehindFailedAt[key]; ok && time.Since(failedAt) < commitBehindRetryAfter {
		s.versionMu.RUnlock()
		return 0, false
	}
	s.versionMu.RUnlock()
	if s.deps == nil || s.deps.GHClient == nil || s.deps.Ctx == nil {
		return 0, false
	}
	count, err := s.deps.GHClient.CompareAheadBy(s.deps.Ctx, "hivecommons", "hive", base, head)
	if err != nil {
		s.versionMu.Lock()
		if s.commitBehindFailedAt == nil {
			s.commitBehindFailedAt = map[string]time.Time{}
		}
		s.commitBehindFailedAt[key] = time.Now()
		s.versionMu.Unlock()
		s.logger.Warn("failed to compare commits behind stable tip; not retrying for a while", "base", base, "head", head, "retry_after", commitBehindRetryAfter.String(), "error", err)
		return 0, false
	}
	s.versionMu.Lock()
	if s.commitBehindCache == nil {
		s.commitBehindCache = map[string]int{}
	}
	s.commitBehindCache[key] = count
	s.versionMu.Unlock()
	return count, true
}

func (s *Server) reconcileDashboardUpgradeState(st *dashboardUpgradeState, runningSHA string, now time.Time, outcome *upgradeOutcome, latestReachable string) *dashboardUpgradeState {
	if st == nil || (st.State != dashboardUpgradeStateStarted && st.State != dashboardUpgradeStateQueued) {
		return st
	}
	runningSHA = strings.TrimSpace(runningSHA)
	target := strings.TrimSpace(st.Target)
	latestReachable = strings.TrimSpace(latestReachable)
	next := *st
	markDone := func(reason string) *dashboardUpgradeState {
		next.State = dashboardUpgradeStateDone
		if runningSHA != "" {
			next.Target = runningSHA
		}
		next.UpdatedAt = now
		next.Reason = reason
		s.rememberDashboardUpgradeState(next)
		return &next
	}
	markSuperseded := func(reason string) *dashboardUpgradeState {
		next.State = dashboardUpgradeStateSuperseded
		next.UpdatedAt = now
		next.Reason = reason
		s.rememberDashboardUpgradeState(next)
		return &next
	}
	if target != "" && sameCommitDashboard(runningSHA, target) {
		return markDone("")
	}
	if target != "" && s.dashboardCommitAtOrAhead(runningSHA, target) {
		return markDone("running commit " + shortSHADashboard(runningSHA) + " is at or ahead of requested target " + shortSHADashboard(target))
	}
	if st.State == dashboardUpgradeStateQueued {
		grace := dashboardUnpublishedTargetGrace()
		queuedAt := st.UpdatedAt
		if queuedAt.IsZero() {
			queuedAt = st.StartedAt
		}
		if latestReachable != "" && runningSHA != "" && sameCommitDashboard(runningSHA, latestReachable) {
			return markSuperseded("unpublished target " + shortSHADashboard(target) + " was superseded because this hive is already running latest published image " + shortSHADashboard(latestReachable))
		}
		if target != "" && latestReachable != "" && !sameCommitDashboard(target, latestReachable) &&
			!queuedAt.IsZero() && now.Sub(queuedAt) >= grace {
			next.State = dashboardUpgradeStateSuperseded
			next.Target = latestReachable
			next.UpdatedAt = now
			next.Reason = "unpublished target " + shortSHADashboard(target) + " exceeded " + grace.String() + "; falling forward to newest published image " + shortSHADashboard(latestReachable)
			s.rememberDashboardUpgradeState(next)
			if s != nil && s.logger != nil {
				s.logger.Info("dashboard self-upgrade fell forward from unpublished target",
					"old_target", target, "new_target", latestReachable, "grace", grace.String())
			}
			return &next
		}
		return st
	}
	if outcome != nil && !outcome.CompletedAt.IsZero() && !outcome.CompletedAt.Before(st.StartedAt) &&
		runningSHA != "" && sameCommitDashboard(runningSHA, outcome.TargetSHA) {
		return markDone("floating tag landed " + shortSHADashboard(outcome.TargetSHA) + " after the request")
	}

	if st.StartedFrom != "" && runningSHA != "" && !sameCommitDashboard(runningSHA, st.StartedFrom) {
		reason := "running commit changed from " + shortSHADashboard(st.StartedFrom) + " to " + shortSHADashboard(runningSHA)
		if target == "" {
			return markDone(reason)
		}
		return markSuperseded(reason + " instead of requested target " + shortSHADashboard(target))
	}
	startedAt := st.StartedAt
	if startedAt.IsZero() {
		startedAt = st.UpdatedAt
	}
	if !startedAt.IsZero() && now.Sub(startedAt) >= dashboardUpgradeInProgressMaxAge {
		next.State = dashboardUpgradeStateFailed
		next.UpdatedAt = now
		next.Reason = "no rollout or completion signal arrived within " + dashboardUpgradeInProgressMaxAge.String()
		s.rememberDashboardUpgradeState(next)
		return &next
	}
	return st
}

func dashboardUnpublishedTargetGrace() time.Duration {
	raw := strings.TrimSpace(os.Getenv(dashboardUnpublishedTargetGraceEnv))
	if raw == "" {
		return dashboardUnpublishedTargetGraceDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return dashboardUnpublishedTargetGraceDefault
	}
	return d
}

func (s *Server) dashboardCommitAtOrAhead(runningSHA, targetSHA string) bool {
	runningSHA = strings.TrimSpace(runningSHA)
	targetSHA = strings.TrimSpace(targetSHA)
	if runningSHA == "" || targetSHA == "" {
		return false
	}
	if sameCommitDashboard(runningSHA, targetSHA) {
		return true
	}
	if s == nil || s.deps == nil || s.deps.GHClient == nil || s.deps.Ctx == nil {
		return false
	}
	aheadBy, err := s.deps.GHClient.CompareAheadBy(s.deps.Ctx, "hivecommons", "hive", targetSHA, runningSHA)
	return err == nil && aheadBy > 0
}

// commitBehindRetryAfter is how long a failed base...head compare is left
// alone before the status builder asks GitHub again.
const commitBehindRetryAfter = 5 * time.Minute

func shortSHADashboard(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > spoke.StandardSHALen {
		return s[:spoke.StandardSHALen]
	}
	return s
}

func sameCommitDashboard(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}

func (s *Server) fetchLatestRemoteHash() (string, error) {
	return s.fetchRemoteHashForBranch(upstreamBranch())
}

func (s *Server) fetchRemoteHashForBranch(branch string) (string, error) {
	if s.deps == nil || s.deps.GHClient == nil {
		return "", fmt.Errorf("no github client")
	}
	ctx := s.deps.Ctx
	if ctx == nil {
		return "", fmt.Errorf("no context")
	}
	return s.deps.GHClient.LatestCommitHash(ctx, "hivecommons", "hive", branch)
}

// fetchCommitMessage returns the first line of the commit message for a given SHA.
// Returns empty string on any error (best-effort for tooltip display).
func (s *Server) fetchCommitMessage(sha string) string {
	if s.deps == nil || s.deps.GHClient == nil || s.deps.Ctx == nil {
		return ""
	}
	msg, err := s.deps.GHClient.CommitMessage(s.deps.Ctx, "hivecommons", "hive", sha)
	if err != nil {
		s.logger.Warn("failed to fetch commit message", "sha", sha, "error", err)
		return ""
	}
	return msg
}

// ghcrTagExistsCached checks whether a container tag exists on ghcr.io/hivecommons/hive,
// caching the result to avoid repeated network calls on each version poll.
var (
	ghcrCacheMu     sync.RWMutex
	ghcrCacheResult = map[string]bool{}
	ghcrCacheExpiry = map[string]time.Time{}
)

const ghcrCacheTTL = 2 * time.Minute

const ghcrCheckTimeout = 5 * time.Second

var (
	ghcrCheckBaseURL = "https://ghcr.io"
	ghcrCheckClient  = &http.Client{Timeout: ghcrCheckTimeout}
)

func ghcrTagExistsCached(tag string) bool {
	ghcrCacheMu.RLock()
	if exp, ok := ghcrCacheExpiry[tag]; ok && time.Now().Before(exp) {
		result := ghcrCacheResult[tag]
		ghcrCacheMu.RUnlock()
		return result
	}
	ghcrCacheMu.RUnlock()

	result := ghcrTagExists(tag)
	ghcrCacheMu.Lock()
	ghcrCacheResult[tag] = result
	ghcrCacheExpiry[tag] = time.Now().Add(ghcrCacheTTL)
	ghcrCacheMu.Unlock()
	return result
}

func ghcrTagExists(tag string) bool {
	return ghcrTagExistsWithClient(ghcrCheckClient, ghcrCheckBaseURL, tag)
}

func ghcrTagExistsWithClient(client *http.Client, baseURL, tag string) bool {
	if client == nil {
		client = &http.Client{Timeout: ghcrCheckTimeout}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	tokenResp, err := client.Get(baseURL + "/token?scope=repository:hivecommons/hive:pull")
	if err != nil {
		return false
	}
	defer closeHTTPBody(tokenResp.Body)
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return false
	}

	manifestURL := fmt.Sprintf("%s/v2/hivecommons/hive/manifests/%s", baseURL, tag)
	req, _ := http.NewRequest("HEAD", manifestURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	closeHTTPBody(resp.Body)
	return resp.StatusCode == http.StatusOK
}

func (s *Server) handleSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" {
		jsonError(w, "owner access required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	deployment := s.detectDeployment()
	switch deployment.Runtime {
	case deploymentRuntimeUnknown:
		jsonError(w, deployment.Reason, http.StatusConflict)
		return
	case deploymentRuntimePodmanQuadlet, deploymentRuntimeDockerCompose:
		if !deployment.UpgradeSupported {
			s.rememberDashboardUpgradeState(dashboardUpgradeState{
				State:     dashboardUpgradeStateFailed,
				UpdatedAt: time.Now().UTC(),
				Reason:    deployment.Reason,
			})
			s.logger.Error("self-upgrade refused by deployment precheck", "runtime", deployment.Runtime, "reason", deployment.Reason)
			jsonError(w, deployment.Reason, http.StatusConflict)
			return
		}
		if err := s.runStandaloneUpgrade(r, deployment); err != nil {
			s.rememberDashboardUpgradeState(dashboardUpgradeState{
				State:     dashboardUpgradeStateFailed,
				UpdatedAt: time.Now().UTC(),
				Reason:    err.Error(),
			})
			s.logger.Error("self-upgrade failed before rollout", "runtime", deployment.Runtime, "error", err)
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.rememberDashboardUpgradeState(dashboardUpgradeState{
			State:       dashboardUpgradeStateStarted,
			StartedFrom: versionHash,
			StartedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Action:      deployment.UpgradeAction,
		})
		s.auditFromRequest(r, "self_upgrade", deployment.Runtime, "")
		if deployment.UpgradeAction == "podman-quadlet-request" {
			jsonResponse(w, map[string]any{"status": "accepted", "runtime": deployment.Runtime, "message": "upgrade request accepted for the host bridge; upgrade has not completed"})
			return
		}
		jsonResponse(w, map[string]any{"status": "upgrading", "runtime": deployment.Runtime})
		return
	}
	target := dashboardUpgradeTargetFromRequest(r)
	if target != "" {
		if err := s.precheckKubernetesSelfUpgrade(target); err != nil {
			if upgradeImageBuildingReason(err.Error()) {
				s.rememberDashboardUpgradeState(dashboardUpgradeState{
					State:     dashboardUpgradeStateQueued,
					Target:    target,
					UpdatedAt: time.Now().UTC(),
					Reason:    err.Error(),
				})
				s.logger.Info("self-upgrade queued until target image is published", "target", target, "reason", err.Error())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":  "queued",
					"target":  target,
					"message": "Upgrade to " + shortSHADashboard(target) + " queued — image building",
				})
				return
			}
			s.rememberDashboardUpgradeState(dashboardUpgradeState{
				State:     dashboardUpgradeStateFailed,
				Target:    target,
				UpdatedAt: time.Now().UTC(),
				Reason:    err.Error(),
			})
			s.logger.Error("self-upgrade refused by spoke precheck", "target", target, "error", err)
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
	}
	hubURL := s.deps.Config.Hub.URL
	hiveID := s.deps.Config.HiveID
	if hubURL == "" || hiveID == "" {
		s.rememberDashboardUpgradeState(dashboardUpgradeState{
			State:     dashboardUpgradeStateFailed,
			Target:    target,
			UpdatedAt: time.Now().UTC(),
			Reason:    "hub URL or hive ID not configured",
		})
		s.logger.Error("self-upgrade failed before hub request", "target", target, "reason", "hub URL or hive ID not configured")
		jsonError(w, "hub URL or hive ID not configured", http.StatusBadRequest)
		return
	}
	upgradeURL := hubURL + "/api/saas/hives/" + url.PathEscape(hiveID) + "/upgrade"

	user := r.Header.Get("X-Hive-User")
	proof := s.authToken
	if proof == "" && s.deps.Config.Dashboard.AuthToken != "" {
		proof = s.deps.Config.Dashboard.AuthToken
	}
	cookie, _ := r.Cookie("hive_hub_user")
	if proof == "" && cookie == nil {
		// Fail fast and honestly: with no dashboard-token proof and no hub
		// session cookie to relay, the hub is guaranteed to reject this request,
		// and "not authenticated" would mislead a logged-in owner. Name the
		// missing credential and how to configure it (#4446 honest-error
		// standard).
		reason := "self-upgrade needs this spoke's dashboard token to prove itself to the hub — set DASHBOARD_AUTH_TOKEN (the hive-secrets/dashboard-token secret) and restart the spoke"
		s.rememberDashboardUpgradeState(dashboardUpgradeState{
			State:     dashboardUpgradeStateFailed,
			Target:    target,
			UpdatedAt: time.Now().UTC(),
			Reason:    reason,
		})
		s.logger.Error("self-upgrade failed before hub request", "target", target, "reason", reason)
		jsonError(w, reason, http.StatusBadRequest)
		return
	}
	const upgradeTimeout = 30 * time.Second
	client := &http.Client{Timeout: upgradeTimeout}
	req, err := http.NewRequest("POST", upgradeURL, nil)
	if err != nil {
		jsonError(w, "failed to create upgrade request", http.StatusInternalServerError)
		return
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if user != "" && !s.syntheticInternalUser(r, user) {
		req.Header.Set("X-Hive-User", user)
	}
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if proof != "" {
		req.Header.Set(proxyAuthHeader, proof)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.hivecommons.dev")

	resp, err := client.Do(req)
	if err != nil {
		s.rememberDashboardUpgradeState(dashboardUpgradeState{
			State:     dashboardUpgradeStateFailed,
			Target:    target,
			UpdatedAt: time.Now().UTC(),
			Reason:    "hub unreachable: " + err.Error(),
		})
		s.logger.Error("self-upgrade: hub request failed", "target", target, "error", err)
		jsonError(w, "hub unreachable", http.StatusBadGateway)
		return
	}
	defer closeHTTPBody(resp.Body)
	const maxUpgradeResponseBytes = 1 << 16
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpgradeResponseBytes))
	if resp.StatusCode < 300 {
		now := time.Now().UTC()
		s.rememberDashboardUpgradeState(dashboardUpgradeState{
			State:       dashboardUpgradeStateStarted,
			Target:      target,
			StartedFrom: versionHash,
			StartedAt:   now,
			UpdatedAt:   now,
		})
		s.auditFromRequest(r, "self_upgrade", "", "")
	} else {
		reason := upgradeErrorFromHubBody(body, resp.Status)
		s.rememberDashboardUpgradeState(dashboardUpgradeState{
			State:     dashboardUpgradeStateFailed,
			Target:    target,
			UpdatedAt: time.Now().UTC(),
			Reason:    reason,
		})
		s.logger.Error("self-upgrade: hub refused upgrade", "target", target, "status", resp.Status, "reason", reason)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func upgradeErrorFromHubBody(body []byte, fallback string) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && strings.TrimSpace(parsed.Error) != "" {
		return strings.TrimSpace(parsed.Error)
	}
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return msg
	}
	return fallback
}

func (s *Server) syntheticInternalUser(r *http.Request, user string) bool {
	return strings.TrimSpace(user) == dashboardInternalActorUser &&
		strings.TrimSpace(r.Header.Get("X-Hive-Internal")) != "" &&
		s.configuredOwnerUser() == ""
}

func (s *Server) handleReleaseChannelSwitch(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	current := buildReleaseChannelStatus(selfDeploymentImageForDashboard(), "")
	if !s.releaseChannelSelectorAvailable(current) {
		jsonError(w, "release-channel selection is unavailable because this deployment is not currently hub-managed on a release channel", http.StatusBadRequest)
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	channel := strings.ToLower(strings.TrimSpace(body.Channel))
	if !dashboardReleaseChannelSelectable(channel) {
		jsonError(w, "channel must be stable, candidate, or edge", http.StatusBadRequest)
		return
	}
	hubURL := strings.TrimRight(s.deps.Config.Hub.URL, "/")
	hiveID := s.deps.Config.HiveID
	if hubURL == "" || hiveID == "" {
		jsonError(w, "hub URL or hive ID not configured", http.StatusBadRequest)
		return
	}
	proof := s.authToken
	if proof == "" && s.deps.Config.Dashboard.AuthToken != "" {
		proof = s.deps.Config.Dashboard.AuthToken
	}
	cookie, _ := r.Cookie("hive_hub_user")
	if proof == "" && cookie == nil {
		jsonError(w, "release-channel selection needs this spoke's dashboard token to prove itself to the hub — set DASHBOARD_AUTH_TOKEN (the hive-secrets/dashboard-token secret) and restart the spoke", http.StatusBadRequest)
		return
	}

	payload, _ := json.Marshal(map[string]string{"branch": channel})
	req, err := http.NewRequest(http.MethodPost, hubURL+"/api/saas/hives/"+url.PathEscape(hiveID)+"/switch-branch", bytes.NewReader(payload))
	if err != nil {
		jsonError(w, "failed to create release-channel request", http.StatusInternalServerError)
		return
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if user := r.Header.Get("X-Hive-User"); user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	req.Header.Set("X-Hive-Role", "owner")
	if proof != "" {
		req.Header.Set(proxyAuthHeader, proof)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.hivecommons.dev")

	const releaseChannelTimeout = 30 * time.Second
	resp, err := (&http.Client{Timeout: releaseChannelTimeout}).Do(req)
	if err != nil {
		s.logger.Warn("release-channel switch: hub request failed", "error", err)
		jsonError(w, "hub unreachable", http.StatusBadGateway)
		return
	}
	defer closeHTTPBody(resp.Body)
	const maxReleaseChannelResponseBytes = 1 << 16
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxReleaseChannelResponseBytes))
	if resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	setPendingReleaseChannel(channel)
	s.auditFromRequest(r, "release_channel_switch", channel, current.Channel)
	rs := buildSpokeReleaseStatus(selfDeploymentImageForDashboard(), "", readUpgradeOutcome(), readUpgradeMarker(), versionHash, time.Time{}, false, dashboardHeartbeatStaleAfter)
	rs.Channel.SelectorEnabled = true
	rs.Channel.SelectorDetail = "Switch requested. The hub has recorded intent; the current channel remains the observed Deployment image until the next heartbeat/rollout lands."
	if rs.Channel.Channel != channel {
		rs.Channel.PendingChannel = channel
	}
	var hubBody any
	if len(respBody) > 0 && json.Unmarshal(respBody, &hubBody) != nil {
		hubBody = string(respBody)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":        "switching",
		"channel":       channel,
		"releaseStatus": rs,
		"hub":           hubBody,
	})
}

func dashboardReleaseChannelSelectable(channel string) bool {
	switch channel {
	case "stable", "candidate", "edge":
		return true
	default:
		return false
	}
}

func (s *Server) releaseChannelSelectorAvailable(st ReleaseChannelStatus) bool {
	if !st.Resolved || !dashboardReleaseChannelSelectable(st.Channel) {
		return false
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return false
	}
	return strings.TrimSpace(s.deps.Config.Hub.URL) != "" && strings.TrimSpace(s.deps.Config.HiveID) != ""
}
