// Branch head SHA and image status tracking: tracked branch
// discovery, GitHub commit/workflow queries, GHCR tag checks, the
// persisted SHA snapshot, and the background poller.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// branchSHAInfo holds a short SHA and the first line of its commit message.
type branchSHAInfo struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

// Container-image build status values for a branch head commit, exposed to
// the dashboard as latest_sha_image_status.
const (
	imageStatusReady    = "ready"    // image tag verified on GHCR
	imageStatusBuilding = "building" // docker workflow queued/in progress, or image not yet visible
	imageStatusFailed   = "failed"   // docker workflow completed unsuccessfully
)

// branchHeadInfo tracks the branch HEAD commit, which may be ahead of the
// image-verified SHA in latestSHAByBranch while its container image builds.
type branchHeadInfo struct {
	SHA         string
	Message     string
	ImageStatus string
	// BuildStartedAt is when THIS SHA first entered the "building" status, so the
	// dashboard can show an elapsed build timer. It is stamped once on the
	// ready→building (or new-SHA→building) transition and preserved across polls
	// while the same SHA keeps building; it is zeroed when the image finishes
	// (ready/failed) or a new SHA takes over. Zero means "not building / unknown".
	BuildStartedAt time.Time
}

// githubAPIBase and ghcrBase are the GitHub/GHCR origins used by the SHA-poll
// fetch helpers. They are vars (not consts) so tests can point those helpers at
// a local httptest server; production never reassigns them.
var (
	githubAPIBase = "https://api.github.com"
	ghcrBase      = "https://ghcr.io"
)

// GHCR image repositories built by .github/workflows/docker.yml. Each is a
// SEPARATE build job tagged with the short git SHA, so one can succeed while
// another fails for the same commit. The hub runs ghcrRepoHub; spokes run
// ghcrRepoSpoke. The hub self-upgrade MUST verify ghcrRepoHub (not ghcrRepoSpoke)
// before targeting a SHA — otherwise a failed hive-hub build leaves the hub
// chasing a SHA whose hub image was never pushed, and the rollout falls back to
// a stale cached image.
const (
	ghcrRepoSpoke = "kubestellar/hive"
	ghcrRepoHub   = "hivecommons/hive-hub"

	// hubDeploymentName / hubContainerName / hubNamespace identify the hub's own
	// Kubernetes objects for self-upgrade. NOTE the container is named "hub", not
	// "hive-hub" — a `kubectl set image` that names the wrong container silently
	// no-ops the target (or errors), so pin the image against hubContainerName.
	hubDeploymentName = "hive-hub"
	hubContainerName  = "hub"
	hubNamespace      = "hive-hub"

	// hubUpgradeStateFailed is the hubUpgradeState() value meaning the hub is
	// behind latest AND its last upgrade attempt was refused or wedged, so it
	// cannot reach latest without operator action.
	hubUpgradeStateFailed = "failed"
)

// hubImageExists reports whether the hub's own container image is published on
// GHCR for the given SHA. A var (not a plain call) so tests can stub the GHCR
// round-trip; production checks ghcrRepoHub over the network.
var hubImageExists = func(sha string, logger *slog.Logger) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	return ghcrTagExists(client, ghcrRepoHub, sha, logger)
}

// spokeImageExists is the ghcrRepoSpoke counterpart of hubImageExists: it
// reports whether a SPOKE tag (a "<branch>-latest" channel tag or a SHA) is
// actually published. Separate from hubImageExists because the two repos are
// independent build jobs — the hub image for a SHA can exist while the spoke
// image for that same SHA does not. A var so tests can stub the GHCR round-trip.
var spokeImageExists = func(tag string, logger *slog.Logger) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	return ghcrTagExists(client, ghcrRepoSpoke, tag, logger)
}

var (
	latestSHAMu sync.RWMutex
	// latestSHAByBranch only ever advances to SHAs whose container image is
	// verified pullable on GHCR — it drives upgrade targets.
	latestSHAByBranch = map[string]branchSHAInfo{}
	// latestHubSHAByBranch is the same idea for the HUB's own image, which is a
	// separate build from the spoke's (ghcrRepoHub vs ghcrRepoSpoke) and can
	// succeed or fail independently for the same commit. Tracking one shared
	// value meant the hub's upgrade target was gated on the SPOKE image: when a
	// spoke build failed but the hub build succeeded, that SHA never entered
	// latestSHAByBranch, so the hub could never roll to it and reported
	// "current" against a stale target.
	latestHubSHAByBranch = map[string]branchSHAInfo{}
	// headSHAByBranch advances to the branch HEAD immediately so the
	// dashboard can show the newest commit with a build-status indicator.
	headSHAByBranch = map[string]branchHeadInfo{}
	// commitMsgBySHA caches the first line of each commit message, keyed by short SHA.
	commitMsgBySHA = map[string]string{}
)

// trackedBranches lists the legacy always-tracked branches that still produce
// Docker images via CI. Personal dev branches (e.g. mk) are tracked
// dynamically: see HubServer.trackedBranchList. v3 is retired and must not be
// offered solely because old image tags or persisted SHA cache entries linger.
var trackedBranches = []string{"v2"}

var retiredBranches = map[string]struct{}{
	"v3": {},
}

// trackedBranchList returns the static CI branches plus every branch some
// registered hive is assigned to, so a personal dev branch gets SHA polling,
// Latest-images display, branch-switch validation, and auto-upgrade without
// a hub code change per branch. Static branches keep their order and come
// first. Caller must not hold s.mu.
func (s *HubServer) trackedBranchList() []string {
	seen := make(map[string]bool, len(trackedBranches))
	out := make([]string, 0, len(trackedBranches))
	add := func(b string) {
		if _, retired := retiredBranches[b]; retired {
			return
		}
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, b := range trackedBranches {
		add(b)
	}
	// Branches already assigned to a hive.
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		add(h.GitBranch)
	}
	s.mu.RUnlock()
	// Any branch that has a published <branch>-latest image on GHCR, even
	// with no hive assigned yet — so the UI lists every assignable branch
	// and a user can switch to one before any hive uses it.
	for _, b := range discoveredImageBranches() {
		add(b)
	}
	return out
}

var (
	imageBranchMu       sync.RWMutex
	imageBranchCache    []string
	imageBranchCachedAt time.Time
)

const imageBranchCacheTTL = 5 * time.Minute

// discoveredImageBranches returns branch names inferred from published
// ghcr.io/hivecommons/hive:<branch>-latest tags (cached). A tag with a '-'
// that our sanitizer would have produced can't be reversed unambiguously, so
// we only surface tags that round-trip: the tag minus the "-latest" suffix.
// Slashless branches (v2, mk) round-trip exactly; slashed branches
// (feat/x → feat-x-latest) surface as "feat-x", which is still a valid
// switch target because switch-branch/branchToTag normalize both to the same
// image tag.
func discoveredImageBranches() []string {
	imageBranchMu.RLock()
	if time.Since(imageBranchCachedAt) < imageBranchCacheTTL && imageBranchCache != nil {
		cp := append([]string(nil), imageBranchCache...)
		imageBranchMu.RUnlock()
		return cp
	}
	imageBranchMu.RUnlock()

	const listTimeout = 8 * time.Second
	client := &http.Client{Timeout: listTimeout}
	imageBranches := listLatestImageBranches(client)

	// A merged branch is deleted but its <branch>-latest image lingers on
	// GHCR, so the image list alone would offer dead branches in the
	// switcher. Keep only image branches whose branch still EXISTS on the
	// repo. Branch names are compared in their sanitized (tag) form because
	// the image tag is branchToTag(branch); e.g. real "feat/x" ⇒ image
	// "feat-x-latest" ⇒ we must match it back to the live "feat/x".
	live := map[string]struct{}{}
	for _, b := range listRepoBranches(client) {
		live[branchToTag(b)] = struct{}{}
	}
	branches := imageBranches
	if len(live) > 0 { // only filter when the branch list fetch succeeded
		branches = branches[:0]
		for _, b := range imageBranches {
			if _, ok := live[b]; ok {
				branches = append(branches, b)
			}
		}
	}

	imageBranchMu.Lock()
	imageBranchCache = branches
	imageBranchCachedAt = time.Now()
	imageBranchMu.Unlock()
	return branches
}

// listRepoBranches returns the names of branches on kubestellar/hive via the
// GitHub API (paginated). Best-effort: returns nil on any failure so callers
// treat "unknown" as "don't filter" rather than hiding valid branches.
func listRepoBranches(client *http.Client) []string {
	var names []string
	url := githubAPIBase + "/repos/kubestellar/hive/branches?per_page=100"
	const maxPages = 10
	for page := 0; url != "" && page < maxPages; page++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		var body []struct {
			Name string `json:"name"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decErr != nil {
			return nil
		}
		for _, b := range body {
			names = append(names, b.Name)
		}
		url = nextGitHubLink(link)
	}
	return names
}

// nextGitHubLink extracts the rel="next" URL from a GitHub Link header (an
// absolute URL), or "".
func nextGitHubLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		if i, j := strings.Index(part, "<"), strings.Index(part, ">"); i >= 0 && j > i {
			return part[i+1 : j]
		}
	}
	return ""
}

// listLatestImageBranches queries the GHCR tag list for
// ghcr.io/hivecommons/hive and returns the branch name of every "<x>-latest"
// tag (the "<x>" part).
func listLatestImageBranches(client *http.Client) []string {
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:kubestellar/hive:pull")
	if err != nil {
		return nil
	}
	defer func() { _ = tokenResp.Body.Close() }()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return nil
	}

	// The registry tag list is paginated via RFC5988 Link headers. Untagged
	// build digests aside, the repo can hold thousands of SHA tags, so the
	// "<branch>-latest" tags we want may live on a later page — follow Link
	// until exhausted (bounded) rather than reading only the first page.
	branchSet := map[string]struct{}{}
	next := ghcrBase + "/v2/hivecommons/hive/tags/list?n=1000"
	const maxPages = 20 // bound: up to ~20k tags
	for page := 0; next != "" && page < maxPages; page++ {
		req, _ := http.NewRequest("GET", next, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err := client.Do(req)
		if err != nil {
			break
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			break
		}
		for _, t := range body.Tags {
			if strings.HasSuffix(t, "-latest") {
				branchSet[strings.TrimSuffix(t, "-latest")] = struct{}{}
			}
		}
		next = nextLinkURL(link)
	}
	branches := make([]string, 0, len(branchSet))
	for b := range branchSet {
		branches = append(branches, b)
	}
	return branches
}

// nextLinkURL extracts the rel="next" URL from a registry Link header, or "".
// GHCR returns a relative path (e.g. </v2/.../tags/list?last=...&n=1000>),
// which we resolve against the registry host.
func nextLinkURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end < 0 || end <= start {
			return ""
		}
		u := part[start+1 : end]
		if strings.HasPrefix(u, "/") {
			return ghcrBase + u
		}
		return u
	}
	return ""
}

const latestSHAPollInterval = 2 * time.Minute

func getLatestSHA() string {
	return getLatestSHAForBranch("v2")
}

func getLatestSHAForBranch(branch string) string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return latestSHAByBranch[branch].SHA
}

// getLatestHubSHAForBranch returns the newest SHA on this branch whose HUB
// image is verified pullable. Use this — never getLatestSHAForBranch — for the
// hub's own upgrade target, since the hub and spoke images are separate builds
// that can fail independently for the same commit.
func getLatestHubSHAForBranch(branch string) string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return latestHubSHAByBranch[branch].SHA
}

// getLatestHubSHAs returns a branch→SHA map of the newest commit per branch
// whose HUB image is verified pullable. Exposed alongside getLatestSHAs so an
// operator can see the two independent build results side by side: the hub and
// spoke images are separate builds that can succeed in either order (or fail
// independently) for the same commit, which is why the caches are separate.
func getLatestHubSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestHubSHAByBranch))
	for k, v := range latestHubSHAByBranch {
		cp[k] = v.SHA
	}
	return cp
}

// getHeadSHAs returns a branch→HEAD-SHA map: the newest commit the poller has
// seen on each branch, regardless of whether its images exist yet. Comparing
// this against getLatestSHAs tells an operator WHY the advertised SHA is behind
// HEAD — see getImageStatuses for the distinguishing signal.
func getHeadSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(headSHAByBranch))
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.SHA
		}
	}
	return cp
}

// getLatestSHAs returns a branch→SHA map (backward-compatible string values).
func getLatestSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.SHA
	}
	return cp
}

// getLatestSHAMessages returns a branch→commit-message map for tooltip display.
func getLatestSHAMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.Message
	}
	return cp
}

// getCommitMessages returns a short-SHA→commit-message map for tooltip display.
func getCommitMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(commitMsgBySHA))
	for k, v := range commitMsgBySHA {
		cp[k] = v
	}
	return cp
}

// getBranchHead returns the tracked HEAD info for a branch (zero value if
// no head fetch has succeeded since startup).
func getBranchHead(branch string) branchHeadInfo {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return headSHAByBranch[branch]
}

// setBranchHead records the branch HEAD and its image build status, keeping
// the previous commit message when the new fetch didn't include one.
func setBranchHead(branch, sha, msg, status string) {
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	prev := headSHAByBranch[branch]
	if msg == "" && prev.SHA == sha {
		msg = prev.Message
	}
	// Stamp the build-start time on the first poll that sees this SHA building,
	// and carry it forward on every subsequent poll while the same SHA is still
	// building — so the dashboard's elapsed timer counts from when the build
	// actually started, not from each poll. Clear it once the image is
	// ready/failed or a different SHA takes over.
	var buildStartedAt time.Time
	if status == imageStatusBuilding {
		if prev.SHA == sha && !prev.BuildStartedAt.IsZero() {
			buildStartedAt = prev.BuildStartedAt // same build, keep the original start
		} else {
			buildStartedAt = time.Now() // newly observed building SHA
		}
	}
	headSHAByBranch[branch] = branchHeadInfo{SHA: sha, Message: msg, ImageStatus: status, BuildStartedAt: buildStartedAt}
	if msg != "" {
		commitMsgBySHA[sha] = msg
	}
}

// getDisplaySHAs returns the branch→SHA map shown under "Latest images":
// the branch HEAD when known (its image may still be building), falling back
// to the last image-verified SHA (e.g. right after a hub restart, before the
// first head fetch succeeds).
func getDisplaySHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.SHA
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.SHA
		}
	}
	return cp
}

// getDisplaySHAMessages returns branch→commit-message for the SHAs returned
// by getDisplaySHAs.
func getDisplaySHAMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.Message
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.Message
		}
	}
	return cp
}

// getImageStatuses returns branch→image build status for the SHAs returned
// by getDisplaySHAs. Branches known only from the image-verified cache are
// ready by definition.
func getImageStatuses() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k := range latestSHAByBranch {
		cp[k] = imageStatusReady
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" && v.ImageStatus != "" {
			cp[k] = v.ImageStatus
		}
	}
	return cp
}

// getImageBuildStartTimes returns branch→unix-millis of when each currently
// "building" head first started building, for the dashboard's elapsed build
// timer. Only branches that are actively building have an entry; ready/failed/
// unknown branches are omitted. Millis match the JS side (Date.now()).
func getImageBuildStartTimes() map[string]int64 {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]int64, len(headSHAByBranch))
	for k, v := range headSHAByBranch {
		if v.SHA != "" && v.ImageStatus == imageStatusBuilding && !v.BuildStartedAt.IsZero() {
			cp[k] = v.BuildStartedAt.UnixMilli()
		}
	}
	return cp
}

// latestSHAsPath persists the last-known-good branch→SHA cache across hub
// restarts (PVC-backed, like the rest of /data/saas). The hub restarts on
// every auto-upgrade; without this file the cache starts empty, and if the
// unauthenticated GitHub branches API is rate-limited for one branch at
// startup, that branch silently disappears from "Latest images" until a
// later poll succeeds (up to an hour under rate limiting).
var latestSHAsPath = "/data/saas/latest-shas.json"

// snapshotBranchSHAs returns a copy of the full branch→info cache.
func snapshotBranchSHAs() map[string]branchSHAInfo {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]branchSHAInfo, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v
	}
	return cp
}

// loadPersistedSHAs restores the last-known-good SHA cache from disk so a
// freshly restarted hub serves the previous values while live fetches are
// failing or rate-limited. Branches no longer in trackedBranches are dropped;
// branches already populated by a live fetch are never overwritten.
func loadPersistedSHAs(logger *slog.Logger, branches []string) {
	data, err := os.ReadFile(latestSHAsPath)
	if err != nil {
		return // first run or no PVC — nothing to restore
	}
	var persisted map[string]branchSHAInfo
	if err := json.Unmarshal(data, &persisted); err != nil {
		logger.Warn("SHA poll: persisted SHA cache unreadable, ignoring", "path", latestSHAsPath, "error", err)
		return
	}
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	for _, branch := range branches {
		info, ok := persisted[branch]
		if !ok || info.SHA == "" {
			continue
		}
		if _, exists := latestSHAByBranch[branch]; exists {
			continue // live fetch already populated this branch
		}
		latestSHAByBranch[branch] = info
		if info.Message != "" {
			commitMsgBySHA[info.SHA] = info.Message
		}
		logger.Info("SHA poll: restored last-known SHA from disk", "branch", branch, "sha", info.SHA)
	}
}

// persistLatestSHAs writes the current SHA cache to disk (atomic tmp+rename,
// same pattern as the other /data/saas state files).
func persistLatestSHAs(logger *slog.Logger) {
	snapshot := snapshotBranchSHAs()
	if len(snapshot) == 0 {
		return // never overwrite a good file with an empty cache
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		logger.Warn("SHA poll: persist marshal failed", "error", err)
		return
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(latestSHAsPath), 0o755)
	tmpPath := latestSHAsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		logger.Warn("SHA poll: persist write failed", "path", latestSHAsPath, "error", err)
		return
	}
	if err := os.Rename(tmpPath, latestSHAsPath); err != nil {
		logger.Warn("SHA poll: persist rename failed", "path", latestSHAsPath, "error", err)
	}
}

// StartLatestSHAPoller polls GitHub/GHCR for the latest branch SHAs until ctx
// is cancelled. It takes a context so the loop can be stopped for a clean
// shutdown (and so tests can stop the goroutine rather than leaking it past the
// test, which otherwise races the package-level saas path state that per-test
// temp-dir setup rewrites — same class as startProvisionWatcher).
func (s *HubServer) StartLatestSHAPoller(ctx context.Context) {
	// Serve last-known-good SHAs immediately; live fetches below refresh them.
	loadPersistedSHAs(s.logger, s.trackedBranchList())
	prevInfos := snapshotBranchSHAs()
	fetchAllBranchSHAs(s.logger, s.trackedBranchList())
	if cur := snapshotBranchSHAs(); !maps.Equal(cur, prevInfos) {
		persistLatestSHAs(s.logger)
	}
	// On first poll, check if any auto-upgrade hives are behind
	s.triggerAutoUpgrades()
	// Clear Upgrading flags orphaned by spoke pods that vanished mid-upgrade.
	// Runs alongside — not inside — triggerAutoUpgrades because that function
	// only ever considers hives with AutoUpgrade enabled, while the flag is set
	// by the admin and bulk upgrade paths for any hive. Throttled internally to
	// orphanedUpgradeSweepInterval — corrective work, not a hot path.
	s.sweepOrphanedUpgradesIfDue()
	// Auto-reset any placeholder wedged at assigned && !claim_delivered past the
	// timeout, so an assigned-but-unclaimed slot can never dead-end silently.
	// Throttled internally to stuckAssignmentSweepInterval — a wedge only
	// becomes actionable after assignStuckResetTimeout, far longer than a tick.
	s.sweepStuckAssignmentsIfDue()
	// Repair pre-#1222 NET_ADMIN securityContext drift so the F5 fatal-egress
	// image (#2664) can't crash-loop drifted hives. Throttled internally to
	// netAdminReconcileInterval — this poller ticks far more often than the
	// static drift needs re-checking. See netadmin_reconcile.go / issue #2674.
	s.reconcileNetAdminIfDue()
	// Ensure the five per-hive security env vars are present and correct on
	// every hosted spoke. Nothing else re-asserts them after provision time, so
	// without this the fleet's key posture survives only as an out-of-band
	// manual patch. Throttled internally to perHiveEnvReconcileInterval and
	// rate-limited to perHiveEnvMaxPatchesPerCycle patches per cycle, because
	// each patch rolls that hive's pod. See perhive_env_reconcile.go.
	s.reconcilePerHiveEnvIfDue()
	// Force-delete hive-namespace pods stuck in Terminating past
	// orphanedPodMinAge with no finalizers and a non-Running phase — the
	// residue of nodes disappearing without draining (#5328). Throttled
	// internally to orphanedPodReapInterval and capped per cycle; nothing else
	// ever removes these, so without this they accumulate indefinitely (32
	// measured across 16 namespaces, oldest three weeks). See
	// orphaned_pod_reaper.go.
	s.reapOrphanedPodsIfDue()
	// Drop master generations whose verify window has closed, and warn when one
	// is closing while spokes still carry it. Throttled internally to
	// generationRetireInterval. This lane PERSISTS the drop and ALERTS; it is
	// not what enforces expiry — acceptableGenerations already refuses an
	// expired generation at every verify, on the wall clock, whether or not
	// this ever runs. See hub_generations_retire.go.
	s.retireExpiredGenerationsIfDue()
	// Persist and audit the revocation of expired Manage Access grants (#4150).
	// Throttled internally to accessExpirySweepInterval. This lane PERSISTS the
	// prune and stamps the timeline event; it is not what enforces expiry —
	// loadSaaSUser already drops an expired grant at every read, on the wall
	// clock, whether or not this ever runs. See access_expiry.go.
	s.sweepExpiredAccessIfDue()
	// Keep each cluster's placeholder pool at its configured watermark so
	// approvals never dead-end on "no available placeholder". Throttled
	// internally to poolReplenishInterval; disabled per cluster unless
	// pool_target is set. See pool_replenisher.go.
	s.replenishPoolsIfDue()
	// Record the per-release image-pulls snapshots (external-adoption chart). The
	// call is internally guarded to snapshot only when a release line's SHA
	// advances, so ticking it alongside the frequent SHA poll is cheap — no
	// separate scheduler. See image_pulls.go.
	s.maybeSnapshotImagePulls(ctx, time.Now())
	ticker := time.NewTicker(latestSHAPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		oldSHAs := getLatestSHAs()
		oldInfos := snapshotBranchSHAs()
		// Re-resolve each tick so branches from newly registered hives are
		// picked up without a hub restart.
		fetchAllBranchSHAs(s.logger, s.trackedBranchList())
		newSHAs := getLatestSHAs()
		if !maps.Equal(snapshotBranchSHAs(), oldInfos) {
			persistLatestSHAs(s.logger)
		}
		// Always check for pending auto-upgrades (retries failed/missed hives).
		s.triggerAutoUpgrades()
		s.sweepOrphanedUpgradesIfDue()
		s.sweepStuckAssignmentsIfDue()
		s.reconcileNetAdminIfDue()
		s.reconcilePerHiveEnvIfDue()
		s.reapOrphanedPodsIfDue()
		s.retireExpiredGenerationsIfDue()
		s.sweepExpiredAccessIfDue()
		s.replenishPoolsIfDue()
		s.maybeSnapshotImagePulls(ctx, time.Now())
		changed := false
		for branch, sha := range newSHAs {
			if sha != "" && sha != oldSHAs[branch] {
				changed = true
				break
			}
		}
		_ = changed
		// Hub auto-upgrade — checked EVERY cycle, not only when the SHA just
		// changed. Previously this lived inside `if changed {}`, so if the hub
		// missed the one poll where v2's SHA flipped (busy, mid-restart, or the
		// SHA moved between polls), it stayed "queued" forever and never retried,
		// while spokes retry every cycle via triggerAutoUpgrades() above. Mirror
		// that: whenever auto-upgrade is on and the hub is behind latest v2, trigger
		// a rollout restart. A debounce prevents re-restarting every 2min while a
		// restart is already rolling out (the new pod reports the new hash, which
		// clears the condition, but the poll can fire before the rollout lands).
		// Use the hub's OWN branch, not a hardcoded "v2": hubUpgradeState() and
		// handleHubSelfUpgrade() both read s.hubGitBranch, so hardcoding here
		// made the badge and the poller disagree the moment a hub ran on v3.
		hubBranchSHA := getLatestHubSHAForBranch(s.hubGitBranch)
		s.hubUpgradeMu.Lock()
		debounced := time.Since(s.lastHubUpgradeTrigger) > hubUpgradeDebounce
		s.hubUpgradeMu.Unlock()
		// Admin kill switch: while hub upgrades are paused the hub stays on its
		// current build regardless of new tags — the auto-trigger below never
		// fires. Checked every cycle (like the trigger itself), so flipping the
		// switch takes effect on the next poll without a restart.
		if hubPauseSw, hubPaused := s.hubUpgradesPaused(); hubPaused {
			if isHubAutoUpgrade() && hubBranchSHA != "" && !sameCommit(hubBranchSHA, s.hubGitHash) {
				s.logger.Debug("hub auto-upgrade suppressed — hub upgrades are paused",
					"behind", hubBranchSHA, "paused_by", hubPauseSw.By, "paused_at", hubPauseSw.At)
			}
		} else if isHubAutoUpgrade() && hubBranchSHA != "" && !sameCommit(hubBranchSHA, s.hubGitHash) && debounced {
			s.logger.Info("audit: hub auto-upgrade triggered", "from", s.hubGitHash, "to", hubBranchSHA)
			// rolloutHubToSHA verifies the hub image exists (skips a doomed roll
			// when the hive-hub build for this SHA failed) and pins the SHA so a
			// stale cached v2-latest can't come back up. It records the in-flight
			// state on success so the dashboard shows "Upgrading", not "queued".
			if err := s.rolloutHubToSHA(hubBranchSHA); err != nil {
				s.logger.Warn("hub auto-upgrade skipped", "to", hubBranchSHA, "reason", err)
			}
		}
	}
}

func fetchAllBranchSHAs(logger *slog.Logger, branches []string) {
	for _, branch := range branches {
		fetchBranchSHA(logger, branch)
	}
}

func fetchBranchSHA(logger *slog.Logger, branch string) {
	// Step 1: get the latest commit SHA on the branch from the GitHub API
	const shaFetchTimeout = 10 * time.Second
	client := &http.Client{Timeout: shaFetchTimeout}
	branchURL := fmt.Sprintf("%s/repos/kubestellar/hive/branches/%s", githubAPIBase, branch)
	req, _ := http.NewRequest("GET", branchURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: branch API request failed", "branch", branch, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: branch API non-200", "branch", branch, "status", resp.StatusCode)
		// Backfill missing commit messages for already-cached SHAs
		backfillCommitMessage(client, branch, logger)
		return
	}
	var branchResult struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branchResult); err != nil {
		logger.Warn("SHA poll: branch decode failed", "branch", branch, "error", err)
		return
	}
	if len(branchResult.Commit.SHA) < StandardSHALen {
		logger.Warn("SHA poll: branch SHA too short", "branch", branch, "sha", branchResult.Commit.SHA)
		return
	}
	candidateSHA := shortSHA(branchResult.Commit.SHA)
	// Extract only the first line of the commit message for tooltip display.
	commitMsg := branchResult.Commit.Commit.Message
	if idx := strings.Index(commitMsg, "\n"); idx >= 0 {
		commitMsg = commitMsg[:idx]
	}

	// Step 2: verify that a container image with this SHA tag exists on GHCR.
	// The image-verified cache (latestSHAByBranch) only advances once it does;
	// the head cache advances immediately with a build-status indicator so the
	// dashboard can show the new commit while its image builds.
	prevHead := getBranchHead(branch)
	headChanged := prevHead.SHA != candidateSHA

	// The hub image is a SEPARATE build from the spoke image and can land in
	// either order (or fail independently). Probe it on its own so the hub's
	// upgrade target is never gated on the spoke build, and vice versa.
	if candidateSHA != getLatestHubSHAForBranch(branch) &&
		ghcrTagExists(client, ghcrRepoHub, candidateSHA, logger) {
		latestSHAMu.Lock()
		latestHubSHAByBranch[branch] = branchSHAInfo{SHA: candidateSHA, Message: commitMsg}
		latestSHAMu.Unlock()
		logger.Info("SHA poll: hub image verified on GHCR", "branch", branch, "sha", candidateSHA)
	}

	if candidateSHA == getLatestSHAForBranch(branch) {
		// Head unchanged since its image was verified — nothing to re-check.
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		return
	}

	if ghcrTagExists(client, ghcrRepoSpoke, candidateSHA, logger) {
		// If commit message is empty (rate-limited or missing), fetch it separately
		// from the commits API using the full SHA (one-shot, only on new SHAs).
		if commitMsg == "" {
			commitMsg = fetchCommitMessage(client, branchResult.Commit.SHA, logger)
		}
		latestSHAMu.Lock()
		latestSHAByBranch[branch] = branchSHAInfo{SHA: candidateSHA, Message: commitMsg}
		commitMsgBySHA[candidateSHA] = commitMsg
		latestSHAMu.Unlock()
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		logger.Info("SHA poll: latest image verified on GHCR", "branch", branch, "sha", candidateSHA)
		return
	}

	// Image not on GHCR yet — ask the docker workflow whether the build for
	// this head commit is still running or has failed.
	status := fetchImageBuildStatus(client, branchResult.Commit.SHA, logger)
	if status == "" {
		// Actions API unavailable (rate-limited/network): keep the last-known
		// status for this head; a brand-new head with no image is presumed
		// building. Never invent "failed" from an API error.
		status = prevHead.ImageStatus
		if headChanged || status == "" || status == imageStatusReady {
			status = imageStatusBuilding
		}
	}
	if commitMsg == "" && headChanged {
		commitMsg = fetchCommitMessage(client, branchResult.Commit.SHA, logger)
	}
	setBranchHead(branch, candidateSHA, commitMsg, status)
	logger.Info("SHA poll: container image not yet on GHCR", "branch", branch, "sha", candidateSHA, "image_status", status)
}

// dockerWorkflowFile is the workflow that builds and pushes the container
// images (ghcr.io/hivecommons/hive:<branch>-latest and :<short-sha>) on
// every push to a tracked branch.
const dockerWorkflowFile = "docker.yml"

// fetchImageBuildStatus queries the docker workflow run for a specific head
// commit and maps it to an image build status. Returns "" when the API is
// unavailable so the caller can keep the last-known status instead of
// flapping ready/building on transient errors.
func fetchImageBuildStatus(client *http.Client, fullSHA string, logger *slog.Logger) string {
	runsURL := fmt.Sprintf("%s/repos/kubestellar/hive/actions/workflows/%s/runs?head_sha=%s&per_page=1", githubAPIBase, dockerWorkflowFile, fullSHA)
	req, _ := http.NewRequest("GET", runsURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: workflow runs request failed", "sha", fullSHA, "error", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: workflow runs non-200", "sha", fullSHA, "status", resp.StatusCode)
		return ""
	}
	var result struct {
		WorkflowRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Warn("SHA poll: workflow runs decode failed", "sha", fullSHA, "error", err)
		return ""
	}
	if len(result.WorkflowRuns) == 0 {
		// The push event may not have spawned the workflow run yet.
		return imageStatusBuilding
	}
	run := result.WorkflowRuns[0]
	if run.Status != "completed" {
		return imageStatusBuilding // queued, in_progress, waiting, pending
	}
	if run.Conclusion == "success" {
		// Workflow finished but the manifest isn't visible on GHCR yet —
		// treat as still publishing; the GHCR check flips it to ready.
		return imageStatusBuilding
	}
	return imageStatusFailed // failure, cancelled, timed_out, startup_failure
}

// fetchCommitMessage fetches the first line of a commit message from the GitHub API.
// Uses a separate endpoint that's less likely to be rate-limited since it's called
// only once per new SHA (not every poll cycle).
func fetchCommitMessage(client *http.Client, fullSHA string, logger *slog.Logger) string {
	commitURL := fmt.Sprintf("%s/repos/kubestellar/hive/commits/%s", githubAPIBase, fullSHA)
	req, _ := http.NewRequest("GET", commitURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: commit message fetch non-200", "sha", fullSHA[:7], "status", resp.StatusCode)
		return ""
	}
	var result struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	msg := result.Commit.Message
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return msg
}

// backfillCommitMessage fills in a missing commit message for an already-cached SHA.
// Called when the branches API is rate-limited but we have the SHA from a prior poll.
func backfillCommitMessage(client *http.Client, branch string, logger *slog.Logger) {
	latestSHAMu.RLock()
	info := latestSHAByBranch[branch]
	latestSHAMu.RUnlock()
	if info.SHA == "" || info.Message != "" {
		return // no SHA cached, or message already present
	}
	msg := fetchCommitMessage(client, info.SHA, logger)
	if msg == "" {
		return
	}
	latestSHAMu.Lock()
	info.Message = msg
	latestSHAByBranch[branch] = info
	commitMsgBySHA[info.SHA] = msg
	latestSHAMu.Unlock()
	logger.Info("SHA poll: backfilled commit message", "branch", branch, "sha", info.SHA, "message", msg)
}

// ghcrTagExists checks whether a container tag exists on ghcr.io/<repo> (e.g.
// ghcrRepoSpoke or ghcrRepoHub). Uses an anonymous token (public package) and a
// HEAD on the manifest endpoint. The repo is a parameter because the hub and the
// spoke are DIFFERENT images built by separate jobs — verifying the wrong one
// lets the hub target a SHA whose own image was never published.
func ghcrTagExists(client *http.Client, repo, tag string, logger *slog.Logger) bool {
	// Get anonymous pull token
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + repo + ":pull")
	if err != nil {
		logger.Warn("SHA poll: GHCR token request failed", "repo", repo, "error", err)
		return false
	}
	defer func() { _ = tokenResp.Body.Close() }()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return false
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, repo, tag)
	req, _ := http.NewRequest("HEAD", manifestURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *HubServer) handleLatestSHA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"sha": getLatestSHA()})
}
