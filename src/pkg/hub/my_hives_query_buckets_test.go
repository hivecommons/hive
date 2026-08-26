package hub

import "testing"

// The bucketed status filters and the remaining sort keys are what the
// dashboard's filter chips actually send; each must scope the wire set.
func TestApplyMyHivesQueryBucketFilters(t *testing.T) {
	entries := testMyHivesEntries()
	entries[0].AdvisoryIssueActivity.Bucket = advisoryIssueBucketFresh
	entries[1].AdvisoryIssueActivity.Bucket = advisoryIssueBucketStale
	entries[2].BudgetHealth.Bucket = budgetBucketCritical
	entries[3].BudgetHealth.Bucket = budgetBucketOK

	cases := []struct {
		query string
		want  int
	}{
		{"status=advisory-fresh", 1},
		{"status=advisory-stale", 1},
		{"status=advisory-aging", 0},
		{"status=budget-critical", 1},
		{"status=budget-ok", 1},
		{"status=budget-exhausted", 0},
		{"status=offline", 2},
		{"status=provisioning", 1},
		{"q=no-such-hive", 0},
	}
	for _, c := range cases {
		if _, matched := applyMyHivesQuery(entries, mhq(t, c.query)); matched != c.want {
			t.Errorf("%s matched %d, want %d", c.query, matched, c.want)
		}
	}
}

func TestApplyMyHivesQuerySortKeys(t *testing.T) {
	entries := testMyHivesEntries()
	entries[0].Name = "zeta" // name sort must use Name when set, ID otherwise
	entries[2].AdvisoryIssueActivity.LastActivityAt = "2026-08-20T09:00:00Z"
	entries[3].AdvisoryIssueActivity.LastActivityAt = "2026-08-21T09:00:00Z"
	entries[1].BudgetHealth.PercentUsed = 90
	entries[2].BudgetHealth.PercentUsed = 10

	first := func(query string) string {
		view, _ := applyMyHivesQuery(entries, mhq(t, query))
		if len(view) == 0 {
			t.Fatalf("%s returned no rows", query)
		}
		return view[0].ID
	}

	if got := first("sort=name"); got != "hosted-beta" {
		// beta has no Name so its ID "hosted-beta" sorts before "hosted-delta"/"hosted-gamma"/"zeta"
		t.Errorf("sort=name first = %s, want hosted-beta", got)
	}
	if got := first("sort=name&order=desc"); got != "hosted-alpha" {
		t.Errorf("sort=name desc first = %s, want hosted-alpha (Name zeta)", got)
	}
	if got := first("sort=org"); got == "" {
		t.Error("sort=org returned empty first ID")
	}
	if got := first("sort=owner"); got != "hosted-alpha" && got != "hosted-gamma" {
		t.Errorf("sort=owner first = %s, want an alice-owned hive", got)
	}
	if got := first("sort=cluster"); got != "hosted-gamma" {
		// gamma/delta carry no ClusterName, and "\x00"+ID sorts before any
		// named cluster; stable sort keeps gamma (earlier input) first.
		t.Errorf("sort=cluster first = %s, want hosted-gamma (unnamed sorts first)", got)
	}
	if got := first("sort=cluster&order=desc"); got != "hosted-alpha" {
		t.Errorf("sort=cluster desc first = %s, want hosted-alpha (VLLM Dallas)", got)
	}
	if got := first("sort=status"); got == "" {
		t.Error("sort=status returned empty first ID")
	}
	if got := first("sort=advisory_activity&order=desc"); got != "hosted-delta" {
		t.Errorf("sort=advisory_activity desc first = %s, want hosted-delta", got)
	}
	if got := first("sort=budget&order=desc"); got != "hosted-beta" {
		t.Errorf("sort=budget desc first = %s, want hosted-beta (90%% used)", got)
	}
	// Unknown sort key: stable no-op ordering rather than a crash.
	if got := first("sort=bogus"); got != "hosted-alpha" {
		t.Errorf("sort=bogus first = %s, want original order (hosted-alpha)", got)
	}
}

func TestMyHivesSummaryBuckets(t *testing.T) {
	entries := testMyHivesEntries()
	entries[0].AdvisoryIssueActivity.Bucket = advisoryIssueBucketFresh
	entries[1].AdvisoryIssueActivity.Bucket = advisoryIssueBucketAging
	entries[2].AdvisoryIssueActivity.Bucket = advisoryIssueBucketStale
	entries[0].BudgetHealth.Bucket = budgetBucketOK
	entries[1].BudgetHealth.Bucket = budgetBucketWarning
	entries[2].BudgetHealth.Bucket = budgetBucketCritical
	entries[3].BudgetHealth.Bucket = budgetBucketExhausted
	entries[0].GitHubAppHealth.Bucket = ghAppBucketOK
	entries[1].GitHubAppHealth.Bucket = ghAppBucketDegraded
	entries[2].GitHubAppHealth.Bucket = ghAppBucketBroken
	entries[2].ProvStatus = "error"
	entries[3].Unassigned = true
	entries[3].UpgradeFailed = true

	s := myHivesSummary(entries)
	want := map[string]int{
		"advisory_issue_fresh": 1, "advisory_issue_aging": 1,
		"advisory_issue_stale": 1, "advisory_issue_unknown": 1,
		"budget_ok": 1, "budget_warning": 1, "budget_critical": 1, "budget_exhausted": 1,
		"github_app_ok": 1, "github_app_degraded": 1, "github_app_broken": 1, "github_app_unknown": 1,
		"errors": 1, "unassigned": 1, "upgrade_failed": 1,
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("summary[%s] = %d, want %d", k, s[k], v)
		}
	}
}
