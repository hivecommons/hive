package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The shipping default patterns, resolved the way a real hive resolves them
// (config.Load applies the defaults), so these tests exercise what actually
// runs in production rather than a convenient stand-in that could drift from
// it. defaultLoginPatterns is unexported, and loading is the supported way to
// see the applied set.
func defaultLoginRegexps(t *testing.T) []*regexp.Regexp {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hive.yaml")
	minimal := "project:\n  name: login-scan-test\n  org: kubestellar\n" +
		"github:\n  token: t-not-a-real-token\n" +
		"agents:\n  supervisor:\n    backend: claude\n"
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	patterns := cfg.Governor.Sensing.LoginPatterns
	if len(patterns) == 0 {
		t.Fatal("the default config carries no login patterns — this test would prove nothing")
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			t.Fatalf("default pattern %q does not compile: %v", p, err)
		}
		compiled = append(compiled, re)
	}
	return compiled
}

// loginResiduePane is the shape observed in kubestellar/hive#5291: the pane tail
// still carries login-flow chrome minutes AFTER the operator's /login restored a
// valid credential.
var loginResiduePane = strings.Join([]string{
	"╭──────────────────────────────────────────────╮",
	"│  Please run /login to authenticate            │",
	"╰──────────────────────────────────────────────╯",
	"",
	"Login successful. Press Enter to continue…",
	"",
	"❯ ",
}, "\n")

// TestLoginScanDecision_ValidCredentialNeverPauses is the issue's primary
// acceptance criterion, and the regression for the reported incident: the same
// pane text decides differently depending on whether the credential is good.
func TestLoginScanDecision_ValidCredentialNeverPauses(t *testing.T) {
	compiled := defaultLoginRegexps(t)

	// Valid credential: never a pause, no matter how many cycles see it.
	for _, sightings := range []int{1, 2, 3, 99} {
		action, re := loginScanDecision("claude", loginResiduePane, compiled, true, sightings)
		if action != loginScanDeferAuthenticated {
			t.Fatalf("sightings=%d: got action %v, want defer-authenticated — a valid credential must never be paused", sightings, action)
		}
		if re == nil {
			t.Fatal("the matched pattern must be reported so the log can explain the decision")
		}
	}

	// Same text, credential NOT provable: still paused, exactly as today, once
	// the sighting has persisted.
	if action, _ := loginScanDecision("claude", loginResiduePane, compiled, false, loginPauseMinSightings); action != loginScanPause {
		t.Fatalf("invalid credential + login prompt: got %v, want pause", action)
	}
}

// TestLoginScanDecision_StreakDefersTheFirstSighting covers the secondary
// hardening: one cycle is not evidence. The 3s pane poller already requires
// three consecutive sightings for the far cheaper action of a restart.
func TestLoginScanDecision_StreakDefersTheFirstSighting(t *testing.T) {
	compiled := defaultLoginRegexps(t)
	for s := 1; s < loginPauseMinSightings; s++ {
		if action, _ := loginScanDecision("claude", loginResiduePane, compiled, false, s); action != loginScanDeferStreak {
			t.Fatalf("sightings=%d: got %v, want defer-streak", s, action)
		}
	}
	if action, _ := loginScanDecision("claude", loginResiduePane, compiled, false, loginPauseMinSightings); action != loginScanPause {
		t.Fatalf("sightings=%d: got %v, want pause", loginPauseMinSightings, action)
	}
}

// TestLoginScanDecision_UnrelatedPaneIsIgnored is the control: without it every
// assertion above would also hold for a decision function that never matched.
func TestLoginScanDecision_UnrelatedPaneIsIgnored(t *testing.T) {
	compiled := defaultLoginRegexps(t)
	working := strings.Join([]string{
		"● Opened https://github.com/hivecommons/hive/pull/1234",
		"",
		"✻ Cogitated for 2m 10s",
		"",
		"❯ ",
	}, "\n")
	for _, credentialValid := range []bool{true, false} {
		if action, re := loginScanDecision("claude", working, compiled, credentialValid, 99); action != loginScanIgnore || re != nil {
			t.Fatalf("credentialValid=%v: got (%v,%v), want ignore/nil", credentialValid, action, re)
		}
	}
}

// TestLoginScanDecision_BlockingPromptStandsDown pins the pre-existing modal
// stand-down through the extracted decision, so the refactor cannot drop it.
// Pausing for a folder-trust modal cancels the watcher that would answer it.
func TestLoginScanDecision_BlockingPromptStandsDown(t *testing.T) {
	compiled := defaultLoginRegexps(t)
	trustPane := strings.Join([]string{
		"Do you trust the files in this folder?",
		"Please run /login after continuing",
		"❯ 1. Yes, proceed",
		"  2. No, exit",
	}, "\n")
	if action, _ := loginScanDecision("copilot", trustPane, compiled, false, 99); action != loginScanIgnore {
		t.Fatalf("a startup modal must not be treated as a login problem, got %v", action)
	}
}

// --- the sighting tracker ---------------------------------------------------

func TestLoginSightingTracker_AccumulatesAndResets(t *testing.T) {
	tr := newLoginSightingTracker()

	if got := tr.observe("supervisor", true); got != 1 {
		t.Fatalf("first sighting = %d, want 1", got)
	}
	if got := tr.observe("supervisor", true); got != 2 {
		t.Fatalf("second consecutive sighting = %d, want 2", got)
	}
	// A clean cycle breaks the streak — the whole point of "consecutive".
	if got := tr.observe("supervisor", false); got != 0 {
		t.Fatalf("clean cycle = %d, want 0", got)
	}
	if got := tr.observe("supervisor", true); got != 1 {
		t.Fatalf("after a clean cycle the count must restart, got %d", got)
	}
	// Agents are counted independently.
	if got := tr.observe("quality", true); got != 1 {
		t.Fatalf("a second agent must have its own count, got %d", got)
	}

	tr.forget("supervisor")
	if got := tr.observe("supervisor", true); got != 1 {
		t.Fatalf("forget must clear the count, got %d", got)
	}
}

// TestLoginSightingTracker_RetainDropsAbsentAgents keeps a long-lived process
// from accumulating a map entry per agent that ever existed.
func TestLoginSightingTracker_RetainDropsAbsentAgents(t *testing.T) {
	tr := newLoginSightingTracker()
	tr.observe("supervisor", true)
	tr.observe("quality", true)

	tr.retain(map[string]bool{"supervisor": true})
	if len(tr.streak) != 1 {
		t.Fatalf("retain left %d entries, want 1: %v", len(tr.streak), tr.streak)
	}
	if got := tr.observe("quality", true); got != 1 {
		t.Fatalf("a dropped agent must start fresh, got %d", got)
	}
	if got := tr.observe("supervisor", true); got != 2 {
		t.Fatalf("a retained agent must keep its count, got %d", got)
	}
}

// TestLoginSightingTracker_NilFallsBackToSingleObservation pins what a missing
// tracker means: the streak gate is disabled and the detector behaves as it did
// before #5291. It must NOT mean "never pause".
func TestLoginSightingTracker_NilFallsBackToSingleObservation(t *testing.T) {
	var tr *loginSightingTracker
	if got := tr.observe("supervisor", true); got < loginPauseMinSightings {
		t.Fatalf("a nil tracker must not silently disable pausing, got %d", got)
	}
	if got := tr.observe("supervisor", false); got != 0 {
		t.Fatalf("a clean pane must still read as no sighting, got %d", got)
	}
	// Must not panic.
	tr.forget("supervisor")
	tr.retain(map[string]bool{})
}

// TestLoginScanVerdict_NoMatchAlwaysIgnores pins the invariant the scan loop
// relies on: a non-Ignore verdict implies a pattern was matched, so the logging
// in each of the other branches can dereference it without a nil check.
func TestLoginScanVerdict_NoMatchAlwaysIgnores(t *testing.T) {
	for _, credentialValid := range []bool{true, false} {
		for _, sightings := range []int{0, 1, loginPauseMinSightings, 99} {
			if got := loginScanVerdict(false, credentialValid, sightings); got != loginScanIgnore {
				t.Fatalf("no match (credentialValid=%v sightings=%d): got %v, want ignore",
					credentialValid, sightings, got)
			}
		}
	}
}

// TestLoginScanMatch_ReturnsThePatternItMatched keeps match and verdict honest
// about each other: the loop advances the streak from this function's answer and
// then logs the pattern it returned.
func TestLoginScanMatch_ReturnsThePatternItMatched(t *testing.T) {
	compiled := defaultLoginRegexps(t)
	re := loginScanMatch("claude", loginResiduePane, compiled)
	if re == nil {
		t.Fatal("the residue pane carries a default login pattern and must match")
	}
	if !re.MatchString(loginResiduePane) {
		t.Fatalf("returned pattern %q does not actually match the pane it was returned for", re.String())
	}
	if got := loginScanMatch("claude", "all quiet\n❯ ", compiled); got != nil {
		t.Fatalf("a clean pane must match nothing, got %q", got.String())
	}
}
