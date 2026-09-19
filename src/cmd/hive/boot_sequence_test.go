package main

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
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
	var got []string
	for _, p := range seq.phases {
		got = append(got, funcName(p))
	}
	want := []string{
		"bootGitHub", "bootGovernor", "bootAdvisory", "bootAgents", "bootState",
		"bootDashboard", "bootStores", "bootCollectors", "bootKnowledge",
		"bootSupervision", "bootDashboardAPI", "bootPolicies", "bootWatchers",
		"bootProxy", "bootLaunch", "bootHeartbeat", "bootLanes",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase order:\n got %v\nwant %v", got, want)
	}
}

func TestRunBootRunsPhasesInOrderThenLoopThenCleanup(t *testing.T) {
	var trace []string
	b := &boot{}
	b.cleanup.push(func() { trace = append(trace, "cleanup") })
	seq := bootSequence{
		config: func(*boot) bool { trace = append(trace, "config"); return true },
		phases: []func(*boot){
			func(*boot) { trace = append(trace, "p1") },
			func(*boot) { trace = append(trace, "p2") },
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
		phases: []func(*boot){func(*boot) { t.Fatal("phase must not run after veto") }},
		loop:   func(*boot) { t.Fatal("loop must not run after veto") },
	}
	runBoot(b, seq)
	if got := strings.Join(trace, ","); got != "config,cleanup" {
		t.Fatalf("trace = %s", got)
	}
}
