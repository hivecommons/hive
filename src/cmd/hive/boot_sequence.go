package main

// bootSequence is the ordered phase list main() runs (#7571, step 2). Naming
// it lets a test pin the order — the phases share state through *boot, so
// swapping two of them is a real bug — and lets runBoot be exercised with
// fakes without booting anything.
type bootSequence struct {
	// config runs first and may veto the boot (--version fast path,
	// HIVE_MODE=hub); nothing else runs when it returns false.
	config func(*boot) bool
	phases []func(*boot)
	// loop blocks until shutdown; it is the last thing main() does, so
	// b.cleanup runs the moment it returns.
	loop func(*boot)
}

func defaultBootSequence() bootSequence {
	return bootSequence{
		config: (*boot).bootConfig,
		phases: []func(*boot){
			(*boot).bootGitHub,
			(*boot).bootGovernor,
			(*boot).bootAdvisory,
			(*boot).bootAgents,
			(*boot).bootState,
			(*boot).bootDashboard,
			(*boot).bootStores,
			(*boot).bootCollectors,
			(*boot).bootKnowledge,
			(*boot).bootSupervision,
			(*boot).bootDashboardAPI,
			(*boot).bootPolicies,
			(*boot).bootWatchers,
			(*boot).bootProxy,
			(*boot).bootLaunch,
			(*boot).bootHeartbeat,
			(*boot).bootLanes,
		},
		loop: (*boot).runLoop,
	}
}

// runBoot drives one hive process through seq. b.cleanup runs on every
// return path, including a config veto.
func runBoot(b *boot, seq bootSequence) {
	defer b.cleanup.run()
	if !seq.config(b) {
		return
	}
	for _, phase := range seq.phases {
		phase(b)
	}
	seq.loop(b)
}
