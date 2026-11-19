package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// fakeSubstrate is an in-memory stand-in for the Control API slice and the
// in-actor bridge the launcher drives. It records the calls so tests can
// assert on the template, actor, policy and cleanup that reached the control
// plane, and lets each step fail or report a state on demand.
type fakeSubstrate struct {
	mu sync.Mutex

	templates       []*substrateapi.ActorTemplate
	actors          []*substrateapi.Actor
	policies        []*substrateapi.EgressPolicy
	calls           []string
	templateErr     error
	actorErr        error
	policyErr       error
	resumeErr       error
	suspendErr      error
	deleteErr       error
	getErr          error
	states          []substrateapi.ActorState
	crashMsg        string
	spiffeID        string
	getCancels      context.CancelFunc
	waitReadyErr    error
	pushErr         error
	runErr          error
	runResp         substratebridge.RunResponse
	runCancels      context.CancelFunc
	pullErr         error
	runReq          substratebridge.RunRequest
	pushedWorkspace string
	pulledWorkspace string
	workers         []*substrateapi.Worker
	listErr         error
	deleted         []string
	closed          bool
	cleanupCtxDead  bool
}

func (f *fakeSubstrate) note(c string, ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if (c == "DeleteActor" || c == "DeleteActorTemplate" || c == "SuspendActor") && ctx.Err() != nil {
		f.cleanupCtxDead = true
	}
}

func (f *fakeSubstrate) Control() substrateControl { return f }
func (f *fakeSubstrate) Bridge() substrateBridge   { return f }
func (f *fakeSubstrate) Close() error              { f.closed = true; return nil }

func (f *fakeSubstrate) CreateActorTemplate(ctx context.Context, tpl *substrateapi.ActorTemplate) (*substrateapi.ActorTemplate, error) {
	f.note("CreateActorTemplate", ctx)
	if f.templateErr != nil {
		return nil, f.templateErr
	}
	f.templates = append(f.templates, tpl)
	return tpl, nil
}

func (f *fakeSubstrate) DeleteActorTemplate(ctx context.Context, _ *substrateapi.ObjectRef) error {
	f.note("DeleteActorTemplate", ctx)
	return nil
}

func (f *fakeSubstrate) CreateActor(ctx context.Context, a *substrateapi.Actor) (*substrateapi.Actor, error) {
	f.note("CreateActor", ctx)
	if f.actorErr != nil {
		return nil, f.actorErr
	}
	f.actors = append(f.actors, a)
	return a, nil
}

func (f *fakeSubstrate) GetActor(ctx context.Context, ref *substrateapi.ObjectRef) (*substrateapi.Actor, error) {
	f.note("GetActor", ctx)
	if f.getErr != nil {
		return nil, f.getErr
	}
	state := substrateapi.ActorState_ACTOR_STATE_RUNNING
	if len(f.states) > 0 {
		state, f.states = f.states[0], f.states[1:]
	}
	if f.getCancels != nil {
		f.getCancels()
	}
	return &substrateapi.Actor{
		Metadata: &substrateapi.ResourceMetadata{Name: ref.GetName()},
		Status: &substrateapi.ActorStatus{
			State: state, Crash: &substrateapi.ActorCrash{Message: f.crashMsg},
			WorkerAssignment: &substrateapi.WorkerAssignment{WorkerSpiffeId: f.spiffeID},
		},
	}, nil
}

func (f *fakeSubstrate) ResumeActor(ctx context.Context, _ *substrateapi.ObjectRef) error {
	f.note("ResumeActor", ctx)
	return f.resumeErr
}

func (f *fakeSubstrate) SuspendActor(ctx context.Context, _ *substrateapi.ObjectRef, mode substrateapi.DurabilityMode) error {
	f.note("SuspendActor", ctx)
	f.calls = append(f.calls, "mode="+mode.String())
	return f.suspendErr
}

func (f *fakeSubstrate) DeleteActor(ctx context.Context, ref *substrateapi.ObjectRef) error {
	f.note("DeleteActor", ctx)
	f.deleted = append(f.deleted, ref.GetName())
	return f.deleteErr
}

func (f *fakeSubstrate) CreateActorEgressPolicy(ctx context.Context, _ *substrateapi.ObjectRef, p *substrateapi.EgressPolicy) error {
	f.note("CreateActorEgressPolicy", ctx)
	if f.policyErr != nil {
		return f.policyErr
	}
	f.policies = append(f.policies, p)
	return nil
}

func (f *fakeSubstrate) ListWorkers(ctx context.Context) ([]*substrateapi.Worker, error) {
	f.note("ListWorkers", ctx)
	return f.workers, f.listErr
}

func (f *fakeSubstrate) WaitReady(ctx context.Context, _ *substrateapi.ObjectRef, _ time.Duration) error {
	f.note("WaitReady", ctx)
	return f.waitReadyErr
}

func (f *fakeSubstrate) Push(ctx context.Context, _ *substrateapi.ObjectRef, ws string) error {
	f.note("Push", ctx)
	f.pushedWorkspace = ws
	return f.pushErr
}

func (f *fakeSubstrate) Run(ctx context.Context, _ *substrateapi.ObjectRef, req substratebridge.RunRequest, live io.Writer) (substratebridge.RunResponse, error) {
	f.note("Run", ctx)
	f.runReq = req
	if live != nil {
		_, _ = io.WriteString(live, f.runResp.Stdout+f.runResp.Stderr)
	}
	if f.runCancels != nil {
		f.runCancels()
	}
	return f.runResp, f.runErr
}

func (f *fakeSubstrate) Pull(ctx context.Context, _ *substrateapi.ObjectRef, ws string) error {
	f.note("Pull", ctx)
	f.pulledWorkspace = ws
	return f.pullErr
}

func (f *fakeSubstrate) saw(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

const substrateTestImage = "registry.example/hive/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestSubstrate(fake *fakeSubstrate) *SubstrateLauncher {
	l := &SubstrateLauncher{Options: SubstrateOptions{
		Endpoint:   "api.ate-system.svc:443",
		Atespace:   "hive",
		Token:      "tok",
		Image:      substrateTestImage,
		AllowHosts: []string{"litellm.example.com", "*.pypi.org:8443"},
	}}
	l.newClient = func(SubstrateOptions) (substrateClient, error) { return fake, nil }
	l.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	l.kicks = &SubstrateKicks{}
	return l
}

func substrateTestSpec() LaunchSpec {
	return LaunchSpec{
		Image:     "registry.example/hive/sandbox:dev",
		Name:      "deps-sandbox",
		Workspace: "/data/agents/sandbox/deps",
		Command:   []string{"crux", "kick", "--agent", "deps"},
		Env:       map[string]string{"HIVE_AGENT": "deps"},
	}
}

func TestSubstrateRunHappyPath(t *testing.T) {
	fake := &fakeSubstrate{runResp: substratebridge.RunResponse{Stdout: "done\n", Stderr: "warn\n", ExitCode: 0}}
	l := newTestSubstrate(fake)
	l.Options.PolicyProfile = "strict"
	l.Options.WorkerSelector = map[string]string{"class": "gvisor"}
	res, err := l.Run(context.Background(), substrateTestSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "done\n" || res.Stderr != "warn\n" || res.ExitCode != 0 {
		t.Errorf("result = %+v", res)
	}
	want := []string{"CreateActorTemplate", "CreateActor", "CreateActorEgressPolicy", "ResumeActor", "GetActor", "WaitReady", "Push", "Run", "Pull", "DeleteActor", "DeleteActorTemplate"}
	if got := strings.Join(fake.calls, " "); got != strings.Join(want, " ") {
		t.Errorf("calls = %s\nwant  = %s", got, strings.Join(want, " "))
	}
	if !fake.closed {
		t.Error("client not closed")
	}
	if len(fake.templates) != 1 {
		t.Fatalf("templates = %d", len(fake.templates))
	}
	tpl := fake.templates[0]
	if tpl.GetMetadata().GetName() != "hive-kick-deps-1700000000" || tpl.GetMetadata().GetAtespace() != "hive" {
		t.Errorf("template metadata = %v", tpl.GetMetadata())
	}
	c := tpl.GetContainers()[0]
	if c.GetImage() != substrateTestImage || strings.Join(c.GetCommand(), " ") != strings.Join(SubstrateDefaultBridgeCommand, " ") {
		t.Errorf("container = %v", c)
	}
	var envNames []string
	for _, e := range c.GetEnv() {
		envNames = append(envNames, e.GetName()+"="+e.GetValue())
	}
	if joined := strings.Join(envNames, ","); !strings.Contains(joined, "HIVE_SUBSTRATE_BRIDGE_PORT=8477") || !strings.Contains(joined, "HIVE_POLICY_PROFILE=strict") {
		t.Errorf("env = %s", joined)
	}
	if tpl.GetWorkerSelector().GetMatchLabels()["class"] != "gvisor" || fake.actors[0].GetWorkerSelector().GetMatchLabels()["class"] != "gvisor" {
		t.Error("worker selector not propagated to template and actor")
	}
	if tpl.GetVolumes()[0].GetDurableDir() == nil || c.GetVolumeMounts()[0].GetMountPath() != SubstrateWorkspaceMount {
		t.Errorf("workspace volume = %v / %v", tpl.GetVolumes(), c.GetVolumeMounts())
	}
	if tpl.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() != substrateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		t.Error("golden snapshot not disabled")
	}
	if lim := tpl.GetResources().GetLimits(); lim.GetCpuMillicores() != SubstrateDefaultCPUMillicores || lim.GetMemoryBytes() != SubstrateDefaultMemoryBytes {
		t.Errorf("limits = %v", lim)
	}
	if fake.actors[0].GetActorTemplate().GetName() != tpl.GetMetadata().GetName() {
		t.Error("actor does not reference the per-kick template")
	}
	if fake.pushedWorkspace != "/data/agents/sandbox/deps" || fake.pulledWorkspace != "/data/agents/sandbox/deps" {
		t.Errorf("workspace push/pull = %q / %q", fake.pushedWorkspace, fake.pulledWorkspace)
	}
	if fake.runReq.WorkDir != SubstrateWorkspaceMount || strings.Join(fake.runReq.Command, " ") != "crux kick --agent deps" || fake.runReq.Env["HIVE_AGENT"] != "deps" {
		t.Errorf("run request = %+v", fake.runReq)
	}
	// Egress: GitHub hosts plus the operator's, grouped by port, sorted.
	pol := fake.policies[0]
	if pol.GetMetadata().GetName() != "default" || len(pol.GetRules()) != 2 {
		t.Fatalf("policy = %v", pol)
	}
	r443 := pol.GetRules()[0].GetTlsPassthrough()
	if strings.Join(r443.GetHostnames(), ",") != "api.github.com,github.com,litellm.example.com" || r443.GetPorts().GetNumbers()[0] != 443 {
		t.Errorf("443 rule = %v", r443)
	}
	r8443 := pol.GetRules()[1].GetTlsPassthrough()
	if strings.Join(r8443.GetHostnames(), ",") != "*.pypi.org" || r8443.GetPorts().GetNumbers()[0] != 8443 {
		t.Errorf("8443 rule = %v", r8443)
	}
	if fake.cleanupCtxDead {
		t.Error("cleanup ran on a dead context although the caller's was live")
	}
}

func TestSubstrateRunHonoursSpecOverrides(t *testing.T) {
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	l.Options.Image = ""
	l.Options.Command = []string{"/opt/bridge"}
	l.Options.CPUMillicores = 500
	l.Options.MemoryBytes = 1 << 30
	l.Options.BridgePort = 9000
	l.Options.Timeout = time.Minute
	spec := substrateTestSpec()
	spec.Image = substrateTestImage
	spec.WorkDir = "/workspace/sub"
	spec.NetworkNone = true
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := fake.templates[0].GetContainers()[0]
	if c.GetImage() != substrateTestImage || c.GetCommand()[0] != "/opt/bridge" || c.GetEnv()[0].GetValue() != "9000" {
		t.Errorf("container = %v", c)
	}
	if lim := fake.templates[0].GetResources().GetLimits(); lim.GetCpuMillicores() != 500 || lim.GetMemoryBytes() != 1<<30 {
		t.Errorf("limits = %v", lim)
	}
	if fake.templates[0].GetWorkerSelector() != nil {
		t.Error("empty worker selector must be nil")
	}
	if len(fake.policies[0].GetRules()) != 0 {
		t.Errorf("network_none must send an empty policy, got %v", fake.policies[0].GetRules())
	}
	if fake.runReq.WorkDir != "/workspace/sub" {
		t.Errorf("workdir = %q", fake.runReq.WorkDir)
	}
}

func TestSubstrateRunRefusesClineProvider(t *testing.T) {
	spec, _ := clineLaunchFixture(t)
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	spec.Workspace = "/data/agents/sandbox/deps"
	if _, err := l.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "Podman-only") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Error("control plane touched despite the Cline provider refusal")
	}
}

func TestSubstrateRunRejectsBadOptionsAndSpec(t *testing.T) {
	cases := []struct {
		name string
		mut  func(l *SubstrateLauncher, spec *LaunchSpec)
		want string
	}{
		{"bad endpoint", func(l *SubstrateLauncher, _ *LaunchSpec) { l.Options.Endpoint = "ftp://x" }, "unsupported scheme"},
		{"tag image in options", func(l *SubstrateLauncher, _ *LaunchSpec) { l.Options.Image = "img:latest" }, "pinned by digest"},
		{"no image anywhere", func(l *SubstrateLauncher, s *LaunchSpec) { l.Options.Image = ""; s.Image = "" }, "image is required"},
		{"tag image in spec", func(l *SubstrateLauncher, s *LaunchSpec) { l.Options.Image = ""; s.Image = "img:latest" }, "pinned by digest"},
		{"no workspace", func(_ *SubstrateLauncher, s *LaunchSpec) { s.Workspace = " " }, "workspace is required"},
		{"no command", func(_ *SubstrateLauncher, s *LaunchSpec) { s.Command = nil }, "command is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSubstrate{}
			l := newTestSubstrate(fake)
			spec := substrateTestSpec()
			tc.mut(l, &spec)
			_, err := l.Run(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(fake.calls) != 0 {
				t.Errorf("control plane touched: %v", fake.calls)
			}
		})
	}
}

func TestSubstrateRunClientConstructorError(t *testing.T) {
	l := newTestSubstrate(&fakeSubstrate{})
	l.newClient = func(SubstrateOptions) (substrateClient, error) { return nil, errors.New("no router bundle") }
	if _, err := l.Run(context.Background(), substrateTestSpec()); err == nil || !strings.Contains(err.Error(), "no router bundle") {
		t.Fatalf("err = %v", err)
	}
}

func TestSubstrateRunControlPlaneFailuresCleanUp(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(f *fakeSubstrate)
		want    string
		cleanup []string
	}{
		{"template", func(f *fakeSubstrate) { f.templateErr = errors.New("boom") }, "creating actor template", nil},
		{"actor", func(f *fakeSubstrate) { f.actorErr = errors.New("boom") }, "creating actor hive-kick", []string{"DeleteActorTemplate"}},
		{"policy", func(f *fakeSubstrate) { f.policyErr = errors.New("boom") }, "setting egress policy", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"resume", func(f *fakeSubstrate) { f.resumeErr = errors.New("boom") }, "resuming actor", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"get actor", func(f *fakeSubstrate) { f.getErr = errors.New("boom") }, "never became running", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"crashed", func(f *fakeSubstrate) {
			f.states = []substrateapi.ActorState{substrateapi.ActorState_ACTOR_STATE_CRASHED}
			f.crashMsg = "OOM"
		}, "actor crashed: OOM", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"deleting", func(f *fakeSubstrate) {
			f.states = []substrateapi.ActorState{substrateapi.ActorState_ACTOR_STATE_DELETING}
		}, "entered ACTOR_STATE_DELETING", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"bridge never ready", func(f *fakeSubstrate) { f.waitReadyErr = errors.New("boom") }, "never answered", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"push", func(f *fakeSubstrate) { f.pushErr = errors.New("boom") }, "pushing workspace", []string{"DeleteActor", "DeleteActorTemplate"}},
		{"run transport", func(f *fakeSubstrate) { f.runErr = errors.New("boom") }, "running kick", []string{"Pull", "DeleteActor", "DeleteActorTemplate"}},
		{"run reported", func(f *fakeSubstrate) {
			f.runResp = substratebridge.RunResponse{ExitCode: -1, Error: "exec: not found"}
		}, "exec: not found", []string{"Pull", "DeleteActor", "DeleteActorTemplate"}},
		{"pull", func(f *fakeSubstrate) { f.pullErr = errors.New("boom") }, "pulling workspace", []string{"DeleteActor", "DeleteActorTemplate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSubstrate{}
			tc.mut(fake)
			l := newTestSubstrate(fake)
			_, err := l.Run(context.Background(), substrateTestSpec())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, c := range tc.cleanup {
				if !fake.saw(c) {
					t.Errorf("cleanup %s not called: %v", c, fake.calls)
				}
			}
			if tc.name == "template" && fake.saw("DeleteActorTemplate") {
				t.Error("template deleted although it was never created")
			}
			if !fake.closed {
				t.Error("client not closed")
			}
		})
	}
}

func TestSubstrateRunReportsNonZeroExit(t *testing.T) {
	fake := &fakeSubstrate{runResp: substratebridge.RunResponse{Stdout: "x", ExitCode: 3}}
	res, err := newTestSubstrate(fake).Run(context.Background(), substrateTestSpec())
	if err != nil {
		t.Fatalf("Run: %v (a non-zero exit is a result, not an error)", err)
	}
	if res.ExitCode != 3 || res.Stdout != "x" {
		t.Errorf("result = %+v", res)
	}
}

func TestSubstrateRunSnapshotOnIdle(t *testing.T) {
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fake.saw("SuspendActor") || !fake.saw("mode=DURABILITY_MODE_ASYNCHRONOUS") || fake.saw("DeleteActor") {
		t.Errorf("snapshot_on_idle must suspend asynchronously instead of deleting: %v", fake.calls)
	}
	if !fake.saw("DeleteActorTemplate") {
		t.Error("per-kick template must still be removed")
	}

	// A failed suspend falls back to delete so no actor is left running.
	fake = &fakeSubstrate{suspendErr: errors.New("no snapshot store")}
	l = newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fake.saw("SuspendActor") || !fake.saw("DeleteActor") {
		t.Errorf("suspend failure must fall back to delete: %v", fake.calls)
	}
}

func TestSubstrateSnapshotOnIdleDeletesSupersededActor(t *testing.T) {
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	spec := substrateTestSpec()
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := substrateActorName(spec.Name, l.now())
	if len(fake.deleted) != 0 {
		t.Fatalf("first suspend deleted %v, want nothing", fake.deleted)
	}

	l.now = func() time.Time { return time.Unix(1_700_003_600, 0) }
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second := substrateActorName(spec.Name, l.now())
	if len(fake.deleted) != 1 || fake.deleted[0] != first || first == second {
		t.Errorf("deleted = %v, want only the first actor %s (second %s survives)", fake.deleted, first, second)
	}
}

func TestSubstrateSnapshotOnIdleKeepsOtherAgentsActors(t *testing.T) {
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	other := substrateTestSpec()
	other.Name = "docs-sandbox"
	if _, err := l.Run(context.Background(), other); err != nil {
		t.Fatalf("Run other: %v", err)
	}
	l.now = func() time.Time { return time.Unix(1_700_003_600, 0) }
	if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("deleted %v: one agent's suspend must not delete another's actor", fake.deleted)
	}
}

func TestSubstrateSnapshotOnIdleSkipsSupersededActorInOtherAtespace(t *testing.T) {
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	var logs bytes.Buffer
	l.Options.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	spec := substrateTestSpec()
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := substrateActorName(spec.Name, l.now())

	// The same agent is reconfigured onto another atespace: the launcher's
	// client is for the new one, so the old actor must not be deleted
	// through it (the delete would target a name never created there).
	l.Options.Atespace = "hive-staging"
	l.now = func() time.Time { return time.Unix(1_700_003_600, 0) }
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("deleted = %v, want no delete across atespaces", fake.deleted)
	}
	if out := logs.String(); !strings.Contains(out, first) || !strings.Contains(out, "another atespace") || !strings.Contains(out, "agent="+spec.Name) {
		t.Errorf("log = %q, want a warning naming %s and the agent", out, first)
	}
}

func TestSubstrateSnapshotOnIdleDeleteErrorLogsStructured(t *testing.T) {
	fake := &fakeSubstrate{deleteErr: errors.New("not found")}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	var logs bytes.Buffer
	l.Options.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	for _, unix := range []int64{1_700_000_000, 1_700_003_600} {
		l.now = func() time.Time { return time.Unix(unix, 0) }
		if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "agent=deps-sandbox") || !strings.Contains(out, "error=\"not found\"") {
		t.Errorf("log = %q, want a WARN record with agent and error attrs", out)
	}
}

func TestSubstrateSnapshotOnIdleDeleteErrorDoesNotFailRun(t *testing.T) {
	fake := &fakeSubstrate{deleteErr: errors.New("not found")}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	for _, unix := range []int64{1_700_000_000, 1_700_003_600} {
		l.now = func() time.Time { return time.Unix(unix, 0) }
		if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
			t.Fatalf("Run: %v (a failed GC delete must not fail the kick)", err)
		}
	}
	if len(fake.deleted) != 1 {
		t.Errorf("deleted = %v, want one attempted delete", fake.deleted)
	}
}

func TestSubstrateRunContextCancelledMidKick(t *testing.T) {
	// The kick's context dies while the command runs: Pull is skipped, the
	// actor is deleted on a fresh context, and the caller gets ctx.Err().
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeSubstrate{runCancels: cancel}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true // must not snapshot on a dead context
	_, err := l.Run(ctx, substrateTestSpec())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.saw("Pull") || fake.saw("SuspendActor") {
		t.Errorf("pull or suspend ran after cancel: %v", fake.calls)
	}
	if !fake.saw("DeleteActor") || !fake.saw("DeleteActorTemplate") {
		t.Errorf("cleanup skipped: %v", fake.calls)
	}
	if fake.cleanupCtxDead {
		t.Error("cleanup used the dead caller context instead of a fresh one")
	}
}

func TestSubstrateRunContextCancelledWhileWaitingForRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeSubstrate{getCancels: cancel, states: []substrateapi.ActorState{substrateapi.ActorState_ACTOR_STATE_RESUMING}}
	_, err := newTestSubstrate(fake).Run(ctx, substrateTestSpec())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !fake.saw("DeleteActor") || fake.saw("WaitReady") {
		t.Errorf("calls = %v", fake.calls)
	}
}

func TestSubstrateRunContextCancelledWhileWaitingForBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeSubstrate{waitReadyErr: context.Canceled}
	l := newTestSubstrate(fake)
	// Cancel just before WaitReady by hooking GetActor (the step before).
	fake.getCancels = cancel
	_, err := l.Run(ctx, substrateTestSpec())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.saw("Push") {
		t.Errorf("push ran after cancel: %v", fake.calls)
	}
}

func TestSubstrateWaitRunningPollsUntilRunning(t *testing.T) {
	fake := &fakeSubstrate{states: []substrateapi.ActorState{
		substrateapi.ActorState_ACTOR_STATE_SUSPENDED,
		substrateapi.ActorState_ACTOR_STATE_RESUMING,
		substrateapi.ActorState_ACTOR_STATE_RUNNING,
	}}
	ref := &substrateapi.ObjectRef{Atespace: "hive", Name: "a"}
	if err := substrateWaitRunning(context.Background(), fake, ref, time.Millisecond, nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if n := len(fake.calls); n != 3 {
		t.Errorf("GetActor calls = %d, want 3", n)
	}
	fake = &fakeSubstrate{states: []substrateapi.ActorState{substrateapi.ActorState_ACTOR_STATE_SUSPENDING}}
	if err := substrateWaitRunning(context.Background(), fake, ref, time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "SUSPENDING") {
		t.Errorf("err = %v", err)
	}
}

func TestSubstrateOptionsValidate(t *testing.T) {
	good := SubstrateOptions{Image: substrateTestImage, AllowHosts: []string{"a.example.com", "*.b.example.com:8443"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(good) = %v", err)
	}
	if err := (SubstrateOptions{}).Validate(); err != nil {
		t.Fatalf("Validate(defaults) = %v", err)
	}
	cases := map[string]SubstrateOptions{
		"endpoint no port":      {Endpoint: "api.ate-system.svc"},
		"endpoint bad url":      {Endpoint: "https://"},
		"endpoint scheme":       {Endpoint: "ftp://x:1"},
		"router http":           {RouterURL: "http://router:8444"},
		"router garbage":        {RouterURL: "::not a url"},
		"image tag":             {Image: "img:latest"},
		"negative cpu":          {CPUMillicores: -1},
		"negative mem":          {MemoryBytes: -1},
		"bridge port range":     {BridgePort: 70000},
		"allow host url":        {AllowHosts: []string{"https://x.example.com"}},
		"allow host empty":      {AllowHosts: []string{" "}},
		"allow host userinfo":   {AllowHosts: []string{"u@x.example.com"}},
		"allow host bad port":   {AllowHosts: []string{"x.example.com:0"}},
		"allow host port alpha": {AllowHosts: []string{"x.example.com:abc"}},
		"allow host wildcard":   {AllowHosts: []string{"*x.example.com"}},
	}
	for name, o := range cases {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, o)
		}
	}
}

func TestSubstrateAddressForms(t *testing.T) {
	cases := []struct {
		in   string
		addr string
		tls  bool
	}{
		{"api.ate-system.svc:443", "api.ate-system.svc:443", true},
		{"https://api.ate-system.svc", "api.ate-system.svc:443", true},
		{"grpcs://api:9443", "api:9443", true},
		{"http://localhost:8080", "localhost:8080", false},
		{"http://localhost", "localhost:80", false},
		{"grpc://api:50051", "api:50051", false},
	}
	for _, tc := range cases {
		addr, tls, err := substrateAddress(tc.in)
		if err != nil || addr != tc.addr || tls != tc.tls {
			t.Errorf("substrateAddress(%q) = %q %v %v", tc.in, addr, tls, err)
		}
	}
	if got, err := substrateRouterAddress("https://atenet-router.ate-system.svc"); err != nil || got != "atenet-router.ate-system.svc:443" {
		t.Errorf("router default port: %q %v", got, err)
	}
	if got, err := substrateRouterAddress("https://r:8444"); err != nil || got != "r:8444" {
		t.Errorf("router explicit port: %q %v", got, err)
	}
}

func TestSubstrateHostPort(t *testing.T) {
	host, port, err := substrateHostPort("API.Example.COM")
	if err != nil || host != "api.example.com" || port != 443 {
		t.Errorf("got %q %d %v", host, port, err)
	}
	host, port, err = substrateHostPort("*.pypi.org:8443")
	if err != nil || host != "*.pypi.org" || port != 8443 {
		t.Errorf("got %q %d %v", host, port, err)
	}
	for _, bad := range []string{"", "a b", "x/y", "x.example.com:99999", "-bad.example.com", "x.example.com:"} {
		if _, _, err := substrateHostPort(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSubstrateEgressPolicyDedupesAndErrors(t *testing.T) {
	pol, err := SubstrateEgressPolicy([]string{"github.com", "GitHub.com:443", "x.example.com:8443"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.GetRules()) != 2 || len(pol.GetRules()[0].GetTlsPassthrough().GetHostnames()) != 2 {
		t.Errorf("policy = %v", pol)
	}
	if _, err := SubstrateEgressPolicy([]string{"bad host"}); err == nil {
		t.Error("bad host accepted")
	}
}

func TestSubstrateActorName(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	if got := substrateActorName("Deps_Review-sandbox", at); got != "hive-kick-deps-review-1700000000" {
		t.Errorf("got %q", got)
	}
	if got := substrateActorName("---", at); got != "hive-kick-agent-1700000000" {
		t.Errorf("empty base: %q", got)
	}
	long := substrateActorName(strings.Repeat("a", 80)+"-b", at)
	if len(long) > substrateNameMax || !strings.HasPrefix(long, "hive-kick-aaaa") || !strings.HasSuffix(long, "-1700000000") {
		t.Errorf("long name = %q (%d)", long, len(long))
	}
	trimmed := substrateActorName(strings.Repeat("a", 41)+"-"+strings.Repeat("b", 20), at)
	if strings.Contains(trimmed, "--") {
		t.Errorf("truncation left a dangling dash: %q", trimmed)
	}
}

func TestSubstrateOptionDefaults(t *testing.T) {
	o := SubstrateOptions{}
	if o.endpoint() != SubstrateDefaultEndpoint || o.atespace() != SubstrateDefaultAtespace || o.routerURL() != SubstrateDefaultRouterURL || o.bridgePort() != substratebridge.DefaultPort || o.tokenFile() != SubstrateDefaultTokenFile {
		t.Errorf("defaults = %q %q %q %d %q", o.endpoint(), o.atespace(), o.routerURL(), o.bridgePort(), o.tokenFile())
	}
	o = SubstrateOptions{Endpoint: " e:1 ", Atespace: " a ", RouterURL: " https://r ", BridgePort: 1, TokenFile: " /t "}
	if o.endpoint() != "e:1" || o.atespace() != "a" || o.routerURL() != "https://r" || o.bridgePort() != 1 || o.tokenFile() != "/t" {
		t.Errorf("trimmed = %q %q %q %d %q", o.endpoint(), o.atespace(), o.routerURL(), o.bridgePort(), o.tokenFile())
	}
}

func TestSubstrateEnvFiltersCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	t.Setenv("HIVE_SUBSTRATE_TEST_KEEP", "yes")
	spec := substrateTestSpec()
	spec.EnvAllowlist = []string{"HIVE_SUBSTRATE_TEST_KEEP"}
	env := substrateEnv(spec)
	if env["HIVE_SUBSTRATE_TEST_KEEP"] != "yes" || env["HIVE_AGENT"] != "deps" {
		t.Errorf("allowlisted values missing: %v", env)
	}
	if _, ok := env["GITHUB_TOKEN"]; ok {
		t.Error("GITHUB_TOKEN leaked into the actor env")
	}
}

func TestSubstrateLauncherDefaultClientIsGRPC(t *testing.T) {
	// Without a test hook the launcher builds the real client; with no
	// router bundle on disk that fails before any network I/O.
	l := &SubstrateLauncher{Options: SubstrateOptions{InsecureTLS: true, RouterClientBundle: "/nonexistent/bundle.pem"}}
	if _, err := l.client(); err == nil || !strings.Contains(err.Error(), "router_client_bundle") {
		t.Fatalf("client() = %v", err)
	}
}
