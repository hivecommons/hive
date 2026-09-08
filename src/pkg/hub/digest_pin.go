package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Digest pin: rollback as a first-class run-state (#6290, #6267, #6268).
//
// Before this file, rolling a hosted spoke back to a known-good build meant a
// raw `kubectl set image` in the spoke's namespace, reaching around the hub.
// That left no record of who did it or why (#6268), and it did not even hold:
// the tracked-channel re-arm in handleHeartbeat honoured only the admin
// upgrade pause, so a channel-tracking hive an operator had hand-pinned was
// dragged back to the channel head on its next beat (#6267). A rollback that
// silently reverts is worse than none, because the operator believes the
// fleet is on the known-good digest when it is not.
//
// The shape follows the per-repo pause (#6203): the pin is a RUN-STATE on the
// hub-owned hive record, carrying who/when/digest/reason, persisted next to
// every other hub-owned fact about the hive so it survives hub restarts and
// hub self-upgrades. It is not an edit to the hive's identity: TrackedChannel
// is left exactly as it was, so lifting the pin resumes the selection the
// operator had before the incident.
//
// Enforcement is deterministic and lives at every path that would write an
// image onto the spoke:
//
//   - the tracked-channel re-arm in handleHeartbeat skips a pinned hive
//     (the #6267 fix, and the half that matters);
//   - handleHeartbeat withholds any armed SwitchToTag / UpgradeTo while the
//     pin holds, so a directive armed before the pin cannot undo it;
//   - triggerAutoUpgrades never arms a pinned hive;
//   - the manual upgrade, branch/channel switch and bulk equivalents refuse
//     with 409 naming the pin, exactly as they refuse under the admin pause.
//
// The pin is applied through the SAME deployment-patch path handleSwitchBranch
// already uses: `kubectl set image deployment/hive "*=<ref>" -n
// hive-hosted-<id>`. The `*=` form covers the init containers too. Delivery is
// PUSH only: the heartbeat fallback carries a bare tag which the spoke turns
// into ghcr.io/hivecommons/hive:<tag>, and a digest cannot ride that channel.
// On a cluster the hub cannot reach the pin is refused rather than recorded
// as applied, because a recorded pin that never landed is the exact lie this
// feature exists to remove.

// DigestPin records that a hive's spoke image is held at one immutable
// manifest digest, together with the provenance of the decision.
type DigestPin struct {
	// Digest is the manifest digest the Deployment is held at, always in the
	// canonical "sha256:<64 hex>" form regardless of how the operator spelled
	// it in the request.
	Digest string `json:"digest"`
	// SourceSHA is the short git SHA the operator gave when the digest was
	// resolved from a short-SHA tag, or "" when the digest was given directly.
	// Display only: it lets the pill read "pinned at abc1234" rather than a
	// 64-character digest, while Digest stays the value the Deployment holds.
	SourceSHA string `json:"source_sha,omitempty"`
	// By is the acting operator. Never empty on a pin written through the API;
	// kept optional in the JSON so a hand-edited record still loads.
	By string `json:"by,omitempty"`
	// At is when the pin was recorded, RFC3339 UTC.
	At string `json:"at,omitempty"`
	// Reason is the operator's free-text explanation ("rollback: 3f2a1c9 broke
	// the MITM proxy"). Optional, but the dashboard shows it verbatim so an
	// operator days later can tell a deliberate rollback from a malfunction.
	Reason string `json:"reason,omitempty"`
	// PreviousImage is the image the spoke reported running when the pin was
	// taken. Unpin uses its tag to put a plain-branch hive back on the moving
	// tag it was on; a channel-tracking hive is restored to its channel
	// regardless.
	PreviousImage string `json:"previous_image,omitempty"`
}

const (
	// digestHexLen is the length of a sha256 manifest digest in hex.
	digestHexLen = 64
	// digestPinReasonMaxLen bounds the free-text reason so a pasted log cannot
	// bloat the hive record.
	digestPinReasonMaxLen = 500
	// digestPinRequestMaxBytes bounds the request body, mirroring the other
	// small JSON control endpoints.
	digestPinRequestMaxBytes = 8 << 10
	// digestPrefix is the algorithm prefix every accepted digest carries.
	digestPrefix = "sha256:"
	// spokeImageRepoRef is the registry path every hosted spoke image lives
	// under. The pin writes "<repo>@<digest>"; the tag paths write "<repo>:<tag>".
	spokeImageRepoRef = "ghcr.io/" + ghcrRepoSpoke
)

// digestHexPattern matches the hex body of a sha256 digest, anchored so a
// value that merely contains hex cannot slip through.
var digestHexPattern = regexp.MustCompile(fmt.Sprintf(`^[0-9a-f]{%d}$`, digestHexLen))

// normalizeImageDigest accepts "sha256:<64 hex>" or a bare 64-hex string and
// returns the canonical prefixed form. Anything else is refused with an error
// naming the offending value, for the same reason validateImageTag refuses an
// unparseable tag: an unresolvable reference written onto a Deployment strands
// the spoke on its old ReplicaSet while looking healthy.
func normalizeImageDigest(raw string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	if d == "" {
		return "", fmt.Errorf("digest is empty")
	}
	d = strings.TrimPrefix(d, digestPrefix)
	if !digestHexPattern.MatchString(d) {
		return "", fmt.Errorf("digest %q is not a sha256 manifest digest (%d hex characters, optionally prefixed sha256:)", raw, digestHexLen)
	}
	return digestPrefix + d, nil
}

// pinnedImageRef is the full reference the pin writes onto the Deployment.
func pinnedImageRef(digest string) string {
	return spokeImageRepoRef + "@" + digest
}

// imageDigestOf extracts the digest from a reference in the "<repo>@<digest>"
// form, or "" when the reference is not digest-pinned. It is the digest
// counterpart of imageTagOf.
func imageDigestOf(ref string) string {
	ref = strings.TrimSpace(ref)
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ""
	}
	return ref[at+1:]
}

// resolveSpokeDigest resolves a short-SHA spoke tag to its manifest-list
// digest on GHCR, so an operator can pin by the SHA the My Hives page shows
// rather than fetching the digest by hand. The digest comes from the
// registry's Docker-Content-Digest header on a HEAD of the manifest, which is
// the manifest-LIST digest for the multi-arch spoke image, never the
// per-architecture digest a local pull would report. A var so tests can stub
// the round-trip.
var resolveSpokeDigest = func(tag string, logger *slog.Logger) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + ghcrRepoSpoke + ":pull")
	if err != nil {
		return "", fmt.Errorf("GHCR token request failed: %w", err)
	}
	defer func() { _ = tokenResp.Body.Close() }()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("GHCR token decode failed: %w", err)
	}
	req, _ := http.NewRequest(http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, ghcrRepoSpoke, tag), nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GHCR manifest request failed: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("no published spoke image for tag %q (HTTP %d)", tag, resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("registry returned no digest for tag %q", tag)
	}
	if logger != nil {
		logger.Info("digest pin: resolved spoke tag to manifest digest", "tag", tag, "digest", digest)
	}
	return digest, nil
}

// DigestPinned reports whether the hive carries a digest pin. It is the single
// predicate every enforcement point asks, so there is exactly one definition
// of "pinned".
func (h *SaaSHive) DigestPinned() bool {
	return h != nil && h.DigestPin != nil && h.DigestPin.Digest != ""
}

// digestPinRefusal is the operator-facing 409 body for an image change refused
// because the hive is pinned. It names the pin's provenance so the refusal is
// actionable (who to ask, what to lift) rather than a bare "conflict".
func digestPinRefusal(h *SaaSHive) string {
	p := h.DigestPin
	label := p.Digest
	if p.SourceSHA != "" {
		label = p.SourceSHA + " (" + p.Digest + ")"
	}
	msg := "hive is pinned to image digest " + label
	if p.By != "" {
		msg += " by " + p.By
	}
	if p.At != "" {
		msg += " at " + p.At
	}
	if p.Reason != "" {
		msg += " (" + p.Reason + ")"
	}
	return msg + " - unpin it before changing its image"
}

// writeDigestPinRefusal answers an image-changing request on a pinned hive.
func writeDigestPinRefusal(w http.ResponseWriter, h *SaaSHive) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": digestPinRefusal(h)})
}

// digestPinRequest is the body of POST /api/saas/hives/{id}/pin-digest. Exactly
// one of Digest or SHA is required: a manifest digest pins directly, a short
// git SHA is resolved to the digest of the spoke image CI published for it.
type digestPinRequest struct {
	Digest string `json:"digest,omitempty"`
	SHA    string `json:"sha,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// digestUnpinRequest is the body of POST /api/saas/hives/{id}/unpin-digest.
// The reason is recorded on the timeline; the body may be empty.
type digestUnpinRequest struct {
	Reason string `json:"reason,omitempty"`
}

func writeDigestPinError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// digestPinCORS mirrors the same-origin CORS preamble the other hive control
// handlers carry. The bool reports whether the caller should continue.
func digestPinCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

// registryImageRef returns the image the spoke last reported running, "" when
// the hive has no registry entry or the spoke never reported one.
func (s *HubServer) registryImageRef(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			return s.registry.Hives[i].ImageRef
		}
	}
	return ""
}

// handlePinDigest pins a hosted spoke to an immutable image digest. Owner-only
// (hub admins pass userIsHiveOwner). The pin record is written BEFORE the
// Deployment is patched so no heartbeat can slip in between and re-arm the
// channel; if the patch then fails the record is removed again and the
// request fails loudly, so "pinned" on the hub always means "the Deployment
// holds this digest".
//
// Deliberately NOT gated on the admin upgrade pause: the pause freezes the
// upgrade train, and a rollback is the operator steering by hand while it is
// frozen. Unpin, which returns the hive to the train, IS gated.
func (s *HubServer) handlePinDigest(w http.ResponseWriter, r *http.Request) {
	if !digestPinCORS(w, r) {
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		writeDigestPinError(w, http.StatusNotFound, "hive not found")
		return
	}
	if !userIsHiveOwner(username, h) {
		writeDigestPinError(w, http.StatusForbidden, "only the owner can pin this hive's image")
		return
	}
	var body digestPinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, digestPinRequestMaxBytes)).Decode(&body); err != nil {
		writeDigestPinError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	body.Digest = strings.TrimSpace(body.Digest)
	body.SHA = strings.TrimSpace(body.SHA)
	body.Reason = strings.TrimSpace(body.Reason)
	if len(body.Reason) > digestPinReasonMaxLen {
		writeDigestPinError(w, http.StatusBadRequest, fmt.Sprintf("reason is over %d characters", digestPinReasonMaxLen))
		return
	}
	if (body.Digest == "") == (body.SHA == "") {
		writeDigestPinError(w, http.StatusBadRequest, "exactly one of digest (sha256:...) or sha (short git SHA) is required")
		return
	}
	var digest string
	if body.SHA != "" {
		// A SHA is a TAG on the registry and goes through the same shape check
		// every tag writer applies, then is resolved to the digest CI published
		// under it. Refusing an unknown SHA here is what keeps a typo from
		// becoming an ImagePullBackOff behind a still-serving old ReplicaSet.
		if err := validateImageTag(body.SHA); err != nil || !imageTagSHAPattern.MatchString(body.SHA) {
			writeDigestPinError(w, http.StatusBadRequest, fmt.Sprintf("sha %q is not a %d-%d character hex git SHA", body.SHA, minImageTagSHALen, maxImageTagSHALen))
			return
		}
		resolved, err := resolveSpokeDigest(body.SHA, s.logger)
		if err != nil {
			writeDigestPinError(w, http.StatusBadRequest, "could not resolve sha to a published spoke image: "+err.Error())
			return
		}
		digest = resolved
	} else {
		digest = body.Digest
	}
	digest, err := normalizeImageDigest(digest)
	if err != nil {
		writeDigestPinError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Existence, not just shape: a well-formed digest that was pruned past the
	// 90-day short-SHA retention window pulls as "manifest unknown", and
	// Kubernetes keeps the old ReplicaSet serving while the hive looks alive.
	if !spokeImageExists(digest, s.logger) {
		writeDigestPinError(w, http.StatusBadRequest, "no published spoke image for digest "+digest+" (pruned, or never built)")
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		writeDigestPinError(w, http.StatusInternalServerError, "no cluster config for this hive")
		return
	}
	if !cluster.KubectlReachable() {
		// The heartbeat fallback carries a bare tag; a digest cannot ride it.
		// Refuse rather than record a pin the Deployment will never hold.
		writeDigestPinError(w, http.StatusConflict, "the hub cannot reach this hive's cluster over kubectl; a digest pin is applied by patching the Deployment directly and cannot be delivered over the heartbeat")
		return
	}
	// No-op guard, mirroring the repo pause: re-pinning the SAME digest keeps
	// the ORIGINAL provenance rather than restamping who/when.
	if h.DigestPinned() && h.DigestPin.Digest == digest {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "pinned", "changed": false, "pin": h.DigestPin, "image": pinnedImageRef(digest)})
		return
	}

	image := pinnedImageRef(digest)
	previous := s.registryImageRef(id)
	if h.DigestPinned() && h.DigestPin.PreviousImage != "" {
		// Re-pinning to a different digest keeps the pre-incident image, which
		// is what unpin must return to - not the previous pin.
		previous = h.DigestPin.PreviousImage
	}
	prior := h.DigestPin
	h.DigestPin = &DigestPin{
		Digest:        digest,
		SourceSHA:     body.SHA,
		By:            username,
		At:            time.Now().UTC().Format(time.RFC3339),
		Reason:        body.Reason,
		PreviousImage: previous,
	}
	// Persist FIRST. From this write on, the heartbeat handler skips the
	// channel re-arm and withholds armed directives for this hive, so the
	// kubectl patch below cannot race a beat that would undo it.
	if err := saveSaaSHive(h); err != nil {
		writeDigestPinError(w, http.StatusInternalServerError, "could not record the pin: "+err.Error())
		return
	}
	// Drop any directive armed before the pin: an in-memory switch or upgrade
	// target delivered on the next beat would overwrite the digest.
	s.mu.Lock()
	delete(s.heartbeatSwitchTag, id)
	delete(s.heartbeatUpgrade, id)
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.clearUpgradeLatch(i)
			break
		}
	}
	s.mu.Unlock()

	ns := hiveHostedNamespacePrefix + id
	// Same object, same "*=" form handleSwitchBranch writes: every container
	// including the init containers moves to the digest.
	cmd := kubectlForCluster(cluster, "set", "image", "deployment/hive", "*="+image, "-n", ns)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Roll the record back: a pin the Deployment does not hold must not
		// exist on the hub, or the dashboard would report a rollback that
		// never happened.
		h.DigestPin = prior
		if saveErr := saveSaaSHive(h); saveErr != nil {
			s.logger.Error("digest pin: kubectl failed AND the pin record could not be rolled back - the hub now claims a pin the Deployment does not hold",
				"hive", id, "digest", digest, "kubectl_output", strings.TrimSpace(string(out)), "save_error", saveErr)
		}
		s.logger.Error("digest pin REFUSED: kubectl set image failed",
			"hive", id, "digest", digest, "cluster", cluster.ID, "output", strings.TrimSpace(string(out)))
		writeDigestPinError(w, http.StatusBadGateway, "kubectl set image failed - the pin was not applied and has not been recorded")
		return
	}
	s.logger.Info("audit: hive pinned to image digest",
		"hive_id", id, "by", username, "digest", digest, "source_sha", body.SHA,
		"reason", body.Reason, "previous_image", previous, "cluster", cluster.ID)
	detail := "pinned to image digest " + digest
	if body.SHA != "" {
		detail = "pinned to image digest " + digest + " (resolved from " + body.SHA + ")"
	}
	if body.Reason != "" {
		detail += ": " + body.Reason
	}
	s.recordTimeline(id, TimelineDigestPinned, detail, username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "status": "pinned", "changed": true, "pin": h.DigestPin, "image": image,
	})
}

// unpinRestoreTag is the moving tag a hive returns to when its pin is lifted:
// the tracked channel when it has one, else the tag the spoke was running when
// the pin was taken, else the running branch's "-latest" tag. Empty when none
// of those is known - the caller then lifts the pin without rewriting the
// image, leaving the Deployment on the digest until an explicit switch.
func unpinRestoreTag(h *SaaSHive, registryBranch string) string {
	if isReleaseChannel(h.TrackedChannel) {
		return h.TrackedChannel
	}
	if h.DigestPin != nil {
		if tag := imageTagOf(h.DigestPin.PreviousImage); tag != "" && imageTagIsMutable(h.DigestPin.PreviousImage) {
			return tag
		}
	}
	if registryBranch != "" {
		return upgradeTargetTag(registryBranch)
	}
	return ""
}

// handleUnpinDigest lifts a hive's digest pin and returns it to its moving
// tag. Owner-only. Refused under the admin upgrade pause with the same 409 a
// branch switch gets: lifting a pin puts the hive back on the upgrade train,
// which is exactly what the pause holds still.
//
// The pin is cleared BEFORE the image is rewritten, and the restore tag is
// also armed as a heartbeat switch, so a cluster the hub cannot patch still
// converges: for a channel-tracking hive the durable re-arm (now no longer
// skipped) would heal it on the next beat anyway.
func (s *HubServer) handleUnpinDigest(w http.ResponseWriter, r *http.Request) {
	if !digestPinCORS(w, r) {
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		writeDigestPinError(w, http.StatusNotFound, "hive not found")
		return
	}
	if !userIsHiveOwner(username, h) {
		writeDigestPinError(w, http.StatusForbidden, "only the owner can unpin this hive's image")
		return
	}
	if sw, paused := s.spokeUpgradesPaused(); paused {
		writeDigestPinError(w, http.StatusConflict, upgradePauseRefusal("spoke", sw))
		return
	}
	var body digestUnpinRequest
	if r.Body != nil {
		// An empty body is fine; only a malformed one is refused.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, digestPinRequestMaxBytes)).Decode(&body); err != nil && !strings.Contains(err.Error(), "EOF") {
			writeDigestPinError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}
	if !h.DigestPinned() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "unpinned", "changed": false})
		return
	}
	s.mu.RLock()
	var registryBranch string
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			registryBranch = s.registry.Hives[i].GitBranch
			break
		}
	}
	s.mu.RUnlock()
	restoreTag := unpinRestoreTag(h, registryBranch)
	lifted := *h.DigestPin
	h.DigestPin = nil
	if err := saveSaaSHive(h); err != nil {
		writeDigestPinError(w, http.StatusInternalServerError, "could not clear the pin: "+err.Error())
		return
	}
	detail := "digest pin " + lifted.Digest + " lifted"
	if strings.TrimSpace(body.Reason) != "" {
		detail += ": " + strings.TrimSpace(body.Reason)
	}
	via := "none"
	image := ""
	if restoreTag != "" {
		image = spokeImageRepoRef + ":" + restoreTag
		cluster := s.clusterForHive(h)
		applied := false
		if cluster != nil && cluster.KubectlReachable() {
			ns := hiveHostedNamespacePrefix + id
			cmd := kubectlForCluster(cluster, "set", "image", "deployment/hive", "*="+image, "-n", ns)
			if out, err := cmd.CombinedOutput(); err != nil {
				s.logger.Warn("digest unpin: kubectl set image failed, using heartbeat fallback",
					"hive", id, "tag", restoreTag, "output", strings.TrimSpace(string(out)))
			} else {
				applied = true
				via = "kubectl"
			}
		}
		if !applied {
			// The spoke patches its own Deployment from the tag on its next beat.
			s.mu.Lock()
			s.heartbeatSwitchTag[id] = restoreTag
			s.mu.Unlock()
			via = "heartbeat"
		}
		detail += ", returning to " + image
	}
	s.logger.Info("audit: hive digest pin lifted",
		"hive_id", id, "by", username, "digest", lifted.Digest, "restore_tag", restoreTag, "via", via)
	s.recordTimeline(id, TimelineDigestUnpinned, detail, username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "status": "unpinned", "changed": true, "lifted": lifted,
		"restore_tag": restoreTag, "image": image, "via": via,
	})
}

// handleGetDigestPin reports a hive's pin with its provenance, for scripts
// verifying a rollback from the hub's side. Owner-or-admin, like the timeline.
func (s *HubServer) handleGetDigestPin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		writeDigestPinError(w, http.StatusNotFound, "hive not found")
		return
	}
	if !userIsHiveOwner(username, h) {
		writeDigestPinError(w, http.StatusForbidden, "only the owner can read this hive's pin")
		return
	}
	out := map[string]any{"ok": true, "pinned": h.DigestPinned(), "reported_image": s.registryImageRef(id)}
	if h.DigestPinned() {
		out["pin"] = h.DigestPin
		out["image"] = pinnedImageRef(h.DigestPin.Digest)
		// "landed" is the hub-side witness the runbook asks for: the spoke's
		// reported Deployment image carries the pinned digest.
		out["landed"] = imageDigestOf(s.registryImageRef(id)) == h.DigestPin.Digest
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
