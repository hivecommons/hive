package cruxtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/watchdog"
)

// TestRecorderCoversRuntime calls every crux.Runtime and crux.Host method by
// reflection with zero arguments and checks each one was recorded under its
// own name, so a new interface method cannot ship without the fake growing
// with it (the Background loops take a cancelled context so they return).
func TestRecorderCoversRuntime(t *testing.T) {
	r := NewRecorder()
	r.Add("alpha", "a1", crux.StateRunning)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, iface := range []reflect.Type{
		reflect.TypeOf((*crux.Runtime)(nil)).Elem(),
		reflect.TypeOf((*crux.Host)(nil)).Elem(),
	} {
		for i := 0; i < iface.NumMethod(); i++ {
			m := iface.Method(i)
			r.Reset()
			fn := reflect.ValueOf(r).MethodByName(m.Name)
			args := make([]reflect.Value, m.Type.NumIn())
			for j := range args {
				in := m.Type.In(j)
				if in == reflect.TypeOf((*context.Context)(nil)).Elem() {
					args[j] = reflect.ValueOf(ctx)
				} else {
					args[j] = reflect.Zero(in)
				}
			}
			fn.Call(args)
			if _, ok := r.Last(m.Name); !ok {
				t.Errorf("%s not recorded; got %v", m.Name, r.Methods())
			}
		}
	}
}

func TestRecorderState(t *testing.T) {
	r := NewRecorder()
	p := r.Add("alpha", "a1", crux.StateStopped)
	p.RestartCount = 3
	r.Pane["alpha"] = []string{"a", "b", "c"}
	r.Buffer["alpha"] = []string{"x", "y"}
	r.FullLog["alpha"] = "log"
	r.Header["alpha"] = "hdr"
	ctx := context.Background()

	if err := r.Start(ctx, "alpha"); err != nil || r.Procs["alpha"].State != crux.StateRunning {
		t.Fatalf("Start: %v %v", err, r.Procs["alpha"].State)
	}
	if err := r.Start(ctx, "nope"); err == nil {
		t.Fatal("Start unknown agent should fail")
	}
	if err := r.Stop("alpha"); err != nil || r.Procs["alpha"].State != crux.StateStopped {
		t.Fatal("Stop")
	}
	if err := r.PauseBy("alpha", "t", "r", "me"); err != nil || !r.IsPaused("alpha") || r.Procs["alpha"].PausedBy != "me" {
		t.Fatal("PauseBy")
	}
	if err := r.PauseByCause("alpha", "t", "r", "me", crux.PauseCausation{}); err != nil {
		t.Fatal("PauseByCause")
	}
	if err := r.Resume(ctx, "alpha", "t", "r"); err != nil || r.IsPaused("alpha") {
		t.Fatal("Resume")
	}
	if r.IsPaused("nope") {
		t.Fatal("unknown agent is not paused")
	}
	if err := r.ResetRestartCount("alpha"); err != nil || r.Procs["alpha"].RestartCount != 0 {
		t.Fatal("ResetRestartCount")
	}
	got, err := r.GetStatus("alpha")
	if err != nil || got.ID != "a1" || len(got.PaneLines(2)) != 2 {
		t.Fatalf("GetStatus: %v %+v", err, got)
	}
	if _, err := r.GetStatus("nope"); err == nil {
		t.Fatal("GetStatus unknown")
	}
	if _, err := r.GetStatusFast("nope"); err == nil {
		t.Fatal("GetStatusFast unknown")
	}
	if fast, err := r.GetStatusFast("alpha"); err != nil || fast.Name != "alpha" {
		t.Fatal("GetStatusFast")
	}
	if all := r.AllStatuses(); len(all) != 1 || all["alpha"] == p {
		t.Fatal("AllStatuses must copy")
	}
	if r.ResolveAgent("a1") != "alpha" || r.ResolveAgent("alpha") != "alpha" || r.ResolveAgent("zz") != "zz" {
		t.Fatal("ResolveAgent")
	}
	if out, err := r.GetOutput("alpha", 2); err != nil || len(out) != 2 || out[0] != "b" {
		t.Fatalf("GetOutput: %v %v", err, out)
	}
	if out, err := r.GetBufferOutput("alpha", 0); err != nil || len(out) != 2 {
		t.Fatal("GetBufferOutput")
	}
	if _, err := r.GetOutput("nope", 1); err == nil {
		t.Fatal("GetOutput unknown")
	}
	if _, err := r.GetBufferOutput("nope", 1); err == nil {
		t.Fatal("GetBufferOutput unknown")
	}
	if l, err := r.CaptureFullLog("alpha"); err != nil || l != "log" || r.FullLogHeader("alpha") != "hdr" {
		t.Fatal("CaptureFullLog")
	}
	if _, err := r.CaptureFullLog("nope"); err == nil {
		t.Fatal("CaptureFullLog unknown")
	}

	// Kicker.
	r.KickStarted = true
	if started, err := r.SendKickAsync("alpha", "m"); err != nil || !started {
		t.Fatal("SendKickAsync")
	}
	if _, err := r.SendKickAsync("nope", "m"); err == nil {
		t.Fatal("SendKickAsync unknown")
	}
	r.KickErr = errors.New("busy")
	if err := r.SendKick("alpha", "m"); err == nil {
		t.Fatal("SendKick KickErr")
	}
	if _, err := r.SendKickAsync("alpha", "m"); err == nil {
		t.Fatal("SendKickAsync KickErr")
	}
	if err := r.ValidateKick("alpha"); err == nil {
		t.Fatal("ValidateKick KickErr")
	}
	r.KickErr = nil
	if err := r.SendKick("alpha", "m"); err != nil || r.ValidateKick("alpha") != nil {
		t.Fatal("SendKick ok")
	}
	r.Dispatch["alpha"] = crux.KickDispatch{}
	if _, ok := r.KickDispatchState("alpha"); !ok {
		t.Fatal("KickDispatchState")
	}
	r.KickLogs["alpha"] = []crux.KickLogInfo{{ID: "k1"}}
	r.KickLogContent["alpha"] = map[string]string{"k1": "body"}
	if logs, err := r.ListKickLogs("alpha"); err != nil || len(logs) != 1 {
		t.Fatal("ListKickLogs")
	}
	if _, err := r.ListKickLogs("nope"); err == nil {
		t.Fatal("ListKickLogs unknown")
	}
	if body, err := r.ReadKickLog("alpha", "k1"); err != nil || body != "body" {
		t.Fatal("ReadKickLog")
	}
	if _, err := r.ReadKickLog("alpha", "k2"); err == nil {
		t.Fatal("ReadKickLog missing id")
	}
	if _, err := r.ReadKickLog("nope", "k1"); err == nil {
		t.Fatal("ReadKickLog unknown agent")
	}
	var kicks []string
	r.SetKickObserver(func(a, e, d string) { kicks = append(kicks, a+e+d) })
	r.EmitKick("alpha", "sent", "ok")
	if len(kicks) != 1 || kicks[0] != "alphasentok" {
		t.Fatal("EmitKick")
	}
	r.SetKickObserver(nil)
	r.EmitKick("alpha", "sent", "ok")

	// Definitions.
	r.AddAgent("beta", r.Procs["alpha"].Config)
	r.AddAgent("beta", r.Procs["alpha"].Config)
	if len(r.Procs) != 2 {
		t.Fatal("AddAgent")
	}
	cfg := r.Procs["beta"].Config
	cfg.Model = "sonnet"
	if err := r.UpdateConfig("beta", cfg); err != nil || r.Procs["beta"].Config.Model != "sonnet" {
		t.Fatal("UpdateConfig")
	}
	if err := r.UpdateConfig("nope", cfg); err == nil {
		t.Fatal("UpdateConfig unknown")
	}
	r.RemoveAgent("beta")
	if len(r.Procs) != 1 {
		t.Fatal("RemoveAgent")
	}
	r.Reconciled = []string{"alpha"}
	if got := r.ReconcileAgents(map[string]config.AgentConfig{"b": {}, "a": {}}); len(got) != 1 {
		t.Fatal("ReconcileAgents")
	}
	if c, _ := r.Last("ReconcileAgents"); !reflect.DeepEqual(c.Args[0], []string{"a", "b"}) {
		t.Fatalf("ReconcileAgents args %v", c.Args)
	}
	if err := r.SetModelOverride("alpha", "m"); err != nil || r.Procs["alpha"].ModelOverride != "m" {
		t.Fatal("SetModelOverride")
	}
	if err := r.SetBackendOverride("alpha", "b"); err != nil || r.Procs["alpha"].BackendOverride != "b" {
		t.Fatal("SetBackendOverride")
	}
	if r.SetModelOverride("nope", "m") == nil || r.SetBackendOverride("nope", "b") == nil {
		t.Fatal("overrides unknown")
	}
	if r.PinCLI("alpha", "1") != nil || r.PinModel("alpha", "m") != nil || r.UnpinCLI("alpha") != nil || r.UnpinModel("alpha") != nil {
		t.Fatal("pins")
	}
	if r.PinCLI("nope", "1") == nil || r.PinModel("nope", "m") == nil || r.UnpinCLI("nope") == nil || r.UnpinModel("nope") == nil {
		t.Fatal("pins unknown")
	}

	// Fleet.
	r.BreakerPaused = []string{"alpha"}
	r.Resumed = []string{"alpha"}
	if got := r.EngageBreaker(); len(got) != 1 || !r.BreakerEngaged {
		t.Fatal("EngageBreaker")
	}
	if eng, paused := r.BreakerState(); !eng || len(paused) != 1 {
		t.Fatal("BreakerState")
	}
	if got := r.ReleaseBreaker(ctx); len(got) != 1 || r.BreakerEngaged {
		t.Fatal("ReleaseBreaker")
	}
	r.SetACMMLevel(4)
	if r.GetACMMLevel() != 4 {
		t.Fatal("ACMM")
	}
	r.RestoreBreaker(true, []string{"x"})
	if eng, paused := r.BreakerState(); !eng || paused[0] != "x" {
		t.Fatal("RestoreBreaker")
	}

	// Credentials.
	r.SetCopilotToken("t1")
	if r.CopilotToken() != "t1" {
		t.Fatal("SetCopilotToken")
	}
	if err := r.ActivateCopilotToken("t2"); err != nil || r.CopilotToken() != "t2" {
		t.Fatal("ActivateCopilotToken")
	}
	if r.SubmitLoginCode("nope", "c") == nil || r.SubmitLoginCode("alpha", "c") != nil {
		t.Fatal("SubmitLoginCode")
	}

	// Resumer.
	r.Handles["alpha"] = crux.ResumeHandle{}
	r.Sessions["alpha"] = "s1"
	r.Missing["alpha"] = true
	if _, ok := r.ResumeHandle("alpha"); !ok {
		t.Fatal("ResumeHandle")
	}
	if id, ok := r.SessionID("alpha"); !ok || id != "s1" || !r.SessionMissing("alpha") {
		t.Fatal("SessionID")
	}
	if r.SendResumeKick("alpha", "m", "s1") != nil || r.SendResumeKick("nope", "m", "s1") == nil {
		t.Fatal("SendResumeKick")
	}
	r.ResumeErr = crux.ErrResumeBusy
	if !errors.Is(r.SendResumeKick("alpha", "m", "s1"), crux.ErrResumeBusy) {
		t.Fatal("ResumeErr")
	}

	// Supervisor odds and ends.
	r.BackendAuth["claude"] = [2]bool{true, true}
	if a, k := r.BackendAuthAvailable("claude"); !a || !k {
		t.Fatal("BackendAuthAvailable")
	}
	if r.RefreshAgentTokenFor(ctx, "nope") == nil || r.RefreshAgentTokenFor(ctx, "alpha") != nil {
		t.Fatal("RefreshAgentTokenFor")
	}

	// Generic Err wins over lookups.
	r.Err = ErrRecorder
	if !errors.Is(r.Start(ctx, "alpha"), ErrRecorder) || !errors.Is(r.ActivateCopilotToken("x"), ErrRecorder) {
		t.Fatal("Err")
	}
	r.Err = nil

	// Host observers are kept and reported.
	r.SetKickOutcomeObserver(func(string, crux.KickOutcome) {})
	r.SetPauseTransitionObserver(func(crux.PauseTransitionEvent) {})
	r.SetRelaunchObserver(func(string) {})
	r.SetPersistPauseCallback(func(string, bool) {})
	r.SetRecordPromptCallback(func(string, string, string) {})
	r.SetSandboxAuditCallback(func(string, string, string) {})
	ko, po, ro, pp, rp, sa, _, _ := r.Observers()
	if ko == nil || po == nil || ro == nil || pp == nil || rp == nil || sa == nil {
		t.Fatal("Observers")
	}

	// Background loops return when ctx ends and record their start.
	bctx, cancel := context.WithCancel(ctx)
	cancel()
	r.StartAgentTokenRefresh(bctx)
	if got := r.Background(); len(got) != 1 || got[0] != "StartAgentTokenRefresh" {
		t.Fatal("background")
	}

	// Watchdog source.
	r.WatchdogNames = []string{"alpha"}
	r.Observations["alpha"] = watchdog.Observation{}
	r.Production["alpha"] = time.Unix(1, 0)
	src := r.WatchdogSource()
	if len(src.AgentNames()) != 1 {
		t.Fatal("AgentNames")
	}
	if _, err := src.Observe("alpha"); err != nil {
		t.Fatal("Observe")
	}
	if _, err := src.Observe("nope"); err == nil {
		t.Fatal("Observe unknown")
	}
	if ts, ok := src.LastProduction("alpha"); !ok || !ts.Equal(time.Unix(1, 0)) {
		t.Fatal("LastProduction")
	}
	src.SetConditions("alpha", []crux.Condition{{Type: "Ready"}})
	if len(r.SetConds["alpha"]) != 1 {
		t.Fatal("SetConditions")
	}

	if c, ok := r.Last("SetConditions"); !ok || c.String() == "" {
		t.Fatal("Last/String")
	}
	if _, ok := r.Last("Never"); ok {
		t.Fatal("Last missing")
	}
}
