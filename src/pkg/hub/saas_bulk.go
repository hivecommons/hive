package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Bulk fleet operations for the hub dashboard's "My Hives" list.
//
// Every bulk action is a thin fan-out over the SAME logic the single-hive
// handlers use (handleUpgradeHive, handleToggleAutoUpgrade, handleSwitchBranch).
// The rules that make this safe to expose:
//
//  1. Authorization is re-derived PER HIVE from the hub's own store. The
//     client-supplied ID list is treated as untrusted input: every ID is
//     loaded with loadSaaSHive and checked against the same
//     "owner or hub admin" predicate the single-hive handlers apply. A hive
//     the caller may not touch is reported as a per-hive error, never applied.
//  2. Partial failure is the normal case (a hive may be offline, unclaimed,
//     or on a firewalled cluster), so the response is a per-hive result list
//     rather than a single boolean.
//  3. Blast radius is bounded by maxBulkHivesPerRequest and the work is run
//     through a fixed-size worker pool (bulkActionConcurrency) so a 42-hive
//     fleet never becomes 42 simultaneous kubectl processes.

// maxBulkHivesPerRequest caps how many hives a single bulk request may target.
// The fleet is ~42 hives across 2 clusters, so this comfortably covers
// "select all" today while keeping a malicious or buggy client from asking the
// hub to shell out to kubectl an unbounded number of times.
const maxBulkHivesPerRequest = 100

// bulkActionConcurrency bounds how many hives are acted on at once. Each unit
// of work can shell out to kubectl (up to upgradeKubectlTimeout each), so this
// trades wall-clock time for a predictable ceiling on concurrent subprocesses
// and API-server load. 4 keeps a 100-hive worst case well inside
// bulkRequestBudget while never stampeding a single cluster's API server.
const bulkActionConcurrency = 4

// bulkRequestBudget bounds the total wall-clock time of one bulk request so a
// batch of unreachable clusters can never hold an HTTP handler open forever.
// Worst case per hive is upgradeKubectlTimeout (15s); with the concurrency
// above, 100 hives need ~100/4*15s = 375s, so this leaves headroom without
// being unbounded.
const bulkRequestBudget = 8 * time.Minute

// maxBulkRequestBodyBytes caps the bulk request body. A request is a short JSON
// object plus at most maxBulkHivesPerRequest hive IDs, so 64KiB is generous.
const maxBulkRequestBodyBytes = 64 * 1024

// Bulk action names accepted by handleBulkHiveAction.
const (
	bulkActionRestart            = "restart"
	bulkActionUpgrade            = "upgrade"
	bulkActionEnableAutoUpgrade  = "enable-auto-upgrade"
	bulkActionDisableAutoUpgrade = "disable-auto-upgrade"
	// bulkActionDailyAutoUpgrade enables auto-upgrade in daily mode. The plain
	// enable action keeps meaning INSTANT so existing muscle memory and any
	// existing API callers are unchanged.
	bulkActionDailyAutoUpgrade = "daily-auto-upgrade"
	// bulkActionWeeklyAutoUpgrade enables auto-upgrade in weekly mode
	// (Tuesday at 1pm ET — see AutoUpgradeModeWeekly).
	bulkActionWeeklyAutoUpgrade = "weekly-auto-upgrade"
	bulkActionSwitchBranch      = "switch-branch"
)

// BulkHiveRequest is the body of POST /api/saas/hives/bulk.
type BulkHiveRequest struct {
	Action  string   `json:"action"`
	HiveIDs []string `json:"hive_ids"`
	// Branch is only read by the switch-branch action.
	Branch string `json:"branch"`
}

// BulkHiveResult is the outcome for ONE hive in a bulk request. Ok=false always
// carries a human-readable Error; Via records the delivery path actually used.
// Under the pull model this is "heartbeat" for every cluster action ("store" for
// ones that only write hub-side state), which is also the honest signal to the
// UI that success means "queued for the spoke's next check-in", not "already
// rolled".
type BulkHiveResult struct {
	HiveID string `json:"hive_id"`
	Ok     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Via    string `json:"via,omitempty"`
}

// Delivery paths reported in BulkHiveResult.Via.
const (
	// bulkViaKubectl was removed with the push path: no bulk action shells out
	// to a spoke cluster any more, so no result can be delivered "via kubectl".
	bulkViaHeartbeat = "heartbeat"
	bulkViaStore     = "store"
)

// bulkDefaultBranch is the branch assumed when a hive has never reported one.
// Matches the same "" -> "v2" default the single-hive upgrade paths apply.
const bulkDefaultBranch = "v2"

// bulkHiveImageRepo is the container image repository the hosted hives run,
// matching the image handleSwitchBranch sets and docker.yml publishes.
const bulkHiveImageRepo = "ghcr.io/hivecommons/hive"

// registerBulkRoutes wires the bulk endpoint. Kept separate from
// registerSaaSRoutes so this feature's diff does not collide with the
// concurrently-edited route table.
func (s *HubServer) registerBulkRoutes() {
	s.mux.HandleFunc("POST /api/saas/hives/bulk", s.requireAuth(s.handleBulkHiveAction))
}

// authorizeBulkHive re-derives authorization for one hive from the hub's own
// store, mirroring the owner check every single-hive handler performs
// (!canonicalEqual(h.Owner, username) && !isHubAdmin(username)). It returns the loaded
// hive on success, or a reason string on refusal. The client's list is never
// trusted: an ID that does not resolve, or resolves to someone else's hive, is
// refused here rather than acted on.
func authorizeBulkHive(id, username string) (*SaaSHive, string) {
	if username == "" {
		return nil, "not authenticated"
	}
	h := loadSaaSHive(id)
	if h == nil {
		return nil, "hive not found"
	}
	if !canonicalEqual(h.Owner, username) && !isHubAdmin(username) {
		return nil, "not authorized for this hive"
	}
	return h, ""
}

func (s *HubServer) handleBulkHiveAction(w http.ResponseWriter, r *http.Request) {
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

	username := s.getAuthUser(r)
	if username == "" {
		writeBulkError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBulkRequestBodyBytes)
	var body BulkHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeBulkError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch body.Action {
	case bulkActionRestart, bulkActionUpgrade, bulkActionEnableAutoUpgrade,
		bulkActionDisableAutoUpgrade, bulkActionDailyAutoUpgrade, bulkActionSwitchBranch:
	default:
		writeBulkError(w, http.StatusBadRequest, "unknown bulk action")
		return
	}

	// Admin kill switch: bulk upgrade / branch-switch are the same image-change
	// deliveries the single-hive handlers refuse while spoke upgrades are
	// paused, so they get the same explicit 409 — for the whole batch, before
	// any hive is touched. Auto-upgrade preference changes and plain restarts
	// (no image target) pass through.
	if body.Action == bulkActionUpgrade || body.Action == bulkActionSwitchBranch {
		if sw, paused := s.spokeUpgradesPaused(); paused {
			writeBulkError(w, http.StatusConflict, upgradePauseRefusal("spoke", sw))
			return
		}
	}

	// De-duplicate while preserving the client's order: a repeated ID would
	// otherwise restart the same hive twice within one request.
	ids := make([]string, 0, len(body.HiveIDs))
	seen := map[string]bool{}
	for _, id := range body.HiveIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeBulkError(w, http.StatusBadRequest, "no hives selected")
		return
	}
	if len(ids) > maxBulkHivesPerRequest {
		writeBulkError(w, http.StatusBadRequest,
			fmt.Sprintf("too many hives in one request (%d, max %d)", len(ids), maxBulkHivesPerRequest))
		return
	}

	if body.Action == bulkActionSwitchBranch {
		if body.Branch == "" {
			writeBulkError(w, http.StatusBadRequest, "branch is required for switch-branch")
			return
		}
		// Validate ONCE for the whole batch rather than per hive — the target
		// branch is the same for every selected hive, and the fallback check
		// hits the GitHub API.
		if !s.validateBulkBranch(body.Branch) {
			writeBulkError(w, http.StatusBadRequest,
				"unknown branch (does not exist on the hive repo)")
			return
		}
	}

	s.logger.Info("audit: bulk hive action requested",
		"action", body.Action, "by", username, "count", len(ids), "hives", ids, "branch", body.Branch)

	results := s.runBulkAction(body.Action, body.Branch, ids, username)

	okCount := 0
	for _, res := range results {
		if res.Ok {
			okCount++
		}
	}
	s.logger.Info("audit: bulk hive action completed",
		"action", body.Action, "by", username, "requested", len(ids),
		"succeeded", okCount, "failed", len(ids)-okCount)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"action":    body.Action,
		"requested": len(ids),
		"succeeded": okCount,
		"failed":    len(ids) - okCount,
		"results":   results,
	})
}

// runBulkAction fans the action out over a bounded worker pool and returns one
// result per requested ID, in the order the IDs were given.
func (s *HubServer) runBulkAction(action, branch string, ids []string, username string) []BulkHiveResult {
	results := make([]BulkHiveResult, len(ids))
	deadline := time.Now().Add(bulkRequestBudget)

	workers := bulkActionConcurrency
	if len(ids) < workers {
		workers = len(ids)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				id := ids[idx]
				if time.Now().After(deadline) {
					// The request budget is spent — report the remainder
					// honestly rather than holding the connection open.
					results[idx] = BulkHiveResult{HiveID: id, Ok: false,
						Error: "skipped — bulk request time budget exhausted"}
					continue
				}
				results[idx] = s.applyBulkAction(action, branch, id, username)
			}
		}()
	}
	for i := range ids {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

// applyBulkAction authorizes ONE hive and applies the action, reusing the exact
// mechanics of the corresponding single-hive handler.
func (s *HubServer) applyBulkAction(action, branch, id, username string) BulkHiveResult {
	h, reason := authorizeBulkHive(id, username)
	if h == nil {
		return BulkHiveResult{HiveID: id, Ok: false, Error: reason}
	}
	switch action {
	case bulkActionRestart, bulkActionUpgrade:
		return s.bulkRestartOrUpgrade(h, id, username, action)
	case bulkActionEnableAutoUpgrade:
		return s.bulkSetAutoUpgrade(h, id, username, true, AutoUpgradeModeInstant)
	case bulkActionDailyAutoUpgrade:
		return s.bulkSetAutoUpgrade(h, id, username, true, AutoUpgradeModeDaily)
	case bulkActionWeeklyAutoUpgrade:
		return s.bulkSetAutoUpgrade(h, id, username, true, AutoUpgradeModeWeekly)
	case bulkActionDisableAutoUpgrade:
		return s.bulkSetAutoUpgrade(h, id, username, false, AutoUpgradeModeInstant)
	case bulkActionSwitchBranch:
		return s.bulkSwitchBranch(h, id, username, branch)
	}
	return BulkHiveResult{HiveID: id, Ok: false, Error: "unknown bulk action"}
}

// bulkRestartOrUpgrade restarts or upgrades one hive, PULL ONLY — the hub never
// kubectls into the spoke's cluster.
//
// An upgrade arms s.heartbeatUpgrade[id] with the branch's latest SHA and marks
// the registry Upgrading; a restart arms the durable RequestedRestartAt, which
// the heartbeat path turns into resp.RestartSpoke. Either way the spoke applies
// the change to its own Deployment on its next check-in, so this is reported as
// success with Via="heartbeat".
//
// This is the same mechanism that was previously the "fallback" for firewalled
// clusters; it is now the only path, on every cluster. Delivery is therefore
// bounded by the heartbeat interval rather than immediate.
func (s *HubServer) bulkRestartOrUpgrade(h *SaaSHive, id, username, action string) BulkHiveResult {
	cluster := s.clusterForHive(h)
	if cluster == nil {
		return BulkHiveResult{HiveID: id, Ok: false, Error: "no cluster config for this hive"}
	}

	// The branch a hive actually runs is reported by heartbeat into the
	// registry, not stored on the SaaS record — same lookup
	// handleToggleAutoUpgrade performs before computing an upgrade target.
	s.mu.RLock()
	branch := ""
	for _, reg := range s.registry.Hives {
		if reg.ID == id {
			branch = reg.GitBranch
			break
		}
	}
	s.mu.RUnlock()
	if branch == "" {
		branch = bulkDefaultBranch
	}
	latestSHA := getLatestSHAForBranch(branch)

	// PULL ONLY — no kubectl push. An upgrade is delivered by arming
	// heartbeatUpgrade below; a plain restart is delivered by arming the
	// durable RequestedRestartAt, which the heartbeat path turns into
	// resp.RestartSpoke and the spoke applies to itself. Neither needs the hub
	// to hold write access into the spoke's cluster.
	delivered := false
	if action == bulkActionRestart {
		if rh := loadSaaSHive(id); rh != nil {
			rh.RequestedRestartAt = time.Now().UTC().Format(time.RFC3339)
			if err := saveSaaSHive(rh); err != nil {
				s.logger.Warn("bulk restart: failed to arm spoke restart", "hive", id, "error", err)
			} else {
				delivered = true
			}
		}
	}

	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID != id {
			continue
		}
		// Only an upgrade advances the target; a plain restart re-pulls the
		// same tag and must not make the dashboard claim a version change.
		if action == bulkActionUpgrade && latestSHA != "" {
			s.beginUpgrade(i, latestSHA)
		}
		break
	}
	// Unconditional under the pull model: arming the heartbeat IS the delivery
	// for an upgrade, not a fallback for when a push failed.
	if action == bulkActionUpgrade && latestSHA != "" {
		s.heartbeatUpgrade[id] = latestSHA
		delivered = true
	}
	s.mu.Unlock()

	if !delivered && action == bulkActionRestart {
		// Arming the durable RequestedRestartAt is the only thing that can fail
		// here now (a store write), and it is a real error rather than an
		// unreachable-cluster report: the pull path does not depend on the hub
		// being able to reach the cluster at all.
		s.logger.Warn("bulk restart could not be armed", "hive", id, "cluster", cluster.ID)
		return BulkHiveResult{HiveID: id, Ok: false,
			Error: "could not arm the restart for delivery over the heartbeat"}
	}
	if !delivered {
		s.logger.Info("audit: bulk upgrade queued via heartbeat",
			"hive_id", id, "by", username, "target", latestSHA, "cluster", cluster.ID)
		return BulkHiveResult{HiveID: id, Ok: true, Via: bulkViaHeartbeat}
	}
	s.logger.Info("audit: bulk hive "+action, "hive_id", id, "by", username, "cluster", cluster.ID)
	// Pull only — the spoke applies this on its next check-in.
	return BulkHiveResult{HiveID: id, Ok: true, Via: bulkViaHeartbeat}
}

// bulkSetAutoUpgrade mirrors handleToggleAutoUpgrade's persistence: the SaaS
// store and the registry are the source of truth for the dashboard, and the
// preference reaches the spoke on its next heartbeat. No kubectl is involved,
// so this works identically for firewalled clusters.
func (s *HubServer) bulkSetAutoUpgrade(h *SaaSHive, id, username string, enabled bool, mode string) BulkHiveResult {
	h.AutoUpgrade = enabled
	h.AutoUpgradeMode = mode
	// Mode changes clear the day's fire record for the same reason the
	// single-hive handler does — see handleToggleAutoUpgrade.
	h.AutoUpgradeLastFired = ""
	if err := saveSaaSHive(h); err != nil {
		return BulkHiveResult{HiveID: id, Ok: false, Error: "failed to save"}
	}
	s.logger.Info("audit: bulk auto-upgrade toggled",
		"hive_id", id, "auto_upgrade", enabled, "mode", normalizeAutoUpgradeMode(mode), "by", username)
	return BulkHiveResult{HiveID: id, Ok: true, Via: bulkViaStore}
}

// bulkSwitchBranch points one hive at another branch's image. It reuses
// branchToTag (the same sanitization handleSwitchBranch and docker.yml apply)
// and the same heartbeat fallback: when kubectl cannot set the image,
// s.heartbeatSwitchTag[id] is recorded and the spoke — which holds the
// hive-self-upgrade RBAC to patch its own deployment — applies it on its next
// check-in. Branch validity is checked ONCE by the caller, not per hive.
func (s *HubServer) bulkSwitchBranch(h *SaaSHive, id, username, branch string) BulkHiveResult {
	cluster := s.clusterForHive(h)
	if cluster == nil {
		return BulkHiveResult{HiveID: id, Ok: false, Error: "no cluster config for this hive"}
	}
	imageTag := branchToTag(branch) + "-latest"
	image := bulkHiveImageRepo + ":" + imageTag

	// PULL ONLY — no `kubectl set image` push. heartbeatSwitchTag armed below
	// is the delivery: the spoke holds the hive-self-upgrade RBAC to patch its
	// own Deployment and applies the new tag on its next check-in, which the
	// doc comment above already describes as the mechanism. Dropping the push
	// removes the last reason for the hub to hold write-capable kubeconfigs
	// into spoke clusters.
	_ = image
	delivered := false

	s.mu.Lock()
	if !delivered {
		s.heartbeatSwitchTag[id] = imageTag
	}
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, imageTag)
			break
		}
	}
	s.mu.Unlock()

	// Pull only: the switch is delivered by heartbeatSwitchTag, never by a push.
	via := bulkViaHeartbeat
	_ = delivered
	s.logger.Info("audit: bulk hive branch switched",
		"hive_id", id, "branch", branch, "image", image, "by", username, "via", via)
	return BulkHiveResult{HiveID: id, Ok: true, Via: via}
}

// validateBulkBranch resolves whether a branch is a legitimate switch target,
// using the same two-step rule as handleSwitchBranch: prefer the tracked branch
// list, then fall back to a live existence check against the hive repo (which
// also populates the SHA cache, so the branch is tracked from then on).
func (s *HubServer) validateBulkBranch(branch string) bool {
	for _, b := range s.trackedBranchList() {
		if b == branch {
			return true
		}
	}
	fetchBranchSHA(s.logger, branch)
	return getLatestSHAForBranch(branch) != ""
}

// sortedBulkResults returns results ordered failures-first then by hive ID, for
// callers that want a stable, most-actionable-first presentation.
func sortedBulkResults(results []BulkHiveResult) []BulkHiveResult {
	out := make([]BulkHiveResult, len(results))
	copy(out, results)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ok != out[j].Ok {
			return !out[i].Ok
		}
		return out[i].HiveID < out[j].HiveID
	})
	return out
}

func writeBulkError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
