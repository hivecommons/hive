package agent

import "testing"

// ============================================================
// manager.go — SetPauseTransitionObserver nil branch
//
// Passing nil must CLEAR a previously wired observer (the documented way to
// unhook), not store a nil function pointer that notifyPauseTransition would
// then dereference. Only the non-nil path was covered before this test.
// ============================================================

func TestSetPauseTransitionObserverNilClears(t *testing.T) {
	m := provenanceTestManager(t)

	m.SetPauseTransitionObserver(func(PauseTransitionEvent) {})
	if m.pauseObserver.Load() == nil {
		t.Fatal("observer was not stored")
	}

	m.SetPauseTransitionObserver(nil)
	if m.pauseObserver.Load() != nil {
		t.Fatal("SetPauseTransitionObserver(nil) must clear the stored observer")
	}

	// A pause after clearing must not panic on a nil observer.
	if err := m.PauseBy("scanner", "dashboard-api", "manual pause", "tester"); err != nil {
		t.Fatalf("PauseBy after clearing observer: %v", err)
	}
}
