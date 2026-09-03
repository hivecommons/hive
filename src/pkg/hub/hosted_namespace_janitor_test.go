package hub

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"text/template"
	"time"
)

// ============================================================================
// THE INVARIANT: the janitor NEVER selects a namespace lacking the ephemeral
// label.
// ============================================================================
//
// This is the guard that makes an unattended background deleter safe, so it is
// asserted as a PROPERTY over randomised inputs rather than by example. The
// generator deliberately produces namespaces that are attractive in every
// OTHER dimension — ancient, unregistered, not Terminating, carrying the owner
// label, carrying the hosted name prefix, carrying near-miss values of the
// ephemeral label itself — so the only thing standing between them and
// deletion is the clause under test. An example-based test would pass against
// an implementation that checked, say, the name prefix instead; this one does
// not.
//
// TestInvariantCanFail below proves the property has teeth by running the same
// generator against a deliberately loosened predicate and requiring it to be
// caught.

// nearMissEphemeralValues are values that are NOT the exact string "true" and
// must therefore never authorise a delete. Case variants are included on
// purpose: Kubernetes label values are case-sensitive, and an implementation
// that folded case here would be silently widening the invariant.
var nearMissEphemeralValues = []string{
	"", "false", "True", "TRUE", "yes", "1", "on", "ephemeral", " true", "true ", "tru",
}

// generateUnlabelledCandidate builds a namespace that is maximally attractive
// to the reaper EXCEPT that it lacks a valid ephemeral=true label.
func generateUnlabelledCandidate(rnd *rand.Rand, now time.Time) hostedNamespaceCandidate {
	labels := map[string]string{}
	switch rnd.Intn(4) {
	case 0:
		// No labels at all — an operator's hand-made namespace.
	case 1:
		// Unrelated labels only.
		labels["app"] = "something"
		labels["kubernetes.io/metadata.name"] = "whatever"
	case 2:
		// Carries the OWNER label but not the ephemeral one. This is the
		// tempting near-miss: it looks hub-made, and is still out of scope.
		labels[hostedNamespaceOwnerLabel] = hostedNamespaceOwnerValue
	case 3:
		// Carries the ephemeral KEY with a non-"true" value.
		labels[hostedNamespaceEphemeralLabel] = nearMissEphemeralValues[rnd.Intn(len(nearMissEphemeralValues))]
	}

	// Names that look exactly like the ones the leak produced, so a
	// prefix-matching implementation would happily select them.
	names := []string{
		hiveHostedNamespacePrefix + "hosted-available-cluster-01-placeholder-abcd",
		hiveHostedNamespacePrefix + "hosted-example-org-project-wxyz",
		hiveHostedNamespacePrefix + "scratch",
		"kube-system",
		"default",
		"operator-made-namespace",
	}

	return hostedNamespaceCandidate{
		Name:   names[rnd.Intn(len(names))],
		Labels: labels,
		// Ancient: the age clause can never be what spares it.
		CreatedAt: now.Add(-time.Duration(1+rnd.Intn(1000)) * time.Hour),
		Phase:     "Active",
	}
}

// TestJanitorNeverSelectsUnlabelledNamespace is THE invariant test.
func TestJanitorNeverSelectsUnlabelledNamespace(t *testing.T) {
	now := time.Now()
	rnd := rand.New(rand.NewSource(20260903))

	// A registry that knows about nothing, so the registry clause can never be
	// what spares these either. Non-empty so the sweep's own empty-registry
	// guard is not what is being exercised here.
	registered := map[string]bool{"some-unrelated-live-hive": true}

	const iterations = 2000
	for i := 0; i < iterations; i++ {
		ns := generateUnlabelledCandidate(rnd, now)
		if namespaceIsReapableLeak(ns, registered, now, hostedNamespaceLeakMinAge) {
			t.Fatalf("INVARIANT VIOLATED: janitor selected a namespace without %s=true\n"+
				"  name:   %q\n  labels: %v\n  age:    %s\n"+
				"A namespace the hub did not stamp as ephemeral is not ours to delete.",
				hostedNamespaceEphemeralLabel, ns.Name, ns.Labels, now.Sub(ns.CreatedAt))
		}
	}
}

// TestInvariantCanFail proves the property above is capable of failing.
//
// A property test that cannot fail is worse than no test: it reports safety it
// never checked. This runs the SAME generator against a deliberately loosened
// predicate — one that selects on the "hive-hosted-" name prefix, the exact
// mistake the real implementation is written to avoid — and requires the
// generator to catch it. If this ever stops finding a violation, the generator
// has drifted into producing only inputs that no plausible bug would select,
// and TestJanitorNeverSelectsUnlabelledNamespace has quietly stopped proving
// anything.
func TestInvariantCanFail(t *testing.T) {
	now := time.Now()
	rnd := rand.New(rand.NewSource(20260903))
	registered := map[string]bool{"some-unrelated-live-hive": true}

	// The bug this guards against: selecting by NAME instead of by LABEL.
	loosened := func(ns hostedNamespaceCandidate) bool {
		if !strings.HasPrefix(ns.Name, hiveHostedNamespacePrefix) {
			return false
		}
		if registered[hiveIDFromHostedNamespace(ns.Name)] {
			return false
		}
		if ns.CreatedAt.IsZero() {
			return false
		}
		return now.Sub(ns.CreatedAt) > hostedNamespaceLeakMinAge
	}

	caught := false
	for i := 0; i < 2000; i++ {
		ns := generateUnlabelledCandidate(rnd, now)
		if loosened(ns) && !namespaceIsReapableLeak(ns, registered, now, hostedNamespaceLeakMinAge) {
			caught = true
			break
		}
	}
	if !caught {
		t.Fatal("the invariant property never caught a prefix-matching predicate — " +
			"the generator no longer produces inputs that distinguish label-matching " +
			"from name-matching, so the invariant test proves nothing")
	}
}

// TestJanitorSparesRegisteredHives asserts the second guard: a namespace whose
// hive is in the registry is a LIVE SPOKE and is never selected, even though it
// carries the ephemeral label (every hosted namespace does) and is arbitrarily
// old. Without this clause the sweep would select the entire healthy fleet.
func TestJanitorSparesRegisteredHives(t *testing.T) {
	now := time.Now()
	registered := map[string]bool{"hosted-live-org-project-abcd": true}

	live := hostedNamespaceCandidate{
		Name:      hiveHostedNamespacePrefix + "hosted-live-org-project-abcd",
		Labels:    map[string]string{hostedNamespaceEphemeralLabel: "true"},
		CreatedAt: now.Add(-30 * 24 * time.Hour), // a month old
		Phase:     "Active",
	}
	if namespaceIsReapableLeak(live, registered, now, hostedNamespaceLeakMinAge) {
		t.Fatal("janitor selected a namespace whose hive IS in the registry — " +
			"that is a running spoke, not a leak")
	}

	// The same namespace with its registry entry gone IS a leak.
	if !namespaceIsReapableLeak(live, map[string]bool{"other": true}, now, hostedNamespaceLeakMinAge) {
		t.Fatal("janitor failed to select a labelled, ancient, unregistered namespace — " +
			"that is exactly the leak this sweep exists to reap")
	}
}

// TestJanitorSparesYoungAndTerminating covers the remaining two clauses.
func TestJanitorSparesYoungAndTerminating(t *testing.T) {
	now := time.Now()
	registered := map[string]bool{"unrelated": true}
	base := func() hostedNamespaceCandidate {
		return hostedNamespaceCandidate{
			Name:      hiveHostedNamespacePrefix + "hosted-fresh-abcd",
			Labels:    map[string]string{hostedNamespaceEphemeralLabel: "true"},
			CreatedAt: now.Add(-30 * 24 * time.Hour),
			Phase:     "Active",
		}
	}

	// In-flight provisioning: labelled, unregistered, but young. Reaping this
	// would delete a namespace whose spoke is still coming up.
	young := base()
	young.CreatedAt = now.Add(-1 * time.Minute)
	if namespaceIsReapableLeak(young, registered, now, hostedNamespaceLeakMinAge) {
		t.Fatal("janitor selected a namespace younger than the min age — " +
			"provisioning in flight has no registry entry yet and must be spared")
	}

	// Exactly at the boundary is NOT past it (strict >).
	boundary := base()
	boundary.CreatedAt = now.Add(-hostedNamespaceLeakMinAge)
	if namespaceIsReapableLeak(boundary, registered, now, hostedNamespaceLeakMinAge) {
		t.Fatal("janitor selected a namespace exactly at the min age; the bound must be strict")
	}

	// Delete already in flight.
	terminating := base()
	terminating.Phase = namespacePhaseTerminating
	if namespaceIsReapableLeak(terminating, registered, now, hostedNamespaceLeakMinAge) {
		t.Fatal("janitor selected an already-Terminating namespace")
	}

	// Unknown creation time must never be read as "old enough".
	unknownAge := base()
	unknownAge.CreatedAt = time.Time{}
	if namespaceIsReapableLeak(unknownAge, registered, now, hostedNamespaceLeakMinAge) {
		t.Fatal("janitor selected a namespace with an unparseable creation time — " +
			"an unknown age must never be treated as old enough to delete")
	}
}

// TestParseHostedNamespaceCandidates covers the kubectl-output seam, including
// the annotation-preferred / creationTimestamp-fallback age rule and the
// never-guess-an-age behaviour on a bad timestamp.
func TestParseHostedNamespaceCandidates(t *testing.T) {
	raw := []byte(`{"items":[
	  {"metadata":{"name":"hive-hosted-a","labels":{"hive.kubestellar.io/ephemeral":"true"},
	    "annotations":{"hive.kubestellar.io/created-at":"2026-01-02T03:04:05Z"},
	    "creationTimestamp":"2026-06-01T00:00:00Z"},"status":{"phase":"Active"}},
	  {"metadata":{"name":"hive-hosted-b","labels":{},
	    "creationTimestamp":"2026-06-01T00:00:00Z"},"status":{"phase":"Active"}},
	  {"metadata":{"name":"hive-hosted-c","annotations":{"hive.kubestellar.io/created-at":"not-a-time"},
	    "creationTimestamp":"garbage"},"status":{"phase":"Terminating"}}
	]}`)

	got, err := parseHostedNamespaceCandidates(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(got))
	}

	// The hub's own stamp wins over creationTimestamp.
	wantA := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !got[0].CreatedAt.Equal(wantA) {
		t.Errorf("annotation stamp should win over creationTimestamp: got %v want %v", got[0].CreatedAt, wantA)
	}
	// Fallback to creationTimestamp when the annotation is absent.
	wantB := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !got[1].CreatedAt.Equal(wantB) {
		t.Errorf("should fall back to creationTimestamp: got %v want %v", got[1].CreatedAt, wantB)
	}
	// Both unparseable — zero, which the predicate rejects.
	if !got[2].CreatedAt.IsZero() {
		t.Errorf("unparseable timestamps must yield a zero time, got %v", got[2].CreatedAt)
	}
	if got[2].Phase != namespacePhaseTerminating {
		t.Errorf("phase not carried through: %q", got[2].Phase)
	}

	if _, err := parseHostedNamespaceCandidates([]byte("not json")); err == nil {
		t.Error("expected an error on malformed kubectl output")
	}
}

// TestSelectLeakedHostedNamespaces exercises selection over a mixed list — the
// shape a real cluster returns.
func TestSelectLeakedHostedNamespaces(t *testing.T) {
	now := time.Now()
	old := now.Add(-24 * time.Hour)
	registered := map[string]bool{"live-one": true}

	candidates := []hostedNamespaceCandidate{
		// Leak: labelled, old, unregistered.
		{Name: hiveHostedNamespacePrefix + "leak-one", Labels: map[string]string{hostedNamespaceEphemeralLabel: "true"}, CreatedAt: old, Phase: "Active"},
		// Live spoke.
		{Name: hiveHostedNamespacePrefix + "live-one", Labels: map[string]string{hostedNamespaceEphemeralLabel: "true"}, CreatedAt: old, Phase: "Active"},
		// Unlabelled — never.
		{Name: hiveHostedNamespacePrefix + "operator-scratch", CreatedAt: old, Phase: "Active"},
		// Another leak.
		{Name: hiveHostedNamespacePrefix + "leak-two", Labels: map[string]string{hostedNamespaceEphemeralLabel: "true"}, CreatedAt: old, Phase: "Active"},
	}

	got := selectLeakedHostedNamespaces(candidates, registered, now, hostedNamespaceLeakMinAge)
	if len(got) != 2 {
		t.Fatalf("want 2 leaks, got %d: %+v", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name}
	for _, want := range []string{hiveHostedNamespacePrefix + "leak-one", hiveHostedNamespacePrefix + "leak-two"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q among selected leaks, got %v", want, names)
		}
	}
}

// TestHiveIDFromHostedNamespace covers the registry-key derivation, including
// the case that matters most: a name without the prefix yields "", which can
// never match a registry entry and so never marks a namespace as "live".
func TestHiveIDFromHostedNamespace(t *testing.T) {
	cases := map[string]string{
		hiveHostedNamespacePrefix + "hosted-org-repo-abcd": "hosted-org-repo-abcd",
		hiveHostedNamespacePrefix:                          "",
		"kube-system":                                      "",
		"":                                                 "",
		"xhive-hosted-a":                                   "",
	}
	for in, want := range cases {
		if got := hiveIDFromHostedNamespace(in); got != want {
			t.Errorf("hiveIDFromHostedNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHostedNamespaceJanitorDryRun covers the report-only toggle, which is how
// the sweep gets validated against a real cluster before it deletes anything.
func TestHostedNamespaceJanitorDryRun(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		t.Setenv(hostedNamespaceJanitorDryRunEnv, v)
		if !hostedNamespaceJanitorDryRun() {
			t.Errorf("value %q should enable dry run", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Setenv(hostedNamespaceJanitorDryRunEnv, v)
		if hostedNamespaceJanitorDryRun() {
			t.Errorf("value %q should NOT enable dry run", v)
		}
	}
}

// TestProvisioningManifestStampsOwnershipLabels asserts the labels the janitor
// selects on are actually emitted by the provisioning manifest — at CREATION,
// in the same apply that makes the namespace.
//
// This is the coupling that makes the whole design work: if the namespace were
// labelled by a follow-up `kubectl label` call, the label would be missing on
// exactly the failed-provision namespaces the janitor exists to find, and the
// sweep would be permanently unable to see its own leaks.
func TestProvisioningManifestStampsOwnershipLabels(t *testing.T) {
	tmpl, err := template.New("m").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}
	created := "2026-09-03T00:00:00Z"
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"Namespace": "hive-hosted-hosted-test-abcd",
		"CreatedAt": created,
	}); err != nil {
		t.Fatalf("template exec: %v", err)
	}

	// Only the first document — the Namespace object.
	nsDoc := buf.String()
	if i := strings.Index(nsDoc, "\n---"); i > 0 {
		nsDoc = nsDoc[:i]
	}

	for _, want := range []string{
		fmt.Sprintf("%s: %s", hostedNamespaceOwnerLabel, hostedNamespaceOwnerValue),
		fmt.Sprintf("%s: \"true\"", hostedNamespaceEphemeralLabel),
		fmt.Sprintf("%s: \"%s\"", hostedNamespaceCreatedAtAnnotation, created),
	} {
		if !strings.Contains(nsDoc, want) {
			t.Errorf("namespace manifest is missing %q; the janitor cannot identify\n"+
				"its own leaked namespaces without it.\ngot:\n%s", want, nsDoc)
		}
	}

	// And the rendered namespace must actually be selectable by the janitor.
	parsedCreated, _ := time.Parse(time.RFC3339, created)
	ns := hostedNamespaceCandidate{
		Name:      "hive-hosted-hosted-test-abcd",
		Labels:    map[string]string{hostedNamespaceEphemeralLabel: "true"},
		CreatedAt: parsedCreated,
		Phase:     "Active",
	}
	if !namespaceIsReapableLeak(ns, map[string]bool{"other": true}, parsedCreated.Add(48*time.Hour), hostedNamespaceLeakMinAge) {
		t.Error("a namespace created by the provisioning manifest is not selectable by the " +
			"janitor once orphaned — the label the manifest stamps and the label the sweep " +
			"selects on have diverged")
	}
}
