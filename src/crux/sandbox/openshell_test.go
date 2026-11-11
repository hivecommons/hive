package sandbox

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	openshell "github.com/ai-openshell/sdk-go/openshell/v1"
)

// fakeOpenShell is an in-memory stand-in for the SDK slices the launcher
// uses. It records the calls so tests can assert on the spec that reached
// the gateway and on cleanup, and lets each step fail or block on demand.
type fakeOpenShell struct {
	created     []fakeCreate
	deleted     []string
	waitErr     error
	waitBlocks  bool
	runErr      error
	runBlocks   bool
	runResult   *openshell.ExecResult
	runCmd      []string
	runOpts     openshell.ExecOptions
	createErr   error
	closed      bool
	deleteCtxOK bool
}

type fakeCreate struct {
	workspace, name string
	spec            *openshell.SandboxSpec
}

func (f *fakeOpenShell) Sandboxes() openshellSandboxes { return f }
func (f *fakeOpenShell) Exec() openshellExec           { return f }
func (f *fakeOpenShell) Close() error                  { f.closed = true; return nil }

func (f *fakeOpenShell) Create(_ context.Context, ws, name string, spec *openshell.SandboxSpec, _ ...openshell.CreateOption) (*openshell.Sandbox, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, fakeCreate{ws, name, spec})
	return &openshell.Sandbox{Name: name}, nil
}

func (f *fakeOpenShell) WaitReady(ctx context.Context, _, name string, _ ...openshell.WaitOptions) (*openshell.Sandbox, error) {
	if f.waitBlocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return &openshell.Sandbox{Name: name}, nil
}

func (f *fakeOpenShell) Delete(ctx context.Context, _, name string, _ ...openshell.DeleteOptions) (*openshell.DeletionResult, error) {
	// Cleanup must arrive on a live context even when the kick's has died.
	f.deleteCtxOK = ctx.Err() == nil
	f.deleted = append(f.deleted, name)
	return &openshell.DeletionResult{}, nil
}

func (f *fakeOpenShell) Run(ctx context.Context, _, _ string, cmd []string, opts ...openshell.ExecOptions) (*openshell.ExecResult, error) {
	f.runCmd = cmd
	if len(opts) > 0 {
		f.runOpts = opts[0]
	}
	if f.runBlocks {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.runResult != nil {
		return f.runResult, nil
	}
	return &openshell.ExecResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

func newTestOpenShell(fake *fakeOpenShell) *OpenShellLauncher {
	return &OpenShellLauncher{
		Options: OpenShellOptions{
			GatewayURL:     "openshell-gateway.openshell.svc:8080",
			WorkspaceClaim: "hive-data",
			AllowHosts:     []string{"litellm.example.com", "hub.example.org:8443"},
			NodeSelector:   map[string]string{"kernel": "6.12"},
			Tolerations:    []OpenShellToleration{{Key: "sandbox", Operator: "Exists", Effect: "NoSchedule"}},
		},
		newClient: func(OpenShellOptions) (openshellClient, error) { return fake, nil },
		now:       func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func testSpec() LaunchSpec {
	return LaunchSpec{
		Image:        "registry.example/dev:sha256-abc",
		Name:         "deps-sandbox",
		Workspace:    "/data/agents/sandbox/deps",
		Command:      []string{"crux", "turn", "--prompt-file", ".hive/prompt.md"},
		Env:          map[string]string{"HIVE_AGENT": "deps", "GITHUB_TOKEN": "leak"},
		EnvAllowlist: []string{"PATH_NOT_SET_IN_TEST"},
	}
}

func TestOpenShellRunSuccess(t *testing.T) {
	fake := &fakeOpenShell{runResult: &openshell.ExecResult{ExitCode: 3, Stdout: []byte("out"), Stderr: []byte("err")}}
	l := newTestOpenShell(fake)
	res, err := l.Run(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 3 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("result = %+v", res)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created = %d, want 1", len(fake.created))
	}
	c := fake.created[0]
	if c.workspace != OpenShellDefaultWorkspace {
		t.Errorf("workspace = %q", c.workspace)
	}
	if c.name != "hive-kick-deps-1700000000" {
		t.Errorf("name = %q", c.name)
	}
	if c.spec.Template.Image != "registry.example/dev:sha256-abc" {
		t.Errorf("image = %q", c.spec.Template.Image)
	}
	if _, leaked := c.spec.Template.Environment["GITHUB_TOKEN"]; leaked {
		t.Error("credential env reached the sandbox template")
	}
	if _, leaked := fake.runOpts.Env["GITHUB_TOKEN"]; leaked {
		t.Error("credential env reached exec")
	}
	if fake.runOpts.Env["HIVE_AGENT"] != "deps" {
		t.Errorf("exec env = %v, want HIVE_AGENT", fake.runOpts.Env)
	}
	if fake.runOpts.WorkDir != OpenShellWorkspaceMount {
		t.Errorf("workdir = %q", fake.runOpts.WorkDir)
	}
	if strings.Join(fake.runCmd, " ") != "crux turn --prompt-file .hive/prompt.md" {
		t.Errorf("cmd = %v", fake.runCmd)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != c.name {
		t.Errorf("deleted = %v, want the sandbox removed after success", fake.deleted)
	}
	if !fake.closed {
		t.Error("client not closed")
	}
}

func TestOpenShellRunDriverConfigMountsWorkspaceClaim(t *testing.T) {
	fake := &fakeOpenShell{}
	l := newTestOpenShell(fake)
	if _, err := l.Run(context.Background(), testSpec()); err != nil {
		t.Fatal(err)
	}
	dc := fake.created[0].spec.Template.DriverConfig
	k8s, ok := dc["kubernetes"].(map[string]any)
	if !ok {
		t.Fatalf("driver config = %v", dc)
	}
	vols := k8s["volumes"].([]any)
	pvc := vols[0].(map[string]any)["persistent_volume_claim"].(map[string]any)
	if pvc["claim_name"] != "hive-data" {
		t.Errorf("claim = %v", pvc)
	}
	mount := k8s["containers"].(map[string]any)["agent"].(map[string]any)["volume_mounts"].([]any)[0].(map[string]any)
	if mount["sub_path"] != "agents/sandbox/deps" || mount["mount_path"] != OpenShellWorkspaceMount {
		t.Errorf("mount = %v", mount)
	}
	pod := k8s["pod"].(map[string]any)
	if pod["node_selector"].(map[string]string)["kernel"] != "6.12" {
		t.Errorf("pod = %v", pod)
	}
	if tols := pod["tolerations"].([]any); len(tols) != 1 || tols[0].(map[string]any)["key"] != "sandbox" {
		t.Errorf("tolerations = %v", tols)
	}
}

func TestOpenShellRunRejectsWorkspaceOutsideClaim(t *testing.T) {
	fake := &fakeOpenShell{}
	l := newTestOpenShell(fake)
	spec := testSpec()
	spec.Workspace = "/srv/elsewhere/deps"
	_, err := l.Run(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "workspace claim mount") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("sandbox created despite invalid workspace")
	}
}

func TestOpenShellRunTimeoutDeletesSandbox(t *testing.T) {
	fake := &fakeOpenShell{runBlocks: true}
	l := newTestOpenShell(fake)
	l.Options.Timeout = 50 * time.Millisecond
	res, err := l.Run(context.Background(), testSpec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
	if res.ExitCode != OpenShellExitCodeUnknown {
		t.Errorf("exit = %d, want unknown", res.ExitCode)
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted = %v, want the running sandbox removed on timeout", fake.deleted)
	}
	if !fake.deleteCtxOK {
		t.Error("cleanup used the dead kick context")
	}
}

func TestOpenShellRunWaitReadyFailureCleansUp(t *testing.T) {
	fake := &fakeOpenShell{waitErr: errors.New("image pull backoff")}
	l := newTestOpenShell(fake)
	_, err := l.Run(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "never became ready") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.deleted) != 1 {
		t.Errorf("deleted = %v", fake.deleted)
	}
}

func TestOpenShellRunRequiresImage(t *testing.T) {
	fake := &fakeOpenShell{}
	l := newTestOpenShell(fake)
	spec := testSpec()
	spec.Image = ""
	if _, err := l.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("sandbox created without an image")
	}
}

func TestOpenShellNetworkPolicy(t *testing.T) {
	p, err := OpenShellNetworkPolicy([]string{"litellm.example.com", "hub.example.org:8443", "github.com", "litellm.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != OpenShellPolicyVersion {
		t.Errorf("version = %d", p.Version)
	}
	git := p.NetworkPolicies["hive-git"]
	if len(git.Endpoints) != 1 || git.Endpoints[0].Host != "github.com" || git.Endpoints[0].Access != openshell.NetworkAccessPresetReadWrite {
		t.Errorf("git rule = %+v", git)
	}
	api := p.NetworkPolicies["hive-github-api"]
	if api.Endpoints[0].Access != openshell.NetworkAccessPresetReadOnly {
		t.Errorf("api.github.com must be read-only: %+v", api)
	}
	if len(git.Binaries) != len(OpenShellDefaultBinaries) {
		t.Errorf("binaries = %d, want defaults applied", len(git.Binaries))
	}
	extra := p.NetworkPolicies["hive-allow-hosts"]
	if len(extra.Endpoints) != 2 {
		t.Fatalf("extra endpoints = %+v, want deduplicated hosts", extra.Endpoints)
	}
	if extra.Endpoints[0].Host != "hub.example.org" || extra.Endpoints[0].Port != 8443 || extra.Endpoints[1].Port != 443 {
		t.Errorf("extra = %+v", extra.Endpoints)
	}
	for _, r := range p.NetworkPolicies {
		for _, e := range r.Endpoints {
			if e.Enforcement != openshell.NetworkEnforcementModeEnforce {
				t.Errorf("%s/%s not enforced", r.Name, e.Host)
			}
		}
	}
	if p.Filesystem == nil || !p.Filesystem.IncludeWorkdir {
		t.Error("filesystem policy must include the workdir")
	}
}

func TestOpenShellNetworkPolicyRejectsURLs(t *testing.T) {
	if _, err := OpenShellNetworkPolicy([]string{"https://litellm.example.com"}, nil); err == nil {
		t.Error("URL accepted as allow_hosts entry")
	}
	if _, err := OpenShellNetworkPolicy([]string{"host:99999"}, nil); err == nil {
		t.Error("bad port accepted")
	}
}

func TestOpenShellNetworkNoneDropsEgress(t *testing.T) {
	fake := &fakeOpenShell{}
	l := newTestOpenShell(fake)
	spec := testSpec()
	spec.NetworkNone = true
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.created[0].spec.Policy.NetworkPolicies); n != 0 {
		t.Errorf("network rules = %d, want none", n)
	}
}

func TestOpenShellOptionsValidate(t *testing.T) {
	cases := map[string]OpenShellOptions{
		"no gateway":  {WorkspaceClaim: "c"},
		"no claim":    {GatewayURL: "gw:8080"},
		"bad scheme":  {GatewayURL: "ftp://gw", WorkspaceClaim: "c"},
		"bad host":    {GatewayURL: "gw:8080", WorkspaceClaim: "c", AllowHosts: []string{"http://x"}},
		"bare scheme": {GatewayURL: "gw", WorkspaceClaim: "c"},
	}
	for name, o := range cases {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	ok := []string{"gw:8080", "http://gw:8080", "https://gw", "grpcs://gw:9443"}
	for _, g := range ok {
		if err := (OpenShellOptions{GatewayURL: g, WorkspaceClaim: "c"}).Validate(); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
}

func TestOpenShellAddress(t *testing.T) {
	cases := []struct {
		in, addr string
		tls      bool
	}{
		{"gw:8080", "gw:8080", false},
		{"http://gw:8080", "gw:8080", false},
		{"https://gw", "gw:443", true},
		{"https://gw:9443", "gw:9443", true},
	}
	for _, c := range cases {
		addr, tls, err := openshellAddress(c.in)
		if err != nil || addr != c.addr || tls != c.tls {
			t.Errorf("%s: %q %v %v", c.in, addr, tls, err)
		}
	}
}

func TestOpenShellSandboxName(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if got := openshellSandboxName("Deps Agent-sandbox", now); got != "hive-kick-deps-agent-1700000000" {
		t.Errorf("name = %q", got)
	}
	long := openshellSandboxName(strings.Repeat("a", 100), now)
	if len(long) > openshellNameMax || !strings.HasSuffix(long, "-1700000000") {
		t.Errorf("long name = %q (%d)", long, len(long))
	}
	if got := openshellSandboxName("---", now); got != "hive-kick-agent-1700000000" {
		t.Errorf("empty base = %q", got)
	}
}

func TestOpenShellRunRefusesClineProvider(t *testing.T) {
	spec, _ := clineLaunchFixture(t)
	fake := &fakeOpenShell{}
	l := newTestOpenShell(fake)
	spec.Image = "registry.example/dev:sha256-abc"
	spec.Workspace = "/data/agents/sandbox/deps"
	if _, err := l.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "Podman-only") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("sandbox created despite the Cline provider refusal")
	}
}

func TestOpenShellRunRejectsBadOptionsAndSpec(t *testing.T) {
	fake := &fakeOpenShell{}
	l := newTestOpenShell(fake)
	l.Options.WorkspaceClaim = ""
	if _, err := l.Run(context.Background(), testSpec()); err == nil || !strings.Contains(err.Error(), "workspace_claim is required") {
		t.Fatalf("missing claim: err = %v", err)
	}

	l = newTestOpenShell(fake)
	spec := testSpec()
	spec.Workspace = ""
	if _, err := l.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("missing workspace: err = %v", err)
	}

	spec = testSpec()
	spec.Command = nil
	if _, err := l.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("missing command: err = %v", err)
	}

	l.Options.AllowHosts = []string{"https://not-a-host"}
	if _, err := l.Run(context.Background(), testSpec()); err == nil || !strings.Contains(err.Error(), "must be host or host:port") {
		t.Fatalf("bad allow_hosts: err = %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("a sandbox was created for an invalid launch")
	}
}

func TestOpenShellRunClientAndCreateErrors(t *testing.T) {
	l := newTestOpenShell(&fakeOpenShell{})
	l.newClient = func(OpenShellOptions) (openshellClient, error) { return nil, errors.New("dial refused") }
	if _, err := l.Run(context.Background(), testSpec()); err == nil || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("client error: err = %v", err)
	}

	fake := &fakeOpenShell{createErr: errors.New("quota exceeded")}
	l = newTestOpenShell(fake)
	res, err := l.Run(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "creating sandbox") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("create error: err = %v", err)
	}
	if res.ExitCode != OpenShellExitCodeUnknown {
		t.Errorf("exit = %d, want unknown", res.ExitCode)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("deleted = %v, nothing to clean up when create failed", fake.deleted)
	}
	if !fake.closed {
		t.Error("client not closed after a failed create")
	}
}

func TestOpenShellRunExecErrorCleansUpAndWraps(t *testing.T) {
	fake := &fakeOpenShell{runErr: errors.New("stream reset")}
	l := newTestOpenShell(fake)
	_, err := l.Run(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "running kick in sandbox") || !strings.Contains(err.Error(), "stream reset") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.deleted) != 1 {
		t.Errorf("deleted = %v, want the sandbox removed after an exec failure", fake.deleted)
	}
}

func TestOpenShellRunWaitReadyCanceledReturnsContextError(t *testing.T) {
	fake := &fakeOpenShell{waitBlocks: true}
	l := newTestOpenShell(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.Run(ctx, testSpec())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if len(fake.deleted) != 1 || !fake.deleteCtxOK {
		t.Errorf("deleted = %v ctxOK = %v, want cleanup on a live context", fake.deleted, fake.deleteCtxOK)
	}
}

func TestOpenShellRunPolicyProfileRuntimeClassAndOutputCap(t *testing.T) {
	big := bytes.Repeat([]byte("x"), openshellMaxOutput+10)
	fake := &fakeOpenShell{runResult: &openshell.ExecResult{ExitCode: 0, Stdout: big, Stderr: big}}
	l := newTestOpenShell(fake)
	l.Options.PolicyProfile = "strict"
	l.Options.RuntimeClassName = "gvisor"
	l.Options.Workspace = "team-a"
	res, err := l.Run(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != openshellMaxOutput || len(res.Stderr) != openshellMaxOutput {
		t.Errorf("stdout %d stderr %d, want capped at %d", len(res.Stdout), len(res.Stderr), openshellMaxOutput)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created = %d", len(fake.created))
	}
	c := fake.created[0]
	if c.workspace != "team-a" {
		t.Errorf("workspace = %q, want the configured OpenShell workspace", c.workspace)
	}
	if c.spec.Template.RuntimeClassName != "gvisor" {
		t.Errorf("runtime class = %q", c.spec.Template.RuntimeClassName)
	}
	k8s, _ := c.spec.Template.DriverConfig["kubernetes"].(map[string]any)
	pod, _ := k8s["pod"].(map[string]any)
	if pod == nil || pod["runtime_class_name"] != "gvisor" {
		t.Errorf("driver_config pod = %v, want runtime_class_name gvisor", pod)
	}
}

func TestOpenShellNetworkPolicySortsAllowHostsByPort(t *testing.T) {
	policy, err := OpenShellNetworkPolicy([]string{"svc.example:9443", "svc.example:8443"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eps := policy.NetworkPolicies["hive-allow-hosts"].Endpoints
	if len(eps) != 2 || eps[0].Port != 8443 || eps[1].Port != 9443 {
		t.Errorf("endpoints = %+v, want sorted by port within a host", eps)
	}
}

func TestOpenShellAddressAndHostPortErrors(t *testing.T) {
	for _, in := range []string{"", "gw", "gw:notaport", "ftp://gw:21", "http://", "http://%zz"} {
		if _, _, err := openshellAddress(in); err == nil {
			t.Errorf("openshellAddress(%q) accepted", in)
		}
	}
	if addr, tls, err := openshellAddress("http://gw"); err != nil || addr != "gw:80" || tls {
		t.Errorf("http default port: %q %v %v", addr, tls, err)
	}
	if addr, tls, err := openshellAddress("grpcs://gw:7443"); err != nil || addr != "gw:7443" || !tls {
		t.Errorf("grpcs: %q %v %v", addr, tls, err)
	}
	for _, in := range []string{"", "  ", "host:0", "host:70000", ":443", "host:abc", "host/path", "has space"} {
		if _, _, err := openshellHostPort(in); err == nil {
			t.Errorf("openshellHostPort(%q) accepted", in)
		}
	}
}

func TestNewOpenShellSDKClientBuildsLazyClient(t *testing.T) {
	// grpc.NewClient does not dial until the first RPC, so constructing the
	// real SDK client against an unreachable address is hermetic.
	for _, o := range []OpenShellOptions{
		{GatewayURL: "127.0.0.1:1"},
		{GatewayURL: "https://127.0.0.1:1", Token: "tok"},
		{GatewayURL: "127.0.0.1:1", InsecureTLS: true},
	} {
		c, err := newOpenShellSDKClient(o)
		if err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		if c.Sandboxes() == nil || c.Exec() == nil {
			t.Errorf("%+v: nil SDK service", o)
		}
		if err := c.Close(); err != nil {
			t.Errorf("%+v: close: %v", o, err)
		}
	}
	if _, err := newOpenShellSDKClient(OpenShellOptions{GatewayURL: "nope"}); err == nil {
		t.Error("bad gateway_url accepted")
	}
	l := &OpenShellLauncher{Options: OpenShellOptions{GatewayURL: "127.0.0.1:1"}}
	c, err := l.client()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
