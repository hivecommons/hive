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
)

const (
	selfUpgradeDeployName = "hive"
	selfUpgradeTimeout    = 10 * time.Second
)

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
		`{"spec":{"template":{"metadata":{"annotations":{"hive.kubestellar.io/restart-at":"%s"}}}}}`,
		time.Now().UTC().Format(time.RFC3339),
	)

	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	if err := k8sAPIPatch(path, []byte(restartAnnotation)); err != nil {
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

	var cs, ics []string
	for _, n := range names.containers {
		cs = append(cs, fmt.Sprintf(`{"name":%q,"image":%q}`, n, image))
	}
	for _, n := range names.initContainers {
		ics = append(ics, fmt.Sprintf(`{"name":%q,"image":%q}`, n, image))
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[%s],"initContainers":[%s]},"metadata":{"annotations":{"hive.kubestellar.io/restart-at":%q}}}}}`,
		strings.Join(cs, ","), strings.Join(ics, ","), time.Now().UTC().Format(time.RFC3339),
	)
	if err := k8sAPIPatch(path, []byte(patch)); err != nil {
		return fmt.Errorf("patching deployment image: %w", err)
	}
	logger.Info("deployment image switched via in-cluster API, pod will roll",
		"namespace", namespace, "deployment", selfUpgradeDeployName, "image", image)
	return nil
}

// mutableTagSuffix marks image tags that CI republishes in place (v2-latest,
// v3-latest, mk-latest, ...). A restart re-pulls those and lands on the new
// build; any other tag is an immutable SHA pin where a restart is a no-op.
const mutableTagSuffix = "-latest"

// imageTagIsMutable reports whether restarting the pod can pick up new code for
// this image reference. Digest pins (@sha256:...) and SHA tags never can.
func imageTagIsMutable(image string) bool {
	if image == "" || strings.Contains(image, "@") {
		return false
	}
	// Only the part after the LAST colon is a tag, and only if it has no slash
	// after it — otherwise it's a registry port (host:5000/repo).
	idx := strings.LastIndex(image, ":")
	if idx < 0 || strings.Contains(image[idx+1:], "/") {
		return false // no tag at all — implicitly :latest, but don't guess
	}
	tag := image[idx+1:]
	// Release-channel tags ("stable"/"candidate"/"edge") are retagged digests —
	// mutable by definition, even though they carry no "-latest" suffix.
	// Without this, a channel-tracking hive classifies as an immutable pin and
	// a delivered upgrade rewrites its image to a SHA tag, silently un-tracking
	// the channel in-cluster.
	if isReleaseChannel(tag) {
		return true
	}
	return strings.HasSuffix(tag, mutableTagSuffix)
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
const selfUpgradeTargetAnnotation = "hive.kubestellar.io/upgrade-target-sha"

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
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q,"hive.kubestellar.io/restart-at":%q}}}}}`,
		selfUpgradeTargetAnnotation, targetSHA,
		time.Now().UTC().Format(time.RFC3339),
	)
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	if err := k8sAPIPatch(path, []byte(patch)); err != nil {
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
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return "", fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return "", fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)
	body, err := k8sAPIGet(path)
	if err != nil {
		return "", err
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return "", err
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("deployment %s/%s has no containers", namespace, selfUpgradeDeployName)
	}
	return dep.Spec.Template.Spec.Containers[0].Image, nil
}

type deploymentContainerNames struct {
	containers     []string
	initContainers []string
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
						Name string `json:"name"`
					} `json:"containers"`
					InitContainers []struct {
						Name string `json:"name"`
					} `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return out, err
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		out.containers = append(out.containers, c.Name)
	}
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		out.initContainers = append(out.initContainers, c.Name)
	}
	return out, nil
}

func k8sAPIPatch(path string, body []byte) error {
	token, err := os.ReadFile(k8sTokenPath)
	if err != nil {
		return fmt.Errorf("reading service account token: %w", err)
	}

	tlsConfig, err := k8sTLSConfig()
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout:   selfUpgradeTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	ctx, cancel := context.WithTimeout(context.Background(), selfUpgradeTimeout)
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
	selfImageMu        sync.RWMutex
	selfImageCached    string
	selfImageFetched   time.Time
	selfImageAttempted bool
)

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
// SelfImageReleaseChannel returns the release-channel name ("stable",
// "candidate", "edge") when this pod's own Deployment image tag IS a channel
// tag, else "". The spoke's dashboard uses it to label its version badge
// "stable (v4)" instead of the bare baked-in branch — the binary itself has no
// idea it was delivered via a channel (a stable retag of a v4 build is built
// from v4), only the deployment's image tag knows. Inherits
// SelfDeploymentImage's caching and its "" on any failure.
func SelfImageReleaseChannel() string {
	tag := imageTagOf(SelfDeploymentImage())
	if isReleaseChannel(tag) {
		return tag
	}
	return ""
}

func SelfDeploymentImage() string {
	selfImageMu.RLock()
	if selfImageAttempted && time.Since(selfImageFetched) < selfImageCacheTTL {
		img := selfImageCached
		selfImageMu.RUnlock()
		return img
	}
	selfImageMu.RUnlock()

	img, err := selfDeploymentImage()
	if err != nil {
		img = ""
	}

	selfImageMu.Lock()
	selfImageCached = img
	selfImageFetched = time.Now()
	selfImageAttempted = true
	selfImageMu.Unlock()
	return img
}
