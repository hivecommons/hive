package kubejob

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

const labelledSelfPod = `{"metadata":{"labels":{"app":"hive"},"annotations":{}},"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:stable"}]},"status":{"containerStatuses":[{"name":"hive","imageID":"docker-pullable://` + testHiveDigest + `"}]}}`

// startKickBridge runs a real job bridge on loopback whose command writes
// new.txt into its workspace, and reports when Serve returned.
func startKickBridge(t *testing.T, token string) (string, *atomic.Bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	k := &substratebridge.KickServer{
		Workspace: work,
		Command:   []string{"sh", "-c", `cp "$0/in.txt" "$0/new.txt"`, work},
		Token:     token,
		Wait:      10 * time.Second,
	}
	var finished atomic.Bool
	go func() {
		_ = k.Serve(context.Background(), ln)
		finished.Store(true)
	}()
	return "http://" + ln.Addr().String(), &finished
}

func copyOptions() Options {
	uid := int64(1000)
	return Options{WorkspaceTransport: "copy", RelayImage: testHiveDigest, RelayRunAsUser: &uid}
}

// TestCopyTransportPushesRunsAndPullsTheWorkspace (#19621): the Job mounts no
// claim, runs the kick behind the job bridge, and hive's workspace gets the
// agent's changes back before the Job is deleted.
func TestCopyTransportPushesRunsAndPullsTheWorkspace(t *testing.T) {
	const token = "test-token"
	api := newFakeAPI()
	api.statuses = []string{statusPending}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	api.selfPod = labelledSelfPod
	l := newLauncher(srv, copyOptions())
	l.Options.WorkspaceClaim = ""
	if err := l.Options.Validate(); err != nil {
		t.Fatalf("copy transport without a claim must validate: %v", err)
	}
	var finished *atomic.Bool
	l.BridgeURL, finished = startKickBridge(t, token)
	l.bridgeToken = token
	// The Job completes once the bridge has exited, as the container would.
	api.onJobGet = func() {
		if finished.Load() {
			api.statuses = []string{statusComplete}
		}
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "in.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := specFor(ws)
	spec.Relays = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "new.txt")); string(got) != "hi" {
		t.Errorf("hive workspace new.txt = %q, want the Job's write copied back", got)
	}
	if !api.deleted {
		t.Error("a completed Job must be deleted")
	}
	got := asJSON(podSpecOf(t, api.created))
	if strings.Contains(got, "persistentVolumeClaim") || strings.Contains(got, "subPath") {
		t.Errorf("copy transport must not mount hive's claim: %s", got)
	}
	if !strings.Contains(got, `{"emptyDir":{},"name":"workspace"}`) {
		t.Errorf("workspace volume is not an emptyDir: %s", got)
	}
	if strings.Contains(got, `"name":"`+relayVolumeName+`"`) {
		t.Errorf("relays must be off on the copy transport: %s", got)
	}
	if !strings.Contains(got, `"command":["cp","/usr/local/bin/crux","/opt/hive-bridge/"]`) {
		t.Errorf("no init container copying crux: %s", got)
	}
	c := containerOf(t, api.created)
	if cmd := asJSON(c["command"]); !strings.HasPrefix(cmd, `["/opt/hive-bridge/crux","job-bridge",`) || !strings.Contains(cmd, `"--","claude","-p"`) {
		t.Errorf("container command = %s, want the kick behind crux job-bridge", cmd)
	}
	env := asJSON(c["env"])
	if strings.Contains(env, token) || !strings.Contains(env, `"name":"`+BridgeTokenEnv+`","valueFrom":{"secretKeyRef":`) {
		t.Errorf("container env = %s, want the bridge token by secretKeyRef, never a literal", env)
	}
	if !strings.Contains(asJSON(api.secret), `"token":"`+token+`"`) || !api.secretDeleted {
		t.Errorf("token Secret = %s deleted=%v, want it created with the token and removed with the Job", asJSON(api.secret), api.secretDeleted)
	}
	// Outside network: none the bridge port is still limited to hive's pod.
	if got := asJSON(api.policy); !strings.Contains(got, `"policyTypes":["Ingress"]`) || !strings.Contains(got, `"matchLabels":{"app":"hive"}`) || strings.Contains(got, "egress") {
		t.Errorf("policy = %s, want an ingress-only policy admitting hive's pod", got)
	}
}

func containerOf(t *testing.T, job map[string]any) map[string]any {
	t.Helper()
	pod := podSpecOf(t, job)
	cs, _ := pod["containers"].([]any)
	if len(cs) == 0 {
		t.Fatalf("no containers in %s", asJSON(pod))
	}
	return cs[0].(map[string]any)
}

func TestCopyTransportNetworkNoneAdmitsOnlyHive(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = labelledSelfPod
	api.createErr = 500 // the policy is what is under test
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	opts := copyOptions()
	opts.Network = NetworkNone
	l := newLauncher(srv, opts)
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/x")); err == nil {
		t.Fatal("Run succeeded with a failing Job create")
	}
	got := asJSON(api.policy)
	if !strings.Contains(got, `"ingress":[{"from":[{"podSelector":{"matchLabels":{"app":"hive"}}}],"ports":[{"port":8477,"protocol":"TCP"}]}]`) {
		t.Errorf("policy = %s, want ingress from hive's pod labels to the bridge port only", got)
	}
	if strings.Contains(got, "egress") {
		t.Errorf("policy must still deny all egress: %s", got)
	}
}

func TestCopyTransportValidation(t *testing.T) {
	if err := (Options{WorkspaceTransport: "rsync"}).Validate(); err == nil || !strings.Contains(err.Error(), "workspace_transport") {
		t.Errorf("unknown transport: err = %v", err)
	}
	if err := (Options{WorkspaceTransport: "copy", RelayImage: RelayImageNone}).Validate(); err == nil || !strings.Contains(err.Error(), "relay_image") {
		t.Errorf("copy with relay_image none: err = %v", err)
	}
	if err := (Options{WorkspaceTransport: "claim"}).Validate(); err == nil || !strings.Contains(err.Error(), "workspace_claim is required") {
		t.Errorf("claim transport without a claim: err = %v", err)
	}
	api := newFakeAPI()
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	spec := specFor("/data/agents/sandbox/x")
	spec.Command = nil
	if _, err := newLauncher(srv, copyOptions()).Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "explicit command") {
		t.Errorf("copy without a command: err = %v", err)
	}
}

func TestPodIPReadsTheJobPod(t *testing.T) {
	api := newFakeAPI()
	api.podLists = []string{`{"items":[{"metadata":{"name":"p"},"status":{"podIP":"10.1.2.3"}}]}`}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	ip, err := l.podIP(context.Background(), "hive", "job")
	if err != nil || ip != "10.1.2.3" {
		t.Errorf("podIP = %q, %v; want 10.1.2.3", ip, err)
	}
	url, err := l.bridgeURL(context.Background(), "hive", "job")
	if err != nil || url != "http://10.1.2.3:8477" {
		t.Errorf("bridgeURL = %q, %v", url, err)
	}
}
