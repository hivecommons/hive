package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
)

// sidecarRuntimeFake is a crux.Runtime with the sidecar client's event
// subscription and no crux.Host: what cruxruntime.New returns in sidecar
// mode, as the boot sequence sees it.
type sidecarRuntimeFake struct {
	crux.Runtime
	mu       sync.Mutex
	observer func(api.Event)
}

func newSidecarRuntimeFake() *sidecarRuntimeFake {
	return &sidecarRuntimeFake{Runtime: cruxtest.NewRecorder()}
}

func (f *sidecarRuntimeFake) SetEventObserver(fn func(api.Event)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observer = fn
}

func (f *sidecarRuntimeFake) emit(t *testing.T, typ, agent string, data any) {
	t.Helper()
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	f.mu.Lock()
	fn := f.observer
	f.mu.Unlock()
	if fn == nil {
		t.Fatal("no event observer installed")
	}
	fn(api.Event{Time: time.Now(), Type: typ, Agent: agent, Data: raw})
}

type recordingConditions struct {
	mu       sync.Mutex
	observed []string
	cleared  []string
}

func (r *recordingConditions) Observe(class, key, message, diagnostics string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, class+"/"+key+":"+message+":"+diagnostics)
}

func (r *recordingConditions) Clear(class, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared = append(r.cleared, class+"/"+key)
}

type recordingAudit struct {
	mu      sync.Mutex
	entries []string
}

func (r *recordingAudit) Record(actor, action, agentName string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, actor+"|"+action+"|"+agentName+"|"+fields["outcome"].(string))
}

// hostRuntime hands an in-process Host straight through, wraps a sidecar
// runtime, and refuses anything else: a Host-less runtime in in-process mode
// is a boot error, not a silent downgrade.
func TestHostRuntime_SelectsByModeAndShape(t *testing.T) {
	rec := cruxtest.NewRecorder()
	hr, err := hostRuntime(cruxconfig.RuntimeConfig{Mode: cruxconfig.ModeInProcess}, rec, nil)
	if err != nil || hr != crux.Runtime(rec) {
		t.Fatalf("in-process Host: got (%T, %v), want the recorder itself", hr, err)
	}

	sc := newSidecarRuntimeFake()
	hr, err = hostRuntime(cruxconfig.RuntimeConfig{Mode: cruxconfig.ModeSidecar}, sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hr.(*sidecarHost); !ok {
		t.Fatalf("sidecar runtime wrapped as %T, want *sidecarHost", hr)
	}

	_, err = hostRuntime(cruxconfig.RuntimeConfig{Mode: cruxconfig.ModeInProcess}, sc, nil)
	if !errors.Is(err, cruxruntime.ErrNotInProcess) {
		t.Fatalf("Host-less in-process runtime: err = %v, want ErrNotInProcess", err)
	}
}

// Every Host observer the boot sequence installs fires from the matching
// event on the runtime's stream, with the payload mapped back to the crux
// types; the subscription is installed once, on the first observer.
func TestSidecarHost_ObserversFedFromEventStream(t *testing.T) {
	sc := newSidecarRuntimeFake()
	var log bytes.Buffer
	h := newSidecarHost(sc, slog.New(slog.NewTextHandler(&log, nil)))

	var (
		mu       sync.Mutex
		kicks    []string
		pauses   []crux.PauseTransitionEvent
		relaunch []string
		persist  []string
		prompts  []string
		sandbox  []string
	)
	h.SetKickOutcomeObserver(func(agent string, o crux.KickOutcome) {
		mu.Lock()
		defer mu.Unlock()
		kicks = append(kicks, agent+":"+o.Kind+":"+o.Reason)
	})
	if sc.observer == nil {
		t.Fatal("first observer did not subscribe to the event stream")
	}
	h.SetPauseTransitionObserver(func(ev crux.PauseTransitionEvent) {
		mu.Lock()
		defer mu.Unlock()
		pauses = append(pauses, ev)
	})
	h.SetRelaunchObserver(func(agent string) {
		mu.Lock()
		defer mu.Unlock()
		relaunch = append(relaunch, agent)
	})
	h.SetPersistPauseCallback(func(name string, paused bool) {
		mu.Lock()
		defer mu.Unlock()
		persist = append(persist, name+":"+map[bool]string{true: "paused", false: "resumed"}[paused])
	})
	h.SetRecordPromptCallback(func(agent, trigger, prompt string) {
		mu.Lock()
		defer mu.Unlock()
		prompts = append(prompts, agent+":"+trigger+":"+prompt)
	})
	h.SetSandboxAuditCallback(func(agent, action, detail string) {
		mu.Lock()
		defer mu.Unlock()
		sandbox = append(sandbox, agent+":"+action+":"+detail)
	})
	audit := &recordingAudit{}
	h.SetAuditSink(audit)
	conds := &recordingConditions{}
	h.SetConditionReporter(conds)

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	sc.emit(t, api.EventKickOutcome, "scanner", api.KickOutcome{Kind: "question", Reason: "asked", At: at, KickAt: at})
	sc.emit(t, api.EventPauseTransition, "scanner", api.PauseTransition{Paused: true, Trigger: "hook", Reason: "budget", By: "system", At: at,
		Causation: api.PauseCausation{Depth: 2, HookName: "pre-kick", OriginTransition: "pause"}})
	sc.emit(t, api.EventRelaunch, "scanner", nil)
	sc.emit(t, api.EventPausePersist, "scanner", api.PausePersist{Paused: true})
	sc.emit(t, api.EventPrompt, "scanner", api.Prompt{Trigger: "schedule", Prompt: "go"})
	sc.emit(t, api.EventSandboxAudit, "scanner", api.SandboxAudit{Action: "push", Detail: "main"})
	sc.emit(t, api.EventAudit, "scanner", api.Audit{Actor: "alice", Action: "restart", Fields: map[string]any{"outcome": "ok"}})
	sc.emit(t, api.EventCondition, "scanner", api.ConditionEvent{Action: api.ConditionObserve, Class: "stall", Key: "scanner", Message: "m", Diagnostics: "d"})
	sc.emit(t, api.EventCondition, "scanner", api.ConditionEvent{Action: api.ConditionClear, Class: "stall", Key: "scanner"})
	// Unknown types and undecodable payloads are logged, never fatal.
	sc.emit(t, "agent.state", "scanner", nil)
	sc.observer(api.Event{Type: api.EventPrompt, Agent: "scanner", Data: json.RawMessage(`{bad`)})

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(kicks, ","); got != "scanner:question:asked" {
		t.Errorf("kick outcomes = %q", got)
	}
	if len(pauses) != 1 || pauses[0].Agent != "scanner" || !pauses[0].Paused || pauses[0].Causation.Depth != 2 ||
		pauses[0].Causation.HookName != "pre-kick" || pauses[0].Causation.OriginTransition != "pause" || !pauses[0].At.Equal(at) {
		t.Errorf("pause transitions = %+v", pauses)
	}
	if got := strings.Join(relaunch, ","); got != "scanner" {
		t.Errorf("relaunches = %q", got)
	}
	if got := strings.Join(persist, ","); got != "scanner:paused" {
		t.Errorf("persisted pauses = %q", got)
	}
	if got := strings.Join(prompts, ","); got != "scanner:schedule:go" {
		t.Errorf("prompts = %q", got)
	}
	if got := strings.Join(sandbox, ","); got != "scanner:push:main" {
		t.Errorf("sandbox audits = %q", got)
	}
	audit.mu.Lock()
	if got := strings.Join(audit.entries, ","); got != "alice|restart|scanner|ok" {
		t.Errorf("audit entries = %q", got)
	}
	audit.mu.Unlock()
	conds.mu.Lock()
	if strings.Join(conds.observed, ",") != "stall/scanner:m:d" || strings.Join(conds.cleared, ",") != "stall/scanner" {
		t.Errorf("conditions observed=%v cleared=%v", conds.observed, conds.cleared)
	}
	conds.mu.Unlock()
	if !strings.Contains(log.String(), "undecodable host event") {
		t.Errorf("bad payload not logged:\n%s", log.String())
	}
}

// Observers set to nil stop firing; a runtime without an event stream warns
// once rather than panicking.
func TestSidecarHost_NilObserverAndNoStream(t *testing.T) {
	sc := newSidecarRuntimeFake()
	h := newSidecarHost(sc, nil)
	fired := 0
	h.SetRelaunchObserver(func(string) { fired++ })
	h.SetRelaunchObserver(nil)
	sc.emit(t, api.EventRelaunch, "scanner", nil)
	if fired != 0 {
		t.Fatalf("nil observer fired %d times", fired)
	}

	var log bytes.Buffer
	plain := newSidecarHost(struct{ crux.Runtime }{cruxtest.NewRecorder()}, slog.New(slog.NewTextHandler(&log, nil)))
	plain.SetRelaunchObserver(func(string) {})
	plain.SetKickOutcomeObserver(nil)
	if n := strings.Count(log.String(), "no event stream"); n != 1 {
		t.Fatalf("no-stream warning logged %d times, want 1:\n%s", n, log.String())
	}
}

// Capability and policy setters are inert in sidecar mode and say so once
// per setter: capabilities travel over the callback endpoint, policy is the
// runtime's own configuration until stage 4.
func TestSidecarHost_CapabilityAndPolicySettersLogOnce(t *testing.T) {
	var log bytes.Buffer
	h := newSidecarHost(newSidecarRuntimeFake(), slog.New(slog.NewTextHandler(&log, nil)))
	h.SetAppAuth(nil)
	h.SetAppAuth(nil)
	h.SetAgentMint(nil)
	h.SetBobAPIKeyResolver(nil)
	h.SetBobKeySourceResolver(nil)
	h.SetLinearCredentialResolver(nil)
	h.SetSandboxPushMinter(nil)
	h.SetSandboxPRClient(nil)
	h.SetBuildInfo(nil)
	h.SetGatewayBackendChecker(nil)
	h.SetExplainModeDefaultResolver(nil)
	h.SetRepoAutoMergeEnabledResolver(nil)
	h.SetKickLogSourceResolver(nil)
	h.SetInferenceCallbacks(nil, nil)
	h.SetSandboxConfig(cruxconfig.AgentSandboxConfig{})
	h.SetSandboxMutationBoundary(nil)
	h.SetDeadSessionRecoveryOwner(true)
	h.SetSpanStarter(nil)
	out := log.String()
	if n := strings.Count(out, "setter=SetAppAuth"); n != 1 {
		t.Fatalf("SetAppAuth noted %d times, want once:\n%s", n, out)
	}
	if strings.Count(out, "served by the hive callback") != 4 {
		t.Errorf("callback-carried setters = %d, want 4:\n%s", strings.Count(out, "served by the hive callback"), out)
	}
	if strings.Count(out, "not carried") != 13 {
		t.Errorf("not-carried setters = %d, want 13:\n%s", strings.Count(out, "not carried"), out)
	}
}

// In sidecar mode bootAgents serves the hive callback endpoint before the
// rest of the wiring, with the capability set built from the boot; the
// in-process path never touches it. A callback that cannot bind exits.
func TestBootAgentsWith_SidecarServesHiveCallback(t *testing.T) {
	t.Setenv(cruxconfig.ModeEnvVar, string(cruxconfig.ModeSidecar))
	f := newBootAgentsFake()
	sc := newSidecarRuntimeFake()
	f.deps.newRuntime = func(cfg cruxconfig.RuntimeConfig, _ ...cruxruntime.Option) (hostedRuntime, error) {
		return hostRuntime(cfg, sc, nil)
	}
	var served callback.Capabilities
	f.deps.startHiveCallback = func(_ context.Context, caps callback.Capabilities, _ *slog.Logger) error {
		served = caps
		return nil
	}
	cfg := bootAgentsConfig()
	b := newBootAgentsBoot(t, f, cfg)

	b.bootAgentsWith(f.deps)

	if len(f.exitCodes) != 0 {
		t.Fatalf("exit codes = %v", f.exitCodes)
	}
	if served == nil {
		t.Fatal("hive callback endpoint not served in sidecar mode")
	}
	if f.loopsMgr != b.agentMgr {
		t.Fatal("agent loops not started on the sidecar runtime")
	}
	if sc.observer == nil {
		t.Fatal("boot installed no Host observer on the sidecar runtime")
	}
	if !strings.Contains(f.log.String(), "setter=SetSpanStarter") {
		t.Fatalf("policy setter not noted:\n%s", f.log.String())
	}
	// The Linear resolver is read lazily: bootSupervision sets it later.
	if cred, err := served.LinearCredential(context.Background()); err != nil || !cred.Empty() {
		t.Fatalf("Linear before bootSupervision = (%v, %v), want configured and empty", cred, err)
	}
	b.linearCredentialResolver = func() crux.LinearCredential { return crux.LinearCredential{AccessToken: "tok", APIKey: "key"} }
	if cred, err := served.LinearCredential(context.Background()); err != nil || cred.AccessToken != "tok" || cred.APIKey != "key" {
		t.Fatalf("Linear after bootSupervision = (%v, %v)", cred, err)
	}
}

func TestBootAgentsWith_SidecarCallbackBindFailureExits(t *testing.T) {
	t.Setenv(cruxconfig.ModeEnvVar, string(cruxconfig.ModeSidecar))
	f := newBootAgentsFake()
	f.deps.newRuntime = func(cfg cruxconfig.RuntimeConfig, _ ...cruxruntime.Option) (hostedRuntime, error) {
		return hostRuntime(cfg, newSidecarRuntimeFake(), nil)
	}
	f.deps.startHiveCallback = func(context.Context, callback.Capabilities, *slog.Logger) error {
		return errors.New("address in use")
	}
	b := newBootAgentsBoot(t, f, bootAgentsConfig())

	b.bootAgentsWith(f.deps)

	if len(f.exitCodes) != 1 || f.exitCodes[0] != 1 {
		t.Fatalf("exit codes = %v, want [1]", f.exitCodes)
	}
	if f.loopsMgr != nil {
		t.Fatal("wiring installed after the callback endpoint failed to bind")
	}
	if !strings.Contains(f.log.String(), "hive callback endpoint unavailable") {
		t.Fatalf("log missing the bind error:\n%s", f.log.String())
	}
}

func TestBootAgentsWith_InProcessNeverServesHiveCallback(t *testing.T) {
	f := newBootAgentsFake()
	f.deps.startHiveCallback = func(context.Context, callback.Capabilities, *slog.Logger) error {
		t.Fatal("hive callback served in in-process mode")
		return nil
	}
	b := newBootAgentsBoot(t, f, bootAgentsConfig())
	b.bootAgentsWith(f.deps)
	if len(f.exitCodes) != 0 || b.agentMgr == nil {
		t.Fatalf("in-process boot broken: exits=%v mgr=%v", f.exitCodes, b.agentMgr)
	}
}

// The real startHiveCallback binds the socket named by $CRUX_HIVE_SOCKET and
// serves until the context ends.
func TestStartHiveCallback_BindsConfiguredSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "hcb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "hive.sock")
	t.Setenv(callback.SocketEnvVar, sock)
	t.Setenv(callback.ListenEnvVar, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log bytes.Buffer
	caps := (&boot{}).hiveCallbackCapabilities()
	if err := startHiveCallback(ctx, caps, slog.New(slog.NewTextHandler(&log, nil))); err != nil {
		t.Fatal(err)
	}
	c, err := callback.NewClient(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	if cred, err := c.LinearCredential(ctx); err != nil || !cred.Empty() {
		t.Fatalf("empty boot Linear = (%v, %v), want empty", cred, err)
	}
	if !strings.Contains(log.String(), "hive callback endpoint serving") {
		t.Fatalf("serving not logged:\n%s", log.String())
	}
	if err := startHiveCallback(ctx, caps, slog.Default()); err == nil {
		t.Fatal("second bind on the same socket succeeded")
	}
}
