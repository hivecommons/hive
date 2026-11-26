package kubejob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/egressmark"
	"github.com/hivecommons/hive/crux/posture"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// The copy workspace transport (hivecommons/hive#19621).
//
// With workspace_transport: copy the Job never mounts hive's data claim. Its
// workspace is a pod-local emptyDir, and its container command is
// `crux job-bridge -- <kick command>`, run from a copy of hive's own crux
// binary that an init container from the hive image (the image the relays
// come from) puts in a second emptyDir. Hive reaches the bridge at the pod IP
// and:
//
//  1. pushes the workspace it prepared (PUT /workspace); the bridge then runs
//     the kick command, whose output and exit code stay the container's;
//  2. once the command has exited, pulls the workspace back (GET /workspace,
//     answered 409 while the command runs) and swaps it in on its own claim;
//  3. tells the bridge it is done (POST /done), and the container exits.
//
// Every request but /healthz carries a per-run bearer token, held in a
// per-Job Secret and read by the pod through secretKeyRef (never a literal
// in the Job spec); the bridge removes it from the kick command's environment. A NetworkPolicy
// admits ingress to the bridge port from hive's own pod labels only: on
// network: none it is the deny-all policy plus that rule, on other networks
// an ingress-only policy. No GitHub credential enters
// the Job: hive pushes and opens the PR from the pulled workspace as before.
//
// The relay spool lives in the workspace and needs a directory hive and the
// Job share while the kick runs, so the relays are off on this transport.

const (
	// DefaultBridgeMount is where the crux binary is mounted in the Job.
	DefaultBridgeMount = "/opt/hive-bridge"

	bridgeVolumeName        = "hive-bridge"
	bridgeInitContainerName = "hive-bridge"
	bridgePortName          = "hive-bridge"
	// bridgeBinary is where the hive image installs crux (Dockerfile).
	bridgeBinary   = "/usr/local/bin/crux"
	workspaceName  = "workspace"
	bridgeTokenLen = 32
	bridgeTokenKey = "token"

	// DefaultWorkspaceSizeLimit bounds the workspace emptyDir when
	// Options.WorkspaceSizeLimit is empty. The Kubernetes scheduler counts
	// emptyDir data as ephemeral storage, so an unbounded one lets an agent
	// push a node into DiskPressure.
	DefaultWorkspaceSizeLimit = "10Gi"
	// bridgeVolumeSizeLimit bounds the emptyDir that holds the crux binary.
	bridgeVolumeSizeLimit = "1Gi"

	// bridgeRequestTimeout bounds one push or pull request end to end. It
	// matches the bridge's own wait for hive, so a half-open connection
	// cannot outlive the bridge.
	bridgeRequestTimeout = 10 * time.Minute
	// bridgeLogEvery is how often a repeating transport failure is logged,
	// after the first one.
	bridgeLogEvery = 15
	// bridgeMaxTimeouts is how many consecutive connect timeouts fail the
	// push. A pod that is still starting refuses connections; one behind a
	// default-deny NetworkPolicy drops them, which looks like a timeout.
	bridgeMaxTimeouts = 6
	// bridgeMaxReplaceTries is how many times a pull whose unpack failed is
	// asked for again. GET /workspace is idempotent and ReplaceWorkspace
	// removes its staging directory on failure.
	bridgeMaxReplaceTries = 3
	// bridgeRescueTimeout bounds the stop-and-pull that runs after a kick
	// timeout or cancel, on a fresh context.
	bridgeRescueTimeout = 2 * time.Minute
)

// copiesWorkspace reports whether the Job uses the copy transport.
func (o Options) copiesWorkspace() bool {
	return strings.TrimSpace(o.WorkspaceTransport) == config.SandboxJobWorkspaceCopy
}

// validateTransport checks WorkspaceTransport and the claim it may need.
func (o Options) validateTransport() error {
	switch strings.TrimSpace(o.WorkspaceTransport) {
	case "", config.SandboxJobWorkspaceClaim:
		if strings.TrimSpace(o.WorkspaceClaim) == "" {
			return errors.New("kubejob: workspace_claim is required (the PVC hive's sandbox workspace root lives on), or set workspace_transport: copy")
		}
	case config.SandboxJobWorkspaceCopy:
		if strings.TrimSpace(o.RelayImage) == RelayImageNone {
			return fmt.Errorf("kubejob: workspace_transport: copy runs hive's crux binary from relay_image, so relay_image cannot be %q", RelayImageNone)
		}
		if o.network() == NetworkHost {
			// The bridge would listen on the node's address, and most CNIs
			// do not apply NetworkPolicy to host-network pods.
			return fmt.Errorf("kubejob: workspace_transport: copy cannot run with network: %s; the job bridge would listen on the node's address outside NetworkPolicy", NetworkHost)
		}
	default:
		return fmt.Errorf("kubejob: workspace_transport must be %q or %q, got %q", config.SandboxJobWorkspaceClaim, config.SandboxJobWorkspaceCopy, o.WorkspaceTransport)
	}
	return nil
}

// refuseCopyWhenHardened fails a copy-transport kick closed when
// posture.RequireHardenedEnv is set. The copy transport runs the kick without
// the relays, and so without the relay containment preflight that enforces
// that variable inside the pod (relays.go): a hive that asks for hardening
// must not get a kick whose containment nothing checked.
func refuseCopyWhenHardened(getenv func(string) string) error {
	if !posture.RequireHardened(getenv) {
		return nil
	}
	err := fmt.Errorf("kubejob: workspace_transport: copy is refused because %s is set: the copy transport runs the kick without the relay containment preflight that enforces it; use the claim transport (workspace_transport: claim) or unset %s", posture.RequireHardenedEnv, posture.RequireHardenedEnv)
	slog.Error("kubejob: refusing the copy workspace transport on a hardened hive", "env", posture.RequireHardenedEnv, "err", err)
	return err
}

// refuseCopyOutsideRuntimeClass fails a copy-transport kick closed when
// hive's own pod runs under a runtime class (a Kata VM, for example) and the
// Job names none: the Job would run the agent under the cluster's default
// runtime, outside the VM, with nothing saying so.
func refuseCopyOutsideRuntimeClass(hiveClass, jobClass string) error {
	hiveClass, jobClass = strings.TrimSpace(hiveClass), strings.TrimSpace(jobClass)
	if hiveClass == "" || jobClass != "" {
		return nil
	}
	err := fmt.Errorf("kubejob: workspace_transport: copy is refused because hive's own pod runs under runtimeClassName %q and job.runtime_class_name is empty; the Job would run the agent outside that runtime. Set job.runtime_class_name (or %s), for example to %q", hiveClass, config.SandboxJobRuntimeClassEnv, hiveClass)
	slog.Error("kubejob: refusing the copy workspace transport outside hive's runtime class", "hive_runtime_class", hiveClass, "err", err)
	return err
}

// copyPlan is what one copy-transport run adds to its Jobs.
type copyPlan struct {
	image              *relayPlan
	token              string
	hiveLabels         map[string]string
	workspaceSizeLimit string
}

// resolveCopy builds the copy plan: the image that supplies crux, a fresh
// token and the labels that select hive's own pod.
func (l *Launcher) resolveCopy(ctx context.Context, ns string, spec sandbox.LaunchSpec) (*copyPlan, error) {
	if len(spec.Command) == 0 {
		return nil, errors.New("kubejob: workspace_transport: copy needs an explicit command to run behind the job bridge; this launch has none")
	}
	image, err := l.resolveRelays(ctx, ns)
	if err != nil {
		return nil, err
	}
	if image == nil {
		return nil, fmt.Errorf("kubejob: workspace_transport: copy needs relay_image to supply crux; it cannot be %q", RelayImageNone)
	}
	plan := &copyPlan{image: image, token: l.bridgeToken, workspaceSizeLimit: strings.TrimSpace(l.Options.WorkspaceSizeLimit)}
	if plan.workspaceSizeLimit == "" {
		plan.workspaceSizeLimit = DefaultWorkspaceSizeLimit
	}
	if plan.token == "" {
		raw := make([]byte, bridgeTokenLen)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("kubejob: generating the job bridge token: %w", err)
		}
		plan.token = hex.EncodeToString(raw)
	}
	// Hive's own pod labels select the only ingress the bridge port admits,
	// whatever the network mode: the token alone must not be the only gate.
	pod, err := l.selfPod(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("kubejob: workspace_transport: copy reads hive's own pod labels to admit it to the job bridge: %w", err)
	}
	if len(pod.Metadata.Labels) == 0 {
		return nil, errors.New("kubejob: workspace_transport: copy needs hive's own pod to carry labels the NetworkPolicy can admit; it has none")
	}
	if err := refuseCopyOutsideRuntimeClass(pod.Spec.RuntimeClassName, l.Options.RuntimeClassName); err != nil {
		return nil, err
	}
	plan.hiveLabels = pod.Metadata.Labels
	return plan, nil
}

// apply turns a claim-transport manifest into a copy-transport one.
func (p *copyPlan) apply(manifest map[string]any, secretName string) error {
	jobSpec, _ := manifest["spec"].(map[string]any)
	if jobSpec == nil {
		return errors.New("kubejob: copy transport: manifest has no spec")
	}
	// The Job starts suspended: its Secret and NetworkPolicy carry an
	// ownerReference to it, which needs its UID, so it is created first and
	// resumed once they exist. No pod runs before the policy does.
	jobSpec["suspend"] = true
	tmpl, _ := jobSpec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	containers, _ := pod["containers"].([]map[string]any)
	if len(containers) == 0 {
		return errors.New("kubejob: copy transport: manifest has no container")
	}
	c := containers[0]
	command, _ := c["command"].([]string)
	if len(command) == 0 {
		return errors.New("kubejob: copy transport: the kick has no command")
	}
	// These assertions mirror the concrete types jobManifest builds
	// ([]map[string]any volumes and mounts, []map[string]string env). If
	// jobManifest changes them they stop matching, so the swap below must
	// report that instead of leaving the claim mounted.
	volumes, _ := pod["volumes"].([]map[string]any)
	swapped := false
	for i, v := range volumes {
		if v["name"] == workspaceName {
			volumes[i] = map[string]any{"name": workspaceName, "emptyDir": map[string]any{"sizeLimit": p.workspaceSizeLimit}}
			swapped = true
		}
	}
	if !swapped {
		return errors.New("kubejob: copy transport: manifest has no workspace volume to replace")
	}
	pod["volumes"] = append(volumes, map[string]any{"name": bridgeVolumeName, "emptyDir": map[string]any{"sizeLimit": bridgeVolumeSizeLimit}})
	mounts, _ := c["volumeMounts"].([]map[string]any)
	workspace := sandbox.DefaultWorkspaceMount
	for _, m := range mounts {
		if m["name"] == workspaceName {
			delete(m, "subPath")
			if mp, ok := m["mountPath"].(string); ok {
				workspace = mp
			}
		}
	}
	c["volumeMounts"] = append(mounts, map[string]any{"name": bridgeVolumeName, "mountPath": DefaultBridgeMount, "readOnly": true})
	env, _ := c["env"].([]map[string]string)
	envAny := make([]any, 0, len(env)+1)
	for _, e := range env {
		envAny = append(envAny, e)
	}
	c["env"] = append(envAny, map[string]any{"name": substratebridge.JobTokenEnv, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": bridgeTokenKey}}})
	c["ports"] = []map[string]any{{"name": bridgePortName, "containerPort": substratebridge.DefaultPort, "protocol": "TCP"}}
	bridge := []string{
		path.Join(DefaultBridgeMount, path.Base(bridgeBinary)), "job-bridge",
		"-listen", ":" + strconv.Itoa(substratebridge.DefaultPort),
		"-workspace", workspace,
		"-token-env", substratebridge.JobTokenEnv,
		"--",
	}
	c["command"] = append(bridge, command...)
	inits, _ := pod["initContainers"].([]map[string]any)
	// imagePullSecrets for the hive image come from resolvePullSecrets.
	pod["initContainers"] = append(inits, p.initContainer())
	return nil
}

// initContainer copies crux into the bridge emptyDir, built like the relay
// init container so it passes the same admission policies.
func (p *copyPlan) initContainer() map[string]any {
	c := p.image.initContainer()
	c["name"] = bridgeInitContainerName
	c["command"] = []string{"cp", bridgeBinary, DefaultBridgeMount + "/"}
	c["volumeMounts"] = []map[string]any{{"name": bridgeVolumeName, "mountPath": DefaultBridgeMount}}
	return c
}

// allowBridgeIngress admits hive's own pod to the bridge port through the
// deny-all policy; egress stays denied.
func (p *copyPlan) allowBridgeIngress(policy map[string]any) {
	spec, _ := policy["spec"].(map[string]any)
	if spec == nil || len(p.hiveLabels) == 0 {
		return
	}
	spec["ingress"] = []map[string]any{{
		"from":  []map[string]any{{"podSelector": map[string]any{"matchLabels": p.hiveLabels}}},
		"ports": []map[string]any{{"protocol": "TCP", "port": substratebridge.DefaultPort}},
	}}
}

// ownedBy adds an ownerReference to the Job to obj's metadata, so the
// garbage collector removes obj with the Job even when hive is not running
// to do it (a restart, a crash, a Job removed by its TTL). It is also the
// ONLY way these objects are removed: hive deletes the Job with Background
// propagation and holds no delete verb on Secrets or NetworkPolicies
// (sandbox-job-copy-rbac.yaml). blockOwnerDeletion is left out on purpose:
// Background deletion does not need it, and where the
// OwnerReferencesPermissionEnforcement admission plugin runs, setting it
// needs update on jobs/finalizers, which no hive Role grants.
func ownedBy(obj map[string]any, job, uid string) {
	md, _ := obj["metadata"].(map[string]any)
	if md == nil {
		return
	}
	md["ownerReferences"] = []map[string]any{{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"name":       job,
		"uid":        uid,
	}}
}

// jobUID reads metadata.uid from the API server's answer to the Job create.
func jobUID(raw []byte) string {
	var got struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &got) != nil {
		return ""
	}
	return strings.TrimSpace(got.Metadata.UID)
}

// tokenSecret is the per-Job Secret that holds the bridge token.
func (p *copyPlan) tokenSecret(ns, name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    map[string]string{managedByLabel: managedByValue, jobLabel: name},
		},
		"stringData": map[string]string{bridgeTokenKey: p.token},
	}
}

// ingressOnlyPolicy limits ingress to the Job's pods to the bridge port from
// hive's own pod, leaving egress as the chosen network mode has it.
func (p *copyPlan) ingressOnlyPolicy(ns, job string) map[string]any {
	policy := denyAllPolicy(ns, job)
	spec, _ := policy["spec"].(map[string]any)
	spec["policyTypes"] = []string{"Ingress"}
	p.allowBridgeIngress(policy)
	return policy
}

// copyRun is one Job's workspace copy, running beside waitJob.
type copyRun struct {
	cancel context.CancelFunc
	result chan error
	// base is the bridge's URL once known, and pushed whether the workspace
	// reached the Job; both are read after finish, to rescue a timed-out kick.
	base   atomic.Value
	pushed atomic.Bool
}

// pullError marks a failure after the workspace was in the Job: the agent's
// work exists only in the Job's emptyDir, so the Job is kept, not deleted.
type pullError struct{ err error }

func (e *pullError) Error() string { return e.err.Error() }
func (e *pullError) Unwrap() error { return e.err }

// transientError is a step failure bridgePoll logs and retries.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// startCopy pushes the workspace into the Job and pulls it back once the
// kick command exits. A copy that fails calls abort, which ends waitJob.
func (l *Launcher) startCopy(ctx context.Context, abort context.CancelFunc, ns, job, workspace string) *copyRun {
	cctx, cancel := context.WithCancel(ctx)
	run := &copyRun{cancel: cancel, result: make(chan error, 1)}
	go func() {
		err := l.copyWorkspace(cctx, run, ns, job, workspace)
		if err != nil && cctx.Err() == nil {
			abort()
		}
		run.result <- err
	}()
	return run
}

// finish stops the copy and reports how it ended; nil for no copy.
func (r *copyRun) finish() error {
	if r == nil {
		return nil
	}
	r.cancel()
	return <-r.result
}

func (l *Launcher) copyToken() string {
	if l.copyPlan != nil {
		return l.copyPlan.token
	}
	return ""
}

// packRequestBody streams the packed workspace through a pipe, so hive never
// holds the archive in memory. packErr reports a Pack failure after the
// request returns, which must not be mistaken for a transport error.
func packRequestBody(workspace string) (body io.ReadCloser, packErr func() error) {
	pr, pw := io.Pipe()
	var perr atomic.Value
	go func() {
		skipped, err := substratebridge.PackSkipped(workspace, pw)
		if len(skipped) > 0 {
			slog.Warn("kubejob: left entries out of the workspace push (sockets, FIFOs, devices, oversized or vanished files)", "workspace", workspace, "skipped", skipped)
		}
		// A closed pipe means the request ended first (the bridge answered
		// without reading the whole body); the request's own result decides.
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			perr.Store(err)
		}
		_ = pw.CloseWithError(err)
	}()
	return pr, func() error {
		if v, ok := perr.Load().(error); ok {
			return v
		}
		return nil
	}
}

func (l *Launcher) copyWorkspace(ctx context.Context, run *copyRun, ns, job, workspace string) error {
	token := l.copyToken()
	base, err := l.bridgeURL(ctx, ns, job)
	if err != nil {
		return err
	}
	run.base.Store(base)
	// Push. A 409 means an earlier attempt landed and its answer was lost.
	if err := l.bridgePoll(ctx, job, "push", func() (bool, error) {
		body, packErr := packRequestBody(workspace)
		code, err := l.bridgeDo(ctx, http.MethodPut, base+substratebridge.PathWorkspace, token, body, nil)
		_ = body.Close()
		if perr := packErr(); perr != nil {
			return false, fmt.Errorf("kubejob: packing workspace %s: %w", workspace, perr)
		}
		if err != nil {
			return false, &transientError{err}
		}
		if code == http.StatusNoContent || code == http.StatusConflict {
			return true, nil
		}
		return false, fmt.Errorf("kubejob: pushing the workspace into %s: HTTP %d", job, code)
	}); err != nil {
		return err
	}
	run.pushed.Store(true)
	// From here the agent's work lives only in the Job: a failure is a
	// pullError, and the Job is kept.
	if err := l.pullWorkspace(ctx, base, token, job, workspace); err != nil {
		return &pullError{err}
	}
	// The workspace is home, so this cannot fail the kick: a lost /done
	// only makes the bridge wait out its own timeout before it exits, and
	// finish ends the retries once the Job has finished.
	_ = l.bridgePoll(ctx, job, "done", func() (bool, error) {
		code, err := l.bridgeDo(ctx, http.MethodPost, base+substratebridge.PathDone, token, nil, nil)
		return err == nil && code == http.StatusNoContent, nil
	})
	return nil
}

// pullWorkspace pulls the workspace back once the kick command has exited,
// retrying a transport failure until ctx ends and an unpack failure a few
// times.
func (l *Launcher) pullWorkspace(ctx context.Context, base, token, job, workspace string) error {
	replaceTries := 0
	return l.bridgePoll(ctx, job, "pull", func() (bool, error) {
		var replaceErr error
		code, err := l.bridgeDo(ctx, http.MethodGet, base+substratebridge.PathWorkspace, token, nil, func(r io.Reader) {
			replaceErr = sandbox.ReplaceWorkspace(r, workspace)
		})
		switch {
		case code == http.StatusConflict:
			return false, nil // the command is still running
		case err != nil:
			return false, &transientError{err}
		case code != http.StatusOK:
			return false, fmt.Errorf("kubejob: pulling the workspace from %s: HTTP %d", job, code)
		case replaceErr != nil:
			replaceTries++
			if replaceTries >= bridgeMaxReplaceTries {
				return false, fmt.Errorf("kubejob: replacing workspace %s from %s after %d tries: %w", workspace, job, replaceTries, replaceErr)
			}
			return false, &transientError{fmt.Errorf("replacing workspace %s from %s: %w", workspace, job, replaceErr)}
		}
		return true, nil
	})
}

// rescueWorkspace runs after a kick timeout or cancel, when ctx is dead: it
// stops the kick command, then pulls the workspace on a fresh bounded
// context, as the pod log is collected, so the agent's partial work is kept
// as it is on the claim transport. A copy that never pushed has nothing to
// pull.
func (l *Launcher) rescueWorkspace(run *copyRun, job, workspace string) error {
	if run == nil || !run.pushed.Load() {
		return nil
	}
	base, _ := run.base.Load().(string)
	if base == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bridgeRescueTimeout)
	defer cancel()
	token := l.copyToken()
	// Best effort: a command that already exited answers 204 as well.
	_, _ = l.bridgeDo(ctx, http.MethodPost, base+substratebridge.PathStop, token, nil, nil)
	if err := l.pullWorkspace(ctx, base, token, job, workspace); err != nil {
		return &pullError{err}
	}
	_, _ = l.bridgeDo(ctx, http.MethodPost, base+substratebridge.PathDone, token, nil, nil)
	return nil
}

// isTimeout reports whether err is a connect or request timeout, the way a
// NetworkPolicy drop looks from the client.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// bridgePoll calls step every poll interval until it reports done, fails, or
// ctx ends. A *transientError from step is logged (the first, then every
// bridgeLogEvery-th), kept for the final error, and retried; for the push,
// bridgeMaxTimeouts consecutive timeouts fail early with the NetworkPolicy
// requirement spelled out.
func (l *Launcher) bridgePoll(ctx context.Context, job, phase string, step func() (bool, error)) error {
	interval := l.Options.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	var last error
	failures, timeouts := 0, 0
	for {
		done, err := step()
		var te *transientError
		if errors.As(err, &te) {
			last = te.err
			failures++
			if isTimeout(te.err) {
				timeouts++
			} else {
				timeouts = 0
			}
			if failures == 1 || failures%bridgeLogEvery == 0 {
				slog.Warn("kubejob: the job bridge is not answering; retrying", "job", job, "phase", phase, "failures", failures, "port", substratebridge.DefaultPort, "err", te.err)
			}
			if phase == "push" && timeouts >= bridgeMaxTimeouts {
				return fmt.Errorf("kubejob: the job bridge of %s did not answer %d connection attempts in a row on TCP %d (last error: %v); with default-deny NetworkPolicies, allow egress from hive's pod to the Job pods (label app.kubernetes.io/managed-by=%s) on that port", job, timeouts, substratebridge.DefaultPort, last, managedByValue)
			}
			err = nil
		}
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("kubejob: copying the workspace (%s): %w (last bridge error: %v)", phase, ctx.Err(), last)
			}
			return fmt.Errorf("kubejob: copying the workspace (%s): %w", phase, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// bridgeURL waits for the Job's pod to have an IP and returns the bridge's
// base URL there; Launcher.BridgeURL overrides it. A pod that never gets an
// IP is ended by waitJob's schedule timeout, which cancels ctx.
func (l *Launcher) bridgeURL(ctx context.Context, ns, job string) (string, error) {
	if l.BridgeURL != "" {
		return strings.TrimRight(l.BridgeURL, "/"), nil
	}
	var ip string
	err := l.bridgePoll(ctx, job, "pod-ip", func() (bool, error) {
		got, err := l.podIP(ctx, ns, job)
		ip = got
		return err == nil && got != "", nil
	})
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(substratebridge.DefaultPort)), nil
}

// podIP reads the IP of the Job's pod, or "" before it has one.
func (l *Launcher) podIP(ctx context.Context, ns, job string) (string, error) {
	p := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, fmt.Sprintf(labelSelectorFmt, jobLabelKey, job))
	raw, err := l.do(ctx, http.MethodGet, p, nil)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Status struct {
				PodIP string `json:"podIP"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "", nil
	}
	return strings.TrimSpace(list.Items[len(list.Items)-1].Status.PodIP), nil
}

// bridgeDo makes one bridge request and returns its status. A 200 body is
// streamed to onOK when it is set; any other body is discarded.
func (l *Launcher) bridgeDo(ctx context.Context, method, url, token string, body io.Reader, onOK func(io.Reader)) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, bridgeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := l.BridgeClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{DialContext: egressmark.Dialer(requestTimeout).DialContext, DisableKeepAlives: true}}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK && onOK != nil {
		onOK(resp.Body)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}
