package outcomebench

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// NotReported is printed wherever n < MinN.
const NotReported = "not reported"

func pct(r Rate) string {
	if !r.Reported {
		return fmt.Sprintf("%s (n=%d)", NotReported, r.N)
	}
	return fmt.Sprintf("%.1f%% [%.1f–%.1f] (n=%d)", 100*r.Rate, 100*r.Lo, 100*r.Hi, r.N)
}

func hours(m Median) string {
	if !m.Reported {
		return fmt.Sprintf("%s (n=%d)", NotReported, m.N)
	}
	return fmt.Sprintf("%.2f h (n=%d)", m.Value, m.N)
}

func day(t time.Time) string {
	if t.IsZero() {
		return "open"
	}
	return t.UTC().Format("2006-01-02")
}

// Month is the YYYY-MM the report is filed under (its Since month, or the
// generation month when the window is open).
func (r Report) Month() string {
	if !r.Since.IsZero() {
		return r.Since.UTC().Format("2006-01")
	}
	return r.GeneratedAt.UTC().Format("2006-01")
}

// AssetBase is the file stem the charts are written under.
func (r Report) AssetBase() string { return "outcomes-" + r.Month() }

// SplitSupported reports whether both cohorts have at least MinN merged PRs,
// i.e. whether a mediated-vs-unmediated comparison is defensible at all.
func (r Report) SplitSupported() bool {
	return r.Mediated.Merged >= MinN && r.Unmediated.Merged >= MinN
}

// Markdown renders the full report, methodology included.
func (r Report) Markdown() string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("# Outcome benchmark — %s", r.Month())
	w("")
	w("Generated %s from %s. Window: merged %s → %s; maturation %d days (observed as of %s).",
		r.GeneratedAt.UTC().Format("2006-01-02"), r.Source, day(r.Since), day(r.Until), int(Maturation.Hours()/24), day(r.Now))
	w("")
	w("Pull requests that went through Hive's review swarm (**mediated**: at least one recorded verdict or posted review) versus pull requests merged without it (**unmediated**). Every rate carries its n and a 95%% Wilson interval; anything with n < %d is `%s`.", MinN, NotReported)
	w("")
	if !r.SplitSupported() {
		w("> **The data does not support a clean mediated-vs-unmediated split** (mediated n=%d, unmediated n=%d). Only the scale facts below are defensible.", r.Mediated.Merged, r.Unmediated.Merged)
		w("")
	}
	w("## Headline")
	w("")
	w("| Metric | Mediated | Unmediated |")
	w("|---|---|---|")
	w("| Merged PRs in window | %d | %d |", r.Mediated.Merged, r.Unmediated.Merged)
	w("| Revert rate (matured) | %s | %s |", pct(r.Mediated.RevertRate), pct(r.Unmediated.RevertRate))
	w("| Change-failure rate (revert or follow-up fix, matured) | %s | %s |", pct(r.Mediated.ChangeFailure), pct(r.Unmediated.ChangeFailure))
	w("| Median time-to-merge | %s | %s |", hours(r.Mediated.TimeToMerge), hours(r.Unmediated.TimeToMerge))
	w("| CI failure at merged head | %s | %s |", ciCell(r.Mediated), ciCell(r.Unmediated))
	w("| Had a `changes_requested` verdict before merge | %s | — (no verdicts by definition) |", pct(r.Mediated.ChangesRequested))
	w("| Cost per merged PR | %s | — |", r.costCell())
	w("")
	w("![mediated vs unmediated](assets/%s-bars.svg)", r.AssetBase())
	w("")
	w("### Reading the numbers")
	w("")
	for _, f := range r.Findings() {
		w("- %s", f)
	}
	w("")
	w("## Trend by month")
	w("")
	w("Whole evidence history, not just the window; months with n < %d are gaps.", MinN)
	w("")
	w("![trend](assets/%s-trend.svg)", r.AssetBase())
	w("")
	w("| Month | Mediated merged | Mediated revert | Mediated TTM | Unmediated merged | Unmediated revert | Unmediated TTM |")
	w("|---|---|---|---|---|---|---|")
	for i, m := range r.Months {
		a, u := r.Trend[CohortMediated][i], r.Trend[CohortUnmediated][i]
		w("| %s | %d | %s | %s | %d | %s | %s |", m, a.Merged, pct(a.RevertRate), hours(a.TimeToMerge), u.Merged, pct(u.RevertRate), hours(u.TimeToMerge))
	}
	w("")
	w("## Scale")
	w("")
	w("- %d evidence bundles (%d unreadable) folding to %d pull requests across %d repositories; %d verdicts recorded.", r.Bundles, r.Unreadable, r.PRs, len(r.Repos), r.Verdicts)
	w("- Merged in window by repository: %s.", r.repoList())
	w("- Merged in window by author kind — mediated: %s; unmediated: %s.", kinds(r.Mediated.ByAuthorKind), kinds(r.Unmediated.ByAuthorKind))
	if !r.FirstMediatedMerge.IsZero() {
		w("- First mediated merge in the evidence: %s.", day(r.FirstMediatedMerge))
	}
	if r.Hub != nil {
		w("- Hub fleet view: %d spokes registered, %d reporting, window %s → %s.", r.Hub.Spokes, r.Hub.Reporting, r.Hub.Window.From, r.Hub.Window.To)
	} else {
		w("- Hub `GET /api/hub/evidence` was not consulted (it requires an admin session); numbers are this spoke's evidence only.")
	}
	w("")
	w("## Methodology")
	w("")
	w("- **Source.** Only the per-PR evidence bundles the spoke already writes under `/data/evidence` (schema v1), folded per PR with `pkg/evidence.FoldPRs`. No new telemetry.")
	w("- **Cohorts.** A PR is *mediated* if any head carries a Hive verdict or a posted Hive review; otherwise *unmediated*. Backfilled history is unmediated by construction, so the window must start no earlier than the first mediated merge or the comparison is confounded by time.")
	w("- **Revert / change-failure.** Denominator is merged PRs observed at least %d days after merge. A revert is a later PR the evidence linker tagged `reverted_by`; change-failure adds `followed_up_by` fixes.", int(Maturation.Hours()/24))
	w("- **Time-to-merge.** Lower median of merge time minus open time, over merged PRs whose bundle recorded an open time.")
	w("- **CI failure at head.** Share of merged heads with a captured `failure` status among heads with a captured success/failure. The unmediated cohort rarely has a captured status (Hive only records CI for heads it reviews), so its cell reports coverage and is not comparable when coverage is low.")
	w("- **Changes requested.** Share of mediated merged PRs with at least one `changes_requested` verdict on any head before the merge — findings the author had a chance to address pre-merge. Whether the later head fixed the finding, or a human overrode it, is not verified here.")
	w("- **Cost.** `/api/cost` estimated spend divided by its hive-attributed merged-PR count over the same window; model prices are estimates and some models are unpriced, so treat it as an upper-bound order of magnitude.")
	w("- **Intervals.** 95%% Wilson score; n < %d is never reported.", MinN)
	w("- **Regenerate.** `src/scripts/outcome-benchmark.sh --month %s` (see `docs/benchmarks/README.md`).", r.Month())
	return b.String()
}

func ciCell(c Cohort) string {
	if !c.CIFailureAtHead.Reported {
		return fmt.Sprintf("%s (captured on %d of %d)", NotReported, c.CICoverage.K, c.CICoverage.N)
	}
	cov := ""
	if c.CICoverage.Reported && c.CICoverage.Rate < 0.5 {
		cov = fmt.Sprintf(" — captured on only %.0f%% of merges, not comparable", 100*c.CICoverage.Rate)
	}
	return pct(c.CIFailureAtHead) + cov
}

func (r Report) costCell() string {
	per, ok := r.Spend.PerMergedPR()
	if !ok {
		return NotReported + " (no spend data)"
	}
	return fmt.Sprintf("≈$%.2f est. (n=%d merged, %s → %s)", per, r.Spend.MergedPRs, day(r.Spend.WindowStart), day(r.Spend.WindowEnd))
}

func (r Report) repoList() string {
	parts := make([]string, 0, len(r.Repos))
	for _, rc := range r.Repos {
		parts = append(parts, fmt.Sprintf("%s %d", rc.Repo, rc.Merged))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func kinds(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// Findings are the plain-language sentences a reader should take away,
// derived only from reported figures.
func (r Report) Findings() []string {
	var out []string
	m, u := r.Mediated, r.Unmediated
	if m.RevertRate.Reported && u.RevertRate.Reported {
		switch {
		case m.RevertRate.Hi < u.RevertRate.Lo:
			out = append(out, fmt.Sprintf("Mediated PRs are reverted less often (%s vs %s); the intervals do not overlap.", pct(m.RevertRate), pct(u.RevertRate)))
		case u.RevertRate.Hi < m.RevertRate.Lo:
			out = append(out, fmt.Sprintf("Mediated PRs are reverted more often (%s vs %s); the intervals do not overlap.", pct(m.RevertRate), pct(u.RevertRate)))
		default:
			out = append(out, fmt.Sprintf("Revert rates are indistinguishable at this n: %s mediated vs %s unmediated (overlapping intervals).", pct(m.RevertRate), pct(u.RevertRate)))
		}
	}
	if m.TimeToMerge.Reported && u.TimeToMerge.Reported {
		d := m.TimeToMerge.Value - u.TimeToMerge.Value
		out = append(out, fmt.Sprintf("Median time-to-merge is %s for mediated vs %s for unmediated: review adds about %.0f minutes to the median PR.", hours(m.TimeToMerge), hours(u.TimeToMerge), 60*d))
	}
	if m.ChangesRequested.Reported {
		out = append(out, fmt.Sprintf("%s of mediated merged PRs were flagged `changes_requested` on an earlier head before merging — findings an unmediated PR would have carried to main unseen.", pct(m.ChangesRequested)))
	}
	if m.CIFailureAtHead.Reported && !(u.CIFailureAtHead.Reported && u.CICoverage.Rate >= 0.5) {
		out = append(out, fmt.Sprintf("CI at merged head is only captured for reviewed heads (mediated %s); the unmediated cell is not comparable.", pct(m.CIFailureAtHead)))
	}
	if per, ok := r.Spend.PerMergedPR(); ok {
		out = append(out, fmt.Sprintf("Estimated agent spend is ≈$%.2f per hive-attributed merged PR (n=%d).", per, r.Spend.MergedPRs))
	}
	if len(out) == 0 {
		out = append(out, "No metric reached n="+fmt.Sprint(MinN)+" in both cohorts; nothing is claimed.")
	}
	return out
}
