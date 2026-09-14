package github

import "testing"

// The six titles below are the real, simultaneously-open kubestellar/console
// issues #23116/#23121/#23128/#23147/#23150/#23200 — one 1237-line file filed
// six times because each scan appended a different model-authored qualifier
// and dedupe compared exact titles only.
func TestCanonicalIssueSubject_CollapsesModelAuthoredQualifiers(t *testing.T) {
	const base = "[scanner] Split web/e2e/compliance/card-cache-compliance.spec.ts (1237 lines)"
	variants := []string{
		base,
		base + " — extract cache helpers module",
		base + " — extract helpers module",
		base + " — extract snapshot/report helpers",
		base + " — extract snapshot/storage helpers",
		base + " — extract config constants and snapshot-capture helpers",
		base + " -- extract helpers module",
		base + " – extract helpers module",
		"  " + base + "   ",
	}
	want := canonicalIssueSubject(base)
	if want == "" {
		t.Fatalf("base title produced no canonical subject")
	}
	for _, v := range variants {
		if got := canonicalIssueSubject(v); got != want {
			t.Errorf("canonicalIssueSubject(%q)\n got %q\nwant %q", v, got, want)
		}
	}
}

func TestCanonicalIssueSubject_KeepsDistinctSubjectsApart(t *testing.T) {
	cases := [][2]string{
		{
			"[scanner] Split pkg/agent/kagent/kagent.go (1252 lines)",
			"[scanner] Split pkg/agent/kube/client.go (884 lines)",
		},
		{
			// Different line counts are a different finding.
			"[scanner] Split pkg/agent/kagent/kagent.go (1252 lines)",
			"[scanner] Split pkg/agent/kagent/kagent.go (640 lines)",
		},
		{
			// A path-qualified title must not collapse onto a bare filename.
			"[scanner] Split web/src/components/dashboard/ConfigureCardModal.tsx (541 lines)",
			"[scanner] Split ConfigureCardModal.tsx (541 lines)",
		},
	}
	for _, c := range cases {
		a, b := canonicalIssueSubject(c[0]), canonicalIssueSubject(c[1])
		if a == b {
			t.Errorf("distinct findings collapsed:\n %q\n %q\n both -> %q", c[0], c[1], a)
		}
	}
}

// Too-short or too-generic stems must not become dedupe keys, or unrelated
// findings would be silently folded into one issue.
func TestCanonicalIssueSubject_RejectsGenericStems(t *testing.T) {
	for _, title := range []string{
		"",
		"   ",
		"CI failure",
		"[ops] CI failure — GA4 error monitor run 123 failed",
		"Bug — the dashboard renders the wrong total for the deployments card",
	} {
		if got := canonicalIssueSubject(title); got != "" {
			t.Errorf("canonicalIssueSubject(%q) = %q, want \"\" (unsafe as a dedupe key)", title, got)
		}
	}
}
