package kubejob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/hivecommons/hive/crux/sandbox"
)

// Sandbox relays on the Job runtime (#18512).
//
// The project image knows nothing about hive, so an init container from the
// running hive image (by digest, so the relay matches the hive that reads its
// spool) copies sandbox.RelayNames into an emptyDir. The kick container mounts
// that emptyDir read-only at sandbox.RelayMountPath and runs its command
// through sandbox.RelayCommand, which checks for bash and python3 and puts the
// relays first on PATH.
//
// The init container is built to pass OpenShift's restricted SCC and the
// Kubernetes "restricted" Pod Security Standard: non-root, read-only root
// filesystem, no privilege escalation, every capability dropped, RuntimeDefault
// seccomp. Its uid is left for the SCC to assign on OpenShift (a fixed uid
// outside the namespace range is rejected there); elsewhere the hive image's
// default user is root, which runAsNonRoot refuses, so it runs as
// DefaultRelayRunAsUser unless Options.RelayRunAsUser says otherwise.

const (
	// RelayImageNone in Options.RelayImage skips the relays.
	RelayImageNone = "none"
	// DefaultRelayRunAsUser is the relay init container's uid off OpenShift
	// (nobody). It only reads the relays and writes the emptyDir.
	DefaultRelayRunAsUser int64 = 65534

	relayVolumeName        = "hive-relays"
	relayInitContainerName = "hive-relays"
	// relayVolumeSizeLimit bounds the emptyDir; the relays are a few KiB.
	relayVolumeSizeLimit = "16Mi"
	// relaySourceDir is where the hive image installs its relays (Dockerfile).
	relaySourceDir = "/usr/local/bin"
	// hiveContainerName is the hive container in hive's own Deployment.
	hiveContainerName = "hive"
	// openshiftSCCAnnotation is set by OpenShift on every pod it admits.
	openshiftSCCAnnotation = "openshift.io/scc"
	pullIfNotPresent       = "IfNotPresent"
)

// relayPlan is what the manifest needs to add the relay init container.
type relayPlan struct {
	image       string
	runAsUser   *int64
	pullSecrets []map[string]string
}

func (p *relayPlan) initContainer() map[string]any {
	sc := map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []string{"ALL"}},
		"seccompProfile":           map[string]string{"type": "RuntimeDefault"},
		"runAsNonRoot":             true,
		"readOnlyRootFilesystem":   true,
	}
	if p.runAsUser != nil {
		sc["runAsUser"] = *p.runAsUser
	}
	command := []string{"cp"}
	for _, name := range sandbox.RelayNames {
		command = append(command, path.Join(relaySourceDir, name))
	}
	command = append(command, sandbox.RelayMountPath+"/")
	return map[string]any{
		"name":            relayInitContainerName,
		"image":           p.image,
		"imagePullPolicy": pullIfNotPresent,
		"command":         command,
		"securityContext": sc,
		"volumeMounts":    []map[string]any{{"name": relayVolumeName, "mountPath": sandbox.RelayMountPath}},
	}
}

// selfPod is the part of hive's own pod the relay plan reads.
type selfPod struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
		ImagePullSecrets []map[string]string `json:"imagePullSecrets"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses []struct {
			Name    string `json:"name"`
			Image   string `json:"image"`
			ImageID string `json:"imageID"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// resolveRelays builds the relay plan, or nil when relays are switched off.
// Hive's own pod is read only when the image or the uid is not configured.
func (l *Launcher) resolveRelays(ctx context.Context, ns string) (*relayPlan, error) {
	image := strings.TrimSpace(l.Options.RelayImage)
	if image == RelayImageNone {
		return nil, nil
	}
	plan := &relayPlan{image: image, runAsUser: l.Options.RelayRunAsUser}
	if image != "" && plan.runAsUser != nil {
		return plan, nil
	}
	pod, err := l.selfPod(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("kubejob: reading hive's own pod to supply the sandbox relays (%s): %w; set job.relay_image and job.relay_run_as_user, or job.relay_image: %s to skip the relays", strings.Join(sandbox.RelayNames, ", "), err, RelayImageNone)
	}
	if plan.image == "" {
		if plan.image = pod.hiveImage(); plan.image == "" {
			return nil, fmt.Errorf("kubejob: hive's own pod reports no image to supply the sandbox relays from; set job.relay_image, or job.relay_image: %s to skip the relays", RelayImageNone)
		}
		// The init container pulls the hive image, so it needs the
		// credentials hive's own pod pulled it with.
		plan.pullSecrets = pod.Spec.ImagePullSecrets
	}
	if plan.runAsUser == nil {
		if _, openshift := pod.Metadata.Annotations[openshiftSCCAnnotation]; !openshift {
			uid := DefaultRelayRunAsUser
			plan.runAsUser = &uid
		}
	}
	return plan, nil
}

func (l *Launcher) selfPod(ctx context.Context, ns string) (selfPod, error) {
	var pod selfPod
	name, err := l.podName()
	if err != nil {
		return pod, err
	}
	raw, err := l.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, name), nil)
	if err != nil {
		return pod, err
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return pod, fmt.Errorf("decoding pod %s: %w", name, err)
	}
	return pod, nil
}

func (l *Launcher) podName() (string, error) {
	for _, candidate := range []string{l.PodName, os.Getenv("POD_NAME"), os.Getenv("HOSTNAME")} {
		if c := strings.TrimSpace(candidate); c != "" {
			return c, nil
		}
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "", errors.New("cannot determine hive's own pod name")
	}
	return strings.TrimSpace(host), nil
}

// hiveImage is the hive container's running image by digest when the runtime
// reports a pullable one, else the image as written in the status or spec.
// The container named "hive" is preferred; failing that, the first one.
func (p selfPod) hiveImage() string {
	statuses := p.Status.ContainerStatuses
	for i := range statuses {
		if statuses[i].Name == hiveContainerName {
			statuses = statuses[i : i+1]
			break
		}
	}
	if len(statuses) > 0 {
		cs := statuses[0]
		if ref := pullableDigest(cs.ImageID); ref != "" {
			return ref
		}
		if img := strings.TrimSpace(cs.Image); img != "" && !strings.HasPrefix(img, "sha256:") {
			return img
		}
	}
	containers := p.Spec.Containers
	for i := range containers {
		if containers[i].Name == hiveContainerName {
			return strings.TrimSpace(containers[i].Image)
		}
	}
	if len(containers) > 0 {
		return strings.TrimSpace(containers[0].Image)
	}
	return ""
}

// pullableDigest turns a container status imageID into a pullable
// repo@sha256 reference, or "" when it is only a bare image ID.
func pullableDigest(imageID string) string {
	ref := strings.TrimSpace(imageID)
	if _, rest, ok := strings.Cut(ref, "://"); ok {
		ref = rest
	}
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok || repo == "" || !strings.HasPrefix(digest, "sha256:") {
		return ""
	}
	return ref
}
