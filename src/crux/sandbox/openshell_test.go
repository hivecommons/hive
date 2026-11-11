package sandbox

import (
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
