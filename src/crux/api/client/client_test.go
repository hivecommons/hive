package client

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
)

const testToken = "client-token"

// servedBy names the runtime method the server calls for a client method
// whose route is shared: IsPaused reads the agent view the GET agent route
// builds from GetStatus.
var servedBy = map[string]string{"IsPaused": "GetStatus"}

// localOnly is the set of crux.Runtime methods that never cross the wire:
// the Background loops run in the serving process, and SetKickObserver
// subscribes to the event stream rather than calling a route.
var localOnly = map[string]bool{
	"StartAgentTokenRefresh":         true,
	"StartCredentialWatchdog":        true,
	"StartCopilotSessionRefresh":     true,
	"StartTranscriptActivityRefresh": true,
	"SetKickObserver":                true,
}

func runtimeMethods(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf((*crux.Runtime)(nil)).Elem()
	names := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		names = append(names, typ.Method(i).Name)
	}
	return names
}

// TestParityInterfaceVersusRouteTable holds crux.Runtime and
// api.RuntimeRoutes to each other in both directions.
func TestParityInterfaceVersusRouteTable(t *testing.T) {
	methods := runtimeMethods(t)
	for _, m := range methods {
		if _, ok := api.RuntimeRoutes[m]; !ok {
			t.Errorf("crux.Runtime.%s has no entry in api.RuntimeRoutes", m)
		}
	}
	set := map[string]bool{}
	for _, m := range methods {
		set[m] = true
	}
	for m := range api.RuntimeRoutes {
		if !set[m] {
			t.Errorf("api.RuntimeRoutes[%q] names no crux.Runtime method", m)
		}
	}
	// Runtime's method set is exactly the interface: no extra wire methods
	// hide behind the type.
	rtType := reflect.TypeOf((*Runtime)(nil))
	for _, m := range methods {
		if _, ok := rtType.MethodByName(m); !ok {
			t.Errorf("*client.Runtime lacks %s", m)
		}
	}
}

// TestParityHostVersusSetterTable holds crux.Host and api.HostSetters to
// each other: every setter has a disposition (route | callback | event |
// config | in-process-only) and every row names a setter. Callback rows
// must name a callback endpoint route the callback package registers,
// event rows an event type the runtime emits.
func TestParityHostVersusSetterTable(t *testing.T) {
	typ := reflect.TypeOf((*crux.Host)(nil)).Elem()
	setters := map[string]bool{}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !strings.HasPrefix(name, "Set") {
			continue
		}
		setters[name] = true
		if _, ok := api.HostSetters[name]; !ok {
			t.Errorf("crux.Host.%s has no entry in api.HostSetters: say how a sidecar runtime is served", name)
		}
	}
	callbackPaths := map[string]bool{
		callback.PathTokens: true, callback.PathPolicy: true, callback.PathRepoAutoMerge: true,
		callback.PathLinearCredential: true, callback.PathPulls: true, callback.PathConditions: true,
		callback.PathInferenceRoutes + "/{agent}": true, callback.PathEffects: true, callback.PathEffects + "/{id}/end": true,
		callback.PathAgentMint: true, callback.PathBobCredential: true,
	}
	events := map[string]bool{api.EventKickOutcome: true, api.EventPauseTransition: true, api.EventRelaunch: true, api.EventPausePersist: true, api.EventPrompt: true, api.EventSandboxAudit: true, api.EventAudit: true, api.EventCondition: true}
	for name, row := range api.HostSetters {
		if !setters[name] {
			t.Errorf("api.HostSetters[%q] names no crux.Host setter", name)
		}
		if row.Via == "" {
			t.Errorf("api.HostSetters[%q] has no Via", name)
		}
		switch row.Kind {
		case api.HostSetterCallback:
			for _, part := range strings.Split(row.Via, ", ") {
				fields := strings.Fields(part)
				if len(fields) != 2 || !callbackPaths[fields[1]] {
					t.Errorf("api.HostSetters[%q] names unknown callback route %q", name, part)
				}
			}
		case api.HostSetterEvent:
			if !events[strings.Split(row.Via, ",")[0]] {
				t.Errorf("api.HostSetters[%q] names unknown event %q", name, row.Via)
			}
		case api.HostSetterRoute:
			found := false
			for _, r := range api.RuntimeRoutes {
				if r.Method+" "+r.Path == row.Via {
					found = true
				}
			}
			if !found {
				t.Errorf("api.HostSetters[%q] names unknown runtime route %q", name, row.Via)
			}
		case api.HostSetterConfig, api.HostSetterInProcessOnly:
		default:
			t.Errorf("api.HostSetters[%q] has unknown kind %q", name, row.Kind)
		}
	}
	if api.HostSetters["SetSpanStarter"].Kind != api.HostSetterInProcessOnly {
		t.Error("SetSpanStarter is the one in-process-only setter")
	}
}

type harness struct {
	rec *cruxtest.Recorder
	srv *serve.Server
	ts  *httptest.Server
	rt  *Runtime
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec := cruxtest.NewRecorder()
	srv := serve.New(rec)
	srv.Token = testToken
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	rt, err := New(ts.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	rt.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	return &harness{rec: rec, srv: srv, ts: ts, rt: rt}
}

// TestEveryMethodReachesTheRuntime drives each crux.Runtime method through
// the client into a serve.Server over a Recorder and checks the same method
// was recorded on the far side.
func TestEveryMethodReachesTheRuntime(t *testing.T) {
	h := newHarness(t)
	rec, rt := h.rec, h.rt
	p := rec.Add("alpha", "a1", crux.StateRunning)
	p.Paused = true
	rec.Pane["alpha"] = []string{"l1", "l2", "l3"}
	rec.Buffer["alpha"] = []string{"b1"}
	rec.FullLog["alpha"] = "full"
	rec.Header["alpha"] = "hdr"
	rec.KickStarted = true
	rec.Dispatch = map[string]crux.KickDispatch{"alpha": {Agent: "alpha", Phase: "queued"}}
	rec.KickLogs = map[string][]crux.KickLogInfo{"alpha": {{ID: "k1", Reason: "r"}}}
	rec.KickLogContent = map[string]map[string]string{"alpha": {"k1": "content"}}
	rec.Reconciled = []string{"alpha"}
	rec.BreakerEngaged = true
	rec.BreakerPaused = []string{"alpha"}
	rec.Resumed = []string{"alpha"}
	rec.ACMM = 3
	rec.Shared = true
	rec.Relaunched = []string{"alpha"}
	rec.Crashed = []string{"alpha"}
	rec.Wedged = []string{"alpha"}
	rec.WithModel = 4
	rec.Copilot = "ghu"
	rec.CopilotSource = "env"
	rec.ClaudePaths = []string{"/a", "/b"}
	rec.OmpHomes = []string{"/h/a"}
	rec.Handles = map[string]crux.ResumeHandle{"alpha": {Backend: "claude", SessionID: "s1"}}
	rec.Sessions = map[string]string{"alpha": "s1"}
	rec.Missing = map[string]bool{"alpha": true}
	rec.MayStart = true
	rec.Refreshable, rec.RefreshKnown = true, true
	rec.Valid = true
	rec.Caps = [4]bool{true, false, true, true}
	rec.Backend, rec.BackendOK = "codex", true
	rec.Invocation, rec.InvocationOK = [3]string{"codex", "gpt", "high"}, true
	rec.Restarts.Total, rec.Restarts.Last24h, rec.Restarts.LastReason, rec.Restarts.OK = 5, 2, "crash", true
	rec.StartFailure, rec.StartFailureOK = crux.StartFailureSnapshot{Reason: "boom", Count: 2, Blocked: true}, true
	rec.Backoff.Remaining, rec.Backoff.Provider, rec.Backoff.Reason, rec.Backoff.OK = 3*time.Second, "anthropic", "429", true
	rec.OOM = true
	rec.AuthAvail, rec.AuthKnown = true, true
	rec.BackendAuth = map[string][2]bool{"claude": {true, true}}
	rec.Views = []trajectory.AgentView{{Name: "alpha", Running: true}}

	ctx := context.Background()
	check := func(name string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v want %#v", name, got, want)
		}
	}
	must := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Lifecycle
	must("Start", rt.Start(ctx, "alpha"))
	must("Stop", rt.Stop("alpha"))
	must("Restart", rt.Restart(ctx, "alpha"))
	must("RestartWithBootstrap", rt.RestartWithBootstrap(ctx, "alpha", "hello"))
	must("Pause", rt.Pause("alpha", "op", "why"))
	must("PauseBy", rt.PauseBy("alpha", "op", "why", "me"))
	must("PauseByCause", rt.PauseByCause("alpha", "op", "why", "me", crux.PauseCausation{Depth: 1, HookName: "h"}))
	check("IsPaused", rt.IsPaused("alpha"), true)
	must("Resume", rt.Resume(ctx, "alpha", "op", "why"))
	check("IsPaused.resumed", rt.IsPaused("alpha"), false)
	must("ResetRestartCount", rt.ResetRestartCount("alpha"))

	// Observer
	st, err := rt.GetStatus("alpha")
	must("GetStatus", err)
	check("GetStatus.Name", st.Name, "alpha")
	check("GetStatus.Pane", st.PaneLines(2), []string{"l2", "l3"})
	stf, err := rt.GetStatusFast("alpha")
	must("GetStatusFast", err)
	check("GetStatusFast.ID", stf.ID, "a1")
	all := rt.AllStatuses()
	check("AllStatuses.len", len(all), 1)
	check("ResolveAgent", rt.ResolveAgent("a1"), "alpha")
	check("ResolveAgent.unknown", rt.ResolveAgent("nope"), "nope")
	lines, err := rt.GetOutput("alpha", 2)
	must("GetOutput", err)
	check("GetOutput", lines, []string{"l2", "l3"})
	buf, err := rt.GetBufferOutput("alpha", 0)
	must("GetBufferOutput", err)
	check("GetBufferOutput", buf, []string{"b1"})
	full, err := rt.CaptureFullLog("alpha")
	must("CaptureFullLog", err)
	check("CaptureFullLog", full, "full")
	check("FullLogHeader", rt.FullLogHeader("alpha"), "hdr")

	// Kicker
	must("SendKick", rt.SendKick("alpha", "go"))
	must("SendKickWithSource", rt.SendKickWithSource("alpha", "go", "governor-cadence"))
	started, err := rt.SendKickAsync("alpha", "go")
	must("SendKickAsync", err)
	check("SendKickAsync", started, true)
	must("ValidateKick", rt.ValidateKick("alpha"))
	d, ok := rt.KickDispatchState("alpha")
	check("KickDispatchState.ok", ok, true)
	check("KickDispatchState.phase", d.Phase, "queued")
	kl, err := rt.ListKickLogs("alpha")
	must("ListKickLogs", err)
	check("ListKickLogs", kl, []crux.KickLogInfo{{ID: "k1", Reason: "r"}})
	kc, err := rt.ReadKickLog("alpha", "k1")
	must("ReadKickLog", err)
	check("ReadKickLog", kc, "content")

	// Definitions
	rt.AddAgent("beta", config.AgentConfig{ID: "b1", Backend: "claude", Enabled: true})
	rt.RemoveAgent("beta")
	rt.RemoveAgent("never-there")
	must("UpdateConfig", rt.UpdateConfig("alpha", config.AgentConfig{ID: "a1", Backend: "claude", Enabled: true}))
	check("ReconcileAgents", rt.ReconcileAgents(map[string]config.AgentConfig{"alpha": {ID: "a1", Backend: "claude", Enabled: true}}), []string{"alpha"})
	must("SetModelOverride", rt.SetModelOverride("alpha", "opus"))
	must("SetBackendOverride", rt.SetBackendOverride("alpha", "codex"))
	must("PinCLI", rt.PinCLI("alpha", "1.2.3"))
	must("PinModel", rt.PinModel("alpha", "opus"))
	must("UnpinCLI", rt.UnpinCLI("alpha"))
	must("UnpinModel", rt.UnpinModel("alpha"))
	rt.ClearModeOverrides([]string{"alpha"})
	rt.SyncModeFiles(2)
	rt.RefreshInferenceRoutes("claude")

	// Fleet
	eng, paused := rt.BreakerState()
	check("BreakerState", []any{eng, paused}, []any{true, []string{"alpha"}})
	check("EngageBreaker", rt.EngageBreaker(), []string{"alpha"})
	check("ReleaseBreaker", rt.ReleaseBreaker(ctx), []string{"alpha"})
	rt.SetACMMLevel(2)
	check("GetACMMLevel", rt.GetACMMLevel(), 2)
	check("RelaunchBobAgentsAwaitingKey", rt.RelaunchBobAgentsAwaitingKey(ctx), []string{"alpha"})
	check("SharedDevUID", rt.SharedDevUID(), true)

	// Credentials
	must("SubmitLoginCode", rt.SubmitLoginCode("alpha", "1234"))
	check("CopilotToken", rt.CopilotToken(), "ghu")
	check("CopilotTokenSource", rt.CopilotTokenSource(), "env")
	rt.SetCopilotToken("ghu2")
	must("ActivateCopilotToken", rt.ActivateCopilotToken("ghu3"))
	rt.ReloadClaudeToken()
	check("ClaudeCredentialCandidatePaths", rt.ClaudeCredentialCandidatePaths(), []string{"/a", "/b"})
	check("OmpAgentHomes", rt.OmpAgentHomes(), []string{"/h/a"})

	// Resumer
	hd, ok := rt.ResumeHandle("alpha")
	check("ResumeHandle.ok", ok, true)
	check("ResumeHandle", hd.SessionID, "s1")
	must("SendResumeKick", rt.SendResumeKick("alpha", "msg", "s1"))
	sid, ok := rt.SessionID("alpha")
	check("SessionID", []any{sid, ok}, []any{"s1", true})
	check("SessionMissing", rt.SessionMissing("alpha"), true)

	// Supervisor
	check("GovernorMayStart", rt.GovernorMayStart("alpha"), true)
	rt.MarkStartupLaunchQueued([]string{"alpha"})
	check("CheckAndRestartCrashedAgents", rt.CheckAndRestartCrashedAgents(ctx), []string{"alpha"})
	check("ConsentWedgedAgents", rt.ConsentWedgedAgents(), []string{"alpha"})
	rt.RefreshAgentTokens(ctx)
	rf, known := rt.AgentCredentialRefreshable("alpha")
	check("AgentCredentialRefreshable", []bool{rf, known}, []bool{true, true})
	c1, c2, c3, cok := rt.AgentCapabilities("alpha")
	check("AgentCapabilities", []bool{c1, c2, c3, cok}, []bool{true, false, true, true})
	be, ok := rt.EffectiveBackend("alpha")
	check("EffectiveBackend", []any{be, ok}, []any{"codex", true})
	ib, im, ie, iok := rt.InvocationMetadata("alpha")
	check("InvocationMetadata", []any{ib, im, ie, iok}, []any{"codex", "gpt", "high", true})
	tot, last, _, reason, rok := rt.RestartTelemetry("alpha")
	check("RestartTelemetry", []any{tot, last, reason, rok}, []any{5, 2, "crash", true})
	sf, ok := rt.StartFailureState("alpha")
	check("StartFailureState", []any{sf.Reason, sf.Count, sf.Blocked, ok}, []any{"boom", 2, true, true})
	rem, prov, breason, bok := rt.ProviderErrorBackoffRemaining("alpha")
	check("ProviderErrorBackoffRemaining", []any{rem, prov, breason, bok}, []any{3 * time.Second, "anthropic", "429", true})
	rt.ArchiveAllKickLogs("rotate")
	check("CountAgentsWithModel", rt.CountAgentsWithModel(), 4)
	check("CrashOOMSuspected", rt.CrashOOMSuspected("alpha"), true)
	av, ak := rt.BackendAuthAvailable("claude")
	check("BackendAuthAvailable", []bool{av, ak}, []bool{true, true})
	aav, aak := rt.AgentAuthAvailable("alpha")
	check("AgentAuthAvailable", []bool{aav, aak}, []bool{true, true})
	rt.SendGitHubAPIBudgetNudge("alpha", "slow down")
	rt.RecordAudit("act", "alpha", map[string]any{"k": "v"})
	check("AgentHasValidCredential", rt.AgentHasValidCredential("alpha"), true)
	must("RefreshAgentTokenFor", rt.RefreshAgentTokenFor(ctx, "alpha"))
	check("TrajectoryAgents", rt.TrajectoryAgents(), []trajectory.AgentView{{Name: "alpha", Running: true}})

	// Restorer
	rt.SeedPauseState("alpha", time.Now(), "op", "why", "me")
	rt.SeedRestartTelemetry("alpha", 1, []crux.RestartEvent{{At: time.Now(), Reason: "r"}}, "r")
	rt.SeedTurnLoss("alpha", crux.TurnLoss{})
	rt.SeedLastKick("alpha", time.Now())
	rt.SeedKickHistory("alpha", []crux.KickRecord{{Agent: "alpha", Snippet: "s"}})
	rt.RestoreBreaker(true, []string{"alpha"})

	// Background: local no-ops.
	rt.StartAgentTokenRefresh(ctx)
	rt.StartCredentialWatchdog(ctx)
	rt.StartCopilotSessionRefresh(ctx)
	rt.StartTranscriptActivityRefresh(ctx)

	// Authorizer
	must("AuthorizePROpen", rt.AuthorizePROpen("alpha", 1000))
	must("AuthorizePushBranch", rt.AuthorizePushBranch("alpha", 1000))
	must("AuthorizeIssueOpen", rt.AuthorizeIssueOpen("alpha", 1000, "bug"))
	must("AuthorizeMerge", rt.AuthorizeMerge("alpha", 1000, "o/r"))
	must("AuthorizeReviewRequest", rt.AuthorizeReviewRequest("alpha", 1000))

	seen := map[string]bool{}
	for _, m := range rec.Methods() {
		seen[m] = true
	}
	var missing []string
	for _, m := range runtimeMethods(t) {
		if localOnly[m] {
			continue
		}
		if alias := servedBy[m]; alias != "" {
			m = alias
		}
		if !seen[m] {
			missing = append(missing, m)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("methods never reached the runtime: %s", strings.Join(missing, ", "))
	}
	for _, m := range []string{"StartAgentTokenRefresh", "StartCredentialWatchdog", "StartCopilotSessionRefresh", "StartTranscriptActivityRefresh"} {
		if seen[m] {
			t.Errorf("%s crossed the wire; Background loops are server-side", m)
		}
	}
}

func TestResumeErrorsMapToSentinels(t *testing.T) {
	h := newHarness(t)
	h.rec.Add("alpha", "a1", crux.StateRunning)
	for _, tc := range []struct{ in, want error }{
		{crux.ErrResumeBusy, crux.ErrResumeBusy},
		{crux.ErrResumeSessionGone, crux.ErrResumeSessionGone},
		{crux.ErrResumeUnsupported, crux.ErrResumeUnsupported},
	} {
		h.rec.ResumeErr = tc.in
		if err := h.rt.SendResumeKick("alpha", "m", "s"); !errors.Is(err, tc.want) {
			t.Errorf("%v: got %v", tc.in, err)
		}
	}
	h.rec.ResumeErr = errors.New("other")
	err := h.rt.SendResumeKick("alpha", "m", "s")
	var se *api.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
		t.Errorf("plain error: got %v", err)
	}
	h.rec.ResumeErr = nil
	if _, ok := h.rt.ResumeHandle("ghost"); ok {
		t.Error("ResumeHandle for unknown agent reported ok")
	}
	if _, ok := h.rt.SessionID("ghost"); ok {
		t.Error("SessionID for unknown agent reported ok")
	}
}

func TestAuthorizerFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.rec.AuthorizeErr = errors.New("tier too low")
	err := h.rt.AuthorizePROpen("alpha", 1000)
	if err == nil || err.Error() != "tier too low" {
		t.Errorf("403 reason not surfaced: %v", err)
	}
	h.ts.Close()
	if err := h.rt.AuthorizeMerge("alpha", 1000, "o/r"); err == nil {
		t.Error("unreachable server must deny")
	}
}

// TestUnreachableServerAnswersZero: every no-error read answers its zero
// value and unknown rather than panicking or guessing.
func TestUnreachableServerAnswersZero(t *testing.T) {
	rt, err := New("127.0.0.1:1", "tok")
	if err != nil {
		t.Fatal(err)
	}
	rt.Timeout = 2 * time.Second
	// Writes with an error return refuse with crux.ErrRuntimeUnavailable
	// (hivecommons/hive#14138 stage 4: kicks are not queued) and the
	// client's Status says the runtime is gone.
	if err := rt.SendKick("a", "m"); !errors.Is(err, crux.ErrRuntimeUnavailable) || !errors.Is(err, api.ErrUnreachable) {
		t.Fatalf("SendKick on dead runtime = %v", err)
	}
	if st := rt.Status(); st.Reachable || st.FailedCalls == 0 || st.LastError == "" {
		t.Fatalf("Status after failures = %+v", st)
	}
	if err := rt.Stop("a"); !errors.Is(err, crux.ErrRuntimeUnavailable) {
		t.Fatalf("Stop = %v", err)
	}
	if err := rt.Resume(context.Background(), "a", "t", "r"); !errors.Is(err, crux.ErrRuntimeUnavailable) {
		t.Fatalf("Resume = %v", err)
	}
	if rt.IsPaused("a") || rt.AllStatuses() != nil || rt.ResolveAgent("a") != "a" || rt.FullLogHeader("a") != "" {
		t.Error("observer reads must be zero")
	}
	if _, ok := rt.KickDispatchState("a"); ok {
		t.Error("dispatch ok")
	}
	if rt.ReconcileAgents(map[string]config.AgentConfig{"a": {}}) != nil {
		t.Error("reconcile")
	}
	if e, p := rt.BreakerState(); e || p != nil {
		t.Error("breaker")
	}
	if rt.EngageBreaker() != nil || rt.GetACMMLevel() != 0 || rt.SharedDevUID() || rt.CopilotToken() != "" || rt.ClaudeCredentialCandidatePaths() != nil {
		t.Error("fleet/credential reads")
	}
	if rt.GovernorMayStart("a") || rt.ConsentWedgedAgents() != nil || rt.CountAgentsWithModel() != 0 || rt.CrashOOMSuspected("a") || rt.AgentHasValidCredential("a") || rt.TrajectoryAgents() != nil {
		t.Error("supervisor reads")
	}
	if _, known := rt.AgentCredentialRefreshable("a"); known {
		t.Error("refreshable known")
	}
	if _, _, _, ok := rt.AgentCapabilities("a"); ok {
		t.Error("caps ok")
	}
	if _, ok := rt.EffectiveBackend("a"); ok {
		t.Error("backend ok")
	}
	if _, _, _, ok := rt.InvocationMetadata("a"); ok {
		t.Error("invocation ok")
	}
	if _, _, _, _, ok := rt.RestartTelemetry("a"); ok {
		t.Error("restarts ok")
	}
	if _, ok := rt.StartFailureState("a"); ok {
		t.Error("start failure ok")
	}
	if _, _, _, ok := rt.ProviderErrorBackoffRemaining("a"); ok {
		t.Error("backoff ok")
	}
	if _, known := rt.BackendAuthAvailable("claude"); known {
		t.Error("backend auth known")
	}
	if _, known := rt.AgentAuthAvailable("a"); known {
		t.Error("agent auth known")
	}
	if rt.SessionMissing("a") {
		t.Error("session missing")
	}
	// Writes are dropped with a log line, never a panic.
	rt.AddAgent("a", config.AgentConfig{})
	rt.RemoveAgent("a")
	rt.ClearModeOverrides(nil)
	rt.SyncModeFiles(1)
	rt.RefreshInferenceRoutes("x")
	rt.SetACMMLevel(1)
	rt.SetCopilotToken("t")
	rt.ReloadClaudeToken()
	rt.MarkStartupLaunchQueued(nil)
	rt.RefreshAgentTokens(context.Background())
	rt.ArchiveAllKickLogs("r")
	rt.SendGitHubAPIBudgetNudge("a", "m")
	rt.RecordAudit("x", "a", nil)
	rt.SeedLastKick("a", time.Now())
	rt.RestoreBreaker(false, nil)
	src := rt.WatchdogSource()
	if src.AgentNames() != nil {
		t.Error("watchdog names")
	}
	if _, err := src.Observe("a"); err == nil {
		t.Error("observe must error")
	}
	if _, ok := src.LastProduction("a"); ok {
		t.Error("production ok")
	}
	src.SetConditions("a", nil)
}

func TestWatchdogSourceOverTheWire(t *testing.T) {
	h := newHarness(t)
	h.rec.Add("alpha", "a1", crux.StateRunning)
	h.rec.WatchdogNames = []string{"alpha"}
	when := time.Now().Truncate(time.Second)
	h.rec.Observations = map[string]watchdog.Observation{"alpha": {Backend: "claude", SessionExists: true, Pane: "x", AuthKnown: true, LastChange: when}}
	h.rec.Production = map[string]time.Time{"alpha": when}
	src := h.rt.WatchdogSource()
	if got := src.AgentNames(); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("AgentNames %v", got)
	}
	o, err := src.Observe("alpha")
	if err != nil || o.Backend != "claude" || !o.SessionExists || !o.AuthKnown || !o.LastChange.Equal(when) {
		t.Errorf("Observe %+v %v", o, err)
	}
	if _, err := src.Observe("ghost"); err == nil {
		t.Error("Observe unknown must error")
	}
	lp, ok := src.LastProduction("alpha")
	if !ok || !lp.Equal(when) {
		t.Errorf("LastProduction %v %v", lp, ok)
	}
	src.SetConditions("alpha", []crux.Condition{{Type: "Ready", Status: "True", Reason: "ok"}})
	if got := h.rec.SetConds["alpha"]; len(got) != 1 || got[0].Type != "Ready" {
		t.Errorf("SetConditions %+v", got)
	}
	var _ watchdog.Source = src
}

// TestKickObserverRidesTheEventStream runs a real Serve so the kick relay
// is installed, then checks a kick event reaches both the kick observer
// and the general event observer, and that clearing both stops the stream.
func TestKickObserverRidesTheEventStream(t *testing.T) {
	rec := cruxtest.NewRecorder()
	rec.Add("alpha", "a1", crux.StateRunning)
	srv := serve.New(rec)
	srv.PollInterval = 10 * time.Millisecond
	dir, err := os.MkdirTemp("", "crux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := dir + "/crux.sock"
	ls, err := serve.Listen(config.ServeConfig{Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ls) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	rt, err := New("unix://"+socket, "")
	if err != nil {
		t.Fatal(err)
	}
	rt.retry = 10 * time.Millisecond
	if err := rt.Ready(ctx, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var kicks []string
	var events []string
	rt.SetKickObserver(func(agent, event, detail string) {
		mu.Lock()
		kicks = append(kicks, agent+"/"+event+"/"+detail)
		mu.Unlock()
	})
	rt.SetEventObserver(func(ev api.Event) {
		mu.Lock()
		events = append(events, string(ev.Type))
		mu.Unlock()
	})
	// Fire until the subscriber is attached (the stream connects async).
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
poll:
	for {
		rec.EmitKick("alpha", "sent", "ok")
		mu.Lock()
		n := len(kicks)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-tick.C:
		case <-deadline:
			break poll
		}
	}
	mu.Lock()
	gotKick := len(kicks) > 0 && kicks[0] == "alpha/sent/ok"
	gotEvent := false
	for _, e := range events {
		if e == string(api.EventKick) {
			gotEvent = true
		}
	}
	mu.Unlock()
	if !gotKick {
		t.Errorf("kick observer never fired: %v", kicks)
	}
	if !gotEvent {
		t.Errorf("event observer never saw a kick: %v", events)
	}
	rt.SetKickObserver(nil)
	rt.mu.Lock()
	stillRunning := rt.cancelEvents != nil
	rt.mu.Unlock()
	if !stillRunning {
		t.Error("stream stopped while the event observer is still set")
	}
	rt.SetEventObserver(nil)
	rt.mu.Lock()
	stopped := rt.cancelEvents == nil
	rt.mu.Unlock()
	if !stopped {
		t.Error("stream still running after both observers cleared")
	}
	// Re-subscribing restarts it.
	rt.SetKickObserver(func(string, string, string) {})
	rt.mu.Lock()
	restarted := rt.cancelEvents != nil
	rt.mu.Unlock()
	if !restarted {
		t.Error("stream not restarted")
	}
	rt.SetKickObserver(nil)
}

func TestStreamReconnectsAfterDrop(t *testing.T) {
	var mu sync.Mutex
	connects := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connects++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"agent.kick\",\"agent\":\"a\",\"state\":\"sent\"}\n\n"))
	}))
	t.Cleanup(ts.Close)
	rt, err := New(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	rt.retry = 5 * time.Millisecond
	got := make(chan string, 16)
	rt.SetKickObserver(func(agent, event, _ string) { got <- agent + "/" + event })
	for i := 0; i < 2; i++ {
		select {
		case s := <-got:
			if s != "a/sent" {
				t.Errorf("got %s", s)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for relayed kick")
		}
	}
	rt.SetKickObserver(nil)
	mu.Lock()
	n := connects
	mu.Unlock()
	if n < 2 {
		t.Errorf("expected a reconnect, got %d connects", n)
	}
}

func TestConstructors(t *testing.T) {
	if _, err := New("://bad", ""); err == nil {
		t.Error("bad address accepted")
	}
	c, err := api.NewClient("127.0.0.1:1", "t")
	if err != nil {
		t.Fatal(err)
	}
	rt := Wrap(c)
	if rt.Client() != c {
		t.Error("Client() mismatch")
	}
	rt.Logger = nil
	if rt.logger() == nil {
		t.Error("nil Logger must yield a discard logger")
	}
	rt.Timeout = 0
	ctx, cancel := rt.ctx()
	defer cancel()
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > DefaultTimeout {
		t.Error("default timeout not applied")
	}
	if got := tail([]string{"a", "b", "c"}, 0); len(got) != 3 {
		t.Error("tail 0 must keep all")
	}
}

func TestSnapshotDecodeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case api.PathSnapshots:
			_, _ = w.Write([]byte(`{"snapshots":[{"agent":"a","process":"notanobject"}]}`))
		default:
			_, _ = w.Write([]byte(`{"agent":"a","process":"notanobject"}`))
		}
	}))
	t.Cleanup(ts.Close)
	rt, err := New(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.GetStatus("a"); err == nil {
		t.Error("bad snapshot must error")
	}
	if got := rt.AllStatuses(); len(got) != 0 {
		t.Errorf("bad snapshots must be skipped, got %d", len(got))
	}
}
