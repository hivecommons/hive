// Hub self-upgrade and spoke upgrade orchestration: upgrade
// latches, hub rollout watching, branch switching, auto-upgrade
// toggles, and the auto-upgrade trigger sweep.
package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// hubUpgradeDebounce is the minimum gap between hub self-upgrade rollout
// restarts. The behind-latest check runs every SHA-poll cycle, so without this
// the hub could re-trigger a restart before the previous rollout's new pod
// reports the new hash. One cycle plus rollout headroom.
const hubUpgradeDebounce = 4 * time.Minute

// upgradeKubectlTimeout bounds a single auto-upgrade kubectl call. kubectl's own
// default retries an unreachable API server for ~2 minutes before giving up;
// paying that per hive serialized the upgrade loop and starved the hub's own
// upgrade check that runs after it. The heartbeat fallback is the real delivery
// path for unreachable clusters, so failing fast costs nothing.
//
// Retained under nolint despite having no current caller: three other files
// cite it BY NAME as the basis for their own timeouts
// (hosted_namespace_identity.go, netadmin_reconcile.go, saas_bulk.go). Deleting
// it to satisfy the linter would orphan those comments and lose the recorded
// reasoning for the 15s figure, which is the thing worth keeping.
//
//nolint:unused // referenced by name from three sibling timeout comments
const upgradeKubectlTimeout = 15 * time.Second

// clusterUnreachableTTL is how long the hub skips kubectl for a cluster after a
// dial failure. Long enough that one poll cycle probes a firewalled cluster at
// most once, short enough that a cluster coming back online is picked up soon.
const clusterUnreachableTTL = 10 * time.Minute

// beginUpgrade marks the registry entry at index i as upgrading toward target
// and stamps UpgradeStartedAt so the dashboard can show a TRUE elapsed time.
//
// The invariant it enforces: UpgradeStartedAt is the moment an upgrade toward a
// given target FIRST began, and it survives every retry of that same upgrade. A
// hive that is already Upgrading toward the SAME target keeps its original start
// time — it is retrying, not starting over. Only a genuinely new upgrade (the
// hive was not upgrading, or the target actually changed) re-stamps the clock.
//
// This is the fix for the reset-every-retry bug: a crash-looping self-upgrade
// re-enters the arm/retry paths every heartbeat cycle with the SAME target, and
// each re-stamp of UpgradeStartedAt to time.Now() reset the displayed
// "Upgrading Ns" back toward zero. The elapsed time therefore never crossed
// staleUpgradeTimeout, so the row never turned red and the stuck-upgrade alert
// never fired even while the hive thrashed for hours. Routing every site that
// sets Upgrading=true through this one helper makes the invariant impossible to
// violate in one place and not another.
//
// The caller MUST hold s.mu. i must be a valid index into s.registry.Hives.
func (s *HubServer) beginUpgrade(i int, target string) {
	h := &s.registry.Hives[i]
	// (Re)stamp the start ONLY on a genuinely new upgrade: the hive was not
	// already upgrading, or it is now aimed at a different target. A retry
	// toward the same target keeps the original start so the timer is honest.
	if !h.Upgrading || h.UpgradeTarget != target || h.UpgradeStartedAt.IsZero() {
		h.UpgradeStartedAt = time.Now()
	}
	h.Upgrading = true
	h.UpgradeTarget = target
}

// clearUpgradeLatch drops every trace of an in-flight upgrade on the entry at
// index i: the Upgrading flag, its target, AND the start clock. Zeroing
// UpgradeStartedAt on completion/cancel/orphan-clear guarantees the NEXT upgrade
// starts a fresh timer even if some future path forgot to re-stamp — the
// stuck-upgrade signal is only as honest as the clock it reads. Callers that
// clear Upgrading MUST route through this so the invariant (a non-zero
// UpgradeStartedAt implies an upgrade is genuinely in flight) holds everywhere.
//
// The caller MUST hold s.mu. i must be a valid index into s.registry.Hives.
func (s *HubServer) clearUpgradeLatch(i int) {
	h := &s.registry.Hives[i]
	h.Upgrading = false
	h.UpgradeTarget = ""
	h.UpgradeStartedAt = time.Time{}
}

// stampObservedUpgrade extends the beginUpgrade invariant to upgrades the hub
// merely OBSERVES rather than arms: whenever an entry ends up Upgrading=true
// from ANY source, it must carry a non-zero UpgradeStartedAt — the dashboard
// row only renders the elapsed counter, and the stuck-upgrade alert only
// fires, when the clock is non-zero. The spoke-reported path (a heartbeat
// whose payload says Upgrading with no hub-side target) rebuilt the registry
// entry from scratch every beat with a zero clock, so the badge rendered
// without its counter and the alert was blind for the whole upgrade.
//
// prevStart is the previous beat's clock for the same entry (zero when there
// is no previous state, e.g. a first heartbeat). Carrying it forward — never
// re-stamping a live clock — is what keeps this compatible with the #2725
// rule that a retry of the same upgrade preserves its original start time.
// Entries that are not Upgrading, or already carry a clock (the hub-armed
// paths route through beginUpgrade), are left untouched.
func stampObservedUpgrade(entry *RegistryEntry, prevStart time.Time) {
	if !entry.Upgrading || !entry.UpgradeStartedAt.IsZero() {
		return
	}
	if !prevStart.IsZero() {
		entry.UpgradeStartedAt = prevStart
		return
	}
	entry.UpgradeStartedAt = time.Now()
}

var hubAutoUpgradePath = "/data/saas/hub-auto-upgrade"

// The hub's own auto-upgrade is deliberately NOT given the daily schedule that
// per-hive auto-upgrade has. The two look symmetric but carry opposite risk:
//
//   - A spoke hive is a workload. Restarting it interrupts running agents, so
//     deferring to an after-hours window is a clear win — that is exactly the
//     "don't disturb a working hive" motivation for the daily mode.
//   - The hub is the control plane. It is what DELIVERS every spoke upgrade,
//     serves the dashboard, and receives every heartbeat. Holding a hub fix for
//     up to 24 hours means holding back fixes to the upgrade machinery itself,
//     including any fix to this scheduler. A hub restart is also cheap: it is a
//     single stateless pod whose state lives on the PVC, and spokes tolerate a
//     missed heartbeat cycle by design.
//
// Deferring hub upgrades would therefore add real risk (a known-bad hub stays
// up all day) to avoid a disruption the hub does not really suffer. It stays a
// plain on/off toggle. Revisit only if hub restarts are ever shown to disrupt
// in-flight spoke work.
func isHubAutoUpgrade() bool {
	data, err := os.ReadFile(hubAutoUpgradePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "true"
}

func (s *HubServer) handleHubAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoUpgrade bool `json:"auto_upgrade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	val := "false"
	if body.AutoUpgrade {
		val = "true"
	}
	if err := os.WriteFile(hubAutoUpgradePath, []byte(val), 0644); err != nil {
		s.logger.Error("hub auto-upgrade toggle save failed", "enabled", body.AutoUpgrade, "error", err)
		http.Error(w, `{"error":"failed to save preference"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: hub auto-upgrade toggled", "enabled", body.AutoUpgrade, "by", s.getAuthUser(r))

	// If enabling and hub is behind, trigger immediately. The kill switch does
	// NOT block saving the preference — only the immediate rollout: with hub
	// upgrades paused the poller stays suppressed too, and the preference takes
	// effect when an admin resumes.
	if body.AutoUpgrade {
		if sw, paused := s.hubUpgradesPaused(); paused {
			s.logger.Info("hub auto-upgrade initial trigger suppressed — hub upgrades are paused",
				"paused_by", sw.By, "paused_at", sw.At)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
			return
		}
		latestSHA := getLatestHubSHAForBranch(s.hubGitBranch)
		if latestSHA != "" && !sameCommit(latestSHA, s.hubGitHash) {
			s.logger.Info("audit: hub auto-upgrade initial trigger", "from", s.hubGitHash, "to", latestSHA)
			// Route through rolloutHubToSHA so this shares the hub-image gate and
			// the SHA-pin (avoids a stale cached v2-latest) with the poller path.
			if err := s.rolloutHubToSHA(latestSHA); err != nil {
				s.logger.Warn("hub auto-upgrade skipped", "to", latestSHA, "reason", err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
}

// rolloutHubToSHA upgrades the hub deployment to a specific v2 SHA. It first
// verifies the hub's OWN image (ghcrRepoHub) exists for that SHA on GHCR — a
// separate build job from the spoke image, so a failed hive-hub build must not
// trigger a doomed rollout — then pins the deployment to the immutable
// hive-hub:<sha> tag via `kubectl set image`. Pinning (rather than
// `rollout restart` of the mutable v2-latest tag) forces the node to pull that
// exact image and stops a stale cached v2-latest digest from coming back up
// (the "rolled but still on the old hash" failure). On success it records the
// in-flight state so the dashboard shows "Upgrading". Returns an error the
// caller surfaces; safe to call from both the poller and the admin handler.
func (s *HubServer) rolloutHubToSHA(sha string) error {
	if sha == "" {
		return fmt.Errorf("empty target SHA")
	}
	// Shape-check the tag BEFORE it can reach a live Deployment. Writing an
	// unresolvable tag (the `target1` incident — a test fixture SHA that escaped
	// to the real cluster) leaves the new ReplicaSet in ImagePullBackOff while
	// the old one keeps serving: the hub stays "up" but silently runs stale
	// code. Refusing here leaves the last good image running and makes the
	// failure loud instead of invisible.
	if err := validateImageTag(sha); err != nil {
		s.logger.Error("hub self-upgrade REFUSED: invalid image tag",
			"tag", sha,
			"deployment", hubDeploymentName,
			"namespace", hubNamespace,
			"error", err)
		s.setHubUpgradeFault(fmt.Sprintf("refused invalid image tag %q: %v", sha, err))
		return err
	}
	if !hubImageExists(sha, s.logger) {
		return fmt.Errorf("hub image %s:%s not published yet", ghcrRepoHub, sha)
	}
	image := fmt.Sprintf("ghcr.io/%s:%s", ghcrRepoHub, sha)
	cmd := kubectlForCluster(s.hubCluster(), "set", "image",
		"deployment/"+hubDeploymentName, hubContainerName+"="+image, "-n", hubNamespace)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl set image failed: %s", strings.TrimSpace(string(out)))
	}
	s.hubUpgradeMu.Lock()
	s.lastHubUpgradeTrigger = time.Now()
	s.hubUpgradeTarget = sha
	s.hubUpgradeFault = "" // a fresh, validated rollout clears any prior refusal
	s.hubUpgradeMu.Unlock()
	// Watch the rollout land. Detect-and-report only: an auto-rollback would
	// race the upgrade poller (which re-triggers the same target every cycle),
	// so a rollback could fight the upgrade loop and flap the deployment. See
	// watchHubRollout.
	go s.watchHubRollout(sha, image)
	return nil
}

// hubRolloutWatchTimeout bounds how long we wait for a self-upgrade we just
// triggered to become Ready before flagging it as stuck. The hub's own image is
// a few hundred MB and a cold node has to pull it, so this is generous enough
// to avoid false alarms while still catching an ImagePullBackOff — which
// back-off-retries indefinitely and would otherwise never surface.
const hubRolloutWatchTimeout = 5 * time.Minute

// hubRolloutPollInterval is how often watchHubRollout re-checks rollout status.
const hubRolloutPollInterval = 15 * time.Second

// watchHubRollout confirms a self-upgrade actually became Ready, and records a
// loud, user-visible fault if it did not.
//
// It deliberately does NOT roll back. The auto-upgrade poller re-evaluates the
// same target every cycle, so an automatic rollback would immediately be undone
// and re-applied, flapping the deployment during an already-degraded window.
// Detect and report is the safe half of the loop: the operator sees the stuck
// upgrade in the UI and in the logs, and decides.
func (s *HubServer) watchHubRollout(sha, image string) {
	deadline := time.Now().Add(hubRolloutWatchTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(hubRolloutPollInterval)
		// `rollout status --timeout=0s` returns non-zero while a rollout is
		// still progressing and zero once it has fully succeeded.
		cmd := kubectlForCluster(s.hubCluster(), "rollout", "status",
			"deployment/"+hubDeploymentName, "-n", hubNamespace, "--timeout=0s")
		if err := cmd.Run(); err == nil {
			s.hubUpgradeMu.Lock()
			s.hubUpgradeFault = ""
			s.hubUpgradeMu.Unlock()
			s.logger.Info("hub self-upgrade rollout completed", "sha", sha, "image", image)
			return
		}
	}
	// Still not Ready. Pull the pod-level reason so the log names the actual
	// cause (ImagePullBackOff / ErrImagePull / CrashLoopBackOff) rather than
	// just "timed out".
	reason := s.hubRolloutFailureReason()
	s.logger.Error("hub self-upgrade STUCK: new ReplicaSet not Ready — hub is still serving the OLD image",
		"sha", sha,
		"image", image,
		"deployment", hubDeploymentName,
		"namespace", hubNamespace,
		"waited", hubRolloutWatchTimeout.String(),
		"reason", reason)
	s.setHubUpgradeFault(fmt.Sprintf("upgrade to %s stuck after %s: %s (still serving the previous image)",
		image, hubRolloutWatchTimeout, reason))
}

// hubRolloutFailureReason best-effort extracts why the hub's pods are not
// Ready, so the stuck-rollout log/status names the real cause.
func (s *HubServer) hubRolloutFailureReason() string {
	const reasonJSONPath = `{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{"\n"}{end}`
	cmd := kubectlForCluster(s.hubCluster(), "get", "pods",
		"-n", hubNamespace, "-l", "app="+hubDeploymentName,
		"-o", "jsonpath="+reasonJSONPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown (could not read pod status)"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "unknown (no waiting container state reported)"
}

// setHubUpgradeFault records a self-upgrade failure for the dashboard so a
// stuck or refused upgrade is visible in the UI, not only in `kubectl describe`.
func (s *HubServer) setHubUpgradeFault(msg string) {
	s.hubUpgradeMu.Lock()
	s.hubUpgradeFault = msg
	s.hubUpgradeMu.Unlock()
}

// HubUpgradeFault returns the current self-upgrade fault message, or "" when
// the last upgrade attempt was healthy.
func (s *HubServer) HubUpgradeFault() string {
	s.hubUpgradeMu.Lock()
	defer s.hubUpgradeMu.Unlock()
	return s.hubUpgradeFault
}

// hubUpgradeState reports the hub's own upgrade status for the dashboard badge:
//   - "current"   — running the latest v2 SHA
//   - "upgrading" — a rollout we triggered is in flight (within the debounce
//     window after the trigger, before the new pod reports the new hash)
//   - "queued"    — behind latest, auto-upgrade ON, no rollout in flight yet
//     (the poller will trigger one shortly)
//   - "behind"    — behind latest, auto-upgrade OFF (admin must click Upgrade)
//   - "failed"    — an upgrade was REFUSED (malformed image tag) or its rollout
//     never became Ready. The hub is behind and cannot self-heal;
//     an operator must look. HubUpgradeFault() carries the reason.
//   - "unknown"   — latest SHA not resolved yet
//
// This is the field the badge needs: previously the frontend could only tell an
// admin-clicked rollout ("upgrading") from everything else ("queued"), so an
// AUTO rollout in progress showed a misleading "queued".
func (s *HubServer) hubUpgradeState() string {
	// Gate the badge on the HUB image, matching what rolloutHubToSHA can
	// actually roll to — otherwise the UI shows "behind"/"queued" against a
	// target the hub is incapable of reaching.
	latest := getLatestHubSHAForBranch(s.hubGitBranch)
	if latest == "" {
		return "unknown"
	}
	if sameCommit(latest, s.hubGitHash) {
		return "current"
	}
	s.hubUpgradeMu.Lock()
	inFlight := s.hubUpgradeTarget != "" &&
		time.Since(s.lastHubUpgradeTrigger) < hubUpgradeDebounce
	fault := s.hubUpgradeFault
	s.hubUpgradeMu.Unlock()
	// A refused or stuck upgrade outranks "upgrading"/"queued": the hub is
	// behind AND cannot get there on its own, which needs an operator. Without
	// this the badge showed a reassuring "queued" while the rollout was wedged.
	if fault != "" {
		return hubUpgradeStateFailed
	}
	if inFlight {
		return "upgrading"
	}
	if isHubAutoUpgrade() {
		return "queued"
	}
	return "behind"
}

func (s *HubServer) handleHubSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	// Admin kill switch: while hub upgrades are paused, a manual trigger is
	// refused loudly — never queued — so the operator learns the state and who
	// set it instead of waiting on a rollout that will not come.
	if sw, paused := s.hubUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("hub", sw)})
		return
	}
	target := getLatestHubSHAForBranch(s.hubGitBranch)
	s.logger.Info("audit: hub self-upgrade triggered", "by", username, "to", target)
	if err := s.rolloutHubToSHA(target); err != nil {
		s.logger.Warn("hub self-upgrade failed", "error", err)
		http.Error(w, `{"error":"hub upgrade failed — check logs"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"upgrading"}`))
}

func (s *HubServer) handleUpgradeHive(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	if username == "" {
		username, _ = s.trustedSpokeUpgradeUser(r, id)
	}
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can upgrade"}`, http.StatusForbidden)
		return
	}
	// Admin kill switch: refuse loudly rather than arm anything — a request
	// accepted here would either restart the pod now or sit silently in
	// heartbeatUpgrade, both of which the pause exists to prevent.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("spoke", sw)})
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}

	// PULL ONLY — no kubectl push. The manual Upgrade button now does exactly
	// what auto-upgrade does: record the target and arm the heartbeat. The
	// spoke collects it on its next beat and patches its own Deployment.
	//
	// UX CONSEQUENCE, DELIBERATE: the click no longer produces an immediate
	// pod roll. Delivery is bounded by the heartbeat interval, so "clicked
	// Upgrade, nothing visible yet" is now normal and must not be mistaken for
	// the wedge this PR fixes. The hive is latched Upgrading with a target the
	// moment the request returns, which is what the dashboard renders.

	// Do not ARM an upgrade this hive cannot COLLECT — the same predicate
	// triggerAutoUpgrades() applies, for the same reason. Delivery is PULL on
	// BOTH paths: the hub only records a target and arms the heartbeat, and the
	// spoke patches its own Deployment when it next beats. A hive that never
	// heartbeats (or is silent past staleRemoveAge) therefore never collects
	// the instruction, while Upgrading=true latches on the hub and the
	// stale-upgrade sweep re-arms it every staleUpgradeTimeout — an unbounded
	// loop the orphan sweep's retry budget cannot break, because such a hive
	// fails evaluateOrphanedUpgrade()'s liveness test. See pullonly_upgrade.go.
	//
	// Without this, the manual button was strictly WORSE than the auto path it
	// diverged from: auto-upgrade refuses and records the refusal on the
	// timeline, whereas the click reported {"status":"upgrading"} and a success
	// toast for an upgrade that could never land. Worse, the asymmetry read as
	// a workaround — the same spoke auto-upgrade had declined would accept a
	// manual click, appearing to fix the problem while only hiding it.
	//
	// lastHeartbeat comes from the REGISTRY entry, which is the only record
	// that carries it; SaaSHive (the loadSaaSHive record `h` above) has no such
	// field. This is the identical source triggerAutoUpgrades() reads.
	//
	// Refused with 409, matching the pause-switch refusal above. The reason is
	// operator-facing by construction and documented to carry no kubeconfig
	// paths or credentials, so it is safe in the body.
	s.mu.Lock()
	var latestSHA, lastHeartbeat string
	var found bool
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			found = true
			lastHeartbeat = s.registry.Hives[i].LastHeartbeat
			branch := s.upgradeBranchOrDefault(s.registry.Hives[i].GitBranch)
			latestSHA = getLatestSHAForBranch(branch)
			break
		}
	}
	// A hive with no registry entry has never checked in at all, so it is
	// uncollectible for exactly the reason the empty-heartbeat case is.
	if !found || !upgradeCollectible(lastHeartbeat, time.Now()) {
		s.mu.Unlock()
		reason := uncollectibleUpgradeReason(lastHeartbeat)
		s.logger.Warn("manual upgrade not armed — hive cannot collect the instruction",
			"hive_id", id, "by", username, "cluster", cluster.ID,
			"would_have_targeted", latestSHA, "last_heartbeat", orDash(lastHeartbeat),
			"reason", reason)
		s.noteUncollectibleUpgrade(id, latestSHA, reason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
		return
	}
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, latestSHA)
			break
		}
	}
	if latestSHA != "" {
		// Arm delivery: the spoke self-restarts onto the target when its next
		// heartbeat carries UpgradeTo. The stale-upgrade sweep re-arms this if
		// the spoke misses it, so the request cannot be lost.
		if s.heartbeatUpgrade == nil {
			s.heartbeatUpgrade = make(map[string]string)
		}
		s.heartbeatUpgrade[id] = latestSHA
	}
	s.mu.Unlock()

	if latestSHA == "" {
		// No build target is known for this hive's branch, so there is nothing
		// the heartbeat could carry. This is the only hard-failure case left.
		s.logger.Warn("upgrade failed: no build target known for this hive's branch",
			"hive", id, "cluster", cluster.ID)
		http.Error(w, `{"error":"upgrade failed — no build target known for this hive's branch"}`, http.StatusBadGateway)
		return
	}

	// Always "heartbeat" now: the push path is retired, so every upgrade is
	// collected by the spoke on its next beat. The UI uses this to say "queued"
	// rather than implying an immediate roll.
	const mode = "heartbeat"
	// Armed successfully, so the uncollectible condition has genuinely cleared:
	// drop the de-duplication memory (as the auto path does on its own successful
	// arm) so a LATER refusal for this same target is reported afresh rather than
	// suppressed by a stale entry.
	s.forgetUncollectibleUpgrade(id)
	s.logger.Info("audit: hosted hive upgrade requested",
		"hive_id", id, "by", username, "cluster", cluster.ID, "mode", mode)
	s.recordTimeline(id, TimelineUpgradeStarted, "upgrade requested from the hub dashboard ("+mode+")", username)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"upgrading","mode":"` + mode + `"}`))
}

// branchToTag converts a git branch name into a valid Docker image tag.
// Branch names may contain '/' (e.g. feat/x) which is illegal in a tag; the
// docker.yml build sanitizes the same way (feat/x -> feat-x-latest), so the
// hub must match to find the image.
func branchToTag(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

func (s *HubServer) handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can switch branches"}`, http.StatusForbidden)
		return
	}
	// Admin kill switch: a branch/channel switch while spoke upgrades are
	// paused gets an explicit 409 naming who paused and when — never a silent
	// queue into heartbeatSwitchTag.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("spoke", sw)})
		return
	}
	var body struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Branch == "" {
		http.Error(w, `{"error":"branch is required"}`, http.StatusBadRequest)
		return
	}
	// A release channel is a moving TAG, not a git ref, so the branch-existence
	// checks below would reject it ("stable" is not a branch on the hive repo).
	// Accept it here and let the shared publish check further down be the real
	// gate — the tag still has to exist on GHCR before we point a hive at it.
	isChannel := isReleaseChannel(body.Branch)
	validBranch := isChannel
	for _, b := range s.trackedBranchList() {
		if b == body.Branch {
			validBranch = true
			break
		}
	}
	// Bootstrap case: trackedBranchList only includes branches already
	// assigned to some hive, so the FIRST hive moved to a new dev branch
	// would never validate. Accept any branch that actually exists on the
	// hive repo — a live one-shot check (fetchBranchSHA populates the SHA
	// cache as a side effect, so the branch is tracked from here on).
	if !validBranch {
		fetchBranchSHA(s.logger, body.Branch)
		if getLatestSHAForBranch(body.Branch) != "" {
			validBranch = true
		}
	}
	if !validBranch {
		http.Error(w, `{"error":"unknown branch (does not exist on the hive repo)"}`, http.StatusBadRequest)
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}
	ns := "hive-hosted-" + id
	// A channel IS the tag ("stable"); a branch's moving tag is "<branch>-latest".
	imageTag := upgradeTargetTag(body.Branch)
	image := "ghcr.io/hivecommons/hive:" + imageTag
	// Refuse a branch name that sanitizes into something that is not a valid
	// channel tag, rather than stranding the spoke on ImagePullBackOff behind a
	// still-serving old ReplicaSet.
	if err := validateImageTag(imageTag); err != nil {
		s.logger.Error("branch switch REFUSED: invalid image tag",
			"hive", id, "branch", body.Branch, "tag", imageTag, "error", err)
		http.Error(w, `{"error":"branch does not map to a valid image tag"}`, http.StatusBadRequest)
		return
	}
	// Shape is not existence. A DEPRECATED branch (v3 was retired while hives
	// were still pointed at it) keeps a perfectly well-formed "<branch>-latest"
	// tag that CI no longer publishes, so validateImageTag passes and the pull
	// then fails with an opaque "manifest unknown". Kubernetes keeps the old
	// ReplicaSet serving, so the hive looks alive while silently running stale
	// code. Verify the tag is actually pullable before writing it.
	if !spokeImageExists(imageTag, s.logger) {
		s.logger.Error("branch switch REFUSED: image tag not published on GHCR",
			"hive", id, "branch", body.Branch, "tag", imageTag,
			"hint", "branch may be deprecated or its CI image build never completed")
		http.Error(w, `{"error":"no published image for that branch (deprecated branch, or its image build has not completed)"}`, http.StatusBadRequest)
		return
	}
	// Persist WHAT the operator selected before delivering it, on the hub-owned
	// hive record. The registry cannot remember a channel selection: the spoke
	// heartbeats the image's baked-in branch (a "stable" retag of a v4 build
	// reports git_branch="v4") and overwrites GitBranch every beat, so within
	// one beat of the switch the dashboard's pill fell back to the branch. Set
	// on a channel switch, cleared on a plain-branch switch, written by no
	// other path — heartbeats never touch it. Done after every validation gate
	// above (so a refused switch records nothing) and before the two delivery
	// paths below (so kubectl-vs-heartbeat delivery cannot diverge on it). A
	// save failure only downgrades the pill to the reported branch; it must
	// not block the switch itself.
	if isChannel {
		h.TrackedChannel = body.Branch
	} else {
		h.TrackedChannel = ""
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("branch switch: failed to persist tracked channel — the version pill will fall back to the spoke-reported branch",
			"hive", id, "target", body.Branch, "error", err)
	}
	// "*=" updates every container including init containers (copy-config,
	// init-permissions) — pinning only "hive" left inits on the old branch tag.
	cmd := kubectlForCluster(cluster, "set", "image", "deployment/hive", "*="+image, "-n", ns)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The hub can't reach this hive's cluster over kubectl (e.g. the heartbeat-only cluster
		// from the hub-reachable-cluster hub). Fall back to the heartbeat path: record the
		// target tag; the spoke — which has in-cluster RBAC (hive-self-upgrade
		// role) to patch its own deployment — applies it on its next
		// heartbeat. This is the ONLY path that works for unreachable
		// clusters, so it's not an error.
		s.logger.Warn("branch switch kubectl failed, using heartbeat fallback",
			"hive", id, "branch", body.Branch, "output", string(out))
		s.mu.Lock()
		s.heartbeatSwitchTag[id] = imageTag
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				s.beginUpgrade(i, imageTag)
				break
			}
		}
		s.mu.Unlock()
		s.logger.Info("audit: hive branch switch queued via heartbeat", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "switching", "branch": body.Branch, "image": image, "via": "heartbeat"})
		return
	}
	// Restart the deployment to pull the new image
	restartCmd := kubectlForCluster(cluster, "rollout", "restart", "deployment/hive", "-n", ns)
	restartOut, restartErr := restartCmd.CombinedOutput()
	if restartErr != nil {
		s.logger.Warn("rollout restart after branch switch failed", "hive", id, "output", string(restartOut))
	}
	s.logger.Info("audit: hive branch switched", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
	s.recordTimeline(id, TimelineBranchChanged,
		fmt.Sprintf("branch switch to %s requested (image %s)", body.Branch, image), username)
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, imageTag)
			break
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "switching",
		"branch": body.Branch,
		"image":  image,
	})
}

func (s *HubServer) handleToggleAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		// Hive may exist in registry via heartbeat but have no SaaS entry yet.
		// Create a minimal entry so the auto-upgrade preference can be stored
		// and delivered via heartbeat response.
		s.mu.RLock()
		var regEntry *RegistryEntry
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				regEntry = &s.registry.Hives[i]
				break
			}
		}
		s.mu.RUnlock()
		if regEntry == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"hive not found"}`)
			return
		}
		h = &SaaSHive{
			ID:    id,
			Owner: regEntry.Owner,
			Org:   regEntry.Org,
			Repos: regEntry.Repos,
		}
	}
	if !userIsHiveOwner(username, h) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":"only the owner can change auto-upgrade"}`)
		return
	}
	// The mode rides on the EXISTING endpoint rather than a second one: it is
	// the same preference, the same owner-or-admin authorization checked above,
	// and the same persistence. A separate endpoint would let the two settings
	// drift apart across two requests. Older clients that send only
	// auto_upgrade omit the field, which reads as "" = instant, preserving
	// their behaviour exactly.
	var body struct {
		AutoUpgrade bool   `json:"auto_upgrade"`
		Mode        string `json:"auto_upgrade_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid request body"}`)
		return
	}
	// Reject unknown modes instead of defaulting — a typo must not silently
	// change how often a hive restarts.
	if !isValidAutoUpgradeMode(body.Mode) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid auto_upgrade_mode (expected \"instant\", \"daily\" or \"weekly\")"}`)
		return
	}
	h.AutoUpgrade = body.AutoUpgrade
	h.AutoUpgradeMode = body.Mode
	// Switching modes clears the day's fire record. Otherwise a hive flipped to
	// daily after an instant upgrade earlier today would inherit a stale date
	// and skip tonight's window.
	h.AutoUpgradeLastFired = ""
	if err := saveSaaSHive(h); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":"failed to save"}`)
		return
	}
	s.logger.Info("audit: auto-upgrade toggled", "hive_id", id, "auto_upgrade", body.AutoUpgrade, "mode", normalizeAutoUpgradeMode(body.Mode), "by", username)

	// If enabling auto-upgrade and hive is behind, trigger immediately via kubectl
	// for hosted hives. For heartbeat-connected hives, the upgrade instruction
	// is delivered via the heartbeat response.
	// Daily mode deliberately does NOT kick an upgrade here: the whole point of
	// choosing it is that turning auto-upgrade on should not immediately
	// restart a working hive. It will roll at the next daily ET window
	// (autoUpgradeDailyHour, currently 13:00 / 1pm ET). The
	// operator who wants it now still has the explicit Upgrade button, which
	// goes through handleUpgradeHive and is never gated by mode.
	// The kill switch does not block saving the PREFERENCE (auto-upgrade
	// on/off is configuration, not delivery) — only the immediate trigger
	// below. While paused, triggerAutoUpgrades stays suppressed anyway, and
	// the hive upgrades after an admin resumes.
	spokePauseSw, spokesPaused := s.spokeUpgradesPaused()
	if body.AutoUpgrade && spokesPaused {
		s.logger.Info("auto-upgrade initial trigger suppressed — spoke upgrades are paused",
			"hive_id", id, "paused_by", spokePauseSw.By, "paused_at", spokePauseSw.At)
	}
	if body.AutoUpgrade && !spokesPaused && normalizeAutoUpgradeMode(body.Mode) == AutoUpgradeModeInstant {
		s.mu.RLock()
		var currentSHA, branch string
		for _, reg := range s.registry.Hives {
			if reg.ID == id {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				break
			}
		}
		s.mu.RUnlock()
		branch = s.upgradeBranchOrDefault(branch)
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA != "" && currentSHA != "" && !sameCommit(currentSHA, latestSHA) {
			s.logger.Info("audit: auto-upgrade initial trigger", "hive_id", id, "from", currentSHA, "to", latestSHA)
			s.mu.Lock()
			for i := range s.registry.Hives {
				if s.registry.Hives[i].ID == id {
					s.beginUpgrade(i, latestSHA)
					break
				}
			}
			s.mu.Unlock()
			hiveCluster := s.clusterForHive(h)
			if hiveCluster != nil {
				ns := "hive-hosted-" + id
				cmd := kubectlForCluster(hiveCluster, "rollout", "restart", "deployment/hive", "-n", ns)
				if out, err := cmd.CombinedOutput(); err != nil {
					s.logger.Warn("auto-upgrade initial trigger failed (will retry via heartbeat)", "hive", id, "cluster", hiveCluster.ID, "output", string(out))
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t,"auto_upgrade_mode":%q}`, body.AutoUpgrade, normalizeAutoUpgradeMode(body.Mode))
}

// clusterRecentlyUnreachable reports whether the hub failed to reach this
// cluster recently enough that another kubectl attempt would just burn the
// timeout again. Callers should go straight to the heartbeat fallback instead.
func (s *HubServer) clusterRecentlyUnreachable(clusterID string) bool {
	if clusterID == "" {
		return false
	}
	// A pull-only cluster is unreachable BY DECLARATION and permanently, so it
	// never has to be learned the expensive way. This breaker exists because
	// discovering unreachability costs a full dial timeout per hive per cycle —
	// ~90s a call, with a pool of hives serialising into tens of minutes of
	// blocking. Saying so in clusters.json means that price is never paid even
	// once, and every caller that already consults this breaker is covered
	// without needing its own pull-only check.
	if c, ok := s.clusters[clusterID]; ok && c.PullOnly {
		return true
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	until, ok := s.clusterUnreachableUntil[clusterID]
	return ok && time.Now().Before(until)
}

// markClusterUnreachable starts (or extends) the kubectl suppression window for
// a cluster the hub just failed to dial.
func (s *HubServer) markClusterUnreachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	if s.clusterUnreachableUntil == nil {
		s.clusterUnreachableUntil = make(map[string]time.Time)
	}
	s.clusterUnreachableUntil[clusterID] = time.Now().Add(clusterUnreachableTTL)
}

// markClusterReachable clears any suppression after a kubectl call succeeds, so
// a cluster that recovers is used immediately rather than waiting out the TTL.
func (s *HubServer) markClusterReachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	delete(s.clusterUnreachableUntil, clusterID)
}

// PUSH PATH RETIRED. rolloutRestartHive() used to live here and issued
// `kubectl rollout restart deployment/hive` into a spoke's cluster. Every
// caller has been converted to the pull model — the hub records a target and
// the spoke collects it on its own outbound heartbeat — so the function is
// gone rather than left dormant.
//
// WHY DELETED RATHER THAN FLAG-DISABLED. A disabled flag would keep the hub's
// need for write-capable kubeconfigs alive in the code, which is precisely the
// standing blast radius this change exists to remove: compromise the hub and
// you inherit write access into every reachable spoke cluster. A flag also
// leaves two live delivery models with no stated direction, and the dormant
// one inevitably rots — it would be the untested path on the day someone
// flipped it back. Deleting it makes the direction unambiguous and lets the
// operator drop or downgrade those kubeconfigs to read-only.
//
// The cost is real and accepted: delivery is now bounded by the heartbeat
// interval instead of being immediate. Nothing is lost in reliability — the
// heartbeat was already the only path that worked for firewalled clusters, and
// the spoke-side self-upgrade is mature (retry budget, per-target attempt
// marker, terminal failure reported back to the hub).

func (s *HubServer) triggerAutoUpgrades() {
	// Admin kill switch: while spoke upgrades are paused this entire reconciler
	// is a no-op — it must start no new upgrades, issue no kubectl restarts,
	// and (critically) not re-arm heartbeatUpgrade through its stale-recovery
	// path, which otherwise re-delivers in-flight targets every cycle. The
	// registry latches and armed targets are left untouched so resuming picks
	// up exactly where the fleet paused.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		s.logger.Debug("auto-upgrade reconciler suppressed — spoke upgrades are paused",
			"paused_by", sw.By, "paused_at", sw.At)
		return
	}
	hives := listSaaSHives()
	// Upgrade waves: bound how many hives may be UPGRADING per cluster at
	// once. A merge used to roll every behind hive simultaneously — observed
	// live as a fleet-wide restart inside minutes, an image-pull + PVC IO
	// storm. Count the in-flight upgrades per cluster first; the arming gate
	// below starts new upgrades only while a cluster is under its wave size.
	// Recovery/latch-clearing paths are deliberately NOT bounded (corrective,
	// not disruptive), and wait-healthy is implicit: Upgrading clears when a
	// spoke reports the target reached, freeing wave slots for the next tick.
	upgradingByCluster := make(map[string]int)
	s.mu.RLock()
	upgradingIDs := make(map[string]bool)
	for _, reg := range s.registry.Hives {
		if reg.Upgrading {
			upgradingIDs[reg.ID] = true
		}
	}
	s.mu.RUnlock()
	for i := range hives {
		if upgradingIDs[hives[i].ID] {
			upgradingByCluster[clusterIDForHive(&hives[i])]++
		}
	}
	waveSize := upgradeWaveSize()
	for _, h := range hives {
		s.mu.RLock()
		var currentSHA, branch, upgradeTarget, imageRef, lastHeartbeat string
		var alreadyUpgrading bool
		var upgradeStartedAt time.Time
		for _, reg := range s.registry.Hives {
			if reg.ID == h.ID {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				alreadyUpgrading = reg.Upgrading
				upgradeTarget = reg.UpgradeTarget
				upgradeStartedAt = reg.UpgradeStartedAt
				imageRef = reg.ImageRef
				lastHeartbeat = reg.LastHeartbeat
				break
			}
		}
		s.mu.RUnlock()
		// Resolve against the hub's own branch when the hive has not reported
		// one, never a hardcoded "v2" — see upgradeBranchOrDefault. A hardcoded
		// v2 is what armed 0b78dc0 (a v2-only commit) at placeholders on a v4
		// hub.
		branch = s.upgradeBranchOrDefault(branch)
		if alreadyUpgrading {
			// Floating-tag convergence. A hive whose Deployment tracks a MUTABLE
			// tag (…-latest) has no stable target commit: a restart re-pulls the
			// tag and lands on whatever CI last published, so its reported GitHash
			// chases an ever-moving branch HEAD and can never equal the SPECIFIC
			// commit the hub armed. Left to the stale-recovery path below, that
			// hive is re-armed and rolled every staleUpgradeTimeout forever —
			// restarting the spoke pod each cycle for no benefit. Once such a hive
			// reports it is running the image-verified latest for its branch, it IS
			// up to date; clear the latch here instead of advancing the target.
			// Commit-pinned hives are unaffected — their tag resolves to exactly
			// one build, so the specific-target check still governs them.
			registryLatestSHA := getLatestSHAForBranch(branch)
			if imageTagIsMutable(imageRef) && registryLatestSHA != "" &&
				sameCommit(currentSHA, registryLatestSHA) {
				s.mu.Lock()
				for i := range s.registry.Hives {
					if s.registry.Hives[i].ID == h.ID {
						s.clearUpgradeLatch(i)
						break
					}
				}
				delete(s.heartbeatUpgrade, h.ID)
				s.mu.Unlock()
				s.logger.Info("clearing upgrade latch — floating-tag hive is at latest",
					"hive", h.ID, "branch", branch, "sha", currentSHA, "image_ref", imageRef)
				continue
			}
			// Target reached or SURPASSED. The equality checks above cannot see
			// a spoke that landed AHEAD of the armed target: a floating-tag
			// re-pull delivers whatever CI last published, so when the branch
			// advanced between arming and pulling — and again before this cycle
			// — the reported hash equals neither the target nor the current
			// latest. Without this ancestry check the stale-recovery below
			// re-arms the ORIGINAL stale pin forever (manual upgrades never
			// advance their target), re-stamping the registry latch and
			// re-instructing a commit the spoke can never report — the
			// vllmd-13 wedge. Cache-only + background resolve, so this loop
			// never blocks on the network; an unresolved pair clears on a
			// later cycle.
			if upgradeTarget != "" && currentSHA != "" &&
				commitAtOrAheadOfTarget(currentSHA, upgradeTarget, s.logger) {
				s.mu.Lock()
				for i := range s.registry.Hives {
					if s.registry.Hives[i].ID == h.ID {
						s.clearUpgradeLatch(i)
						break
					}
				}
				delete(s.heartbeatUpgrade, h.ID)
				s.mu.Unlock()
				s.logger.Info("clearing upgrade latch — hive is at or ahead of its armed target",
					"hive", h.ID, "branch", branch, "sha", currentSHA, "target", upgradeTarget)
				continue
			}
			// Latched-upgrade recovery runs for EVERY hive, deliberately BEFORE
			// the AutoUpgrade gate below (#2476). The registry latch
			// (Upgrading/UpgradeTarget/UpgradeStartedAt) is durable, but
			// heartbeatUpgrade — the map that actually delivers the instruction
			// — is in-memory; when this recovery lived behind
			// `if !h.AutoUpgrade { continue }`, a hub restart orphaned every
			// manually upgraded hive forever: latched "Upgrading" in the
			// registry, zero instructions on the wire.
			upgradeAge := time.Since(upgradeStartedAt)
			// Zero UpgradeStartedAt means the timestamp was lost (heartbeats
			// used to wipe it on rebuild) — treat as stale so already-stuck
			// hives self-heal instead of upgrading forever.
			isStale := upgradeStartedAt.IsZero() || upgradeAge > staleUpgradeTimeout

			if isStale {
				// UNCOLLECTIBLE: abandon, do not re-arm. This is the branch that
				// produced the measured wedge — 26 hives re-arming, stale_minutes
				// climbing past 146 and still rising — because re-arming an
				// instruction nothing will ever pick up reproduces the identical
				// no-op every staleUpgradeTimeout, forever, while beginUpgrade()
				// preserves the original start clock so the elapsed only ever
				// grows. The retry budget in sweepOrphanedUpgrades() cannot bound
				// it: these hives never heartbeated, so evaluateOrphanedUpgrade()
				// bails on the liveness test and the budget is never spent.
				//
				// Clearing the latch outright is what stops staleness
				// accumulating. The hive stays on its old SHA — truthful and
				// visible — instead of misreporting as perpetually "Upgrading"
				// while reading offline. Nothing is lost: the instruction was
				// never being collected, so dropping it forfeits nothing. If the
				// spoke later starts heartbeating, the normal arming path picks
				// it up on the next poll.
				if !upgradeCollectible(lastHeartbeat, time.Now()) {
					reason := uncollectibleUpgradeReason(lastHeartbeat)
					s.logger.Warn("abandoning stale upgrade — hive cannot collect it, not re-arming",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", upgradeTarget, "last_heartbeat", orDash(lastHeartbeat),
						"reason", reason)
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							s.clearUpgradeLatch(i)
							break
						}
					}
					delete(s.heartbeatUpgrade, h.ID)
					s.mu.Unlock()
					s.noteUncollectibleUpgrade(h.ID, upgradeTarget, reason)
					continue
				}
				// Upgrade has been stuck longer than staleUpgradeTimeout.
				// Recover it. Two things can be wrong: (a) the target SHA
				// contains a crashing bug a newer commit fixes — advance the
				// target to latest; (b) the kubectl rollout never reached the
				// spoke (e.g. the hub can't route to the hive's cluster API),
				// so the upgrade was never actually delivered.
				//
				// The heartbeat fallback (heartbeatUpgrade → the spoke
				// self-restarts on its next heartbeat) is the ONLY path that
				// works when kubectl can't reach the cluster, so re-arm it
				// unconditionally for a stale upgrade — not only when the
				// target advances. Previously, when the target already equalled
				// latest, this branch was skipped entirely and the hive stayed
				// latched-upgrading forever behind an unreachable kubectl.
				recoverTarget := upgradeTarget
				if h.AutoUpgrade {
					// Target advancement is an auto-upgrade behaviour: those
					// hives always chase latest. A manual upgrade keeps exactly
					// the target that was requested — with AutoUpgrade off,
					// silently delivering a newer build than the one the owner
					// clicked would override their setting.
					if latestSHA := getLatestSHAForBranch(branch); latestSHA != "" && latestSHA != upgradeTarget {
						recoverTarget = latestSHA
					}
				}
				if recoverTarget != upgradeTarget {
					s.logger.Warn("advancing upgrade target for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"old_target", upgradeTarget, "new_target", recoverTarget)
				} else {
					s.logger.Warn("re-arming heartbeat fallback for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", recoverTarget)
				}
				if recoverTarget != "" {
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							// Route through beginUpgrade so the start time
							// SURVIVES a same-target re-arm (the crash-loop retry
							// case): re-arming delivery for the same target must
							// not reset the elapsed clock, or a thrashing upgrade
							// never crosses staleUpgradeTimeout and the stuck-
							// upgrade alert never fires. A genuinely advanced
							// target (recoverTarget != old UpgradeTarget) is a new
							// upgrade and DOES get a fresh clock. A lost/zero start
							// is re-stamped either way, which self-heals the
							// ibm-alchemy zero-timestamp wedge.
							s.beginUpgrade(i, recoverTarget)
							break
						}
					}
					s.heartbeatUpgrade[h.ID] = recoverTarget
					s.mu.Unlock()
					// PULL ONLY — the heartbeat armed above IS the delivery, as
					// the previous comment here already conceded ("the heartbeat
					// fallback armed above is what actually delivers the
					// upgrade"). The kubectl push that followed it was pure
					// latency optimisation and is deliberately gone; see the
					// push-path retirement note in pullonly_upgrade.go.
					continue
				}
			}

			// Not stale — keep the original target so the hive can satisfy it.
			// Re-populate the heartbeatUpgrade map in case the hub restarted.
			//
			// SAME COLLECTIBILITY GATE AS THE STALE BRANCH ABOVE. Arming is
			// arming: re-populating the map for a hive that cannot collect
			// reproduces the wedge the stale branch just abandoned, only
			// sooner. Because this branch runs on EVERY poll while the hive is
			// latched, it re-arms roughly every 2 minutes, whereas abandonment
			// waits out staleUpgradeTimeout — so without this check the fix
			// merely races the timeout and the uncollectible hive stays armed.
			// The predicate is upgradeCollectible(), reused rather than
			// restated, so there is one definition of "can collect".
			if upgradeTarget != "" && !upgradeCollectible(lastHeartbeat, time.Now()) {
				s.logger.Debug("not re-arming in-progress upgrade — hive cannot collect it",
					"hive", h.ID, "target", upgradeTarget,
					"last_heartbeat", orDash(lastHeartbeat))
				continue
			}
			hiveCluster := s.clusterForHive(&h)
			if hiveCluster != nil && !hiveCluster.InCluster {
				if upgradeTarget != "" {
					s.mu.Lock()
					s.heartbeatUpgrade[h.ID] = upgradeTarget
					s.mu.Unlock()
				}
			}
			s.logger.Debug("skipping target advance — upgrade still in progress",
				"hive", h.ID, "current", currentSHA)
			continue
		}
		// Everything below STARTS a new upgrade, which only auto-upgrade hives
		// opt into. The recovery above must stay ahead of this gate — see #2476.
		if !h.AutoUpgrade {
			continue
		}
		// Claim-in-flight latch (#95). A placeholder that has just been ASSIGNED
		// but whose claim has not yet been delivered (Status==assigned &&
		// !ClaimDelivered) is mid-wiring: the spoke is receiving its org/repos/
		// ACMM over successive heartbeats. Rolling its pod onto a new image now
		// can wedge it — the classic EPM dead-end (task #94). DEFER (do not
		// cancel) the auto-upgrade until the claim lands. This is self-limiting:
		// it releases the moment ClaimDelivered flips true, and if the claim
		// never completes, sweepStuckAssignments returns the slot to available
		// after assignStuckResetTimeout — either way this latch clears and the
		// next cycle upgrades normally. A hard image pin is unaffected: pins are
		// delivered via UpgradeTarget through the recovery path ABOVE this gate,
		// not started here, so a pin still wins.
		if assignmentInFlight(&h) {
			s.logger.Debug("auto-upgrade deferred — claim in flight",
				"hive_id", h.ID, "status", h.Status, "assigned_at", h.AssignedAt)
			continue
		}
		// Scheduling gate. Instant-mode hives (and every legacy record, whose
		// mode is empty) pass straight through, so this changes nothing for the
		// existing fleet. Daily-mode hives are held until the first cycle at or
		// after autoUpgradeDailyHour ET and released only once per ET day.
		// Evaluated BEFORE any of the work below so a held hive costs nothing.
		decision := shouldAutoUpgradeNow(h.AutoUpgradeMode, h.AutoUpgradeLastFired, time.Now())
		if !decision.Allowed {
			s.logger.Debug("auto-upgrade held by schedule",
				"hive_id", h.ID, "mode", h.AutoUpgradeMode, "reason", decision.Reason)
			continue
		}
		// Skip hives that are actively provisioning or in error state.
		// Empty status means the hive predates the provisioning system — treat as eligible.
		if h.Status == "provisioning" || h.Status == "error" {
			continue
		}
		if currentSHA == "" {
			continue
		}
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA == "" || sameCommit(currentSHA, latestSHA) {
			continue
		}
		// Merge-driven debounce (#5391). Reached ONLY on the automatic
		// chase-latest path: everything that starts an upgrade for an operator
		// — a manual "Upgrade now" (upgradeHiveHandler), a bulk upgrade
		// (saas_bulk.go), and a hard image pin (delivered as UpgradeTarget
		// through the stale-recovery branch ABOVE the `if !h.AutoUpgrade` gate)
		// — arms s.heartbeatUpgrade directly and never enters this loop body.
		// So an operator's upgrade and a pin stay IMMEDIATE by construction,
		// and only the merge-frequency-driven roll is held.
		//
		// Placed after every eligibility gate above so a hive that would not
		// upgrade anyway never arms a window, and before the wave gate and the
		// fire-date persistence below so a debounced hive costs no wave slot and
		// keeps its daily/weekly window open.
		debounce := shouldDebounceAutoUpgrade(
			autoUpgradeDebounceState{
				Target:       h.AutoUpgradePendingTarget,
				ArmedAt:      h.AutoUpgradePendingSince,
				FirstArmedAt: h.AutoUpgradePendingFirst,
				Collapsed:    h.AutoUpgradeCollapsed,
			},
			latestSHA, autoUpgradeDebounceInterval(), autoUpgradeMaxHold(), time.Now())
		if !debounce.Allowed {
			// Persist the (possibly just-replaced) pending target so a hub
			// restart inside the window resumes it rather than dropping it.
			s.persistUpgradeDebounceState(&h, debounce.State)
			s.logger.Info("auto-upgrade debounced — holding for a quiet branch",
				"hive_id", h.ID, "branch", branch,
				"target", debounce.State.Target, "current", currentSHA,
				"collapsed", debounce.State.Collapsed,
				"debounce", autoUpgradeDebounceInterval(),
				"reason", debounce.Reason)
			continue
		}
		if debounce.Collapsed > 0 {
			// Report the collapse. Silent batching would trade one invisible
			// problem for another: without this line, N merges producing one
			// roll is indistinguishable from N-1 upgrades having been lost.
			s.logger.Info("auto-upgrade debounce collapsed a merge burst into one roll",
				"hive_id", h.ID, "branch", branch,
				"merges_collapsed", debounce.Collapsed+1,
				"final_target", latestSHA, "current", currentSHA,
				"debounce", autoUpgradeDebounceInterval())
		}
		hiveCluster := s.clusterForHive(&h)
		if hiveCluster == nil {
			s.logger.Warn("auto-upgrade skipped — no cluster config", "hive_id", h.ID, "cluster_id", h.ClusterID)
			continue
		}
		// Do not ARM an upgrade this hive cannot COLLECT. Delivery is PULL: the
		// spoke reads UpgradeTo off its own outbound heartbeat response and then
		// patches its own Deployment with its own ServiceAccount. A hive that
		// never heartbeats therefore never picks the instruction up, while
		// Upgrading=true latches on the hub: the stale-recovery branch above
		// re-arms every staleUpgradeTimeout, beginUpgrade() preserves the
		// original start clock, and the elapsed grows without bound. The orphan
		// sweep's retry budget cannot rescue it — a hive that never heartbeated
		// fails evaluateOrphanedUpgrade()'s liveness test, so the budget is never
		// spent and exhaustion never converts it to a visible failure. See
		// pullonly_upgrade.go for the full measured loop.
		//
		// This is deliberately NOT gated on cluster reachability. The hub's
		// kubectl path is only a fast-path optimisation, so a pull-only cluster
		// is irrelevant here; gating on it would silently disable auto-upgrade
		// for the 40+ pull-only spokes that heartbeat perfectly well.
		//
		// Refused LOUDLY, never silently: a hive with auto_upgrade=true that
		// simply never upgrades is indistinguishable from one already at latest,
		// which is how this stayed unnoticed.
		if !upgradeCollectible(lastHeartbeat, time.Now()) {
			reason := uncollectibleUpgradeReason(lastHeartbeat)
			s.logger.Warn("auto-upgrade not armed — hive cannot collect the instruction",
				"hive_id", h.ID, "cluster", hiveCluster.ID, "branch", branch,
				"from", currentSHA, "would_have_targeted", latestSHA,
				"last_heartbeat", orDash(lastHeartbeat), "reason", reason)
			s.noteUncollectibleUpgrade(h.ID, latestSHA, reason)
			continue
		}
		// Wave gate — evaluated AFTER every eligibility check so a slot is
		// only ever spent on a hive that would actually arm, and BEFORE the
		// fire-date persistence so a deferred daily/weekly hive keeps its
		// window open and simply boards a later wave this same day.
		if waveSize > 0 && upgradingByCluster[hiveCluster.ID] >= waveSize {
			s.logger.Debug("auto-upgrade deferred — cluster upgrade wave is full",
				"hive_id", h.ID, "cluster", hiveCluster.ID,
				"in_flight", upgradingByCluster[hiveCluster.ID], "wave_size", waveSize)
			continue
		}
		upgradingByCluster[hiveCluster.ID]++
		// Record the day's fire BEFORE kicking the rollout. Persisting first
		// means a hub crash between here and the restart cannot cause a second
		// upgrade for the same ET day; at worst the hive waits for tomorrow's
		// window, which is the conservative direction for a "don't disturb it"
		// mode. Only daily-mode hives carry a fire date (decision.FireDate is
		// empty for instant), and a save failure is logged but not fatal — the
		// upgrade itself still proceeds.
		if decision.FireDate != "" {
			stored := loadSaaSHive(h.ID)
			if stored == nil {
				stored = &h
			}
			stored.AutoUpgradeLastFired = decision.FireDate
			if err := saveSaaSHive(stored); err != nil {
				s.logger.Warn("failed to persist auto-upgrade fire date — a hub restart today could re-fire",
					"hive_id", h.ID, "date", decision.FireDate, "error", err)
			}
		}
		// Clear the debounce record now the roll is actually going out. Cleared
		// HERE, after every gate that could still `continue`, so a hive turned
		// away by the wave gate keeps its pending target and simply boards a
		// later wave instead of re-arming a fresh window each cycle. Clearing
		// before the rollout (like the fire date above) means a hub crash in
		// between costs at most a re-armed window, never a duplicate roll.
		s.persistUpgradeDebounceState(&h, autoUpgradeDebounceState{})
		// The hive is deliverable again — drop any suppressed-refusal memory so a
		// future undeliverable episode is reported afresh rather than swallowed.
		s.forgetUncollectibleUpgrade(h.ID)
		s.logger.Info("audit: auto-upgrade triggered", "hive_id", h.ID, "branch", branch, "from", currentSHA, "to", latestSHA, "cluster", hiveCluster.ID, "mode", normalizeAutoUpgradeMode(h.AutoUpgradeMode))
		s.recordTimeline(h.ID, TimelineUpgradeStarted,
			fmt.Sprintf("auto-upgrade triggered on %s: %s → %s", branch, orDash(currentSHA), latestSHA), "auto-upgrade")
		s.mu.Lock()
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == h.ID {
				s.beginUpgrade(i, latestSHA)
				break
			}
		}
		s.mu.Unlock()
		// PULL ONLY — no kubectl push. Arming the heartbeat is the delivery:
		// the spoke reads UpgradeTo off its next heartbeat response and patches
		// its own Deployment with its own ServiceAccount (cmd/hive/main.go →
		// self_upgrade.go). The former `rolloutRestartHive` call here was only a
		// latency optimisation, and it is deliberately gone: keeping it would
		// require the hub to hold write-capable kubeconfigs into every spoke
		// cluster, which is a large standing blast radius for a few seconds of
		// speed. See the push-path retirement note in pullonly_upgrade.go.
		//
		// The trade is real and accepted: delivery is now bounded by the
		// heartbeat interval rather than being immediate.
		s.mu.Lock()
		s.heartbeatUpgrade[h.ID] = latestSHA
		// Keep Upgrading=true so the dashboard shows the correct state.
		s.mu.Unlock()
	}
}
