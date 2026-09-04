package automerge

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Audit F3 (standing). The label-queued auto-merge sweep must re-verify that
// the actor who QUEUED the merge holds the merger tier, at the point the merge
// actually happens. Without it the self-merge ban is the only gate, and that
// ban proves only "queuer != author" — defeated outright by a sockpuppet pair,
// and useless against any actor who can get the queue label applied.
//
// WHY A SOURCE-LEVEL TEST, on top of the behavioural ones in
// automerge_sweep_trusted_merger_test.go.
//
// F3 has been closed and reopened before. It was fixed in 160f585c and then
// SILENTLY REVERTED by a v2->v4 sync merge that resolved a conflict in favour
// of the older side — one of three fixes (F3, F14, F18) lost that way. The
// behavioural tests would not have survived that either: the sweep gate and
// the main.go wiring are in different packages, so dropping the wiring alone
// leaves every test in this package green while the live binary merges
// anything. The failure mode for this class of bug in this repo is a merge
// dropping a gate, not a logic bug.
//
// So these tests assert the INVARIANT in the source text, across all three
// layers that must survive together:
//
//  1. the gate exists inside trySweepQueuedPR and fails closed (this file),
//  2. the helper it calls is nil-safe and fails closed (this file),
//  3. cmd/hive/main.go actually INSTALLS an authorizer (this file) — the layer
//     no test in this package could otherwise see.
//
// Shape follows pkg/dashboard/f16_owner_gate_test.go.

// f3ReadSource returns the contents of a source file relative to this package.
func f3ReadSource(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(raw)
}

// f3FuncBody returns the source of a named func, from its declaration line to
// the first line that is exactly "}".
func f3FuncBody(t *testing.T, src, decl, name string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("%s not found — it was renamed or removed; update this test deliberately, do not delete the case", name)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("could not find the end of %s", name)
	}
	return src[i : i+j]
}

// TestF3SweepConsultsTrustedMergerGate is the core F3 regression: the merge
// path itself must consult the tier gate.
func TestF3SweepConsultsTrustedMergerGate(t *testing.T) {
	body := f3FuncBody(t,
		f3ReadSource(t, "automerge_sweep.go"),
		"func (c *Engine) trySweepQueuedPR(",
		"trySweepQueuedPR")

	if !strings.Contains(body, "c.isTrustedMerger(queuedBy)") {
		t.Error("trySweepQueuedPR does not consult isTrustedMerger(queuedBy) — the trusted-merger " +
			"gate is gone and any actor who can get the queue label applied merges anything (audit F3). " +
			"F3 was fixed in 160f585c and then reverted by a sync merge; if that happened again, " +
			"restore the gate rather than relaxing this test.")
	}

	// The gate must be checked on the QUEUER, not the author. Gating the
	// author would leave the sockpuppet pair (untrusted A authors, untrusted
	// B queues) merging exactly as before.
	if strings.Contains(body, "c.isTrustedMerger(author)") {
		t.Error("trySweepQueuedPR gates isTrustedMerger on the AUTHOR — F3 is about who QUEUED the merge; " +
			"gating the author leaves the sockpuppet pair intact")
	}

	// Fail-closed on an unconfigured authorizer. If `configured` is ignored,
	// a hub with no authorizer installed merges everything.
	if !strings.Contains(body, "autoMergeReasonNoMergerAuthz") {
		t.Error("trySweepQueuedPR does not return autoMergeReasonNoMergerAuthz — an unconfigured " +
			"authorizer must DISABLE the sweep (fail closed), not permit every merge (audit F3)")
	}
	if !strings.Contains(body, "autoMergeReasonUntrustedMerger") {
		t.Error("trySweepQueuedPR does not return autoMergeReasonUntrustedMerger — an untrusted " +
			"queuer must be rejected (audit F3)")
	}

	// The self-merge ban is a DIFFERENT, weaker gate. F3 exists because the
	// ban alone was insufficient, so its presence must never be read as
	// satisfying F3 — assert both are present, not either.
	if !strings.Contains(body, "self-merge-ban") {
		t.Error("the self-merge ban disappeared from trySweepQueuedPR; F3's tier gate ADDS to it " +
			"rather than replacing it, so both must be present")
	}
}

// TestF3GateOrderedBeforeMerge pins that the tier gate is evaluated BEFORE the
// merge is issued. A gate that runs after the merge call would satisfy a naive
// "contains isTrustedMerger" assertion while merging anyway.
func TestF3GateOrderedBeforeMerge(t *testing.T) {
	body := f3FuncBody(t,
		f3ReadSource(t, "automerge_sweep.go"),
		"func (c *Engine) trySweepQueuedPR(",
		"trySweepQueuedPR")

	gate := strings.Index(body, "c.isTrustedMerger(queuedBy)")
	if gate < 0 {
		t.Fatal("isTrustedMerger(queuedBy) absent — see TestF3SweepConsultsTrustedMergerGate")
	}
	// mergeableFromState is the first step of the actual merge attempt; every
	// gate that matters runs above it.
	merge := strings.Index(body, "hgithub.MergeableFromState(")
	if merge < 0 {
		t.Skip("mergeableFromState no longer marks the start of the merge attempt — re-point this test")
	}
	if gate > merge {
		t.Error("the trusted-merger gate is evaluated AFTER the merge attempt begins — it must gate " +
			"the merge, not follow it (audit F3)")
	}
}

// TestF3IsTrustedMergerFailsClosedInSource asserts the helper's fail-closed
// shape at the source level. The behavioural test covers the same ground; this
// one catches a merge that keeps the call site but guts the helper.
func TestF3IsTrustedMergerFailsClosedInSource(t *testing.T) {
	body := f3FuncBody(t,
		f3ReadSource(t, "automerge_sweep.go"),
		"func (c *Engine) isTrustedMerger(",
		"isTrustedMerger")

	for _, want := range []struct{ frag, why string }{
		{"if c == nil", "a nil client must report (false, false), never (true, _)"},
		{"if fn == nil", "a nil authorizer must report NOT CONFIGURED so the sweep disables itself"},
	} {
		if !strings.Contains(body, want.frag) {
			t.Errorf("isTrustedMerger lost its %q guard — %s (audit F3)", want.frag, want.why)
		}
	}
	// The helper must never hand back a bare `true` default.
	if strings.Contains(body, "return true, true") {
		t.Error("isTrustedMerger contains an unconditional `return true, true` — it must fail CLOSED")
	}
}

// TestF3AuthorizerIsWiredInMain is the layer no other test in this package can
// see. The gate is inert unless cmd/hive/main.go installs an authorizer; a sync
// merge that dropped ONLY the SetMergerAuthorizer call would leave every
// behavioural test in this package green while the live sweep fails closed and
// silently stops merging — or, if the fail-closed branch were also lost, merges
// everything.
func TestF3AuthorizerIsWiredInMain(t *testing.T) {
	src := f3ReadSource(t, "../../../cmd/hive/main.go")

	if !strings.Contains(src, "MergerAuthorizer: trustedMergerFunc(cfg)") {
		t.Error("cmd/hive/main.go does not install the trusted-merger authorizer — the F3 gate in " +
			"trySweepQueuedPR is present but INERT (audit F3, standing). Restore " +
			"MergerAuthorizer: trustedMergerFunc(cfg) beside StartMergeRequestWatcher.")
	}

	body := f3FuncBody(t, src, "func trustedMergerFunc(", "trustedMergerFunc")
	if !strings.Contains(body, "config.RoleAtLeast(role, config.RoleMerger)") {
		t.Error("trustedMergerFunc no longer requires at least config.RoleMerger — the sweep would " +
			"admit a tier below the one the dashboard queue endpoint enforces (audit F3)")
	}
	if !strings.Contains(body, "cfg == nil") {
		t.Error("trustedMergerFunc lost its nil-config guard; it must fail CLOSED")
	}
	if !strings.Contains(body, "return false") {
		t.Error("trustedMergerFunc has no `return false` path — an unclassifiable actor must never be trusted")
	}
}

// --- POSITIVE CONTROLS -------------------------------------------------------
//
// Without these, "add isTrustedMerger everywhere" or "make the sweep merge
// nothing" would satisfy every assertion above. Over-gating is a real cost:
// the sweep exists to merge trusted-queued green PRs, and a sweep that merges
// nothing is a broken sweep, not a safe one.

// TestF3TrustedMergerGateIsNotBlanket asserts the gate is applied at the queued
// sweep and NOT copied onto the self-merge path, which is authorized by a
// different mechanism (App authorship + required checks) and has no queuer at
// all. Gating it on a queuer login it never has would disable it permanently.
func TestF3TrustedMergerGateIsNotBlanket(t *testing.T) {
	src := f3ReadSource(t, "automerge_sweep.go")
	// Exactly one non-test call site: the queued sweep. A second would mean the
	// gate leaked onto a path with no queuer identity.
	calls := regexp.MustCompile(`c\.isTrustedMerger\(`).FindAllString(src, -1)
	if len(calls) != 1 {
		t.Errorf("automerge_sweep.go has %d isTrustedMerger call sites, want exactly 1 (the queued "+
			"sweep). More than one suggests the gate was copied onto a path that has no queuer "+
			"identity and would fail closed forever; zero means F3 regressed.", len(calls))
	}
}

// TestF3SweepStillMergesOnTheHappyPath is the source-level companion to the
// behavioural positive control: the merge call must still be reachable from
// trySweepQueuedPR. A "fix" that deleted the merge entirely would pass every
// negative assertion in this file.
func TestF3SweepStillMergesOnTheHappyPath(t *testing.T) {
	body := f3FuncBody(t,
		f3ReadSource(t, "automerge_sweep.go"),
		"func (c *Engine) trySweepQueuedPR(",
		"trySweepQueuedPR")
	if !strings.Contains(body, "hgithub.MergeableFromState(") {
		t.Error("trySweepQueuedPR no longer evaluates mergeability — the sweep appears to have lost " +
			"its merge path; F3 gates the sweep, it does not disable it")
	}
}

// --- COUNT FLOOR -------------------------------------------------------------

// TestF3RejectionReasonCountFloor is the blunt instrument that survives
// renames. trySweepQueuedPR is a chain of fail-closed gates; if the number of
// distinct rejection reasons drops, a gate was removed even if the remaining
// names still look right. Exactly the signal a sync merge trips.
func TestF3RejectionReasonCountFloor(t *testing.T) {
	body := f3FuncBody(t,
		f3ReadSource(t, "automerge_sweep.go"),
		"func (c *Engine) trySweepQueuedPR(",
		"trySweepQueuedPR")

	// Gates trySweepQueuedPR must still contain, as they appear in the SOURCE
	// TEXT — bare literals for the inline reasons, identifiers for the named
	// constants. Matching on the constants' VALUES would silently match
	// nothing, since the body references them by name.
	want := []string{
		`"self-merge-ban"`,
		`"queue-approval-head-changed"`,
		"autoMergeReasonNoMergerAuthz",
		"autoMergeReasonUntrustedMerger",
	}
	missing := []string{}
	for _, r := range want {
		if !strings.Contains(body, r) {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		t.Errorf("trySweepQueuedPR lost %d of %d rejection gates: %v — auto-merge got MORE permissive "+
			"(audit F3 regressed via a sync merge once already)", len(missing), len(want), missing)
	}
}
