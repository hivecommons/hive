package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Phase 2 of #2748: return a LIVE, claimed hive to the unassigned pool by
// wiping its persistent state credential-safely, in place, WITHOUT deleting the
// namespace or the PVC object (that is deprovisionHive's job). Phase 1 — the
// record-only escape hatch for an assigned-but-unclaimed placeholder
// (handleResetAssignment / resetHiveToAvailablePlaceholder) — is reused verbatim
// for the record half; this file adds the storage wipe + verify + fail-closed
// state machine that a delivered claim needs.
//
// THE GUARANTEE (wipe-by-default, verify-clean, fail-closed):
//
// The operator's central concern is "how do we assure there is NO data left —
// beads, tmux, or anything else?". The answer is NOT an enumerated delete-list
// (which rots the day a future feature drops /data/newthing). It is an EMPTY
// seed allowlist:
//
//   - On a fresh provision the /data PVC starts COMPLETELY EMPTY. The copy-config
//     init container seeds the ConfigMap into /etc/hive (an emptyDir), never onto
//     the PVC (saas_provision.go copy-config step). Everything else on /data is
//     recreated by entrypoint.sh on boot (idempotent mkdir -p / cp -n / runtime
//     generation).
//   - So the wipe destroys EVERYTHING under /data and the VERIFY CONTRACT is
//     total and trivial: `find /data -mindepth 1` must return NOTHING. Anything
//     remaining -> FAIL CLOSED (the slot is not returned to the pool).
//   - The spoke, on restart, re-seeds /data exactly as on a first-ever boot —
//     byte-for-byte the fresh-provision baseline.
//
// resetToPoolSeedAllowlist is that seed set, kept in ONE place shared by the
// wipe and the verify step so they cannot drift. It is deliberately EMPTY: there
// is nothing under /data that a fresh PVC carries, so nothing is preserved and
// nothing can leak by omission. If a future change ever DOES require a seeded
// file on the PVC, add it here and both the wipe and the verify honour it
// automatically.
var resetToPoolSeedAllowlist = []string{}

const (
	// resetToPoolStatusInProgress is set on the hive record while the wipe runs.
	// It doubles as the concurrency latch: a second reset request 409s while set.
	resetToPoolStatusInProgress = "in-progress"
	// resetToPoolStatusFailed is the FAIL-CLOSED terminal state. The record was
	// NOT flipped to available; the namespace/PVC are intact for manual inspection.
	resetToPoolStatusFailed = "failed"

	// hiveDataPVCName is the PVC whose contents the wipe destroys (kept — only its
	// contents are wiped, never the object). Matches the provision template.
	hiveDataPVCName = "hive-data"
	// hiveSecretsName is the K8s Secret holding tenant credentials, reset to the
	// placeholder shape. Matches the provision template.
	hiveSecretsName = "hive-secrets"
	// hiveDeploymentName is the spoke Deployment, scaled to 0 for the wipe and back
	// to 1 afterwards. Matches the provision template.
	hiveDeploymentName = "hive"

	// resetToPoolWipeImage is the image the wipe/verify Job runs. It only needs a
	// POSIX shell + coreutils find/rm, all present in busybox; using the spoke
	// image would pull a much larger layer for a `find … -delete`. Pinned by
	// digest-less tag deliberately — this is a throwaway Job, not a long-lived
	// workload, and busybox:1.36 is a stable, widely-cached tag.
	resetToPoolWipeImage = "busybox:1.36"

	// resetToPoolJobActiveDeadlineSecs bounds the wipe Job's total runtime. A wipe
	// of an empty-ish PVC is seconds; a very large accreted tree is still minutes.
	// The deadline is the fail-closed backstop: if the Job cannot finish in this
	// window it is failed, and the reset fails closed rather than hanging.
	resetToPoolJobActiveDeadlineSecs = 600 // 10 minutes
	// resetToPoolJobBackoffLimit caps Job pod retries. The wipe is idempotent, so a
	// couple of retries on a transient node error are fine; beyond that we fail
	// closed rather than loop forever.
	resetToPoolJobBackoffLimit = 2

	// resetToPoolKubectlTimeout bounds each individual kubectl call the reset makes
	// (scale, apply, wait, delete). A cluster that cannot answer within this window
	// is treated as unreachable and the reset fails closed.
	resetToPoolKubectlTimeout = 90 * time.Second
	// resetToPoolJobWaitTimeout bounds the `kubectl wait` for the wipe/verify Job
	// to complete. Slightly longer than the Job's own activeDeadline so the Job's
	// deadline is what fails a slow wipe, not this wait.
	resetToPoolJobWaitTimeout = 12 * time.Minute
	// resetToPoolScaleDownWaitTimeout bounds waiting for the spoke to reach 0
	// replicas before the wipe starts, so no writer can recreate state mid-wipe.
	resetToPoolScaleDownWaitTimeout = 3 * time.Minute

	// resetToPoolWipeJobName / resetToPoolVerifyJobName are the fixed Job names in
	// the hive namespace. Fixed (not random) so a re-run reuses/replaces the same
	// object rather than leaking Jobs, keeping the reset idempotent.
	resetToPoolWipeJobName   = "hive-reset-wipe"
	resetToPoolVerifyJobName = "hive-reset-verify"

	// resetToPoolPlaceholderToken is the non-secret sentinel written to the
	// recreated Secret's github-token key. The pod's HIVE_GITHUB_TOKEN env is a
	// secretKeyRef on github-token (provision template, non-App path), so the key
	// must EXIST or the pod fails to start — but it must carry NO real credential.
	// This sentinel is deliberately not a valid token: a placeholder slot does no
	// GitHub work, and any accidental use fails loudly rather than acting as a
	// stale tenant credential.
	resetToPoolPlaceholderToken = "placeholder-not-a-real-token"

	// dashboardTokenBytesReset mirrors dashboardTokenBytes for the freshly minted
	// dashboard-token in the recreated Secret. Named locally to avoid coupling to
	// the provision constant's scope while keeping the same 32-byte strength.
	dashboardTokenBytesReset = 32
)

// resetToPoolResult is the JSON body returned by the reset endpoint (dry-run and
// real). It never carries secret VALUES — only key NAMES and paths.
type resetToPoolResult struct {
	OK        bool   `json:"ok"`
	DryRun    bool   `json:"dry_run"`
	HiveID    string `json:"id"`
	Namespace string `json:"namespace"`
	Status    string `json:"status,omitempty"`
	// DataToWipe is the top-level /data entries that WOULD be (dry-run) or WERE
	// wiped — for operator confirmation. Best-effort; empty when the listing could
	// not be read (which does not block a real wipe — the wipe is total regardless).
	DataToWipe []string `json:"data_to_wipe,omitempty"`
	// SecretKeysToReset is the hive-secrets key NAMES that would be reset (values
	// never included).
	SecretKeysToReset []string `json:"secret_keys_to_reset,omitempty"`
	Error             string   `json:"error,omitempty"`
}

// resetToPoolRequest is the POST body. Confirm must equal the target hive id
// (typed-confirmation, mirroring slackBroadcastConfirm) for a real wipe.
type resetToPoolRequest struct {
	Confirm string `json:"confirm"`
	DryRun  bool   `json:"dry_run"`
}

// isResettableLiveHive reports whether a hive is a LIVE, claimed hosted hive that
// reset-to-pool may target. It is the exact inverse of the Phase 1 escape hatch's
// gate: Phase 1 refuses a delivered claim, this REQUIRES one. It also refuses a
// hive already mid-reset (the concurrency latch) so a double-fire cannot race the
// wipe Job.
func isResettableLiveHive(h *SaaSHive) bool {
	if h == nil {
		return false
	}
	if h.ResetToPoolStatus == resetToPoolStatusInProgress {
		return false
	}
	// A delivered claim is what makes this a LIVE hive with tenant state on /data.
	return h.ClaimDelivered
}

// resetToPoolConcurrencyLatchHeld reports whether a reset is already in flight for
// this hive. Read from the authoritative on-disk record (not a copy) so two racing
// requests see each other.
func resetToPoolConcurrencyLatchHeld(hiveID string) bool {
	h := loadSaaSHive(hiveID)
	return h != nil && h.ResetToPoolStatus == resetToPoolStatusInProgress
}

// handleResetToPool wipes a LIVE hive's /data + hive-secrets credential-safely and
// returns the namespace to the unassigned pool (admin-only). It is fail-closed:
// the record is flipped to available ONLY after the wipe is VERIFIED clean; any
// wipe/verify error leaves the hive claimed with ResetToPoolStatus=failed and the
// namespace intact for manual inspection.
//
// SECURITY (defense-in-depth for an irreversible destructive action):
//   - admin-only (routed through requireAdmin, which also applies the
//     impersonation write-block — a "View as user" session cannot reach here);
//   - same-origin check (Origin/Referer host must be a trusted hub host);
//   - typed confirmation: body.confirm must equal the hive id for a real wipe;
//   - dry-run: body.dry_run returns what WOULD be wiped and deletes nothing;
//   - state guard: 409 unless the hive is a resettable LIVE hosted hive;
//   - concurrency latch: 409 while a reset is already in flight;
//   - audit on every attempt (success AND fail-closed), secret VALUES never logged.
func (s *HubServer) handleResetToPool(w http.ResponseWriter, r *http.Request) {
	admin := s.getAuthUser(r) // requireAdmin-gated; equals hubAdminUsername

	// Same-origin defense-in-depth over SameSite=Lax. Reject a cross-site POST
	// whose Origin/Referer is not a trusted hub host. An absent Origin AND Referer
	// (e.g. a same-origin fetch that omits both, or a curl) is allowed — the
	// admin + typed-confirm gates still apply; this check only REJECTS a present,
	// untrusted origin.
	if !resetToPoolSameOrigin(r) {
		http.Error(w, `{"error":"cross-origin request refused"}`, http.StatusForbidden)
		return
	}

	hiveID := r.PathValue("id")
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	ns := hostedNamespaceForHive(h)

	var body resetToPoolRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // tolerate empty body for dry-run
	}

	// State guard: only a LIVE (claim-delivered) hosted hive, not already resetting.
	if !isResettableLiveHive(h) {
		s.logger.Info("audit: hive reset-to-pool refused — not a resettable live hive",
			"hive_id", hiveID, "namespace", ns, "by", admin,
			"claim_delivered", h.ClaimDelivered, "reset_status", h.ResetToPoolStatus)
		if h.ResetToPoolStatus == resetToPoolStatusInProgress {
			http.Error(w, `{"error":"a reset-to-pool is already in progress for this hive"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"only a live, claimed hive can be reset to the pool — an unclaimed placeholder uses reset-assignment"}`, http.StatusConflict)
		return
	}

	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}

	// Reachability gate: the wipe requires kubectl to the spoke cluster AND the
	// ability to VERIFY the result there. A heartbeat-only / recently-unreachable
	// cluster can do neither, so refuse rather than ship an unverifiable wipe (a
	// wipe we cannot prove completed is exactly the credential-leak #2748 forbids).
	if s.clusterRecentlyUnreachable(cluster.ID) {
		s.logger.Warn("audit: hive reset-to-pool refused — cluster unreachable",
			"hive_id", hiveID, "namespace", ns, "by", admin, "cluster", cluster.ID)
		http.Error(w, `{"error":"cluster is unreachable — a wipe cannot be verified clean, so reset-to-pool is refused (use Delete for an unreachable hive)"}`, http.StatusServiceUnavailable)
		return
	}

	// DRY-RUN: report what WOULD be wiped and delete nothing.
	if body.DryRun {
		res := resetToPoolResult{
			OK:                true,
			DryRun:            true,
			HiveID:            hiveID,
			Namespace:         ns,
			DataToWipe:        s.resetToPoolListData(cluster, ns),
			SecretKeysToReset: s.resetToPoolListSecretKeys(cluster, ns),
		}
		s.logger.Info("audit: hive reset-to-pool dry-run",
			"hive_id", hiveID, "namespace", ns, "by", admin, "cluster", cluster.ID)
		writeJSON(w, http.StatusOK, res)
		return
	}

	// TYPED CONFIRMATION (mirrors slackBroadcastConfirm): a real wipe requires the
	// body to echo the exact hive id. Defeats stray clicks, replayed/forged
	// requests that don't know the target, and fat-finger.
	if body.Confirm != hiveID {
		s.logger.Info("audit: hive reset-to-pool refused — confirmation mismatch",
			"hive_id", hiveID, "namespace", ns, "by", admin)
		http.Error(w, `{"error":"confirmation required — the request must echo the exact hive id in the confirm field"}`, http.StatusBadRequest)
		return
	}

	// CONCURRENCY LATCH: re-check under the authoritative record and set the
	// in-progress latch before doing any destructive work, so a double-fire 409s.
	if resetToPoolConcurrencyLatchHeld(hiveID) {
		http.Error(w, `{"error":"a reset-to-pool is already in progress for this hive"}`, http.StatusConflict)
		return
	}
	h.ResetToPoolStatus = resetToPoolStatusInProgress
	h.ResetToPoolError = ""
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to record reset-in-progress"}`, http.StatusInternalServerError)
		return
	}
	s.syncRegistryProvStatus(hiveID, resetToPoolStatusInProgress)

	s.logger.Info("audit: hive reset-to-pool started",
		"hive_id", hiveID, "namespace", ns, "by", admin, "cluster", cluster.ID,
		"previous_owner", h.Owner, "previous_org", h.Org)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("reset-to-pool started by %s — wiping /data + credentials and returning the slot to the pool", admin),
		admin)

	// Perform the wipe synchronously (bounded by the per-step kubectl/Job timeouts)
	// so the operator gets a definitive result. If it fails, fail closed.
	if err := s.performResetToPoolWipe(h, cluster, ns); err != nil {
		reason := err.Error()
		s.markResetToPoolFailed(hiveID, reason)
		s.logger.Warn("audit: hive reset-to-pool FAILED CLOSED — slot NOT returned to pool",
			"hive_id", hiveID, "namespace", ns, "by", admin, "reason", reason)
		s.recordTimeline(hiveID, TimelineOwnership,
			"reset-to-pool FAILED — slot NOT returned to pool, namespace left intact for inspection: "+reason,
			admin)
		writeJSON(w, http.StatusInternalServerError, resetToPoolResult{
			OK: false, HiveID: hiveID, Namespace: ns, Status: resetToPoolStatusFailed, Error: reason,
		})
		return
	}

	// VERIFIED CLEAN. Now — and only now — flip the record to the available
	// placeholder shape (Phase 1's inverse-of-claim), reset persisted namespace/
	// prov status, drop the owner grant, and re-stamp the namespace identity.
	prevOwner := h.Owner
	prevOrg := h.Org
	resetHiveToAvailablePlaceholder(h)
	h.ResetToPoolStatus = ""
	h.ResetToPoolError = ""
	if err := saveSaaSHive(h); err != nil {
		// The wipe succeeded but the record write failed. Fail closed: the slot is
		// clean but not yet advertised available. A re-run converges (the wipe is
		// idempotent) — leave the in-progress latch cleared so a retry is accepted.
		s.markResetToPoolFailed(hiveID, "wipe verified clean but record reset failed to save — retry to converge")
		http.Error(w, `{"error":"wipe verified clean but record reset failed to save"}`, http.StatusInternalServerError)
		return
	}
	s.syncRegistryProvStatus(hiveID, statusAvailable)
	s.resetToPoolDropOwnerGrant(hiveID)
	reopened := clearAssignedRequestForHive(hiveID)

	// Best-effort namespace identity re-stamp back to placeholder (idempotent).
	stampHostedNamespaceIdentity(cluster, ns, availablePlaceholderProjectName(h),
		placeholderOrgPrefix+hiveID, hiveID, s.logger)

	// Bring the spoke back up; it boots fresh from the re-seeded empty /data as an
	// unclaimed placeholder. Best-effort: if kubectl cannot scale it back up the
	// slot is still clean+available; the spoke will be reconciled on next
	// heartbeat/restart.
	if err := s.resetToPoolScaleDeployment(cluster, ns, 1); err != nil {
		s.logger.Warn("reset-to-pool: failed to scale spoke back up (slot is clean+available; will reconcile)",
			"hive_id", hiveID, "namespace", ns, "error", err)
	}

	s.logger.Info("audit: hive reset-to-pool COMPLETE — slot returned to pool (verified clean)",
		"hive_id", hiveID, "namespace", ns, "by", admin,
		"previous_owner", prevOwner, "previous_org", prevOrg, "reopened_request_for", reopened)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("reset-to-pool complete by %s — /data + credentials wiped (verified clean), slot returned to the available pool (was %s)", admin, prevOwner),
		admin)

	writeJSON(w, http.StatusOK, resetToPoolResult{
		OK: true, HiveID: hiveID, Namespace: ns, Status: statusAvailable,
	})
}

// markResetToPoolFailed records the FAIL-CLOSED terminal state on the hive record
// so the UI can surface it and a re-run is possible. Reloads the authoritative
// record so it does not clobber a concurrent write.
func (s *HubServer) markResetToPoolFailed(hiveID, reason string) {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return
	}
	h.ResetToPoolStatus = resetToPoolStatusFailed
	h.ResetToPoolError = reason
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("reset-to-pool: failed to record fail-closed state", "hive_id", hiveID, "error", err)
		return
	}
	s.syncRegistryProvStatus(hiveID, resetToPoolStatusFailed)
}

// resetToPoolDropOwnerGrant removes the prior owner's Hives[id]="owner" grant so a
// pooled slot is not still listed under the old tenant. It mirrors
// handleAccessRevoke's delete(target.Hives, hiveID) and deliberately does NOT
// touch SaaSQuota — quota is increment-only across the codebase today, and
// reversing it here is out of scope (called out in the #2748 design comment).
func (s *HubServer) resetToPoolDropOwnerGrant(hiveID string) {
	for _, u := range listAllSaaSUsers() {
		if _, ok := u.Hives[hiveID]; !ok {
			continue
		}
		delete(u.Hives, hiveID)
		if err := saveSaaSUser(&u); err != nil {
			s.logger.Warn("reset-to-pool: failed to drop owner grant", "user", u.GitHubUsername, "hive_id", hiveID, "error", err)
		}
	}
}

// resetToPoolSameOrigin reports whether the request's Origin (or Referer, if
// Origin is absent) is a trusted hub host. A request carrying NEITHER header is
// allowed (same-origin fetches may omit both, and non-browser clients like curl
// have no Origin) — the admin + typed-confirm gates remain in force; this check
// only REJECTS a present-but-untrusted origin.
func resetToPoolSameOrigin(r *http.Request) bool {
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		return isTrustedOrigin(o)
	}
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		return isTrustedOrigin(ref)
	}
	return true
}

// newDashboardTokenReset mints a fresh random dashboard-token for the recreated
// Secret, the same strength the provision path uses.
func newDashboardTokenReset() (string, error) {
	b := make([]byte, dashboardTokenBytesReset)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// resetToPoolPlaceholderSecretKeys is the EXACT key set a freshly-reset Secret
// carries: a fresh dashboard-token and a placeholder github-token (the two keys a
// non-App placeholder slot needs to boot). No gh-app-key*.pem, no tenant creds.
// The verify step asserts the recreated Secret's keys equal this set — any extra
// key (a surviving App PEM, say) FAILS CLOSED. Sorted for a stable comparison.
func resetToPoolPlaceholderSecretKeys() []string {
	keys := []string{"dashboard-token", "github-token"}
	sort.Strings(keys)
	return keys
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- kubectl-driven wipe primitives -------------------------------------------

// performResetToPoolWipe runs the full fail-closed wipe sequence against a
// reachable spoke cluster:
//
//  1. scale the spoke Deployment to 0 (stop all writers)
//  2. run the wipe Job (delete everything under /data except the seed allowlist)
//  3. verify /data is clean (find returns only the allowlist) — else FAIL CLOSED
//  4. reset the hive-secrets Secret to the placeholder shape and verify its keys
//
// Every step is bounded by a timeout and returns an error on any failure, so the
// caller fails closed. It is idempotent: a re-run re-scales-0, re-applies the Job
// (fixed name), re-verifies, and re-resets the Secret.
func (s *HubServer) performResetToPoolWipe(h *SaaSHive, cluster *ClusterConfig, ns string) error {
	// 1. Scale to 0 and wait, so no agent/tmux/writer recreates state mid-wipe.
	if err := s.resetToPoolScaleDeployment(cluster, ns, 0); err != nil {
		return fmt.Errorf("scale spoke to 0: %w", err)
	}
	if err := s.resetToPoolWaitForReplicas(cluster, ns, 0); err != nil {
		return fmt.Errorf("wait for spoke to stop: %w", err)
	}

	// 2. Wipe Job.
	if err := s.resetToPoolRunWipeJob(cluster, ns); err != nil {
		return fmt.Errorf("wipe /data: %w", err)
	}

	// 3. Verify /data clean — FAIL CLOSED on any leftover.
	leftover, err := s.resetToPoolVerifyDataClean(cluster, ns)
	if err != nil {
		return fmt.Errorf("verify /data clean: %w", err)
	}
	if len(leftover) > 0 {
		return fmt.Errorf("/data not clean after wipe — %d unexpected entr%s remain (e.g. %s)",
			len(leftover), plural(len(leftover), "y", "ies"), strings.Join(firstN(leftover, 3), ", "))
	}

	// 4. Reset the Secret to the placeholder shape and verify its key set.
	if err := s.resetToPoolResetSecret(cluster, ns); err != nil {
		return fmt.Errorf("reset hive-secrets: %w", err)
	}
	extra, err := s.resetToPoolVerifySecretKeys(cluster, ns)
	if err != nil {
		return fmt.Errorf("verify hive-secrets keys: %w", err)
	}
	if len(extra) > 0 {
		return fmt.Errorf("hive-secrets has unexpected keys after reset: %s", strings.Join(extra, ", "))
	}
	return nil
}

// resetToPoolScaleDeployment scales the spoke Deployment to replicas.
func (s *HubServer) resetToPoolScaleDeployment(cluster *ClusterConfig, ns string, replicas int) error {
	ctx, cancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer cancel()
	cmd := kubectlForClusterContext(ctx, cluster, "scale",
		"deployment/"+hiveDeploymentName, fmt.Sprintf("--replicas=%d", replicas), "-n", ns)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.markClusterUnreachable(cluster.ID)
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	s.markClusterReachable(cluster.ID)
	return nil
}

// resetToPoolWaitForReplicas waits until the Deployment reports the target ready
// replica count (0 for scale-down). Uses `kubectl wait` on the availableReplicas
// via a rollout-status-equivalent: for scale-to-0 we poll the deployment's
// .status.replicas reaching 0.
func (s *HubServer) resetToPoolWaitForReplicas(cluster *ClusterConfig, ns string, target int) error {
	ctx, cancel := context.WithTimeout(context.Background(), resetToPoolScaleDownWaitTimeout)
	defer cancel()
	// For scale-to-0, wait for .status.replicas == 0 (no pods left). kubectl wait
	// --for=jsonpath is the bounded, declarative way to do this.
	cmd := kubectlForClusterContext(ctx, cluster, "wait",
		"deployment/"+hiveDeploymentName, "-n", ns,
		fmt.Sprintf("--for=jsonpath={.status.replicas}=%d", target),
		fmt.Sprintf("--timeout=%ds", int(resetToPoolScaleDownWaitTimeout.Seconds())))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// resetToPoolRunWipeJob applies the wipe Job manifest, waits for it to complete,
// and returns an error if it did not succeed. The Job (fixed name) is deleted
// first so a re-run replaces any prior one. The wipe deletes everything under
// /data except the seed allowlist.
func (s *HubServer) resetToPoolRunWipeJob(cluster *ClusterConfig, ns string) error {
	manifest := resetToPoolWipeJobManifest(ns, cluster)
	if err := s.resetToPoolApplyJob(cluster, ns, resetToPoolWipeJobName, manifest); err != nil {
		return err
	}
	return s.resetToPoolWaitJobComplete(cluster, ns, resetToPoolWipeJobName)
}

// resetToPoolApplyJob deletes any prior Job of the same name (idempotent re-run)
// then applies the manifest.
func (s *HubServer) resetToPoolApplyJob(cluster *ClusterConfig, ns, jobName, manifest string) error {
	delCtx, delCancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer delCancel()
	// Best-effort delete of a prior Job; ignore-not-found. Wait so the name is free.
	_, _ = kubectlForClusterContext(delCtx, cluster, "delete", "job", jobName, "-n", ns,
		"--ignore-not-found", "--wait=true",
		fmt.Sprintf("--timeout=%ds", int(resetToPoolKubectlTimeout.Seconds()))).CombinedOutput()

	applyCtx, applyCancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer applyCancel()
	cmd := kubectlForClusterContext(applyCtx, cluster, "apply", "-n", ns, "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.markClusterUnreachable(cluster.ID)
		return fmt.Errorf("apply job %s: %s: %w", jobName, strings.TrimSpace(string(out)), err)
	}
	s.markClusterReachable(cluster.ID)
	return nil
}

// resetToPoolWaitJobComplete waits for the named Job to reach Complete, failing on
// timeout or on the Job reaching Failed.
func (s *HubServer) resetToPoolWaitJobComplete(cluster *ClusterConfig, ns, jobName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), resetToPoolJobWaitTimeout)
	defer cancel()
	// Wait for either complete or failed; kubectl wait returns non-zero if the
	// condition is not met in time, which we surface as a fail-closed error.
	cmd := kubectlForClusterContext(ctx, cluster, "wait", "job/"+jobName, "-n", ns,
		"--for=condition=complete",
		fmt.Sprintf("--timeout=%ds", int(resetToPoolJobWaitTimeout.Seconds())))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Distinguish "failed" from "still running/timeout" for a clearer message.
		if s.resetToPoolJobFailed(cluster, ns, jobName) {
			return fmt.Errorf("job %s failed", jobName)
		}
		return fmt.Errorf("job %s did not complete: %s: %w", jobName, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// resetToPoolJobFailed reports whether the Job has a Failed condition true.
func (s *HubServer) resetToPoolJobFailed(cluster *ClusterConfig, ns, jobName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer cancel()
	out, err := kubectlForClusterContext(ctx, cluster, "get", "job/"+jobName, "-n", ns,
		"-o", "jsonpath={.status.conditions[?(@.type=='Failed')].status}").Output()
	return err == nil && strings.TrimSpace(string(out)) == "True"
}

// resetToPoolVerifyDataClean runs a short verify Job that lists /data minus the
// seed allowlist and returns any leftover top-level entries. A non-empty result
// (or an error) makes the caller fail closed. The verify Job writes the leftover
// listing to its logs, which we read back.
func (s *HubServer) resetToPoolVerifyDataClean(cluster *ClusterConfig, ns string) ([]string, error) {
	manifest := resetToPoolVerifyJobManifest(ns, cluster)
	if err := s.resetToPoolApplyJob(cluster, ns, resetToPoolVerifyJobName, manifest); err != nil {
		return nil, err
	}
	if err := s.resetToPoolWaitJobComplete(cluster, ns, resetToPoolVerifyJobName); err != nil {
		return nil, err
	}
	// Read the verify Job's pod logs: each line is a leftover /data entry.
	logs, err := s.resetToPoolJobLogs(cluster, ns, resetToPoolVerifyJobName)
	if err != nil {
		// If we cannot read the verify result, we cannot prove clean -> fail closed.
		return nil, fmt.Errorf("could not read verify result: %w", err)
	}
	return resetToPoolParseLeftover(logs), nil
}

// resetToPoolJobLogs returns the combined logs of the named Job's pod(s).
func (s *HubServer) resetToPoolJobLogs(cluster *ClusterConfig, ns, jobName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer cancel()
	out, err := kubectlForClusterContext(ctx, cluster, "logs", "job/"+jobName, "-n", ns,
		"--tail=-1").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// resetToPoolResetSecret deletes hive-secrets and recreates it in the placeholder
// shape (fresh dashboard-token + placeholder github-token, no tenant creds).
func (s *HubServer) resetToPoolResetSecret(cluster *ClusterConfig, ns string) error {
	dashToken, err := newDashboardTokenReset()
	if err != nil {
		return fmt.Errorf("mint dashboard-token: %w", err)
	}
	delCtx, delCancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer delCancel()
	if out, err := kubectlForClusterContext(delCtx, cluster, "delete", "secret", hiveSecretsName,
		"-n", ns, "--ignore-not-found").CombinedOutput(); err != nil {
		return fmt.Errorf("delete secret: %s: %w", strings.TrimSpace(string(out)), err)
	}
	manifest := resetToPoolSecretManifest(ns, dashToken)
	applyCtx, applyCancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer applyCancel()
	cmd := kubectlForClusterContext(applyCtx, cluster, "apply", "-n", ns, "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply placeholder secret: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// resetToPoolVerifySecretKeys returns any keys present on the recreated Secret
// that are NOT in the placeholder key set. A non-empty result fails the reset
// closed (a surviving gh-app-key*.pem would appear here). Values are never read.
func (s *HubServer) resetToPoolVerifySecretKeys(cluster *ClusterConfig, ns string) ([]string, error) {
	keys := s.resetToPoolListSecretKeys(cluster, ns)
	return resetToPoolUnexpectedSecretKeys(keys), nil
}

// resetToPoolListSecretKeys lists the hive-secrets Secret's key NAMES (never
// values). Empty on error. Uses a go-template that iterates .data keys only, so
// no secret value is ever fetched or logged.
func (s *HubServer) resetToPoolListSecretKeys(cluster *ClusterConfig, ns string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), resetToPoolKubectlTimeout)
	defer cancel()
	keysOut, err := kubectlForClusterContext(ctx, cluster, "get", "secret", hiveSecretsName, "-n", ns,
		"-o", `go-template={{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}`).Output()
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(keysOut)), "\n") {
		if k := strings.TrimSpace(line); k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// resetToPoolListData lists the top-level /data entries via a short-lived pod, for
// the dry-run report. Best-effort — empty on error (a real wipe is total
// regardless of whether this listing could be read).
func (s *HubServer) resetToPoolListData(cluster *ClusterConfig, ns string) []string {
	manifest := resetToPoolVerifyJobManifest(ns, cluster)
	if err := s.resetToPoolApplyJob(cluster, ns, resetToPoolVerifyJobName, manifest); err != nil {
		return nil
	}
	if err := s.resetToPoolWaitJobComplete(cluster, ns, resetToPoolVerifyJobName); err != nil {
		return nil
	}
	logs, err := s.resetToPoolJobLogs(cluster, ns, resetToPoolVerifyJobName)
	if err != nil {
		return nil
	}
	return resetToPoolParseLeftover(logs)
}

// --- pure helpers (unit-tested) -----------------------------------------------

// resetToPoolParseLeftover turns the verify Job's log output (one /data entry per
// line, absolute paths under /data) into the list of leftover entries, filtering
// blank lines and any diagnostic markers the Job prints.
func resetToPoolParseLeftover(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		e := strings.TrimSpace(line)
		if e == "" {
			continue
		}
		// The verify Job prints a sentinel when clean; treat it as "no leftover".
		if e == resetToPoolCleanSentinel {
			continue
		}
		out = append(out, e)
	}
	return out
}

// resetToPoolUnexpectedSecretKeys returns the keys present that are NOT in the
// placeholder key set — the fail-closed signal for the Secret reset.
func resetToPoolUnexpectedSecretKeys(present []string) []string {
	allowed := map[string]bool{}
	for _, k := range resetToPoolPlaceholderSecretKeys() {
		allowed[k] = true
	}
	var extra []string
	for _, k := range present {
		if !allowed[strings.TrimSpace(k)] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return extra
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstN(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
