package spoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/imageref"
)

const (
	selfUpgradeDeployName = "hive"
	// selfUpgradePatchTimeout bounds a Deployment PATCH. Patches pass through
	// the cluster's validating admission webhooks, whose own default timeout
	// is 10s each; an apiserver waiting on a slow or restart-looping webhook
	// (Kyverno, observed live on vllm-d) does not return the webhook's error
	// until AFTER that, so the previous 10s client deadline cut every patch
	// off with "context deadline exceeded" and hid the real cause.
	selfUpgradePatchTimeout = 45 * time.Second
	// selfUpgradePatchAttempts is the total number of PATCH tries (first
	// attempt plus one retry) before the caller falls back.
	selfUpgradePatchAttempts = 2
	// selfUpgradePatchRetryBackoff is the pause before the single retry.
	selfUpgradePatchRetryBackoff = 3 * time.Second
)

// selfUpgradePatchSleep is the pause between PATCH attempts; a var so tests
// can make the retry path instantaneous.
var selfUpgradePatchSleep = time.Sleep

// k8sNamespacePath is a var (not a const) so tests can redirect the
// service-account namespace file at a temp path. Production never reassigns it.
var k8sNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// RolloutRestartSelf patches this pod's own Deployment to trigger a
// rolling restart via the in-cluster K8s API.  This avoids os.Exit(0)
// which kills the pod immediately and causes downtime for spokes whose
// hub can't reach them via kubectl.
//
// Returns nil on success (K8s will send SIGTERM once the new pod is
// Ready).  On any failure the caller should fall back to os.Exit(0).
func RolloutRestartSelf(logger *slog.Logger) error {
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}

	restartAnnotation := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"hive.hivecommons.dev/restart-at":"%s"}}}}}`,
		time.Now().UTC().Format(time.RFC3339),
	)

	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	if err := k8sUpgradePatch(path, []byte(restartAnnotation)); err != nil {
		return fmt.Errorf("patching deployment %s/%s: %w", namespace, selfUpgradeDeployName, err)
	}

	logger.Info("deployment patched for rolling restart, waiting for SIGTERM",
		"namespace", namespace,
		"deployment", selfUpgradeDeployName,
	)
	return nil
}

// SwitchImageSelf patches this pod's own Deployment to change the image of
// every container (including init containers) to the given tag via the
// in-cluster K8s API. Used for branch switches delivered over heartbeat to
// spokes whose hub can't reach them via kubectl (the spoke has no kubectl
// binary, but its SA holds the hive-self-upgrade role: patch on
// deployment/hive). A strategic-merge patch merges containers by name, so we
// enumerate the deployment's containers first and set each image.
func SwitchImageSelf(logger *slog.Logger, image string) error {
	// Same contract as the hub self-upgrade: never write a tag that cannot
	// resolve. A spoke that patches itself to a bogus tag goes
	// ImagePullBackOff while its old ReplicaSet keeps serving — it looks alive,
	// silently runs stale code, and keeps reporting the OLD git hash, so the
	// hub re-sends the same doomed target every heartbeat forever.
	if err := validateImageRef(image); err != nil {
		logger.Error("self image switch REFUSED: invalid image reference",
			"image", image,
			"deployment", selfUpgradeDeployName,
			"error", err)
		return err
	}
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)

	names, err := k8sDeploymentContainerNames(path)
	if err != nil {
		return fmt.Errorf("listing deployment containers: %w", err)
	}
	if len(names.containers) == 0 && len(names.initContainers) == 0 {
		return fmt.Errorf("no containers found on deployment %s/%s", namespace, selfUpgradeDeployName)
	}

	// Idempotence. The hub re-sends a switch on EVERY heartbeat until the
	// spoke reports the new tag — and it is the OLD pod that heartbeats, on
	// the old tag, for as long as the new pod is still initializing. Each
	// re-send used to land here and stamp a fresh restart-at annotation onto
	// a Deployment whose images already said the target, which is a new pod
	// template: the ReplicaSet controller killed the pod that was mid-init
	// and started another. A spoke whose pod needs longer than one heartbeat
	// interval to become Ready never converged (observed live: 18
	// ReplicaSets in 8 minutes for one hive, none reaching Ready, while a
	// neighbour with the image already cached on its node won the race in
	// 31s). When every container already carries the target image, the
	// rollout is in flight — only backfill a stale startup probe, without
	// re-stamping the restart annotation. Subsequent calls leave it alone.
	if names.allAt(image) {
		if err := k8sUpgradePatch(path, nil); err != nil {
			return fmt.Errorf("backfilling deployment startup probe: %w", err)
		}
		rememberSelfImage(image)
		logger.Info("deployment already at target image — rollout in flight, not re-stamping",
			"namespace", namespace, "deployment", selfUpgradeDeployName, "image", image)
		return nil
	}

	var cs, ics []string
	for _, n := range names.containers {
		cs = append(cs, fmt.Sprintf(`{"name":%q,"image":%q}`, n, image))
	}
	for _, n := range names.initContainers {
		ics = append(ics, fmt.Sprintf(`{"name":%q,"image":%q}`, n, image))
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[%s],"initContainers":[%s]},"metadata":{"annotations":{"hive.hivecommons.dev/restart-at":%q}}}}}`,
		strings.Join(cs, ","), strings.Join(ics, ","), time.Now().UTC().Format(time.RFC3339),
	)
	if err := k8sUpgradePatch(path, []byte(patch)); err != nil {
		return fmt.Errorf("patching deployment image: %w", err)
	}
	rememberSelfImage(image)
	logger.Info("deployment image switched via in-cluster API, pod will roll",
		"namespace", namespace, "deployment", selfUpgradeDeployName, "image", image)
	return nil
}

// rememberSelfImage refreshes the cached SelfDeploymentImage answer after this
// pod has patched its own Deployment. The heartbeat reports that cached value
// as image_ref, and the hub treats a reported image_ref matching the switch
// tag as "switch complete" (server.go). Left to the cache TTL, the OLD pod
// kept reporting the OLD tag for minutes after the patch had landed, so the
// hub kept the switch armed and re-sent it every beat — the same loop the
// allAt guard in SwitchImageSelf closes from the other side.
func rememberSelfImage(image string) {
	selfImageMu.Lock()
	selfImageCached = image
	selfImageSourceCached = SelfImageSourceDeployment
	selfImageFetched = time.Now()
	selfImageAttempted = true
	selfImageMu.Unlock()
}

// mutableTagSuffix is retained for image-tag validation, which accepts the
// same per-branch moving-tag shape as the shared provenance classifier.
const mutableTagSuffix = "-latest"

const (
	SelfImageSourceUnknown    = "unknown"
	SelfImageSourceDeployment = "deployment"
	SelfImageSourcePodmanEnv  = "podman-env"

	selfImageEnv         = "HIVE_SELF_IMAGE"
	selfImageTrackingEnv = "HIVE_SELF_IMAGE_TRACKING"
)

// imageTagIsMutable reports whether restarting the pod can pick up new code for
// this image reference. Digest pins (@sha256:...) and SHA tags never can.
func imageTagIsMutable(image string) bool {
	return imageref.IsMutable(image)
}

// ImageTrackingMode returns the shared operator-facing classification for a
// Deployment image reference: "floating", "pinned", or "unknown".
func ImageTrackingMode(image string) string {
	return string(imageref.Tracking(image))
}

// PodmanSelfImageTrackingMode reports the tracking mode declared by Quadlet's
// setup-time environment when the self image came from HIVE_SELF_IMAGE.
func PodmanSelfImageTrackingMode(image string) string {
	switch strings.TrimSpace(os.Getenv(selfImageTrackingEnv)) {
	case "registry":
		return "registry"
	case "pinned":
		return "pinned"
	default:
		return ImageTrackingMode(image)
	}
}

// ImageReleaseChannel returns the explicit channel of a valid deployment ref.
func ImageReleaseChannel(image string) string {
	return imageref.ReleaseChannel(image)
}

// UpgradeSelfToSHA moves this pod onto targetSHA and returns whether a restart
// still has to be triggered by the caller.
//
// A `rollout restart` only picks up new code when the deployment tracks a
// MUTABLE tag (v2-latest and friends), because the restart re-pulls the same
// tag. A hive pinned to an immutable SHA tag restarts straight back onto the
// identical image, reports the same git hash, and gets told to upgrade again on
// the next heartbeat — an endless restart loop that never advances. torch-spyre
// sat on 63d8902 cycling pods this way while the hub re-sent e4506c4 every beat.
//
// So: patch the image when pinned, restart when mutable. Returns needsRestart
// false when the image patch itself rolls the deployment.
//
// The mutable case is NOT simply "restart and hope". A restart re-pulls the
// tag and therefore lands on whatever digest CI last published under it —
// which is only the requested target by coincidence. Verified on the live
// fleet: v2-latest resolved to fb37afe while the hub was targeting 07ccca1, so
// every restart delivered the wrong build, the spoke reported fb37afe, and the
// hub re-instructed the same upgrade forever. See upgradeSelfMutableToSHA.
func UpgradeSelfToSHA(logger *slog.Logger, targetSHA string) (needsRestart bool, err error) {
	current, err := selfDeploymentImage()
	if err != nil {
		// Can't tell which kind of tag we're on. A restart is the safe default:
		// it is correct for mutable tags (the common case) and merely useless —
		// never harmful — on a pin.
		logger.Warn("could not read own deployment image, defaulting to rollout restart",
			"error", err)
		return true, nil
	}
	if imageTagIsMutable(current) {
		// Release-channel spokes may be instructed to a newer immutable per-SHA
		// image before the floating channel tag has caught up. A restart of the
		// channel tag would just pull the old digest again, so patch the image to
		// the exact published SHA tag. The hub keeps the selected channel in its
		// hive record, so later channel moves still target this spoke as a channel
		// follower even while the Deployment is temporarily on a SHA tag.
		if ImageReleaseChannel(current) != "" && imageTagSHAPattern.MatchString(targetSHA) {
			idx := strings.LastIndex(current, ":")
			if idx >= 0 {
				newImage := current[:idx+1] + targetSHA
				if newImage == current {
					return false, nil
				}
				logger.Info("upgrading a release-channel deployment to an immutable SHA image",
					"from", current, "to", newImage)
				if err := SwitchImageSelf(logger, newImage); err != nil {
					return false, fmt.Errorf("patching release-channel image to %s: %w", newImage, err)
				}
				return false, nil
			}
		}
		return upgradeSelfMutableToSHA(logger, current, targetSHA)
	}
	// Pinned. Rewrite the tag in place so the registry/repo (which may be a
	// mirror or a private registry) is preserved — only the tag changes.
	idx := strings.LastIndex(current, ":")
	if idx < 0 {
		return true, nil
	}
	newImage := current[:idx+1] + targetSHA
	if newImage == current {
		return false, nil // already there; nothing to do
	}
	logger.Info("upgrading a SHA-pinned deployment by image patch, not restart",
		"from", current, "to", newImage)
	if err := SwitchImageSelf(logger, newImage); err != nil {
		return false, fmt.Errorf("patching pinned image to %s: %w", newImage, err)
	}
	// SwitchImageSelf's patch changes the pod template, which rolls the
	// deployment on its own — a restart on top would be redundant.
	return false, nil
}

// selfUpgradeTargetAnnotation records, on the pod template, which SHA the hub
// asked this deployment to run. It exists to make the pod template DIFFER
// between two upgrades that both leave the image string untouched — a mutable
// tag never changes, so without this the strategic-merge patch is a semantic
// no-op, Kubernetes sees no change to the template, and no rollout happens at
// all. Carrying the target (rather than a timestamp) also makes the intent
// readable with `kubectl get deploy -o yaml` when diagnosing a stuck upgrade.
const selfUpgradeTargetAnnotation = "hive.hivecommons.dev/upgrade-target-sha"

// upgradeSelfMutableToSHA advances a deployment that tracks a MUTABLE tag
// (v2-latest and friends) onto targetSHA.
//
// The naive action here — a plain rollout restart — is wrong, and was the
// systemic bug this function exists to fix. Restarting re-pulls the tag, so the
// pod lands on the tag's CURRENT digest. That is the hub's target only if CI
// happens not to have published since the target was chosen. When it has, the
// spoke comes back reporting a SHA that is neither the old one nor the
// requested one, the hub's completion check (which compares the reported
// gitHash against upgradeTarget) never matches, and the hive sits "Upgrading"
// forever while the hub re-instructs on every heartbeat.
//
// The fix keeps the floating tag — the platform's deliberate preference, which
// came out of a self-upgrade CrashLoop incident — and makes the rollout
// deterministic anyway, by writing the target SHA into a pod-template
// annotation. That changes the template even though the image string does not,
// which is what actually forces Kubernetes to roll. Combined with the
// fleet-wide imagePullPolicy: Always, the new pod re-pulls the tag.
//
// This narrows but does not eliminate the race: if CI republishes the tag
// between the hub choosing a target and the kubelet pulling, the pod still
// lands on the newer build. That overshoot is benign only because the hub no
// longer judges a floating-tag hive by whether it reached a SPECIFIC commit —
// a floating tag has no stable target, so that test could never converge while
// the branch kept moving (the pod would report a SHA that is neither the old
// nor the requested one, stay latched "Upgrading", and be re-armed and
// re-rolled by the stale-upgrade sweep every staleUpgradeTimeout). Instead the
// hub treats a floating-tag hive's non-upgrading heartbeat as completion
// (server.go completion check) and, in the sweep, clears the latch once such a
// hive reports the image-verified latest for its branch rather than advancing
// the target (saas.go triggerAutoUpgrades). What this function must never do is
// silently land on the WRONG build with no record of what was asked for; the
// annotation is that record.
func upgradeSelfMutableToSHA(logger *slog.Logger, current, targetSHA string) (needsRestart bool, err error) {
	if targetSHA == "" {
		// No target to record. Fall back to the historical behaviour rather
		// than writing an empty annotation that would defeat the diff.
		return true, nil
	}
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		// Not in-cluster, or the SA projection is missing. A plain restart is
		// the safe fallback: it is what this path did before, and the caller
		// already handles a failed restart loudly.
		logger.Warn("could not read namespace for mutable-tag upgrade, falling back to rollout restart",
			"error", err)
		return true, nil
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		logger.Warn("empty namespace for mutable-tag upgrade, falling back to rollout restart",
			"path", k8sNamespacePath)
		return true, nil
	}

	// Annotate the POD TEMPLATE (not the Deployment) — only template changes
	// roll the pods. restart-at is set alongside the target so two consecutive
	// upgrades to the SAME target still produce distinct templates; without it
	// a retry of an identical target would itself be a no-op.
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q,"hive.hivecommons.dev/restart-at":%q}}}}}`,
		selfUpgradeTargetAnnotation, targetSHA,
		time.Now().UTC().Format(time.RFC3339),
	)
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	if err := k8sUpgradePatch(path, []byte(patch)); err != nil {
		// Report the failure rather than swallowing it: the caller turns a
		// non-nil error into a recorded, hub-visible upgrade failure instead of
		// a permanent spinner.
		return false, fmt.Errorf("annotating mutable-tag deployment for rollout to %s: %w", targetSHA, err)
	}

	logger.Info("upgrading a mutable-tag deployment by pod-template annotation, not a bare restart",
		"image", current,
		"target", targetSHA,
		"annotation", selfUpgradeTargetAnnotation,
		"namespace", namespace,
		"deployment", selfUpgradeDeployName,
	)
	// The annotation patch changes the pod template, which rolls the deployment
	// on its own — a restart on top would be redundant.
	return false, nil
}

// selfDeploymentImage returns the image of this pod's own Deployment's first
// container, via the in-cluster API.
func selfDeploymentImage() (string, error) {
	image, _, err := selfDeploymentImageInfo()
	return image, err
}

// selfDeploymentImageInfo returns the image and imagePullPolicy of this pod's
// own Deployment's first container. The pull policy is returned as written in
// the spec (possibly empty: Kubernetes defaults it at admission and the
// stored spec usually carries the defaulted value, but not always).
func selfDeploymentImageInfo() (image, pullPolicy string, err error) {
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return "", "", fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return "", "", fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	body, err := k8sAPIGet(path)
	if err != nil {
		return "", "", err
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image           string `json:"image"`
						ImagePullPolicy string `json:"imagePullPolicy"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return "", "", err
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return "", "", fmt.Errorf("deployment %s/%s has no containers", namespace, selfUpgradeDeployName)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	return c.Image, c.ImagePullPolicy, nil
}

type deploymentContainerNames struct {
	containers     []string
	initContainers []string
	// images maps every container and init-container name to the image its
	// pod template currently carries, so a switch can tell "already patched,
	// rolling" from "needs patching" without a second GET.
	images map[string]string
}

// allAt reports whether every container and init-container already carries
// image. False for an empty deployment, so a caller never mistakes "nothing
// to compare" for "already there".
func (d deploymentContainerNames) allAt(image string) bool {
	if len(d.containers)+len(d.initContainers) == 0 {
		return false
	}
	for _, n := range append(append([]string{}, d.containers...), d.initContainers...) {
		if d.images[n] != image {
			return false
		}
	}
	return true
}

// k8sDeploymentContainerNames GETs the deployment and returns its container +
// init-container names, so a strategic-merge image patch targets them by name.
func k8sDeploymentContainerNames(path string) (deploymentContainerNames, error) {
	var out deploymentContainerNames
	body, err := k8sAPIGet(path)
	if err != nil {
		return out, err
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
					InitContainers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return out, err
	}
	out.images = make(map[string]string, len(dep.Spec.Template.Spec.Containers)+len(dep.Spec.Template.Spec.InitContainers))
	for _, c := range dep.Spec.Template.Spec.Containers {
		out.containers = append(out.containers, c.Name)
		out.images[c.Name] = c.Image
	}
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		out.initContainers = append(out.initContainers, c.Name)
		out.images[c.Name] = c.Image
	}
	return out, nil
}

// k8sAPIPatch issues a strategic-merge PATCH, retrying once on a failure that
// looks transient (a timeout, an admission webhook that did not answer, or a
// 5xx) so a single slow webhook round does not by itself fail an upgrade.
func k8sAPIPatch(path string, body []byte) error {
	return patchWithRetry(selfUpgradePatchAttempts, selfUpgradePatchRetryBackoff, selfUpgradePatchSleep,
		func() error { return k8sAPIPatchOnce(path, body) })
}

// patchWithRetry runs patch up to attempts times, sleeping backoff between
// tries, and stops early on success or on a failure that a retry cannot fix
// (RBAC denial, a malformed patch, a 4xx that is not an admission rejection).
func patchWithRetry(attempts int, backoff time.Duration, sleep func(time.Duration), patch func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			sleep(backoff)
		}
		err = patch()
		if err == nil {
			return nil
		}
		if !patchFailureRetryable(err) {
			return err
		}
	}
	return err
}

// patchFailureRetryable reports whether a PATCH error is worth one more try.
func patchFailureRetryable(err error) bool {
	switch ClassifyPatchFailure(err) {
	case PatchFailureAdmission, PatchFailureTimeout, PatchFailureServer:
		return true
	}
	return false
}

// PatchFailureKind classifies why a Deployment PATCH against the in-cluster
// API failed, so the caller can pick the right fallback and the hub can show
// an accurate hint instead of guessing.
type PatchFailureKind string

const (
	// PatchFailureNone: no error.
	PatchFailureNone PatchFailureKind = ""
	// PatchFailureAdmission: a validating/mutating admission webhook rejected
	// the patch or did not answer in time ("failed calling webhook", "admission
	// webhook ... denied"). Nothing the spoke does fixes this; a cluster admin
	// must repair or exempt the webhook.
	PatchFailureAdmission PatchFailureKind = "admission"
	// PatchFailureTimeout: the request itself hit the client deadline before the
	// apiserver answered. Observed when a webhook with failurePolicy Fail is
	// stuck: the apiserver waits on it past our timeout.
	PatchFailureTimeout PatchFailureKind = "timeout"
	// PatchFailureForbidden: HTTP 401/403 — the spoke's ServiceAccount lacks
	// patch on deployments/hive (hive-self-upgrade Role/RoleBinding).
	PatchFailureForbidden PatchFailureKind = "forbidden"
	// PatchFailureServer: a 5xx that was not identifiably a webhook failure.
	PatchFailureServer PatchFailureKind = "server"
	// PatchFailureOther: anything else (bad patch, missing token, no network).
	PatchFailureOther PatchFailureKind = "other"
)

// ClassifyPatchFailure inspects a PATCH error's text — the apiserver reports
// admission failures as `Internal error occurred: failed calling webhook
// "<name>": ... context deadline exceeded` inside a 500 body, and a stuck
// webhook surfaces client-side as a plain context deadline — and maps it to a
// PatchFailureKind. Admission wins over timeout and server so the hint names
// the webhook when the body does.
func ClassifyPatchFailure(err error) PatchFailureKind {
	if err == nil {
		return PatchFailureNone
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "failed calling webhook"),
		strings.Contains(msg, "admission webhook"),
		strings.Contains(msg, "admission"):
		return PatchFailureAdmission
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "client.timeout exceeded"),
		strings.Contains(msg, "timeout awaiting response headers"):
		return PatchFailureTimeout
	case strings.Contains(msg, "http 401"), strings.Contains(msg, "http 403"):
		return PatchFailureForbidden
	case strings.Contains(msg, "http 5"):
		return PatchFailureServer
	}
	return PatchFailureOther
}

// PatchFailureHint renders the operator-facing explanation for a kind. The
// admission and timeout hints both name the webhook: on the observed cluster a
// Kyverno webhook (failurePolicy Fail, restart-looping controller) timed out
// EVERY Deployment patch, and the old "Deployment tracks a floating tag" hint
// sent the operator to the wrong place for 46 minutes.
func PatchFailureHint(kind PatchFailureKind) string {
	switch kind {
	case PatchFailureAdmission, PatchFailureTimeout:
		return "cluster admission webhook (e.g. Kyverno) is rejecting or timing out Deployment patches — " +
			"a cluster admin must fix or exempt the webhook; falling back to a process restart"
	case PatchFailureForbidden:
		return "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)"
	case PatchFailureServer:
		return "the Kubernetes API server returned a server error for the Deployment patch; falling back to a process restart"
	case PatchFailureNone:
		return ""
	}
	return "the Deployment patch failed; falling back to a process restart"
}

// RestartFallback is the decision taken when the Deployment PATCH that was
// supposed to roll the pod has failed.
type RestartFallback struct {
	// Kind is the PATCH failure classification.
	Kind PatchFailureKind
	// RestartLands is true when exiting the process (kubelet restarts the
	// container) is expected to deliver the upgrade anyway: the Deployment
	// tracks a floating tag and re-pulls it on every container start. The
	// caller then records the attempt as pending-restart, not failed.
	RestartLands bool
	// Hint is the operator-facing explanation for the hub.
	Hint string
}

// restartFallbackFor decides whether a container restart will land the
// upgrade after a failed PATCH. It lands when the image tracks a mutable tag
// and the pull policy re-pulls on start (Always, or unset — which Kubernetes
// defaults to Always for :latest and we cannot distinguish without the
// defaulted spec). It cannot land on a digest/SHA pin, or when the kubelet
// would reuse the cached image (IfNotPresent/Never). When the image could
// not be read or classified at all, fall through to the restart: it is the
// only lever left and is never harmful.
func restartFallbackFor(patchErr error, image, pullPolicy string, imageErr error) RestartFallback {
	fb := RestartFallback{Kind: ClassifyPatchFailure(patchErr)}
	fb.Hint = PatchFailureHint(fb.Kind)
	switch {
	case imageErr != nil || strings.TrimSpace(image) == "":
		fb.RestartLands = true
	case imageref.IsPinned(image):
		fb.RestartLands = false
	case !imageTagIsMutable(image):
		// Unknown tracking (untagged ref, malformed): no proof it is a pin,
		// so take the restart — it is the only lever and never harmful.
		fb.RestartLands = true
	default:
		switch strings.TrimSpace(pullPolicy) {
		case "", "Always":
			fb.RestartLands = true
		default:
			fb.RestartLands = false
		}
	}
	if !fb.RestartLands && fb.Hint != "" {
		fb.Hint = strings.TrimSuffix(fb.Hint, "; falling back to a process restart") +
			"; a restart cannot land the target because the Deployment image does not re-pull a floating tag"
	}
	return fb
}

// RestartFallbackForPatchFailure reads this pod's own Deployment image and pull
// policy and decides whether a process-exit restart will still deliver the
// upgrade whose PATCH just failed with patchErr.
func RestartFallbackForPatchFailure(logger *slog.Logger, patchErr error) RestartFallback {
	image, pullPolicy, err := selfDeploymentImageInfo()
	if err != nil {
		logger.Warn("could not read own deployment image after a failed patch; assuming a restart re-pulls it",
			"error", err)
	}
	return restartFallbackFor(patchErr, image, pullPolicy, err)
}

func k8sAPIPatchOnce(path string, body []byte) error {
	token, err := os.ReadFile(k8sTokenPath)
	if err != nil {
		return fmt.Errorf("reading service account token: %w", err)
	}

	tlsConfig, err := k8sTLSConfig()
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout:   selfUpgradePatchTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	ctx, cancel := context.WithTimeout(context.Background(), selfUpgradePatchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, k8sAPIServer+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("k8s API PATCH %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("k8s API PATCH %s: HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

// selfImageCacheTTL bounds how long SelfDeploymentImage reuses a cached read.
// The image only changes on an upgrade (which rolls the pod and restarts this
// process, clearing the cache anyway), so a long TTL is safe — and necessary:
// the heartbeat runs on a short interval and an uncached read would hit the
// in-cluster API server on every beat, from every spoke in the fleet.
const selfImageCacheTTL = 10 * time.Minute

var (
	selfImageMu           sync.RWMutex
	selfImageCached       string
	selfImageSourceCached = SelfImageSourceUnknown
	selfImageFetched      time.Time
	selfImageAttempted    bool
)

// SelfImageReleaseChannel returns the explicit channel in the validated
// Deployment ref. It inherits SelfDeploymentImage's caching and returns an
// empty string when discovery fails or the ref has no known channel.
func SelfImageReleaseChannel() string {
	return ImageReleaseChannel(SelfDeploymentImage())
}

// SelfDeploymentImage returns this pod's own Deployment image, cached.
//
// It is exported for the SPOKE's heartbeat collector: the hub cannot read a
// firewalled spoke's Deployment over kubectl, so the spoke must report its own
// image ref for the hub's pinned-image drift detection to work at all.
//
// Returns "" (never an error) when the read fails or this process is not
// running in-cluster — an absent image ref means "unknown", and drift
// detection skips the pinned-image signal rather than inventing one. A failed
// read is cached the same as a successful one so a non-cluster process does
// not retry an API call it can never satisfy on every heartbeat.
func SelfDeploymentImage() string {
	selfImageMu.RLock()
	if selfImageAttempted && time.Since(selfImageFetched) < selfImageCacheTTL {
		img := selfImageCached
		selfImageMu.RUnlock()
		return img
	}
	selfImageMu.RUnlock()

	img, err := selfDeploymentImage()
	source := SelfImageSourceDeployment
	if err != nil {
		img = strings.TrimSpace(os.Getenv(selfImageEnv))
		if img != "" {
			source = SelfImageSourcePodmanEnv
		} else {
			source = SelfImageSourceUnknown
		}
	}

	selfImageMu.Lock()
	selfImageCached = img
	selfImageSourceCached = source
	selfImageFetched = time.Now()
	selfImageAttempted = true
	selfImageMu.Unlock()
	return img
}

// SelfDeploymentImageSource returns where SelfDeploymentImage's current cached
// value came from, forcing the same lookup when no cached value exists yet.
func SelfDeploymentImageSource() string {
	_ = SelfDeploymentImage()
	selfImageMu.RLock()
	source := selfImageSourceCached
	selfImageMu.RUnlock()
	return source
}
