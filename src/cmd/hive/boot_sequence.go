package main

// bootSequence is the ordered phase list main() runs (#7571, step 2). Naming
// it lets a test pin the order — the phases share state through *boot, so
// swapping two of them is a real bug — and lets runBoot be exercised with
// fakes without booting anything.
type bootSequence struct {
	// config runs first and may veto the boot (--version fast path,
	// HIVE_MODE=hub); nothing else runs when it returns false.
	config func(*boot) bool
	// phases run in order; a phase returning false (bootAdvisory when the
	// mutation ledger or journal cannot be opened) ends the boot the same way.
	phases []bootPhase
	// loop blocks until shutdown; it is the last thing main() does, so
	// b.cleanup runs the moment it returns.
	loop func(*boot)
}

// bootPhase is one step of the sequence; false aborts the boot.
type bootPhase func(*boot) bool

// namedPhase pairs a phase with its name so the order is testable even
// though non-veto phases are wrapped in a closure by step.
type namedPhase struct {
	name string
	run  bootPhase
}

// step adapts a phase that cannot veto.
func step(name string, f func(*boot)) namedPhase {
	return namedPhase{name: name, run: func(b *boot) bool { f(b); return true }}
}

// defaultBootPhases is the ordered phase table behind defaultBootSequence.
func defaultBootPhases() []namedPhase {
	return []namedPhase{
		step("bootGitHub", (*boot).bootGitHub),
		step("bootGovernor", (*boot).bootGovernor),
		{name: "bootAdvisory", run: (*boot).bootAdvisory},
		step("bootAgents", (*boot).bootAgents),
		step("bootState", (*boot).bootState),
		step("bootDashboard", (*boot).bootDashboard),
		step("bootStores", (*boot).bootStores),
		step("bootCollectors", (*boot).bootCollectors),
		step("bootKnowledge", (*boot).bootKnowledge),
		step("bootSupervision", (*boot).bootSupervision),
		step("bootDashboardAPI", (*boot).bootDashboardAPI),
		step("bootPolicies", (*boot).bootPolicies),
		step("bootWatchers", (*boot).bootWatchers),
		step("bootProxy", (*boot).bootProxy),
		step("bootLaunch", (*boot).bootLaunch),
		step("bootHeartbeat", (*boot).bootHeartbeat),
		step("bootLanes", (*boot).bootLanes),
	}
}

// defaultBootSequenceNames lists the default phase names in run order.
func defaultBootSequenceNames() []string {
	var names []string
	for _, p := range defaultBootPhases() {
		names = append(names, p.name)
	}
	return names
}

func defaultBootSequence() bootSequence {
	seq := bootSequence{config: (*boot).bootConfig, loop: (*boot).runLoop}
	for _, p := range defaultBootPhases() {
		seq.phases = append(seq.phases, notingPhase(p))
	}
	return seq
}

// bootPhaseFirstEval is the phase name runLoop reports once every boot
// phase has run and the governor is about to publish its first status.
const bootPhaseFirstEval = "firstEval"

// notingPhase wraps a phase so the dashboard learns its name as it starts
// (#15788): a browser reconnecting onto a fresh pod reads startupPhase from
// /api/status and shows "starting: opening stores" instead of a bare
// spinner. Phases before bootDashboard have no server to tell and are
// skipped by noteBootPhase.
func notingPhase(p namedPhase) bootPhase {
	return func(b *boot) bool {
		b.noteBootPhase(p.name)
		return p.run(b)
	}
}

// noteBootPhase records the running boot phase on the dashboard server,
// when one exists yet.
func (b *boot) noteBootPhase(name string) {
	if b == nil || b.dashSrv == nil {
		return
	}
	b.dashSrv.SetStartupPhase(name)
}

// runBoot drives one hive process through seq. b.cleanup runs on every
// return path, including a config veto.
func runBoot(b *boot, seq bootSequence) {
	defer b.cleanup.run()
	if !seq.config(b) {
		return
	}
	for _, phase := range seq.phases {
		if !phase(b) {
			return
		}
	}
	seq.loop(b)
}
