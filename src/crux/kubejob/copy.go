package kubejob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/egressmark"
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
// Every request but /healthz carries a per-run bearer token from the pod's
// environment; the bridge removes it from the kick command's. On network:
// none the deny-all NetworkPolicy also admits ingress from hive's own pod
// labels to the bridge port, and nothing else. No GitHub credential enters
// the Job: hive pushes and opens the PR from the pulled workspace as before.
//
// The relay spool lives in the workspace and needs a directory hive and the
// Job share while the kick runs, so the relays are off on this transport.

const (
	// DefaultBridgeMount is where the crux binary is mounted in the Job.
	DefaultBridgeMount = "/opt/hive-bridge"
	// BridgeTokenEnv carries the bridge's bearer token in the Job pod.
	BridgeTokenEnv = "HIVE_JOB_BRIDGE_TOKEN"

	bridgeVolumeName        = "hive-bridge"
	bridgeInitContainerName = "hive-bridge"
	bridgePortName          = "hive-bridge"
	// bridgeBinary is where the hive image installs crux (Dockerfile).
	bridgeBinary   = "/usr/local/bin/crux"
	workspaceName  = "workspace"
	bridgeTokenLen = 32
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
	default:
		return fmt.Errorf("kubejob: workspace_transport must be %q or %q, got %q", config.SandboxJobWorkspaceClaim, config.SandboxJobWorkspaceCopy, o.WorkspaceTransport)
	}
	return nil
}

// copyPlan is what one copy-transport run adds to its Jobs.
type copyPlan struct {
	image      *relayPlan
	token      string
	hiveLabels map[string]string
}

// resolveCopy builds the copy plan: the image that supplies crux, a fresh
// token and, on network: none, the labels that select hive's own pod.
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
	plan := &copyPlan{image: image, token: l.bridgeToken}
	if plan.token == "" {
		raw := make([]byte, bridgeTokenLen)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("kubejob: generating the job bridge token: %w", err)
		}
		plan.token = hex.EncodeToString(raw)
	}
	if l.Options.network() == NetworkNone {
		pod, err := l.selfPod(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("kubejob: workspace_transport: copy with network: none reads hive's own pod labels to admit it to the job bridge: %w", err)
		}
		if len(pod.Metadata.Labels) == 0 {
			return nil, errors.New("kubejob: workspace_transport: copy with network: none needs hive's own pod to carry labels the NetworkPolicy can admit; it has none")
		}
		plan.hiveLabels = pod.Metadata.Labels
	}
	return plan, nil
}

// apply turns a claim-transport manifest into a copy-transport one.
func (p *copyPlan) apply(manifest map[string]any) error {
	jobSpec, _ := manifest["spec"].(map[string]any)
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
	volumes, _ := pod["volumes"].([]map[string]any)
	for i, v := range volumes {
		if v["name"] == workspaceName {
			volumes[i] = map[string]any{"name": workspaceName, "emptyDir": map[string]any{}}
		}
	}
	pod["volumes"] = append(volumes, map[string]any{"name": bridgeVolumeName, "emptyDir": map[string]any{}})
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
	c["env"] = append(env, map[string]string{"name": BridgeTokenEnv, "value": p.token})
	c["ports"] = []map[string]any{{"name": bridgePortName, "containerPort": substratebridge.DefaultPort, "protocol": "TCP"}}
	bridge := []string{
		path.Join(DefaultBridgeMount, path.Base(bridgeBinary)), "job-bridge",
		"-listen", ":" + strconv.Itoa(substratebridge.DefaultPort),
		"-workspace", workspace,
		"-token-env", BridgeTokenEnv,
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

// copyRun is one Job's workspace copy, running beside waitJob.
type copyRun struct {
	cancel context.CancelFunc
	result chan error
}

// startCopy pushes the workspace into the Job and pulls it back once the
// kick command exits. A copy that fails calls abort, which ends waitJob.
func (l *Launcher) startCopy(ctx context.Context, abort context.CancelFunc, ns, job, workspace string) *copyRun {
	cctx, cancel := context.WithCancel(ctx)
	run := &copyRun{cancel: cancel, result: make(chan error, 1)}
	go func() {
		err := l.copyWorkspace(cctx, ns, job, workspace)
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

func (l *Launcher) copyWorkspace(ctx context.Context, ns, job, workspace string) error {
	token := ""
	if l.copyPlan != nil {
		token = l.copyPlan.token
	}
	var packed bytes.Buffer
	if err := substratebridge.Pack(workspace, &packed); err != nil {
		return fmt.Errorf("kubejob: packing workspace %s: %w", workspace, err)
	}
	base, err := l.bridgeURL(ctx, ns, job)
	if err != nil {
		return err
	}
	// Push. A 409 means an earlier attempt landed and its answer was lost.
	if err := l.bridgePoll(ctx, func() (bool, error) {
		code, err := l.bridgeDo(ctx, http.MethodPut, base+substratebridge.PathWorkspace, token, packed.Bytes(), nil)
		if err != nil {
			return false, nil
		}
		if code == http.StatusNoContent || code == http.StatusConflict {
			return true, nil
		}
		return false, fmt.Errorf("kubejob: pushing the workspace into %s: HTTP %d", job, code)
	}); err != nil {
		return err
	}
	// Pull, once the kick command has exited.
	if err := l.bridgePoll(ctx, func() (bool, error) {
		var replaceErr error
		code, err := l.bridgeDo(ctx, http.MethodGet, base+substratebridge.PathWorkspace, token, nil, func(r io.Reader) {
			replaceErr = sandbox.ReplaceWorkspace(r, workspace)
		})
		switch {
		case err != nil || code == http.StatusConflict:
			return false, nil
		case code != http.StatusOK:
			return false, fmt.Errorf("kubejob: pulling the workspace from %s: HTTP %d", job, code)
		case replaceErr != nil:
			return false, fmt.Errorf("kubejob: replacing workspace %s from %s: %w", workspace, job, replaceErr)
		}
		return true, nil
	}); err != nil {
		return err
	}
	// The workspace is home, so this cannot fail the kick: a lost /done
	// only makes the bridge wait out its own timeout before it exits, and
	// finish ends the retries once the Job has finished.
	_ = l.bridgePoll(ctx, func() (bool, error) {
		code, err := l.bridgeDo(ctx, http.MethodPost, base+substratebridge.PathDone, token, nil, nil)
		return err == nil && code == http.StatusNoContent, nil
	})
	return nil
}

// bridgePoll calls step every poll interval until it reports done, fails, or
// ctx ends.
func (l *Launcher) bridgePoll(ctx context.Context, step func() (bool, error)) error {
	interval := l.Options.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	for {
		done, err := step()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("kubejob: copying the workspace: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// bridgeURL waits for the Job's pod to have an IP and returns the bridge's
// base URL there; Launcher.BridgeURL overrides it.
func (l *Launcher) bridgeURL(ctx context.Context, ns, job string) (string, error) {
	if l.BridgeURL != "" {
		return strings.TrimRight(l.BridgeURL, "/"), nil
	}
	var ip string
	err := l.bridgePoll(ctx, func() (bool, error) {
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
func (l *Launcher) bridgeDo(ctx context.Context, method, url, token string, body []byte, onOK func(io.Reader)) (int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
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
