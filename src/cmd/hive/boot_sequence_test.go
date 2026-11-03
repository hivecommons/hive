package main

import (
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
)

func funcName(f interface{}) string {
	full := runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	return full[strings.LastIndex(full, ".")+1:]
}

func TestDefaultBootSequenceOrder(t *testing.T) {
	seq := defaultBootSequence()
	if got := funcName(seq.config); got != "bootConfig" {
		t.Fatalf("config phase = %s", got)
	}
	if got := funcName(seq.loop); got != "runLoop" {
		t.Fatalf("loop = %s", got)
	}
	want := []string{
		"bootGitHub", "bootGovernor", "bootAdvisory", "bootAgents", "bootState",
		"bootDashboard", "bootStores", "bootCollectors", "bootKnowledge",
		"bootSupervision", "bootDashboardAPI", "bootPolicies", "bootWatchers",
		"bootProxy", "bootLaunch", "bootHeartbeat", "bootLanes",
	}
	if len(seq.phases) != len(want) {
		t.Fatalf("phase count = %d, want %d", len(seq.phases), len(want))
	}
	// step() and notingPhase() wrap every phase in a closure, so names are
	// not recoverable through the func values; the veto-capable bootAdvisory
	// is pinned through the phase table and defaultBootSequenceNames pins
	// the order.
	if got := funcName(defaultBootPhases()[2].run); got != "bootAdvisory" {
		t.Fatalf("phases[2] = %s, want bootAdvisory (the veto-capable phase)", got)
	}
	if got := defaultBootSequenceNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase order:\n got %v\nwant %v", got, want)
	}
}

func TestRunBootRunsPhasesInOrderThenLoopThenCleanup(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return true },
		phases: []bootPhase{
			func(*boot) bool { trace = append(trace, "p1"); return true },
			func(*boot) bool { trace = append(trace, "p2"); return true },
		},
		loop: func(*boot) { trace = append(trace, "loop") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,p1,p2,loop,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}

func TestRunBootConfigVetoSkipsPhasesButRunsCleanup(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return false },
		phases: []bootPhase{func(*boot) bool { t.Fatal("phase must not run after veto"); return true }},
		loop:   func(*boot) { t.Fatal("loop must not run after veto") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}

func TestRunBootPhaseVetoSkipsLaterPhasesAndLoop(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return true },
		phases: []bootPhase{
			func(*boot) bool { trace = append(trace, "p1"); return false },
			func(*boot) bool { t.Fatal("later phase must not run after a veto"); return true },
		},
		loop: func(*boot) { t.Fatal("loop must not run after a phase veto") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,p1,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}

// #15788: each phase reports its name to the dashboard as it starts so a
// browser reconnecting onto a fresh pod can say what it is waiting on; a
// phase before bootDashboard has nowhere to report and must not panic.
func TestNotingPhaseReportsStartupPhaseToDashboard(t *testing.T) {
	b := &boot{}
	ran := false
	phase := notingPhase(namedPhase{name: "bootGitHub", run: func(*boot) bool { ran = true; return false }})
	if phase(b) || !ran {
		t.Fatal("wrapper must run the phase and return its verdict")
	}

	b.dashSrv = dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	notingPhase(namedPhase{name: "bootStores", run: func(*boot) bool { return true }})(b)
	if got := b.dashSrv.StartupPhase(); got != "bootStores" {
		t.Fatalf("StartupPhase = %q, want bootStores", got)
	}
}
