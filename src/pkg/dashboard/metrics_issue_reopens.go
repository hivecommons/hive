package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func writeIssueReopenMetrics(b *strings.Builder, hiveID string) {
	bundles, _, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot)
	if err != nil {
		return
	}
	writeIssueReopenSeries(b, hiveID, evidence.IssueReopenRates(bundles, time.Now().UTC(), ""))
}

func writeIssueReopenSeries(b *strings.Builder, hiveID string, rows []evidence.ReopenRate) {
	for _, metric := range []struct{ name, help string }{
		{"hive_issue_closures_30d", "Distinct issues closed by hive PRs in the trailing 30 days with captured clarity status."},
		{"hive_issue_reopens_30d", "Issues in the trailing 30-day closure cohort subsequently reopened."},
		{"hive_issue_reopen_rate_30d", "Share of the trailing 30-day issue closure cohort subsequently reopened; absent for an empty cohort."},
	} {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", metric.name, metric.help, metric.name)
		for _, row := range rows {
			var value float64
			switch metric.name {
			case "hive_issue_closures_30d":
				value = float64(row.Closed)
			case "hive_issue_reopens_30d":
				value = float64(row.Reopened)
			default:
				if row.Rate == nil {
					continue
				}
				value = *row.Rate
			}
			fmt.Fprintf(b, "%s{hive_id=%s,repo=%s,clarity_gate=\"%t\"} %g\n", metric.name, promQuote(hiveID), promQuote(row.Repo), row.ClarityGate, value)
		}
	}
}
