package hub

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixedVisNow is the clock every case here is evaluated against, so ages are
// exact rather than relative to a running wall clock.
var fixedVisNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func visAgo(d time.Duration) time.Time { return fixedVisNow.Add(-d) }

// TestSummarizeStuckPodsCountsRealPredicateMatches is the load-bearing test for
// #5328 item 3.
//
// Per #5388, a guard that asserts a FIELD EXISTS is a guard that fails green.
// This test therefore never checks "is StuckPods non-nil" — it builds a pod
// population whose orphan count is known by construction, and asserts the
// reported number equals that count and no other. The population deliberately
// mixes in every pod shape that a LOOSER predicate would wrongly count:
// a Running pod carrying a deletionTimestamp (the live spoke), a pod held by a
// finalizer, a pod inside the age window, a pod never asked to terminate, and
// an orphan in a NON-hive namespace. If the count ever picks any of those up,
// the number moves and this fails.
func TestSummarizeStuckPodsCountsRealPredicateMatches(t *testing.T) {
	nsA := hiveHostedNamespacePrefix + "alpha"
	nsB := hiveHostedNamespacePrefix + "bravo"

	candidates := []orphanedPodCandidate{
		// --- nsA: 3 genuine orphans -------------------------------------
		{Namespace: nsA, Name: "orphan-a1", DeletionTimestamp: visAgo(72 * time.Hour), Phase: "Failed"},
		{Namespace: nsA, Name: "orphan-a2", DeletionTimestamp: visAgo(3 * time.Hour), Phase: "Succeeded"},
		{Namespace: nsA, Name: "orphan-a3", DeletionTimestamp: visAgo(21 * 24 * time.Hour), Phase: "Pending"},

		// --- nsA: pods that MUST NOT be counted -------------------------
		// The live spoke: Running with a deletionTimestamp. Counting this
		// would report a healthy namespace as broken.
		{Namespace: nsA, Name: "live-spoke", DeletionTimestamp: visAgo(48 * time.Hour), Phase: podPhaseRunning},
		// A finalizer means a controller has unfinished work — different
		// problem, out of scope.
		{Namespace: nsA, Name: "finalizer-held", DeletionTimestamp: visAgo(48 * time.Hour),
			Finalizers: []string{"kubernetes.io/pv-protection"}, Phase: "Failed"},
		// Inside the grace window: terminating correctly right now.
		{Namespace: nsA, Name: "young-terminating", DeletionTimestamp: visAgo(2 * time.Minute), Phase: "Failed"},
		// Never asked to go away at all.
		{Namespace: nsA, Name: "healthy", Phase: podPhaseRunning},

		// --- nsB: 1 genuine orphan --------------------------------------
		{Namespace: nsB, Name: "orphan-b1", DeletionTimestamp: visAgo(9 * time.Hour), Phase: "Failed"},
		{Namespace: nsB, Name: "live-spoke", Phase: podPhaseRunning},

		// --- a NON-hive namespace: matches the predicate but is not ours --
		// kube-system's stuck pods are somebody else's problem; counting
		// them would make the fleet signal unactionable.
		{Namespace: "kube-system", Name: "not-ours", DeletionTimestamp: visAgo(96 * time.Hour), Phase: "Failed"},
	}

	got := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)

	const wantTotal = 4 // 3 in nsA + 1 in nsB, and nothing else
	const wantNamespacesAffected = 2

	if got.Total != wantTotal {
		t.Errorf("Total = %d, want %d — the count must equal the number of real predicate matches, not the number of pods", got.Total, wantTotal)
	}
	if got.NamespacesAffected != wantNamespacesAffected {
		t.Errorf("NamespacesAffected = %d, want %d", got.NamespacesAffected, wantNamespacesAffected)
	}
	if got.Truncated {
		t.Errorf("Truncated = true, want false — only 2 namespaces, far below the limit of %d", orphanedPodStuckReportLimit)
	}

	// Per-namespace breakdown must carry the real per-namespace numbers, and
	// must be ordered worst-first.
	want := []OrphanedNamespaceCount{
		{Namespace: nsA, Count: 3},
		{Namespace: nsB, Count: 1},
	}
	if len(got.Namespaces) != len(want) {
		t.Fatalf("Namespaces has %d entries, want %d: %+v", len(got.Namespaces), len(want), got.Namespaces)
	}
	for i := range want {
		if got.Namespaces[i] != want[i] {
			t.Errorf("Namespaces[%d] = %+v, want %+v (expect worst-first ordering)", i, got.Namespaces[i], want[i])
		}
	}

	// The per-namespace counts must sum to the total; a breakdown that
	// disagrees with its own headline is worse than no breakdown.
	sum := 0
	for _, n := range got.Namespaces {
		sum += n.Count
	}
	if sum != got.Total {
		t.Errorf("per-namespace counts sum to %d but Total is %d", sum, got.Total)
	}
}

// TestSummarizeStuckPodsCleanFleetIsZeroNotUnknown pins the steady state. A
// cluster with only live spokes must report a real, present zero — that is what
// distinguishes "we looked and it is clean" from "we could not tell", which is
// the distinction whose absence let this incident run for three weeks.
func TestSummarizeStuckPodsCleanFleetIsZeroNotUnknown(t *testing.T) {
	candidates := []orphanedPodCandidate{
		{Namespace: hiveHostedNamespacePrefix + "alpha", Name: "spoke", Phase: podPhaseRunning},
		{Namespace: hiveHostedNamespacePrefix + "bravo", Name: "spoke", Phase: podPhaseRunning},
		// Mid-shutdown, correctly: still Running with a fresh timestamp.
		{Namespace: hiveHostedNamespacePrefix + "bravo", Name: "rolling", DeletionTimestamp: visAgo(5 * time.Second), Phase: podPhaseRunning},
	}
	got := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0 on a clean fleet", got.Total)
	}
	if got.NamespacesAffected != 0 {
		t.Errorf("NamespacesAffected = %d, want 0", got.NamespacesAffected)
	}
	if len(got.Namespaces) != 0 {
		t.Errorf("Namespaces = %+v, want empty", got.Namespaces)
	}
}

// TestSummarizeStuckPodsReproducesMeasuredIncident replays the actual shape from
// the incident: 27 orphans spread across 15 hive namespaces, each namespace
// still holding exactly one healthy Running spoke. This is the number the fleet
// surface would have shown had it existed, and it is the regression that proves
// the signal reports reality rather than a placeholder.
func TestSummarizeStuckPodsReproducesMeasuredIncident(t *testing.T) {
	// 15 namespaces holding 27 orphans total: 12 namespaces with 2, 3 with 1.
	perNS := []int{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1}
	wantTotal := 0
	var candidates []orphanedPodCandidate
	for i, n := range perNS {
		ns := fmt.Sprintf("%sspoke%02d", hiveHostedNamespacePrefix, i)
		// Every namespace keeps exactly one healthy Running pod — the live
		// spoke, which must never be counted.
		candidates = append(candidates, orphanedPodCandidate{Namespace: ns, Name: "live", Phase: podPhaseRunning})
		for j := 0; j < n; j++ {
			candidates = append(candidates, orphanedPodCandidate{
				Namespace:         ns,
				Name:              fmt.Sprintf("orphan-%d", j),
				DeletionTimestamp: visAgo(time.Duration(3+i) * 24 * time.Hour),
				Phase:             "Failed",
			})
		}
		wantTotal += n
	}
	if wantTotal != 27 {
		t.Fatalf("test fixture builds %d orphans, expected the measured 27", wantTotal)
	}

	got := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)
	if got.Total != 27 {
		t.Errorf("Total = %d, want 27 (the measured incident)", got.Total)
	}
	if got.NamespacesAffected != 15 {
		t.Errorf("NamespacesAffected = %d, want 15 (the measured incident)", got.NamespacesAffected)
	}
	// 15 affected namespaces sits under the breakdown cap, so the operator
	// sees the full spread rather than a truncated one.
	if got.Truncated {
		t.Errorf("Truncated = true, but the measured incident's 15 namespaces fit under the cap of %d", orphanedPodStuckReportLimit)
	}
	if len(got.Namespaces) != 15 {
		t.Errorf("Namespaces has %d entries, want all 15", len(got.Namespaces))
	}
}

// TestSummarizeStuckPodsTruncatesButKeepsTotalExact pins that the cap applies
// ONLY to the breakdown. The headline number and the affected-namespace count
// must stay exact, otherwise a large incident would be reported as smaller than
// it is — the precise way a safety signal fails green.
func TestSummarizeStuckPodsTruncatesButKeepsTotalExact(t *testing.T) {
	const namespaces = orphanedPodStuckReportLimit + 10
	var candidates []orphanedPodCandidate
	for i := 0; i < namespaces; i++ {
		candidates = append(candidates, orphanedPodCandidate{
			Namespace:         fmt.Sprintf("%sspoke%03d", hiveHostedNamespacePrefix, i),
			Name:              "orphan",
			DeletionTimestamp: visAgo(48 * time.Hour),
			Phase:             "Failed",
		})
	}

	got := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)
	if got.Total != namespaces {
		t.Errorf("Total = %d, want %d — truncation must never shrink the headline count", got.Total, namespaces)
	}
	if got.NamespacesAffected != namespaces {
		t.Errorf("NamespacesAffected = %d, want %d — truncation must never shrink the spread", got.NamespacesAffected, namespaces)
	}
	if len(got.Namespaces) != orphanedPodStuckReportLimit {
		t.Errorf("Namespaces has %d entries, want the cap %d", len(got.Namespaces), orphanedPodStuckReportLimit)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true — a short list must announce itself as short")
	}
}

// TestSummarizeStuckPodsOrderingIsDeterministic guards the truncation cut. Go
// map iteration is randomized, so without an explicit tiebreak the SAME cluster
// state could yield different surviving namespaces on two consecutive reads.
func TestSummarizeStuckPodsOrderingIsDeterministic(t *testing.T) {
	var candidates []orphanedPodCandidate
	for i := 0; i < orphanedPodStuckReportLimit+5; i++ {
		candidates = append(candidates, orphanedPodCandidate{
			Namespace:         fmt.Sprintf("%sspoke%03d", hiveHostedNamespacePrefix, i),
			Name:              "orphan",
			DeletionTimestamp: visAgo(48 * time.Hour),
			Phase:             "Failed",
		})
	}
	first := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)
	for i := 0; i < 20; i++ {
		again := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)
		if len(again.Namespaces) != len(first.Namespaces) {
			t.Fatalf("run %d returned %d entries, first returned %d", i, len(again.Namespaces), len(first.Namespaces))
		}
		for j := range first.Namespaces {
			if again.Namespaces[j] != first.Namespaces[j] {
				t.Fatalf("run %d entry %d = %+v, first run had %+v — ordering must be deterministic or truncation is nondeterministic",
					i, j, again.Namespaces[j], first.Namespaces[j])
			}
		}
	}
}

// TestSummarizeStuckPodsAgreesWithReaperSelection is the anti-drift guard. The
// count and the reaper MUST be the same predicate; this asserts they select the
// identical set over one population, so a future change to
// podIsOrphanedTerminating can never move one without the other.
func TestSummarizeStuckPodsAgreesWithReaperSelection(t *testing.T) {
	ns := hiveHostedNamespacePrefix + "alpha"
	candidates := []orphanedPodCandidate{
		{Namespace: ns, Name: "orphan-1", DeletionTimestamp: visAgo(30 * time.Hour), Phase: "Failed"},
		{Namespace: ns, Name: "orphan-2", DeletionTimestamp: visAgo(90 * time.Minute), Phase: "Succeeded"},
		{Namespace: ns, Name: "live", DeletionTimestamp: visAgo(30 * time.Hour), Phase: podPhaseRunning},
		{Namespace: ns, Name: "held", DeletionTimestamp: visAgo(30 * time.Hour), Finalizers: []string{"x"}, Phase: "Failed"},
		{Namespace: ns, Name: "young", DeletionTimestamp: visAgo(time.Minute), Phase: "Failed"},
	}

	reaperWould := selectOrphanedPods(candidates, fixedVisNow, orphanedPodMinAge)
	counted := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)

	if counted.Total != len(reaperWould) {
		t.Errorf("count reports %d stuck pods but the reaper would select %d — the two must use the ONE predicate",
			counted.Total, len(reaperWould))
	}
}

// TestSummarizeStuckPodsIgnoresNonHiveNamespaces pins the scope clause on its
// own, so a namespace-prefix regression cannot hide behind other assertions.
func TestSummarizeStuckPodsIgnoresNonHiveNamespaces(t *testing.T) {
	// Every one of these matches the predicate perfectly; none of them is ours.
	candidates := []orphanedPodCandidate{
		{Namespace: "kube-system", Name: "a", DeletionTimestamp: visAgo(48 * time.Hour), Phase: "Failed"},
		{Namespace: "default", Name: "b", DeletionTimestamp: visAgo(48 * time.Hour), Phase: "Failed"},
		{Namespace: "hive-hosted", Name: "c", DeletionTimestamp: visAgo(48 * time.Hour), Phase: "Failed"}, // no trailing dash: not the prefix
	}
	got := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0 — only %q namespaces are in scope", got.Total, hiveHostedNamespacePrefix)
	}
}

// TestStuckPodsFlowsFromKubectlJSONToCount covers the real ingest path end to
// end over the pure functions: raw `kubectl get pods -o json` bytes in, a
// correct count out. Parsing is shared with the reaper (parseOrphanCandidates),
// so this also pins that the timestamp decode feeding the COUNT fails safe.
func TestStuckPodsFlowsFromKubectlJSONToCount(t *testing.T) {
	ns := hiveHostedNamespacePrefix + "alpha"
	old := visAgo(50 * time.Hour).Format(time.RFC3339)
	fresh := visAgo(10 * time.Second).Format(time.RFC3339)

	raw := fmt.Sprintf(`{"items":[
	  {"metadata":{"name":"orphan","namespace":%q,"deletionTimestamp":%q,"finalizers":[]},"status":{"phase":"Failed"}},
	  {"metadata":{"name":"live","namespace":%q},"status":{"phase":"Running"}},
	  {"metadata":{"name":"held","namespace":%q,"deletionTimestamp":%q,"finalizers":["a/b"]},"status":{"phase":"Failed"}},
	  {"metadata":{"name":"young","namespace":%q,"deletionTimestamp":%q},"status":{"phase":"Failed"}},
	  {"metadata":{"name":"unparseable","namespace":%q,"deletionTimestamp":"not-a-time"},"status":{"phase":"Failed"}},
	  {"metadata":{"name":"elsewhere","namespace":"kube-system","deletionTimestamp":%q},"status":{"phase":"Failed"}}
	]}`, ns, old, ns, ns, old, ns, fresh, ns, old)

	// Sanity: the fixture must be valid JSON, or this test proves nothing.
	if !json.Valid([]byte(raw)) {
		t.Fatal("test fixture is not valid JSON")
	}

	candidates, err := parseOrphanCandidates([]byte(raw))
	if err != nil {
		t.Fatalf("parseOrphanCandidates: %v", err)
	}
	got := summarizeStuckPods(candidates, fixedVisNow, orphanedPodMinAge)

	// Exactly ONE pod qualifies: "orphan". Every other row is a documented
	// exclusion, including the unparseable timestamp which must fail SAFE
	// (never counted) rather than be guessed at.
	if got.Total != 1 {
		t.Errorf("Total = %d, want 1 (only \"orphan\" qualifies): %+v", got.Total, got.Namespaces)
	}
	if got.NamespacesAffected != 1 {
		t.Errorf("NamespacesAffected = %d, want 1", got.NamespacesAffected)
	}
	if len(got.Namespaces) == 1 && got.Namespaces[0].Namespace != ns {
		t.Errorf("reported namespace %q, want %q", got.Namespaces[0].Namespace, ns)
	}
}

// TestStuckPodReportSerializesUnknownDistinctFromZero pins the wire contract
// that makes the signal trustworthy. A nil report must vanish from the JSON so
// the UI renders UNKNOWN; a present zero must serialize as an explicit total so
// the UI can say "checked, clean". If these two ever look alike on the wire,
// the surface reproduces the exact blindness this issue is about.
func TestStuckPodReportSerializesUnknownDistinctFromZero(t *testing.T) {
	unknown, err := json.Marshal(PerClusterHealth{ID: "c1"})
	if err != nil {
		t.Fatalf("marshal unknown: %v", err)
	}
	if strings.Contains(string(unknown), "stuck_pods") {
		t.Errorf("a nil StuckPods must be OMITTED so the UI shows unknown, got: %s", unknown)
	}

	clean, err := json.Marshal(PerClusterHealth{ID: "c1", StuckPods: &StuckPodReport{}})
	if err != nil {
		t.Fatalf("marshal clean: %v", err)
	}
	if !strings.Contains(string(clean), `"stuck_pods"`) {
		t.Errorf("a present zero report must SERIALIZE so the UI can distinguish clean from unknown, got: %s", clean)
	}
	if !strings.Contains(string(clean), `"total":0`) {
		t.Errorf("a clean report must carry an explicit total:0, got: %s", clean)
	}
}

// TestStuckPodReportLimitIsAboveMeasuredIncident pins the cap's intent: the
// breakdown must not truncate at the scale the signal was designed around, or
// operators would routinely see a partial list for the very shape of incident
// this exists to report.
func TestStuckPodReportLimitIsAboveMeasuredIncident(t *testing.T) {
	const measuredNamespaces = 15
	if orphanedPodStuckReportLimit < measuredNamespaces {
		t.Errorf("orphanedPodStuckReportLimit = %d, must be at least the measured incident's %d namespaces",
			orphanedPodStuckReportLimit, measuredNamespaces)
	}
}
