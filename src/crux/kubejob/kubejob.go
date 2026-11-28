// Package kubejob runs a sandboxed agent kick as a Kubernetes Job instead of a
// rootless Podman container on the hive pod's own node (#6311).
//
// It implements sandbox.Launcher over the in-cluster REST API with the spoke's
// own ServiceAccount, so a spoke on a pull-only cluster needs no kubectl and
// no hub round-trip. The contract is deliberately the same as the Podman
// launcher: run ONE command in ONE image against the workspace directory hive
// prepared, return stdout, stderr and an exit code, and let the executor read
// the commits back out of the workspace. What the Job adds is exactly what
// Podman on the hive node cannot give an operator: a different image with its
// own toolchain, scheduling onto a node with accelerator hardware via a node
// selector and device resource limits, and secrets delivered by reference.
//
// Workspace transport is a SHARED PersistentVolumeClaim, not a copy. Hive's
// sandbox workspace root lives on its data PVC; the Job mounts the same claim
// with a subPath pointing at the per-kick workspace, at the same mount path the
// Podman launcher uses. When the Job is scheduled onto a different node than
// hive, that claim has to be ReadWriteMany — the launcher cannot check this
// and says so in its documentation rather than guessing.
// workspace_transport: copy (#19621, copy.go) copies the workspace into a
// pod-local volume and back instead, so no shared claim is needed.
//
// The credential boundary is the whole point of keeping the executor
// unchanged: the Job gets only the Secrets the operator names in
// env_from_secrets. Hive's brokered GitHub token never enters the Job; hive
// pushes and opens the PR from the workspace after the Job has exited, through
// the same pushbroker path the Podman sandbox uses.
package kubejob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/posture"
	"github.com/hivecommons/hive/crux/sandbox"
)

const (
	// DefaultAPIServer is the in-cluster API endpoint every pod can reach.
	DefaultAPIServer = "https://kubernetes.default.svc"
	// DefaultTokenPath and DefaultCAPath are the projected ServiceAccount
	// credentials kubelet mounts into every pod.
	DefaultTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	DefaultNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// DefaultWorkspaceClaimMount is where hive's data PVC is mounted inside
	// the hive pod; the per-kick workspace path is made relative to it to
	// derive the Job's subPath.
	DefaultWorkspaceClaimMount = "/data"

	// DefaultTTLSeconds keeps a finished Job (and its pod, and therefore its
	// logs) around for an hour so an operator can inspect a failure with
	// kubectl before the cluster garbage-collects it.
	DefaultTTLSeconds = 3600

	// DefaultPollInterval is how often the launcher re-reads the Job status.
	DefaultPollInterval = 5 * time.Second

	// DefaultKickTimeout is the sandbox kick timeout hive applies when none is
	// configured; crux/agent's DefaultSandboxKickTimeout is this value.
	DefaultKickTimeout = 45 * time.Minute

	// DeadlineMargin is added to a kick's timeout to give the Job's
	// activeDeadlineSeconds. Hive cancels a kick at its timeout and deletes
	// the Job itself; the deadline is the cluster-side backstop for a Job
	// hive lost track of (a restart mid-kick, a Job left Pending or
	// Unschedulable), so it must land after hive's own timeout, never before
	// (hivecommons/hive#18525).
	DeadlineMargin = 15 * time.Minute

	// requestTimeout bounds every single API call; the overall run is bounded
	// by the caller's context (the sandbox kick timeout).
	requestTimeout = 30 * time.Second

	// maxLogBytes caps how much of the pod log is read back as stdout. The
	// transcript is scrubbed and persisted by the executor; a runaway log
	// must not turn into a runaway allocation on the spoke.
	maxLogBytes = 16 << 20

	// jobNameMax is Kubernetes' limit for a Job name that must also serve as
	// a DNS label prefix for its pods.
	jobNameMax = 63

	containerName = "kick"
	jobLabelKey   = config.SandboxPodJobNameLabel
	// managedByLabel marks Jobs this launcher created so an operator (or a
	// future sweeper) can list them without guessing at name prefixes.
	managedByLabel = config.SandboxPodManagedByLabel
	managedByValue = "hive-kubejob"
	// jobLabel marks a Job's pods with the Job name so a per-Job
	// NetworkPolicy can select them without relying on the controller's
	// own job-name label, whose key changed across Kubernetes releases.
	jobLabel         = config.SandboxPodReservedLabelPrefix + "job"
	agentLabel       = config.SandboxPodReservedLabelPrefix + "agent"
	namePrefix       = "hive-kick-"
	restartNever     = "Never"
	deletePropagate  = "Background"
	condComplete     = "Complete"
	condFailed       = "Failed"
	condStatusTrue   = "True"
	stateTerminated  = "terminated"
	exitCodeUnknown  = -1
	labelSelectorFmt = "%s=%s"
)

// Options is everything an operator can set on the Job beyond what the
// LaunchSpec carries. All fields are optional except WorkspaceClaim, which
// the copy WorkspaceTransport does not need.
type Options struct {
	// WorkspaceClaim names the PersistentVolumeClaim hive's sandbox workspace
	// root lives on. Required: without it there is no way for the Job to see
	// the workspace at all.
	WorkspaceClaim string
	// WorkspaceClaimMount is where that claim is mounted INSIDE THE HIVE POD
	// (default /data). The Job's subPath is the workspace path relative to it.
	WorkspaceClaimMount string
	// WorkspaceTransport is "claim" (the default, empty) or "copy", which
	// copies the workspace into a pod-local volume and back (#19621; see
	// copy.go).
	WorkspaceTransport string
	// WorkspaceSizeLimit is the sizeLimit of the copy transport's workspace
	// emptyDir (default DefaultWorkspaceSizeLimit).
	WorkspaceSizeLimit string
	// NodeSelector, Affinity, Tolerations, Resources, ServiceAccount are passed
	// through to the pod template verbatim.
	NodeSelector   map[string]string
	Affinity       *config.SandboxJobAffinity
	Tolerations    []Toleration
	Resources      Resources
	ServiceAccount string
	// SchedulerName is passed through as the pod's schedulerName, for clusters
	// whose admission webhook requires device-requesting pods to use a custom
	// scheduler. Empty leaves the cluster default.
	SchedulerName string
	// RuntimeClassName is the pod's runtimeClassName (for example "kata").
	// Empty leaves the cluster default. The copy transport refuses to run
	// with it empty when hive's own pod has a runtime class (copy.go).
	RuntimeClassName string
	// PodLabels and PodAnnotations are operator-configured Job metadata.
	PodLabels      map[string]string
	PodAnnotations map[string]string
	// EnvFromSecrets are Secret names whose keys become environment variables
	// in the Job. This is the ONLY credential path into the Job.
	EnvFromSecrets []string
	// ImagePullSecrets are Secret names added to the pod's imagePullSecrets
	// alongside hive's own and the service account's (#18819). Names only.
	ImagePullSecrets []string
	// ExtraVolumes mounts additional claims (a compile cache, a model store).
	ExtraVolumes []VolumeMount
	// TTLSeconds is the Job's ttlSecondsAfterFinished (default one hour).
	TTLSeconds int
	// Network is NetworkDefault, NetworkNone or NetworkHost; empty is
	// NetworkDefault so the agent sandbox, which needs the network, is
	// unchanged. The contained-run backend sets NetworkNone by default.
	Network string
	// PollInterval overrides how often the Job status is read (tests).
	PollInterval time.Duration
	// RelayImage is the image of the init container that supplies hive's
	// sandbox relays to a LaunchSpec with Relays set (#18512). Empty resolves
	// the running hive image, by digest, from hive's own pod; RelayImageNone
	// skips the relays.
	RelayImage string
	// RelayRunAsUser pins the relay init container's uid. Nil leaves it to
	// the cluster on OpenShift and uses DefaultRelayRunAsUser elsewhere.
	RelayRunAsUser *int64
	// ActiveDeadlineSeconds is the Job's activeDeadlineSeconds. Non-positive
	// derives it from the run's context deadline, else from
	// DefaultKickTimeout; either way DeadlineMargin is added. Run uses
	// whichever of this and the context-derived value is later, so the
	// deadline never cuts a run short of the timeout hive gave it.
	ActiveDeadlineSeconds int64
	// Preflight, PreflightTimeout, PreflightTotalTimeout and
	// PreflightRetryDelay configure the node preflight (#18811); see
	// preflight.go. Preflight empty leaves the Job exactly as before.
	Preflight             []string
	PreflightTimeout      time.Duration
	PreflightTotalTimeout time.Duration
	PreflightRetryDelay   time.Duration
	// Setup, SetupHealth, SetupTimeout and SetupTotalTimeout configure the
	// workspace setup step (#19574); see setup.go. A failed health command
	// retries on another node after PreflightRetryDelay, as a failed
	// preflight does. Both lists empty leave the Job exactly as before.
	Setup             []string
	SetupHealth       []string
	SetupTimeout      time.Duration
	SetupTotalTimeout time.Duration
	// ScheduleTimeout is how long the pod may stay Unschedulable before the
	// kick fails with waiting_for_node; zero uses DefaultScheduleTimeout.
	ScheduleTimeout time.Duration
	// OnEvent, when set, is told about scheduling and preflight-retry events
	// so the manager can surface them (see Event). It must not block.
	OnEvent func(Event)
}

// ActiveDeadlineSeconds returns the activeDeadlineSeconds for a Job running
// one kick with the given timeout: the timeout (DefaultKickTimeout when
// non-positive) plus DeadlineMargin.
func ActiveDeadlineSeconds(kickTimeout time.Duration) int64 {
	if kickTimeout <= 0 {
		kickTimeout = DefaultKickTimeout
	}
	return int64((kickTimeout + DeadlineMargin) / time.Second)
}

// activeDeadline resolves the deadline for one Run: the configured value or
// the one derived from ctx's remaining time, whichever is later.
func (l *Launcher) activeDeadline(ctx context.Context) int64 {
	deadline := l.Options.ActiveDeadlineSeconds
	if dl, ok := ctx.Deadline(); ok {
		if fromCtx := ActiveDeadlineSeconds(time.Until(dl)); fromCtx > deadline {
			deadline = fromCtx
		}
	}
	if deadline <= 0 {
		deadline = ActiveDeadlineSeconds(DefaultKickTimeout)
	}
	return deadline
}

// network is Options.Network with the empty default applied.
func (o Options) network() string {
	if o.Network == "" {
		return NetworkDefault
	}
	return o.Network
}

// Launcher implements sandbox.Launcher by running the kick as a Job.
type Launcher struct {
	Options Options

	// The fields below default to the in-cluster values and exist so tests
	// can point the launcher at an httptest server with a fixed namespace.
	APIServer     string
	Namespace     string
	TokenPath     string
	CAPath        string
	NamespacePath string
	HTTPClient    *http.Client
	Now           func() time.Time
	// PodName is hive's own pod, read to resolve the relay image; empty
	// falls back to $POD_NAME, then $HOSTNAME, then os.Hostname.
	PodName string
	// BridgeURL and BridgeClient replace the job bridge's address (the pod
	// IP) and HTTP client on the copy transport (tests).
	BridgeURL    string
	BridgeClient *http.Client

	// copyPlan is set on a per-run copy of the Launcher on the copy
	// transport.
	copyPlan *copyPlan
	// bridgeToken fixes the job bridge token instead of a random one (tests).
	bridgeToken string
}

var _ sandbox.Launcher = (*Launcher)(nil)

// Validate reports the configuration errors that would otherwise surface only
// as a failed kick.
func (o Options) Validate() error {
	if err := o.validateTransport(); err != nil {
		return err
	}
	if o.TTLSeconds < 0 {
		return errors.New("kubejob: ttl_seconds must not be negative")
	}
	if err := config.ValidateRuntimeClassName(o.RuntimeClassName); err != nil {
		return fmt.Errorf("kubejob: %w", err)
	}
	if err := config.ValidateSandboxJobMetadata(&config.SandboxJobConfig{PodLabels: o.PodLabels, PodAnnotations: o.PodAnnotations}); err != nil {
		return fmt.Errorf("kubejob: %w", err)
	}
	for i, v := range o.ExtraVolumes {
		if strings.TrimSpace(v.Claim) == "" || strings.TrimSpace(v.MountPath) == "" {
			return fmt.Errorf("kubejob: volumes[%d] needs both claim and mount_path", i)
		}
	}
	if o.RelayRunAsUser != nil && *o.RelayRunAsUser <= 0 {
		return errors.New("kubejob: relay_run_as_user must be a positive (non-root) uid")
	}
	if err := validatePullSecretNames(o.ImagePullSecrets); err != nil {
		return err
	}
	if err := o.validatePreflight(); err != nil {
		return err
	}
	if err := o.validateSetup(); err != nil {
		return err
	}
	switch o.network() {
	case NetworkDefault, NetworkNone, NetworkHost:
	default:
		return fmt.Errorf("kubejob: network must be %q, %q or %q, got %q", NetworkNone, NetworkHost, NetworkDefault, o.Network)
	}
	return nil
}

// Run creates the Job, waits for it to finish or for ctx to end, collects the
// pod log and exit code, and deletes the Job on success. A Job that failed is
// left in place until its TTL so the operator can inspect it.
func (l *Launcher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	var res sandbox.Result
	if spec.ClineProvider != nil {
		_ = spec.ClineProvider.Close()
		return res, errors.New("kubejob: scoped Cline provider files are Podman-only; configure Job Secret references instead")
	}
	if err := l.Options.Validate(); err != nil {
		return res, err
	}
	if strings.TrimSpace(spec.Image) == "" {
		return res, errors.New("kubejob: image is required")
	}
	if strings.TrimSpace(spec.Workspace) == "" {
		return res, errors.New("kubejob: workspace is required")
	}
	copyMode := l.Options.copiesWorkspace()
	if copyMode {
		if err := refuseCopyWhenHardened(os.Getenv); err != nil {
			return res, err
		}
	}
	subPath := ""
	if !copyMode {
		var err error
		if subPath, err = workspaceSubPath(spec.Workspace, l.Options.WorkspaceClaimMount); err != nil {
			return res, err
		}
	}
	ns, err := l.namespace()
	if err != nil {
		return res, err
	}
	var relays *relayPlan
	// The relay spool needs a directory hive and the Job share while the
	// kick runs, which the copy transport does not have.
	if spec.Relays && !copyMode {
		if relays, err = l.resolveRelays(ctx, ns); err != nil {
			return res, err
		}
	}
	if spec.Relays && len(l.Options.Preflight) > 0 && len(spec.Command) == 0 {
		return res, errors.New("kubejob: sandbox.preflight needs an explicit command to run the agent after it; this launch has none")
	}
	if spec.Relays && l.Options.setupEnabled() && len(spec.Command) == 0 {
		return res, errors.New("kubejob: sandbox.setup needs an explicit command to run the agent after it; this launch has none")
	}
	if copyMode {
		plan, err := l.resolveCopy(ctx, ns, spec)
		if err != nil {
			return res, err
		}
		cp := *l
		cp.copyPlan = plan
		l = &cp
	}
	return l.runWithRetry(ctx, spec, ns, subPath, relays)
}

type jobOutcome struct {
	succeeded bool
	reason    string
	// failReason is sandbox.FailReasonWaitingForNode when the pod stayed
	// Unschedulable past the schedule timeout; node is where the pod landed.
	failReason string
	node       string
}

// parseJobStatus reads the Complete/Failed conditions off a Job object.
func parseJobStatus(raw []byte) (jobOutcome, bool) {
	var job struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return jobOutcome{}, false
	}
	for _, c := range job.Status.Conditions {
		if c.Status != condStatusTrue {
			continue
		}
		switch c.Type {
		case condComplete:
			return jobOutcome{succeeded: true}, true
		case condFailed:
			reason := strings.TrimSpace(c.Reason + ": " + c.Message)
			return jobOutcome{reason: strings.TrimPrefix(reason, ": ")}, true
		}
	}
	return jobOutcome{}, false
}

// findPod returns the name of the pod the Job created, or "" if none yet.
func (l *Launcher) findPod(ctx context.Context, ns, job string) (string, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, fmt.Sprintf(labelSelectorFmt, jobLabelKey, job))
	raw, err := l.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "", nil
	}
	// backoffLimit is 0, so there is at most one pod; take the last in case
	// the API ever returns a retried one alongside it.
	return list.Items[len(list.Items)-1].Metadata.Name, nil
}

// podLog reads the kick container's log, capped at maxLogBytes.
func (l *Launcher) podLog(ctx context.Context, ns, pod string) string {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?container=%s&limitBytes=%d", ns, pod, containerName, maxLogBytes)
	raw, err := l.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ""
	}
	return string(raw)
}

// podExitCode reads the kick container's terminated exit code, or
// exitCodeUnknown when the pod is not terminated or cannot be read.
func (l *Launcher) podExitCode(ctx context.Context, ns, pod string) int {
	return l.podDetails(ctx, ns, pod).exitCode
}

func (l *Launcher) delete(ctx context.Context, jobPath string) error {
	body, _ := json.Marshal(map[string]string{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": deletePropagate})
	_, err := l.do(ctx, http.MethodDelete, jobPath, body)
	return err
}

// manifest renders the batch/v1 Job. Kept as plain maps rather than client-go
// types: the repo has no k8s.io dependency and this is the only writer.
func (l *Launcher) manifest(ns, name, subPath string, spec sandbox.LaunchSpec) map[string]any {
	return l.jobManifest(ns, name, subPath, spec, nil)
}

// jobManifest is manifest plus, when relays is non-nil, the relay init
// container, its emptyDir and the agent container's read-only mount of it.
func (l *Launcher) jobManifest(ns, name, subPath string, spec sandbox.LaunchSpec, relays *relayPlan) map[string]any {
	mount := spec.WorkspaceMount
	if mount == "" {
		mount = sandbox.DefaultWorkspaceMount
	}
	workdir := spec.WorkDir
	if workdir == "" {
		workdir = mount
	}
	ttl := l.Options.TTLSeconds
	if ttl == 0 {
		ttl = DefaultTTLSeconds
	}
	deadline := l.Options.ActiveDeadlineSeconds
	if deadline <= 0 {
		deadline = ActiveDeadlineSeconds(DefaultKickTimeout)
	}

	// Same filter as the Podman path; explicit spec.Env entries come last and win.
	merged := map[string]string{}
	for _, entry := range sandbox.SanitizedEnv(os.Environ(), spec.EnvAllowlist, spec.Env) {
		if k, v, ok := strings.Cut(entry, "="); ok {
			merged[k] = v
		}
	}
	if posture.RequireHardened(os.Getenv) {
		// As in PodmanArgs: the relay preflight in the pod then fails the kick
		// on any containment gap instead of only warning (#19479).
		merged[posture.RequireHardenedEnv] = "1"
	}
	env := make([]map[string]string, 0, len(merged))
	for _, k := range sortedKeys(merged) {
		env = append(env, map[string]string{"name": k, "value": merged[k]})
	}
	envFrom := make([]map[string]any, 0, len(l.Options.EnvFromSecrets))
	for _, s := range l.Options.EnvFromSecrets {
		envFrom = append(envFrom, map[string]any{"secretRef": map[string]string{"name": s}})
	}
	volumes := []map[string]any{{
		"name":                  "workspace",
		"persistentVolumeClaim": map[string]string{"claimName": l.Options.WorkspaceClaim},
	}}
	mounts := []map[string]any{{
		"name":      "workspace",
		"mountPath": mount,
		"subPath":   subPath,
	}}
	for i, v := range l.Options.ExtraVolumes {
		vname := v.Name
		if vname == "" {
			vname = fmt.Sprintf("extra-%d", i)
		}
		volumes = append(volumes, map[string]any{
			"name":                  vname,
			"persistentVolumeClaim": map[string]any{"claimName": v.Claim, "readOnly": v.ReadOnly},
		})
		mounts = append(mounts, map[string]any{"name": vname, "mountPath": v.MountPath, "readOnly": v.ReadOnly})
	}

	container := map[string]any{
		// The workspace PVC and the agent's home need to stay writable, so the
		// root filesystem is deliberately not made read-only.
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
			"seccompProfile":           map[string]string{"type": "RuntimeDefault"},
		},
		"name":         containerName,
		"image":        spec.Image,
		"workingDir":   workdir,
		"env":          env,
		"envFrom":      envFrom,
		"volumeMounts": mounts,
	}
	// relay check, then preflight, then setup, then the agent (#19574).
	command := preflightCommand(setupCommand(spec.Command, l.Options), l.Options)
	if relays != nil {
		command = sandbox.RelayCommand(command)
		mounts = append(mounts, map[string]any{"name": relayVolumeName, "mountPath": sandbox.RelayMountPath, "readOnly": true})
		container["volumeMounts"] = mounts
		volumes = append(volumes, map[string]any{
			"name":     relayVolumeName,
			"emptyDir": map[string]string{"sizeLimit": relayVolumeSizeLimit},
		})
	}
	if len(command) > 0 {
		container["command"] = command
	}
	if len(l.Options.Resources.Limits) > 0 || len(l.Options.Resources.Requests) > 0 {
		container["resources"] = l.Options.Resources
	}
	podSpec := map[string]any{
		"restartPolicy": restartNever,
		"containers":    []map[string]any{container},
		"volumes":       volumes,
		// Nothing in the kick pod calls the Kubernetes API, so the SA token is
		// never mounted into a contained run.
		"automountServiceAccountToken": false,
		"securityContext": map[string]any{
			"seccompProfile": map[string]string{"type": "RuntimeDefault"},
		},
	}
	if relays != nil {
		podSpec["initContainers"] = []map[string]any{relays.initContainer()}
		if len(relays.pullSecrets) > 0 {
			podSpec["imagePullSecrets"] = relays.pullSecrets
		}
	}
	if len(l.Options.NodeSelector) > 0 {
		podSpec["nodeSelector"] = l.Options.NodeSelector
	}
	if l.Options.Affinity != nil {
		podSpec["affinity"] = l.Options.Affinity
	}
	if len(l.Options.Tolerations) > 0 {
		podSpec["tolerations"] = l.Options.Tolerations
	}
	if l.Options.ServiceAccount != "" {
		podSpec["serviceAccountName"] = l.Options.ServiceAccount
	}
	if l.Options.SchedulerName != "" {
		podSpec["schedulerName"] = l.Options.SchedulerName
	}
	if rc := strings.TrimSpace(l.Options.RuntimeClassName); rc != "" {
		podSpec["runtimeClassName"] = rc
	}
	if l.Options.network() == NetworkHost {
		podSpec["hostNetwork"] = true
		podSpec["dnsPolicy"] = "ClusterFirstWithHostNet"
	}
	labels := map[string]string{managedByLabel: managedByValue, jobLabel: name}
	for key, value := range l.Options.PodLabels {
		labels[key] = value
	}
	// Fixed labels are authoritative even if a caller bypasses validation.
	labels[managedByLabel], labels[jobLabel] = managedByValue, name
	if spec.Name != "" {
		labels[agentLabel] = sanitizeLabel(spec.Name)
	}
	podMetadata := map[string]any{"labels": labels}
	if len(l.Options.PodAnnotations) > 0 {
		podMetadata["annotations"] = l.Options.PodAnnotations
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]any{
			// One attempt. A retried kick would re-run the agent against a
			// workspace the first attempt already changed.
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": ttl,
			// The cluster fails a Job still running (or never scheduled)
			// past this, so one hive lost track of cannot sit Pending
			// forever (hivecommons/hive#18525).
			"activeDeadlineSeconds": deadline,
			"template": map[string]any{
				"metadata": podMetadata,
				"spec":     podSpec,
			},
		},
	}
}

func (l *Launcher) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Launcher) namespace() (string, error) {
	if l.Namespace != "" {
		return l.Namespace, nil
	}
	path := l.NamespacePath
	if path == "" {
		path = DefaultNamespacePath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("kubejob: reading namespace: %w", err)
	}
	ns := strings.TrimSpace(string(raw))
	if ns == "" {
		return "", fmt.Errorf("kubejob: empty namespace in %s", path)
	}
	return ns, nil
}
