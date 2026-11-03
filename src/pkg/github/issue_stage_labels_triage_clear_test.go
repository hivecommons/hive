package github

import (
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14586: a hive-filed self-report wears `needs-triage` from
// filing; the scanner claims it from 015 (not a gate) and the label survived
// the 015 → 020 write, so a claimed — then closed — issue still read "Triage
// needed". The sweep now takes `needs-triage` off in the same write as the
// stage, and reconciles an issue already at 020 that still wears it.
func TestSyncIssueStageLabels_ClaimClearsNeedsTriage(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	result := &ActionableResult{}
	// 14586: direct claim from 015.
	appendRepoWorkIssueDetail(result, Issue{
		Repo: "acme/widgets", Number: 14586, Author: "hive-app[bot]",
		Labels:    []string{"kind/bug", triageAwaitingLabel, stagelabels.TriageNeeded.Label(), "hive/claimed-by-scanner"},
		ClaimKind: string(claims.KindAgent),
	}, workBucketClaimed, "live claim")
	// 14587: already 020 and correct, but still wearing the stale label.
	appendRepoWorkIssueDetail(result, Issue{
		Repo: "acme/widgets", Number: 14587, Author: "hive-app[bot]",
		Labels:    []string{triageAwaitingLabel, stagelabels.Claimed.Label()},
		ClaimKind: string(claims.KindAgent),
	}, workBucketClaimed, "live claim")
	// 14588: untriaged and unclaimed stays 015 and keeps its label.
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 14588, Author: "alice", AuthorIsHuman: true, Labels: []string{triageAwaitingLabel}}})

	res := syncSeeded(t, fake, c, result, stagelabels.NewStore("", stageNow), stageNow())
	if res.Items != 3 {
		t.Fatalf("items = %d, want 3", res.Items)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.added[14586], ","); got != stagelabels.Claimed.Label() {
		t.Fatalf("direct claim must land 020: added=%v", fake.added[14586])
	}
	if !slices.Contains(fake.removed[14586], triageAwaitingLabel) || !slices.Contains(fake.removed[14586], stagelabels.TriageNeeded.Label()) {
		t.Fatalf("015 and needs-triage must come off in the claim write: removed=%v", fake.removed[14586])
	}
	if slices.Contains(fake.removed[14586], "kind/bug") || slices.Contains(fake.removed[14586], "hive/claimed-by-scanner") {
		t.Fatalf("unrelated labels must not be touched: removed=%v", fake.removed[14586])
	}
	if len(fake.added[14587]) != 0 || !slices.Equal(fake.removed[14587], []string{triageAwaitingLabel}) {
		t.Fatalf("already-claimed issue must shed only the stale label: added=%v removed=%v", fake.added[14587], fake.removed[14587])
	}
	if got := strings.Join(fake.added[14588], ","); got != stagelabels.TriageNeeded.Label() || len(fake.removed[14588]) != 0 {
		t.Fatalf("untriaged issue keeps needs-triage: added=%v removed=%v", fake.added[14588], fake.removed[14588])
	}
}
