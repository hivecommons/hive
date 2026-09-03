package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ── #4246: blocked / unknown / partial admission diagnostics ──────────────────
//
// These tests pin the strict acceptance matrix on the PRODUCTION read paths
// (GET /api/contribute/queue, the SSE hello frame, and the shared snapshot
// behind both) — not on a helper. Two invariants dominate:
//
//   1. DEFAULT OFF: with the convergence toggle unset the payloads carry no
//      diagnostics fields at all — byte-for-byte compatible surface.
//   2. SAME DECISION: a withheld row carries the exact convergence.Decision the
//      queue's own sweep computed; blocked work is never represented as ready.

// diagShadow flips the convergence toggle to shadow for one test, clearing any
// ambient env override first so the test is hermetic.
func diagShadow(t *testing.T, s *Server) {
	t.Helper()
	t.Setenv(config.ConvergenceModeEnvVar, "")
	s.deps.Config.Convergence.Mode = config.ConvergenceModeShadow
}

func withheldNumbers(items []AdmissionWithheldItem) []int {
	out := []int{}
	for _, it := range items {
		out = append(out, it.Number)
	}
	return out
}

// TestAdmissionDiagnostics_OffModeAddsNothing pins the default-off guarantee on
// both HTTP payloads: no "withheld", no "admission_coverage", queue unchanged,
// and blocked work still absent (the pre-existing #3857/#3904 gate is NOT
// toggled by the diagnostics feature).
func TestAdmissionDiagnostics_OffModeAddsNothing(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	// The existing gate still withholds #601 with the toggle off.
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	rec := httptest.NewRecorder()
	s.handleContributeQueue(rec, req)
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad queue JSON: %v", err)
	}
	if _, ok := resp["withheld"]; ok {
		t.Fatal("mode=off must not add a withheld field to /api/contribute/queue")
	}
	if _, ok := resp["admission_coverage"]; ok {
		t.Fatal("mode=off must not add admission_coverage to /api/contribute/queue")
	}

	// Snapshot without diagnostics collects nothing beyond the queue.
	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, false)
	if snap.withheld != nil {
		t.Fatalf("withDiagnostics=false must not collect withheld rows, got %v", snap.withheld)
	}
}

// TestAdmissionDiagnostics_BlockedDependencyIsExplained covers the core rows:
// a candidate with an OPEN dependency is absent from queue and assignment, and
// the shadow diagnostics carry Ready=False, reason WaitingForDependency, and
// name the exact blocker — from the same sweep that built the queue.
func TestAdmissionDiagnostics_BlockedDependencyIsExplained(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)

	// Blocked work stays out of queue and assignment (never represented ready).
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if got := queueNumbers(snap.queue); len(got) != 1 || got[0] != 700 {
		t.Fatalf("queue offered %v, want [700]", got)
	}
	if got := withheldNumbers(snap.withheld); len(got) != 1 || got[0] != 601 {
		t.Fatalf("withheld %v, want [601]", got)
	}
	w := snap.withheld[0]
	if w.Reason != convergence.ReasonWaitingForDependency {
		t.Fatalf("reason %q, want WaitingForDependency", w.Reason)
	}
	if w.Ready != string(convergence.ConditionFalse) {
		t.Fatalf("Ready %q, want False", w.Ready)
	}
	if w.Observed != string(convergence.ConditionTrue) {
		t.Fatalf("Observed %q, want True (a record exists)", w.Observed)
	}
	if len(w.Blockers) != 1 || w.Blockers[0] != blockerID {
		t.Fatalf("blockers %v, want [%s]", w.Blockers, blockerID)
	}
	if w.ObservedRecord == "" || w.ObservedGeneration == "" {
		t.Fatalf("expected observed record+generation, got %q / %q", w.ObservedRecord, w.ObservedGeneration)
	}
	if w.Key != "projectbluefin/dakota#601" {
		t.Fatalf("key %q, want canonical identity", w.Key)
	}
	if w.Repo != "projectbluefin/dakota" || w.Title != "dependent work" {
		t.Fatalf("unexpected candidate reference: %+v", w)
	}

	// Coverage: one store read, full ledger, policy stated.
	if snap.coverage.Partial || snap.coverage.StoresRead != 1 || snap.coverage.StoresFailed != 0 {
		t.Fatalf("coverage %+v, want full single-store coverage", snap.coverage)
	}
	if snap.coverage.Policy != admissionCoveragePolicy {
		t.Fatalf("policy %q, want the fixed #3904 statement", snap.coverage.Policy)
	}
}

// TestAdmissionDiagnostics_UnknownDependencyIsUnknownNotFalse: an unresolvable,
// non-retired dependency yields Ready=Unknown / DependencyUnknown — distinct
// from an established blocker — and the candidate is still withheld.
func TestAdmissionDiagnostics_UnknownDependencyIsUnknownNotFalse(t *testing.T) {
	store := depTestStore(t)
	dependent, err := store.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent: %v", err)
	}
	if err := store.Update(dependent.ID, func(b *beads.Bead) {
		b.DependsOn = []string{"bd-nonexistent"}
	}); err != nil {
		t.Fatalf("adding dangling dependency: %v", err)
	}
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)

	assertQueue(t, hub, 700)
	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if got := withheldNumbers(snap.withheld); len(got) != 1 || got[0] != 601 {
		t.Fatalf("withheld %v, want [601]", got)
	}
	w := snap.withheld[0]
	if w.Reason != convergence.ReasonDependencyUnknown {
		t.Fatalf("reason %q, want DependencyUnknown", w.Reason)
	}
	if w.Ready != string(convergence.ConditionUnknown) {
		t.Fatalf("Ready %q, want Unknown — an unknown must never be reported False", w.Ready)
	}
}

// TestAdmissionDiagnostics_PositiveControls: a terminal-retired dependency and a
// candidate with no record at all are both ADMITTED — present in the queue,
// absent from withheld — so the diagnostics cannot drift into reject-everything.
func TestAdmissionDiagnostics_PositiveControls(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)

	// #601's dependency is satisfied; #700 has no record. Both admitted.
	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if got := queueNumbers(snap.queue); len(got) != 2 || got[0] != 601 || got[1] != 700 {
		t.Fatalf("queue offered %v, want [601 700]", got)
	}
	if len(snap.withheld) != 0 {
		t.Fatalf("nothing should be withheld, got %v", snap.withheld)
	}
}

// TestAdmissionDiagnostics_FlipsWithoutRestart: closing the blocker moves the
// candidate from withheld to queue on the NEXT snapshot — no restart, no cached
// decision — and reopening it moves it back.
func TestAdmissionDiagnostics_FlipsWithoutRestart(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if got := withheldNumbers(snap.withheld); len(got) != 1 || got[0] != 601 {
		t.Fatalf("withheld %v, want [601]", got)
	}

	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	snap = hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if len(snap.withheld) != 0 {
		t.Fatalf("withheld %v after satisfaction, want none", snap.withheld)
	}
	if got := queueNumbers(snap.queue); len(got) != 2 {
		t.Fatalf("queue %v after satisfaction, want both items", got)
	}

	if err := store.Update(blockerID, func(b *beads.Bead) {
		b.Status = beads.StatusOpen
		b.ClosedAt = nil
	}); err != nil {
		t.Fatalf("reopening blocker: %v", err)
	}
	snap = hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if got := withheldNumbers(snap.withheld); len(got) != 1 || got[0] != 601 {
		t.Fatalf("withheld %v after reopening, want [601] — no latched status", got)
	}
}

// TestAdmissionDiagnostics_PartialLedgerCoverage: with one configured store
// unreadable the snapshot REPORTS reduced coverage (partial, counts, policy)
// while behaviour keeps the final #3904 compromise — readable blocked records
// stay withheld, unmapped misses stay admitted.
func TestAdmissionDiagnostics_PartialLedgerCoverage(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	s.deps.BeadStoreLoadFailures = 1
	diagShadow(t, s)

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	cov := snap.coverage
	if !cov.Partial || cov.StoresRead != 1 || cov.StoresFailed != 1 {
		t.Fatalf("coverage %+v, want partial with 1 read / 1 failed", cov)
	}
	if cov.Policy != admissionCoveragePolicy {
		t.Fatalf("policy %q missing", cov.Policy)
	}
	// Readable blocked record stays gated; unmapped miss (#700) stays admitted.
	if got := withheldNumbers(snap.withheld); len(got) != 1 || got[0] != 601 {
		t.Fatalf("withheld %v, want [601]", got)
	}
	if got := queueNumbers(snap.queue); len(got) != 1 || got[0] != 700 {
		t.Fatalf("queue %v, want [700] — misses are admitted, not proof of absence", got)
	}
}

// TestAdmissionDiagnostics_QueueEndpointShadowPayload drives the real HTTP
// handler in shadow mode and asserts the additive fields appear alongside an
// unchanged queue field.
func TestAdmissionDiagnostics_QueueEndpointShadowPayload(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)
	_ = hub

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue", nil)
	rec := httptest.NewRecorder()
	s.handleContributeQueue(rec, req)

	var resp struct {
		Queue    []ReadyQueueItem        `json:"queue"`
		Withheld []AdmissionWithheldItem `json:"withheld"`
		Coverage *AdmissionCoverage      `json:"admission_coverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad queue JSON: %v", err)
	}
	if got := queueNumbers(resp.Queue); len(got) != 1 || got[0] != 700 {
		t.Fatalf("queue %v, want [700]", got)
	}
	if got := withheldNumbers(resp.Withheld); len(got) != 1 || got[0] != 601 {
		t.Fatalf("withheld %v, want [601]", got)
	}
	if resp.Withheld[0].Reason != convergence.ReasonWaitingForDependency {
		t.Fatalf("withheld reason %q", resp.Withheld[0].Reason)
	}
	if resp.Coverage == nil || resp.Coverage.Policy != admissionCoveragePolicy {
		t.Fatalf("admission_coverage missing or wrong: %+v", resp.Coverage)
	}
}

// TestAdmissionDiagnostics_SSEHelloCarriesWithheld asserts the SSE hydration
// path carries the same additive diagnostics in shadow mode.
func TestAdmissionDiagnostics_SSEHelloCarriesWithheld(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newSyncRecorder()
	go s.handleContributeEvents(rec, req)

	waitFor(t, func() bool {
		b := rec.BodyString()
		return strings.Contains(b, `"type":"hello"`) &&
			strings.Contains(b, `"withheld"`) &&
			strings.Contains(b, `"admission_coverage"`) &&
			strings.Contains(b, convergence.ReasonWaitingForDependency)
	}, "hello frame with withheld diagnostics")
}

// TestAdmissionDiagnostics_SSEHelloOffModeOmitsFields: with the toggle off the
// hello frame must not mention the diagnostics keys at all.
func TestAdmissionDiagnostics_SSEHelloOffModeOmitsFields(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/events", nil).WithContext(ctx)
	rec := newSyncRecorder()
	go s.handleContributeEvents(rec, req)

	waitFor(t, func() bool {
		return strings.Contains(rec.BodyString(), `"type":"hello"`)
	}, "hello frame")
	b := rec.BodyString()
	if strings.Contains(b, `"withheld"`) || strings.Contains(b, `"admission_coverage"`) {
		t.Fatal("mode=off hello frame must not carry diagnostics fields")
	}
}

// TestAdmissionDiagnostics_OpenPRClaimIsNotAConvergenceDiagnostic: an open-PR
// claim refusal is a different gate with a zero convergence Decision; it must
// not appear in the withheld collection (which would fabricate conditions).
func TestAdmissionDiagnostics_OpenPRClaimIsNotAConvergenceDiagnostic(t *testing.T) {
	store := depTestStore(t)
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		if number == 601 {
			return ghpkg.IssueClaim{PRNumber: 999}, true
		}
		return ghpkg.IssueClaim{}, false
	}

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, true)
	if got := queueNumbers(snap.queue); len(got) != 1 || got[0] != 700 {
		t.Fatalf("queue %v, want [700]", got)
	}
	if len(snap.withheld) != 0 {
		t.Fatalf("open-PR-claim refusal leaked into convergence withheld: %v", snap.withheld)
	}
}

// TestWithheldItemFromDecision_DegradedObservation pins the mapping for a
// degraded observer: Ready=Unknown with the degraded reason, never admitted,
// and no condition the evaluator did not compute.
func TestWithheldItemFromDecision_DegradedObservation(t *testing.T) {
	d := convergence.Evaluate(convergence.Observation{
		Subject:        convergence.Subject{Repo: "o/r", Number: 5},
		Degraded:       true,
		DegradedReason: "BeadLedgerPartial",
	})
	if d.Admitted {
		t.Fatal("degraded observation must never admit")
	}
	item := withheldItemFromDecision("o/r", worksource.Ref{Repo: "o/r", Number: 5}, "t", "u", d)
	if item.Ready != string(convergence.ConditionUnknown) || item.Observed != string(convergence.ConditionUnknown) {
		t.Fatalf("degraded mapping %+v, want Unknown/Unknown", item)
	}
	if item.Reason != "BeadLedgerPartial" {
		t.Fatalf("reason %q", item.Reason)
	}
}

// TestAdmissionDiagnostics_WithheldIsBounded: the withheld collection is capped
// at the queue limit so a pathological ledger cannot blow out the payload.
func TestAdmissionDiagnostics_WithheldIsBounded(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	diagShadow(t, s)

	snap := hub.admissionQueueSnapshot(1, true)
	if len(snap.withheld) > 1 {
		t.Fatalf("withheld exceeded limit: %d", len(snap.withheld))
	}
}
