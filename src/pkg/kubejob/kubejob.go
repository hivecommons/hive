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
//
// The credential boundary is the whole point of keeping the executor
// unchanged: the Job gets only the Secrets the operator names in
// env_from_secrets. Hive's brokered GitHub token never enters the Job; hive
// pushes and opens the PR from the workspace after the Job has exited, through
// the same pushbroker path the Podman sandbox uses.
package kubejob

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/sandbox"
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
	jobLabelKey   = "job-name"
	// managedByLabel marks Jobs this launcher created so an operator (or a
	// future sweeper) can list them without guessing at name prefixes.
	managedByLabel   = "app.kubernetes.io/managed-by"
	managedByValue   = "hive-kubejob"
	agentLabel       = "hive.kubestellar.io/agent"
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
// LaunchSpec carries. All fields are optional except WorkspaceClaim.
type Options struct {
	// WorkspaceClaim names the PersistentVolumeClaim hive's sandbox workspace
	// root lives on. Required: without it there is no way for the Job to see
	// the workspace at all.
	WorkspaceClaim string
	// WorkspaceClaimMount is where that claim is mounted INSIDE THE HIVE POD
	// (default /data). The Job's subPath is the workspace path relative to it.
	WorkspaceClaimMount string
	// NodeSelector, Tolerations, Resources, ServiceAccount are passed through
	// to the pod template verbatim.
	NodeSelector   map[string]string
	Tolerations    []Toleration
	Resources      Resources
	ServiceAccount string
	// EnvFromSecrets are Secret names whose keys become environment variables
	// in the Job. This is the ONLY credential path into the Job.
	EnvFromSecrets []string
	// ExtraVolumes mounts additional claims (a compile cache, a model store).
	ExtraVolumes []VolumeMount
	// TTLSeconds is the Job's ttlSecondsAfterFinished (default one hour).
	TTLSeconds int
	// PollInterval overrides how often the Job status is read (tests).
	PollInterval time.Duration
}

// Toleration mirrors the subset of corev1.Toleration an operator writes in YAML.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// Resources mirrors corev1.ResourceRequirements with string quantities, so a
// device limit like `<vendor>.com/<device>: "2"` passes through untouched.
type Resources struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

// VolumeMount is an additional PVC to mount into the Job.
type VolumeMount struct {
	Name      string `json:"name"`
	Claim     string `json:"claim"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
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
}

var _ sandbox.Launcher = (*Launcher)(nil)

// Validate reports the configuration errors that would otherwise surface only
// as a failed kick.
func (o Options) Validate() error {
	if strings.TrimSpace(o.WorkspaceClaim) == "" {
		return errors.New("kubejob: workspace_claim is required (the PVC hive's sandbox workspace root lives on)")
	}
	if o.TTLSeconds < 0 {
		return errors.New("kubejob: ttl_seconds must not be negative")
	}
	for i, v := range o.ExtraVolumes {
		if strings.TrimSpace(v.Claim) == "" || strings.TrimSpace(v.MountPath) == "" {
			return fmt.Errorf("kubejob: volumes[%d] needs both claim and mount_path", i)
		}
	}
	return nil
}

// Run creates the Job, waits for it to finish or for ctx to end, collects the
// pod log and exit code, and deletes the Job on success. A Job that failed is
// left in place until its TTL so the operator can inspect it.
func (l *Launcher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	var res sandbox.Result
	if err := l.Options.Validate(); err != nil {
		return res, err
	}
	if strings.TrimSpace(spec.Image) == "" {
		return res, errors.New("kubejob: image is required")
	}
	if strings.TrimSpace(spec.Workspace) == "" {
		return res, errors.New("kubejob: workspace is required")
	}
	subPath, err := workspaceSubPath(spec.Workspace, l.Options.WorkspaceClaimMount)
	if err != nil {
		return res, err
	}
	ns, err := l.namespace()
	if err != nil {
		return res, err
	}
	name := jobName(spec.Name, l.now())
	body, err := json.Marshal(l.manifest(ns, name, subPath, spec))
	if err != nil {
		return res, err
	}
	jobsPath := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", ns)
	if _, err := l.do(ctx, http.MethodPost, jobsPath, body); err != nil {
		return res, fmt.Errorf("kubejob: creating job %s: %w", name, err)
	}
	jobPath := jobsPath + "/" + name

	done, waitErr := l.wait(ctx, jobPath)
	// Collect whatever the pod produced BEFORE deciding anything: a timed-out
	// or failed kick with no transcript is the worst outcome for an operator.
	// After a timeout the caller's ctx is already dead, so the collection and
	// the cleanup below run on a fresh, request-bounded context.
	collectCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		collectCtx, cancel = context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
	}
	pod, podErr := l.findPod(collectCtx, ns, name)
	if podErr == nil && pod != "" {
		res.Stdout = l.podLog(collectCtx, ns, pod)
		res.ExitCode = l.podExitCode(collectCtx, ns, pod)
	} else {
		res.ExitCode = exitCodeUnknown
	}
	if waitErr != nil {
		// Context ended (kick timeout or cancel). The Job would otherwise
		// keep running the workload on the accelerator node; stop it.
		_ = l.delete(collectCtx, jobPath)
		return res, waitErr
	}
	if !done.succeeded {
		res.Stderr = done.reason
		return res, fmt.Errorf("kubejob: job %s failed: %s", name, done.reason)
	}
	if res.ExitCode == exitCodeUnknown {
		// The Job reports Complete, so the container exited 0 even though the
		// pod could not be read back (already GC'd, or RBAC on pods missing).
		res.ExitCode = 0
	}
	_ = l.delete(ctx, jobPath)
	return res, nil
}

type jobOutcome struct {
	succeeded bool
	reason    string
}

// wait polls the Job until a Complete or Failed condition, or ctx ends.
func (l *Launcher) wait(ctx context.Context, jobPath string) (jobOutcome, error) {
	interval := l.Options.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		raw, err := l.do(ctx, http.MethodGet, jobPath, nil)
		if err != nil {
			if ctx.Err() != nil {
				return jobOutcome{}, fmt.Errorf("kubejob: %w", ctx.Err())
			}
			// Transient API error: keep polling; the context still bounds us.
		} else if out, finished := parseJobStatus(raw); finished {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return jobOutcome{}, fmt.Errorf("kubejob: %w", ctx.Err())
		case <-ticker.C:
		}
	}
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
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, pod)
	raw, err := l.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return exitCodeUnknown
	}
	var p struct {
		Status struct {
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State map[string]struct {
					ExitCode int `json:"exitCode"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return exitCodeUnknown
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if term, ok := cs.State[stateTerminated]; ok {
			return term.ExitCode
		}
	}
	return exitCodeUnknown
}

func (l *Launcher) delete(ctx context.Context, jobPath string) error {
	body, _ := json.Marshal(map[string]string{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": deletePropagate})
	_, err := l.do(ctx, http.MethodDelete, jobPath, body)
	return err
}

// manifest renders the batch/v1 Job. Kept as plain maps rather than client-go
// types: the repo has no k8s.io dependency and this is the only writer.
func (l *Launcher) manifest(ns, name, subPath string, spec sandbox.LaunchSpec) map[string]any {
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

	env := make([]map[string]string, 0, len(spec.Env))
	for _, k := range sortedKeys(spec.Env) {
		if sandbox.IsCredentialName(k) {
			continue
		}
		env = append(env, map[string]string{"name": k, "value": spec.Env[k]})
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
		"name":         containerName,
		"image":        spec.Image,
		"workingDir":   workdir,
		"env":          env,
		"envFrom":      envFrom,
		"volumeMounts": mounts,
	}
	if len(spec.Command) > 0 {
		container["command"] = spec.Command
	}
	if len(l.Options.Resources.Limits) > 0 || len(l.Options.Resources.Requests) > 0 {
		container["resources"] = l.Options.Resources
	}
	podSpec := map[string]any{
		"restartPolicy": restartNever,
		"containers":    []map[string]any{container},
		"volumes":       volumes,
	}
	if len(l.Options.NodeSelector) > 0 {
		podSpec["nodeSelector"] = l.Options.NodeSelector
	}
	if len(l.Options.Tolerations) > 0 {
		podSpec["tolerations"] = l.Options.Tolerations
	}
	if l.Options.ServiceAccount != "" {
		podSpec["serviceAccountName"] = l.Options.ServiceAccount
	}
	labels := map[string]string{managedByLabel: managedByValue}
	if spec.Name != "" {
		labels[agentLabel] = sanitizeLabel(spec.Name)
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
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

// workspaceSubPath turns hive's absolute workspace path into the path relative
// to the claim mount, which is what the Job's volumeMount.subPath needs.
func workspaceSubPath(workspace, claimMount string) (string, error) {
	if claimMount == "" {
		claimMount = DefaultWorkspaceClaimMount
	}
	ws := filepath.Clean(workspace)
	cm := filepath.Clean(claimMount)
	rel, err := filepath.Rel(cm, ws)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("kubejob: workspace %q is not under the workspace claim mount %q — set agent_sandbox.workspace_dir under it, or job.workspace_claim_mount to where the claim is mounted", workspace, claimMount)
	}
	return filepath.ToSlash(rel), nil
}

var labelUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// jobName builds a unique, DNS-label-safe Job name from the launch name and a
// timestamp: hive-kick-<agent>-<unix>. Names must survive as pod-name prefixes.
func jobName(launchName string, now time.Time) string {
	base := sanitizeLabel(strings.TrimSuffix(launchName, "-sandbox"))
	if base == "" {
		base = "agent"
	}
	suffix := fmt.Sprintf("-%d", now.Unix())
	room := jobNameMax - len(namePrefix) - len(suffix)
	if len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return namePrefix + base + suffix
}

func sanitizeLabel(s string) string {
	s = labelUnsafe.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort: env maps are tiny and this avoids importing sort for
	// one call site.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
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

// do performs one authenticated API call and returns the response body.
// Non-2xx responses are errors carrying the status and the API's message.
func (l *Launcher) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	client, err := l.client()
	if err != nil {
		return nil, err
	}
	server := l.APIServer
	if server == "" {
		server = DefaultAPIServer
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, server+path, rdr)
	if err != nil {
		return nil, err
	}
	if tok := l.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxLogBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, apiMessage(out))
	}
	return out, nil
}

// apiMessage pulls the human message out of a Kubernetes Status body, falling
// back to a trimmed raw body.
func apiMessage(raw []byte) string {
	var st struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &st) == nil && st.Message != "" {
		return st.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (l *Launcher) token() string {
	path := l.TokenPath
	if path == "" {
		path = DefaultTokenPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// client returns the injected HTTP client, or one trusting the in-cluster CA.
func (l *Launcher) client() (*http.Client, error) {
	if l.HTTPClient != nil {
		return l.HTTPClient, nil
	}
	path := l.CAPath
	if path == "" {
		path = DefaultCAPath
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("kubejob: reading in-cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("kubejob: in-cluster CA bundle contained no certificates")
	}
	l.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return l.HTTPClient, nil
}
