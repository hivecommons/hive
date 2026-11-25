package kubejob

import (
	"context"
	"net"
	"net/http"
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
	if !strings.Contains(got, `{"emptyDir":{"sizeLimit":"10Gi"},"name":"workspace"}`) {
		t.Errorf("workspace volume is not an emptyDir with the default sizeLimit: %s", got)
	}
	if !strings.Contains(got, `{"emptyDir":{"sizeLimit":"`+bridgeVolumeSizeLimit+`"},"name":"`+bridgeVolumeName+`"}`) {
		t.Errorf("bridge volume has no sizeLimit: %s", got)
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
	if strings.Contains(env, token) || !strings.Contains(env, `"name":"`+substratebridge.JobTokenEnv+`","valueFrom":{"secretKeyRef":`) {
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
	api.patchErr = 500 // stop after the objects exist; the policy is what is under test
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	opts := copyOptions()
	opts.Network = NetworkNone
	l := newLauncher(srv, opts)
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/x")); err == nil {
		t.Fatal("Run succeeded with a failing Job resume")
	}
	got := asJSON(api.policy)
	if !strings.Contains(got, `"ingress":[{"from":[{"podSelector":{"matchLabels":{"app":"hive"}}}],"ports":[{"port":8477,"protocol":"TCP"}]}]`) {
		t.Errorf("policy = %s, want ingress from hive's pod labels to the bridge port only", got)
	}
	// Deny-all egress is both policyTypes naming Egress and no egress rule:
	// a policy without Egress in policyTypes allows all egress.
	spec, _ := api.policy["spec"].(map[string]any)
	if got := asJSON(spec["policyTypes"]); got != `["Ingress","Egress"]` {
		t.Errorf("policyTypes = %s, want [\"Ingress\",\"Egress\"]", got)
	}
	if _, has := spec["egress"]; has {
		t.Errorf("policy must have no egress rules: %s", got)
	}
}

// TestCopyTransportOwnsItsSecretAndPolicy (#19694 review): the Secret and
// the NetworkPolicy name the Job as their owner, so the garbage collector
// removes them with it after a hive restart, and the Job is created suspended
// and resumed once both exist so no pod runs before the policy does.
func TestCopyTransportOwnsItsSecretAndPolicy(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = labelledSelfPod
	api.patchErr = 500 // keep the objects' manifests; the resume fails after them
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, copyOptions())
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/x")); err == nil || !strings.Contains(err.Error(), "resuming job") {
		t.Fatalf("Run err = %v, want the resume failure", err)
	}
	if got := strings.Join(api.order, ","); got != "job,secret,policy" {
		t.Errorf("creation order = %q, want the suspended Job first, then its Secret and policy", got)
	}
	jobSpec, _ := api.created["spec"].(map[string]any)
	if jobSpec["suspend"] != true {
		t.Errorf("the Job must be created suspended: %s", asJSON(jobSpec))
	}
	name, _ := api.created["metadata"].(map[string]any)["name"].(string)
	for kind, obj := range map[string]map[string]any{"Secret": api.secret, "NetworkPolicy": api.policy} {
		md, _ := obj["metadata"].(map[string]any)
		want := `[{"apiVersion":"batch/v1","blockOwnerDeletion":true,"kind":"Job","name":"` + name + `","uid":"job-uid-1"}]`
		if got := asJSON(md["ownerReferences"]); got != want {
			t.Errorf("%s ownerReferences = %s, want %s", kind, got, want)
		}
		labels := asJSON(md["labels"])
		if !strings.Contains(labels, managedByLabel) || !strings.Contains(labels, jobLabel) {
			t.Errorf("%s labels = %s, want managed-by and the Job name so it can be selected", kind, labels)
		}
	}
	// The resume failed: what was created is removed, Job included.
	if !api.secretDeleted || !api.policyDeleted || !api.deleted {
		t.Errorf("after a failed resume secret deleted=%v policy deleted=%v job deleted=%v, want all removed", api.secretDeleted, api.policyDeleted, api.deleted)
	}
}

func TestCopyTransportResumesTheJobAfterItsObjects(t *testing.T) {
	const token = "test-token"
	api := newFakeAPI()
	api.statuses = []string{statusPending}
	api.selfPod = labelledSelfPod
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, copyOptions())
	var finished *atomic.Bool
	l.BridgeURL, finished = startKickBridge(t, token)
	l.bridgeToken = token
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, specFor(ws)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(api.patches) != 1 || !strings.Contains(api.patches[0], `"suspend":false`) {
		t.Errorf("Job patches = %v, want one resume", api.patches)
	}
}

// failedCopyRun runs a copy-mode kick against the fake API and returns its
// error; tweak edits the fake before the run.
func failedCopyRun(t *testing.T, tweak func(*fakeAPI, *Launcher)) (*fakeAPI, error) {
	t.Helper()
	api := newFakeAPI()
	api.selfPod = labelledSelfPod
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	l := newLauncher(srv, copyOptions())
	tweak(api, l)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := l.Run(ctx, specFor("/data/agents/sandbox/x"))
	return api, err
}

// TestCopyTransportFailsClosedWhenAnObjectCreateFails: a denied or conflicting
// create names what failed, leaves nothing behind, and never starts a pod.
func TestCopyTransportFailsClosedWhenAnObjectCreateFails(t *testing.T) {
	tests := []struct {
		name         string
		tweak        func(*fakeAPI, *Launcher)
		wantErr      string
		wantSecret   bool // the Secret was created, so it must be deleted
		wantPolicyIn bool
	}{
		{"policy denied", func(a *fakeAPI, _ *Launcher) { a.policyErr = 403 }, "creating the NetworkPolicy", true, false},
		{"secret forbidden", func(a *fakeAPI, _ *Launcher) { a.secretErr = 403 }, "creating the bridge token Secret", false, false},
		{"secret conflict", func(a *fakeAPI, _ *Launcher) { a.secretErr = 409 }, "creating the bridge token Secret", false, false},
		{"resume forbidden", func(a *fakeAPI, _ *Launcher) { a.patchErr = 403 }, "needs the patch verb on jobs", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api, err := failedCopyRun(t, tc.tweak)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !api.deleted {
				t.Error("the suspended Job must be deleted so it cannot run later")
			}
			if api.secretDeleted != tc.wantSecret {
				t.Errorf("secret deleted = %v, want %v", api.secretDeleted, tc.wantSecret)
			}
			if api.policyDeleted != tc.wantPolicyIn {
				t.Errorf("policy deleted = %v, want %v", api.policyDeleted, tc.wantPolicyIn)
			}
			if len(api.patches) != 0 {
				t.Errorf("the Job was resumed (%v) after a failed create", api.patches)
			}
		})
	}
}

// fakeBridge is a job bridge whose answers a test scripts.
func fakeBridge(t *testing.T, put, get http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && put != nil:
			put(w, r)
		case r.Method == http.MethodGet && get != nil:
			get(w, r)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestCopyTransportPushFailureDeletesTheJob: nothing ran, so there is no work
// to keep.
func TestCopyTransportPushFailureDeletesTheJob(t *testing.T) {
	api, err := failedCopyRun(t, func(a *fakeAPI, l *Launcher) {
		a.statuses = []string{statusPending}
		l.BridgeURL = fakeBridge(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, nil)
		l.Options.PollInterval = time.Millisecond
	})
	if err == nil || !strings.Contains(err.Error(), "pushing the workspace") {
		t.Fatalf("err = %v, want the push failure", err)
	}
	if !api.deleted {
		t.Error("a Job whose workspace never arrived must be deleted")
	}
}

// TestCopyTransportPullFailureKeepsTheJob (#19694 review): the workspace
// exists only in the Job, so a failed pull keeps it, as a failed kick does,
// after retrying the unpack.
func TestCopyTransportPullFailureKeepsTheJob(t *testing.T) {
	var gets atomic.Int32
	api, err := failedCopyRun(t, func(a *fakeAPI, l *Launcher) {
		a.statuses = []string{statusPending}
		l.BridgeURL = fakeBridge(t, nil, func(w http.ResponseWriter, _ *http.Request) {
			gets.Add(1)
			_, _ = w.Write([]byte("not a gzip stream"))
		})
		l.Options.PollInterval = time.Millisecond
	})
	if err == nil || !strings.Contains(err.Error(), "replacing workspace") {
		t.Fatalf("err = %v, want the replace failure", err)
	}
	if got := gets.Load(); got != bridgeMaxReplaceTries {
		t.Errorf("pull attempts = %d, want %d: a replace error is retried, then fails", got, bridgeMaxReplaceTries)
	}
	if api.deleted {
		t.Error("the Job holds the only copy of the agent's work and must be kept")
	}
}

// TestCopyTransportTimeoutPullsTheWorkspaceBeforeDeletingTheJob: on a kick
// timeout the command is stopped and the workspace is pulled on a fresh
// context, so the agent's partial work survives as it does on the claim.
func TestCopyTransportTimeoutPullsTheWorkspaceBeforeDeletingTheJob(t *testing.T) {
	const token = "test-token"
	api := newFakeAPI()
	api.statuses = []string{statusPending}
	api.selfPod = labelledSelfPod
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, copyOptions())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	k := &substratebridge.KickServer{
		Workspace: work,
		Command:   []string{"sh", "-c", `echo partial > "$0/partial.txt"; sleep 60`, work},
		Token:     token,
		Wait:      10 * time.Second,
	}
	go func() { _ = k.Serve(context.Background(), ln) }()
	l.BridgeURL = "http://" + ln.Addr().String()
	l.bridgeToken = token
	l.Options.PollInterval = 5 * time.Millisecond
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "in.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, specFor(ws)); err == nil {
		t.Fatal("Run succeeded past its timeout")
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "partial.txt")); string(got) != "partial\n" {
		t.Errorf("hive workspace partial.txt = %q, want the work done before the timeout pulled back", got)
	}
	if !api.deleted {
		t.Error("the timed-out Job must still be deleted, after the pull")
	}
}

func TestCopyTransportValidation(t *testing.T) {
	if err := (Options{WorkspaceTransport: "rsync"}).Validate(); err == nil || !strings.Contains(err.Error(), "workspace_transport") {
		t.Errorf("unknown transport: err = %v", err)
	}
	if err := (Options{WorkspaceTransport: "copy", RelayImage: RelayImageNone}).Validate(); err == nil || !strings.Contains(err.Error(), "relay_image") {
		t.Errorf("copy with relay_image none: err = %v", err)
	}
	if err := (Options{WorkspaceTransport: "copy", RelayImage: testHiveDigest, Network: NetworkHost}).Validate(); err == nil || !strings.Contains(err.Error(), "network: host") {
		t.Errorf("copy with network host: err = %v", err)
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
