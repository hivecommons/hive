package dashboard

import (
	"strings"
	"testing"
)

func TestContributeOpsTableSortStaticWiring9040(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`var OPS_SORT_KEY='hive.ops.tableSort'`,
		`function opsSortableTable`,
		`function opsSortRows`,
		`function opsSortNextState`,
		`localStorage.getItem(OPS_SORT_KEY)`,
		`localStorage.setItem(OPS_SORT_KEY`,
		`data-ops-sort-table`,
		`class="ops-sort-btn"`,
		`aria-sort`,
		`opsSortEmpty`,
		`return ae?1:-1`,
		`type==='number'||type==='percent'`,
		`var first=(type==='number'||type==='percent')?'desc':'asc'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing sortable Operations wiring %q", want)
		}
	}

	for _, want := range []string{
		`opsSortableTable('effective-models'`,
		`opsSortableTable('leaderboard'`,
		`opsSortableTable('admin-tier-limits'`,
		`renderEffectiveModels(effectiveModelsLastData)`,
		`renderLeaderboard(lbLastData.contribs)`,
		`renderAdminTierLimits()`,
		`if(opsSortRead('leaderboard').col==='trend'&&lbLastData)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing sortable table hookup %q", want)
		}
	}
}

func TestContributeOpsTableSortColumns9040(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`sortable.header('model','Model / CLI')`,
		`sortable.header('merged_prs','Merged PRs')`,
		`sortable.header('first_pass_merge_rate','First-pass')`,
		`sortable.header('contributor','Contributor')`,
		`sortable.header('done','Done')`,
		`sortable.header('findings','Findings')`,
		`sortable.header('trend','Trend')`,
		`sortable.header('tier','Tier')`,
		`sortable.header('max_per_hour','Per hr')`,
		`sortable.header('max_per_day','Per day')`,
		`sortable.header('max_concurrent','Concurr.')`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing sortable column %q", want)
		}
	}

	if !strings.Contains(body, `return ae?1:-1`) || !strings.Contains(body, `return an==null?1:-1`) {
		t.Error("sort comparator must keep empty text and numeric values last")
	}

	if !strings.Contains(body, `rank:Number(e.rank||0)||i+1`) {
		t.Error("leaderboard sorting must preserve each contributor's original rank number")
	}
	if !strings.Contains(body, `opsLeaderboardTrendValue(r.entry.github_username)`) {
		t.Error("trend sorting must use leaderboard metric data, not a placeholder column")
	}
	if !strings.Contains(body, `lbRow(sortable.rows[i].entry,sortable.rows[i].rank)`) {
		t.Error("leaderboard rows must render their original rank after sorting")
	}
}
