package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedLeakNow is the clock every case here is evaluated against, so ages are
// exact rather than relative to a running wall clock.
var fixedLeakNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func leakAgo(d time.Duration) time.Time { return fixedLeakNow.Add(-d) }

// nsCandidate builds a candidate with an explicit creation time.
func nsCandidate(name, phase string, created time.Time) hostedNamespaceCandidate {
	return hostedNamespaceCandidate{Name: name, Phase: phase, CreationTimestamp: created}
}

// TestSummarizeLeakedHostedNamespacesCountsRealPredicateMatches is the
// load-bearing test for #5768 ask 3.
//
// Per the repo's #5388 rule, a guard that asserts a FIELD EXISTS is a guard
// that fails green. This test therefore never checks "is LeakedNamespaces
// non-nil" — it builds a namespace population whose leak count is known BY
// CONSTRUCTION and asserts the reported number equals that count and no other.
// The population deliberately mixes in every namespace shape a LOOSER
// predicate would wrongly convict: one the registry knows about, one already
// Terminating, one inside the age window, one at exactly the age boundary, one
// with an unreadable creationTimestamp, and one that is not a hive namespace at
// all. If the count ever picks up any of those, the number moves and this
// fails.
func TestSummarizeLeakedHostedNamespacesCountsRealPredicateMatches(t *testing.T) {
	known := map[string]struct{}{
		hiveHostedNamespacePrefix + "live-1": {},
		hiveHostedNamespacePrefix + "live-2": {},
	}

	candidates := []hostedNamespaceCandidate{
		// --- three genuine leaks -----------------------------------------
		nsCandidate(hiveHostedNamespacePrefix+"hosted-available-oke-03-placeholder-y99x", "Active", leakAgo(30*24*time.Hour)),
		nsCandidate(hiveHostedNamespacePrefix+"leak-b", "Active", leakAgo(40*time.Hour)),
		// kubectl can report a namespace with no status.phase; an empty phase
		// is not "Terminating" and must not be read as one.
		nsCandidate(hiveHostedNamespacePrefix+"leak-c", "", leakAgo(7*time.Hour)),

		// --- namespaces that MUST NOT be counted --------------------------
		// The hub HAS a hive for these. Convicting them is the failure mode
		// that turns a detector into a fleet-wide false alarm.
		nsCandidate(hiveHostedNamespacePrefix+"live-1", "Active", leakAgo(30*24*time.Hour)),
		nsCandidate(hiveHostedNamespacePrefix+"live-2", "Active", leakAgo(30*24*time.Hour)),
		// Already being cleaned up — something IS deleting it.
		nsCandidate(hiveHostedNamespacePrefix+"terminating", namespacePhaseTerminating, leakAgo(30*24*time.Hour)),
		// Inside the grace window: a provision in flight whose record has not
		// landed yet.
		nsCandidate(hiveHostedNamespacePrefix+"young", "Active", leakAgo(10*time.Minute)),
		// EXACTLY at the boundary. The predicate is a strict >, so this is not
		// a leak; an off-by-one to >= would show up here and nowhere else.
		nsCandidate(hiveHostedNamespacePrefix+"boundary", "Active", leakAgo(leakedNamespaceMinAge)),
		// No usable creationTimestamp — an age can never be guessed into guilt.
		nsCandidate(hiveHostedNamespacePrefix+"no-timestamp", "Active", time.Time{}),
		// Not ours. Reporting other tenants' namespaces makes the signal
		// unactionable, the same scoping rule summarizeStuckPods applies.
		nsCandidate("kube-system", "Active", leakAgo(365*24*time.Hour)),
		nsCandidate("openshift-ingress", "Active", leakAgo(365*24*time.Hour)),
	}

	rep := summarizeLeakedHostedNamespaces(candidates, known, fixedLeakNow, leakedNamespaceMinAge)

	if rep.Total != 3 {
		t.Fatalf("Total = %d, want 3 — the population contains exactly three leaked namespaces; report: %+v", rep.Total, rep.Namespaces)
	}
	if rep.Truncated {
		t.Errorf("Truncated = true for a 3-entry report, which would tell a reader the list is short when it is complete")
	}
	if len(rep.Namespaces) != 3 {
		t.Fatalf("len(Namespaces) = %d, want 3", len(rep.Namespaces))
	}

	// Oldest first: the 30-day leak, then 40h, then 7h. The order is what
	// survives truncation, so it is asserted rather than assumed.
	wantOrder := []string{
		hiveHostedNamespacePrefix + "hosted-available-oke-03-placeholder-y99x",
		hiveHostedNamespacePrefix + "leak-b",
		hiveHostedNamespacePrefix + "leak-c",
	}
	for i, want := range wantOrder {
		if rep.Namespaces[i].Namespace != want {
			t.Errorf("Namespaces[%d] = %q, want %q (oldest-first order)", i, rep.Namespaces[i].Namespace, want)
		}
	}
}

// TestNamespaceIsLeakedHostedRejectsEveryNonLeak pins each rejection clause
// individually, so a regression names WHICH clause broke instead of only
// moving an aggregate count.
func TestNamespaceIsLeakedHostedRejectsEveryNonLeak(t *testing.T) {
	known := map[string]struct{}{hiveHostedNamespacePrefix + "known": {}}
	old := leakAgo(30 * 24 * time.Hour)

	cases := []struct {
		name string
		c    hostedNamespaceCandidate
		want bool
		why  string
	}{
		{"leaked", nsCandidate(hiveHostedNamespacePrefix+"gone", "Active", old), true,
			"old, hive-prefixed, unknown to the registry, not terminating"},
		{"not a hive namespace", nsCandidate("kube-system", "Active", old), false,
			"reporting namespaces this fleet does not own makes the signal unactionable"},
		{"registry knows it", nsCandidate(hiveHostedNamespacePrefix+"known", "Active", old), false,
			"a namespace with a hive record is live, not leaked"},
		{"terminating", nsCandidate(hiveHostedNamespacePrefix+"gone", namespacePhaseTerminating, old), false,
			"its delete is already issued; counting it reports cleanup as leakage forever"},
		{"inside the age window", nsCandidate(hiveHostedNamespacePrefix+"gone", "Active", leakAgo(time.Minute)), false,
			"a provision in flight is not a leak"},
		{"unreadable creation time", nsCandidate(hiveHostedNamespacePrefix+"gone", "Active", time.Time{}), false,
			"an unknown age must never be resolved into guilt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namespaceIsLeakedHosted(tc.c, known, fixedLeakNow, leakedNamespaceMinAge)
			if got != tc.want {
				t.Errorf("namespaceIsLeakedHosted(%+v) = %v, want %v — %s", tc.c, got, tc.want, tc.why)
			}
		})
	}
}

// TestSummarizeLeakedHostedNamespacesTruncatesOldestFirst asserts the cap keeps
// the OLDEST entries and marks itself truncated. A cap that silently kept an
// arbitrary 25 would read as a complete list of the wrong namespaces.
func TestSummarizeLeakedHostedNamespacesTruncatesOldestFirst(t *testing.T) {
	const total = leakedNamespaceReportLimit + 12
	var candidates []hostedNamespaceCandidate
	for i := 0; i < total; i++ {
		// Namespace i is (i+1) days old, so higher i is older. Names are
		// deliberately NOT in age order, so a sort that fell back to name order
		// would produce a different list.
		candidates = append(candidates, nsCandidate(
			fmt.Sprintf("%sleak-%02d", hiveHostedNamespacePrefix, i),
			"Active",
			leakAgo(time.Duration(i+1)*24*time.Hour),
		))
	}

	rep := summarizeLeakedHostedNamespaces(candidates, map[string]struct{}{"sentinel": {}}, fixedLeakNow, leakedNamespaceMinAge)

	if rep.Total != total {
		t.Fatalf("Total = %d, want %d — Total is exact and must never be capped", rep.Total, total)
	}
	if len(rep.Namespaces) != leakedNamespaceReportLimit {
		t.Fatalf("len(Namespaces) = %d, want %d", len(rep.Namespaces), leakedNamespaceReportLimit)
	}
	if !rep.Truncated {
		t.Error("Truncated = false on a cut list — a reader would believe they saw every leaked namespace")
	}
	// The oldest is leak-36 (37 days); the cut must start there.
	wantFirst := fmt.Sprintf("%sleak-%02d", hiveHostedNamespacePrefix, total-1)
	if rep.Namespaces[0].Namespace != wantFirst {
		t.Errorf("Namespaces[0] = %q, want %q — truncation must keep the oldest, not an arbitrary slice",
			rep.Namespaces[0].Namespace, wantFirst)
	}
}

// TestParseHostedNamespaceCandidates runs real `kubectl get namespaces -o json`
// shaped output through the parser, including the two shapes that must not be
// guessed at: a malformed creationTimestamp and a namespace with no labels.
func TestParseHostedNamespaceCandidates(t *testing.T) {
	raw := []byte(`{"items":[
	  {"metadata":{"name":"hive-hosted-abc","creationTimestamp":"2026-08-01T10:00:00Z",
	    "labels":{"hive.kubestellar.io/hive-id":"abc","kubernetes.io/metadata.name":"hive-hosted-abc"}},
	   "status":{"phase":"Active"}},
	  {"metadata":{"name":"hive-hosted-nolabels","creationTimestamp":"2026-08-02T10:00:00Z"},
	   "status":{"phase":"Active"}},
	  {"metadata":{"name":"hive-hosted-badtime","creationTimestamp":"not-a-timestamp"},
	   "status":{"phase":"Active"}},
	  {"metadata":{"name":"kube-system","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "status":{"phase":"Active"}}
	]}`)

	got, err := parseHostedNamespaceCandidates(raw)
	if err != nil {
		t.Fatalf("parseHostedNamespaceCandidates: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("parsed %d namespaces, want 4", len(got))
	}

	if got[0].HiveID != "abc" {
		t.Errorf("HiveID = %q, want %q — the identity label is what lets an operator attribute a leaked namespace", got[0].HiveID, "abc")
	}
	if got[0].CreationTimestamp.IsZero() {
		t.Error("a well-formed creationTimestamp parsed to zero, which the predicate would read as 'never old enough'")
	}
	if got[1].HiveID != "" {
		t.Errorf("HiveID = %q for an unlabelled namespace, want empty — the ABSENCE of the stamp is itself diagnostic", got[1].HiveID)
	}
	if !got[2].CreationTimestamp.IsZero() {
		t.Error("an unparseable creationTimestamp must yield the zero time so the predicate skips it, never a guessed age")
	}
}

// TestRegistryHostedNamespacesMatchesReaperDerivation asserts the known set is
// derived exactly the way every other hosted-namespace computation in the
// package derives a namespace name. Two derivations that drift would make the
// detector convict live hives.
func TestRegistryHostedNamespacesMatchesReaperDerivation(t *testing.T) {
	hives := []SaaSHive{{ID: "alpha"}, {ID: "hosted-available-oke-03-placeholder-y99x"}}
	known := registryHostedNamespaces(hives)

	for i := range hives {
		ns := hostedNamespaceForHive(&hives[i])
		if _, ok := known[ns]; !ok {
			t.Errorf("registryHostedNamespaces omitted %q, which hostedNamespaceForHive derives for hive %q", ns, hives[i].ID)
		}
	}
	if len(known) != 2 {
		t.Errorf("len(known) = %d, want 2", len(known))
	}
}

// TestHostedNamespacesForHiveIDsSkipsBlankIDs asserts a blank hive id never
// becomes the bare prefix "hive-hosted-". Such an entry would be harmless in
// the set but is a symptom worth failing on: it means an id reached the health
// path empty.
func TestHostedNamespacesForHiveIDsSkipsBlankIDs(t *testing.T) {
	known := hostedNamespacesForHiveIDs(map[string]bool{"alpha": true, "": true, "   ": true})
	if _, bad := known[hiveHostedNamespacePrefix]; bad {
		t.Error("a blank hive id produced the bare prefix as a namespace name")
	}
	if len(known) != 1 {
		t.Errorf("len(known) = %d, want 1 (only the real id)", len(known))
	}
}

// installNamespaceKubectl writes a fake kubectl that answers
// `get namespaces -o json` with the supplied document and prepends it to PATH.
func installNamespaceKubectl(t *testing.T, doc string) {
	t.Helper()
	dir := t.TempDir()
	docPath := filepath.Join(dir, "namespaces.json")
	if err := os.WriteFile(docPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$*\" in\n  *\"get namespaces\"*)\n    exec cat " + docPath + "\n    ;;\n  *)\n    echo '{}'\n    ;;\nesac\nexit 0\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// namespaceListDoc renders a kubectl namespace list with the given
// name -> age entries, all Active.
func namespaceListDoc(t *testing.T, ages map[string]time.Duration) string {
	t.Helper()
	type item struct {
		Metadata struct {
			Name              string `json:"name"`
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	var doc struct {
		Items []item `json:"items"`
	}
	for name, age := range ages {
		var it item
		it.Metadata.Name = name
		it.Metadata.CreationTimestamp = fixedLeakNow.Add(-age).Format(time.RFC3339)
		it.Status.Phase = "Active"
		doc.Items = append(doc.Items, it)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCollectLeakedHostedNamespacesRefusesToReportOnAnEmptyRegistry is the
// safety test for the guard the whole design rests on.
//
// listSaaSHives() returns nil both when the hub genuinely hosts nothing AND
// when it cannot read the hive directory at all. Under an empty known set every
// hosted namespace on the cluster satisfies "has no registry entry", so a
// detector without this guard would report the entire fleet as leaked the first
// time a disk read hiccuped — and would hand a future janitor a delete list
// containing every live spoke.
//
// The test is written so it cannot pass vacuously: the SAME fake cluster, with
// a non-empty known set, must report the leaks. If the guard were removed the
// first assertion fails; if collection were broken entirely the second one
// does.
func TestCollectLeakedHostedNamespacesRefusesToReportOnAnEmptyRegistry(t *testing.T) {
	doc := namespaceListDoc(t, map[string]time.Duration{
		hiveHostedNamespacePrefix + "live":   30 * 24 * time.Hour,
		hiveHostedNamespacePrefix + "leak-a": 30 * 24 * time.Hour,
		hiveHostedNamespacePrefix + "leak-b": 30 * 24 * time.Hour,
		"kube-system":                        365 * 24 * time.Hour,
	})
	installNamespaceKubectl(t, doc)
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}

	ctx := context.Background()
	if rep := collectLeakedHostedNamespaces(ctx, cluster, time.Second, nil, fixedLeakNow, slog.Default()); rep != nil {
		t.Fatalf("an empty registry produced a report of %d leaked namespaces — an unreadable hive directory is indistinguishable from an empty one, and this would convict the whole fleet", rep.Total)
	}

	known := map[string]struct{}{hiveHostedNamespacePrefix + "live": {}}
	rep := collectLeakedHostedNamespaces(ctx, cluster, time.Second, known, fixedLeakNow, slog.Default())
	if rep == nil {
		t.Fatal("collect returned nil with a populated registry — the guard above would then be vacuous")
	}
	if rep.Total != 2 {
		t.Fatalf("Total = %d, want 2 (leak-a and leak-b; 'live' is known and kube-system is not ours)", rep.Total)
	}
}

// TestCollectLeakedHostedNamespacesUnknownForUnreachableCluster asserts a
// pull-only cluster reports UNKNOWN rather than a reassuring zero. The hub has
// no kubectl path into such a pool, so "0 leaks" there would be a fabricated
// clean bill of health.
func TestCollectLeakedHostedNamespacesUnknownForUnreachableCluster(t *testing.T) {
	cluster := &ClusterConfig{ID: "a-ks-wec2", PullOnly: true}
	known := map[string]struct{}{hiveHostedNamespacePrefix + "live": {}}
	if rep := collectLeakedHostedNamespaces(context.Background(), cluster, time.Second, known, fixedLeakNow, slog.Default()); rep != nil {
		t.Errorf("pull-only cluster reported %+v, want nil (unknown) — the hub cannot run kubectl there at all", rep)
	}
}

// TestLeakedNamespaceReportJSONOmitsEmptyList asserts the wire shape a console
// reads: a clean cluster serialises as a total of zero with no list, and never
// as a null-vs-absent ambiguity in the entries.
func TestLeakedNamespaceReportJSONOmitsEmptyList(t *testing.T) {
	b, err := json.Marshal(LeakedNamespaceReport{Total: 0})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "namespaces") {
		t.Errorf("clean report serialised as %s — the empty breakdown must be omitted, not rendered as null", b)
	}
	b, err = json.Marshal(LeakedNamespaceReport{
		Total:      1,
		Namespaces: []LeakedNamespace{{Namespace: "hive-hosted-x", Age: "40h0m0s"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hive_id") {
		t.Errorf("an unstamped namespace serialised %s — hive_id must be omitted rather than sent as an empty string", b)
	}
}
