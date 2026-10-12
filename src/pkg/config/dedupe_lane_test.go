package config

// Governance tests for the `dedupe` lane (hivecommons/hive#11582): the
// scheduled issue+PR duplicate triage that posts one consolidation checklist
// per repository. Each invariant below is a one-line pack edit away from being
// undone while the YAML still parses, so each is pinned.

import (
	"testing"
	"time"
)

var dedupeLevels = []int{5, 6}

var nonDedupeLevels = []int{1, 2, 3, 4}

func dedupeIn(t *testing.T, level int) (PackAgent, bool) {
	t.Helper()
	p, err := ACMMPackByLevel(level)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(%d): %v", level, err)
	}
	for _, a := range p.Agents {
		if a.Name == "dedupe" {
			return a, true
		}
	}
	return PackAgent{}, false
}

// TestDedupeLaneShipsAtHighTrustLevels pins where the lane is defined and that
// it is wired to its own template in issues-only mode: it files and edits the
// checklist issue, and nothing else.
func TestDedupeLaneShipsAtHighTrustLevels(t *testing.T) {
	for _, lvl := range dedupeLevels {
		a, ok := dedupeIn(t, lvl)
		if !ok {
			t.Fatalf("L%d pack is missing the dedupe agent", lvl)
		}
		if a.Role != "dedupe" {
			t.Errorf("L%d: role = %q, want \"dedupe\"", lvl, a.Role)
		}
		if a.KickTemplate != "dedupe-issues.md" {
			t.Errorf("L%d: kick_template = %q, want dedupe-issues.md", lvl, a.KickTemplate)
		}
		if a.Mode != "ISSUES_ONLY" {
			t.Errorf("L%d: mode = %q, want ISSUES_ONLY — the lane writes a checklist issue and must never touch PRs", lvl, a.Mode)
		}
		if a.OnDemand {
			t.Errorf("L%d: dedupe is on_demand; it must be cadence-kicked or the triage never runs", lvl)
		}
	}
	for _, lvl := range nonDedupeLevels {
		if _, ok := dedupeIn(t, lvl); ok {
			t.Errorf("L%d pack defines dedupe; the lane ships only at L5/L6", lvl)
		}
	}
}

// TestDedupeLaneIsScheduledInEveryMode pins that every governor mode kicks the
// lane on a real interval (not paused) and that stale_timeout exceeds that
// interval, so a long triage over a large queue is not reaped before it posts.
func TestDedupeLaneIsScheduledInEveryMode(t *testing.T) {
	for _, lvl := range dedupeLevels {
		p, err := ACMMPackByLevel(lvl)
		if err != nil {
			t.Fatalf("ACMMPackByLevel(%d): %v", lvl, err)
		}
		a, _ := dedupeIn(t, lvl)
		if len(p.Governor.Cadences) == 0 {
			t.Fatalf("L%d governor has no cadence modes", lvl)
		}
		for mode, cadences := range p.Governor.Cadences {
			raw, ok := cadences["dedupe"]
			if !ok {
				t.Errorf("L%d governor cadence %q has no dedupe entry; the lane is scheduled, not PR-triggered", lvl, mode)
				continue
			}
			c := NewIntervalCadence(raw)
			if c.IsPaused() {
				t.Errorf("L%d cadence %q pauses dedupe; the issue asks for a scheduled sweep", lvl, mode)
				continue
			}
			d, err := time.ParseDuration(c.Interval())
			if err != nil || d <= 0 {
				t.Errorf("L%d cadence %q: dedupe interval %q is not a duration", lvl, mode, raw)
				continue
			}
			if time.Duration(a.StaleTimeout)*time.Second <= d {
				t.Errorf("L%d cadence %q: stale_timeout %ds must exceed the %s cadence", lvl, mode, a.StaleTimeout, d)
			}
		}
	}
}
