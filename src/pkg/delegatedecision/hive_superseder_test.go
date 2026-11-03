package delegatedecision

import (
	"strings"
	"testing"
)

// TestDecideHiveSuperseder: the owner's rule in the delegate — the hive's
// superseding PR is released with no decision; a hard stop is the one
// thing that holds it, and the recommendation names it.
func TestDecideHiveSuperseder(t *testing.T) {
	base := Input{Repo: "hivecommons/hive", Kind: KindPR, Number: 15649, Author: "hivecommons-hive[bot]",
		Body: "Supersedes #15646", HiveAuthored: true, TrustedAuthor: true, HiveSuperseding: true, CIGreen: true,
		Reasons: []string{"verdict:requires_human"}}

	d := Decide(base)
	if d.Verdict != VerdictApprove || d.Rule != RuleHiveSuperseder || !strings.Contains(d.Text, "auto-merging once CI/review are clean") {
		t.Fatalf("decision = %+v, want release under the superseder rule", d)
	}

	stopped := base
	stopped.HiveSupersederHardStop = "sentinel-unacknowledged"
	stopped.SentinelAlert = true
	d = Decide(stopped)
	if d.Verdict != VerdictRecommendOnly || d.Rule != RuleHiveSuperseder || !strings.Contains(d.Text, "sentinel-unacknowledged") {
		t.Fatalf("decision = %+v, want recommend-only naming the stop", d)
	}

	// An unacknowledged sentinel with no stop token recorded still defers
	// to the sentinel rules rather than releasing.
	sentinel := base
	sentinel.SentinelAlert = true
	if d = Decide(sentinel); d.Rule == RuleHiveSuperseder {
		t.Fatalf("decision = %+v, sentinel must not be released by the superseder rule", d)
	}

	plain := base
	plain.HiveSuperseding = false
	if d = Decide(plain); d.Rule == RuleHiveSuperseder {
		t.Fatalf("decision = %+v, a non-superseding PR takes the ordinary rules", d)
	}
}
