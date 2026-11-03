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
	w("## Real examples")
	w("")
	if len(r.Stories) == 0 {
		w("No curated examples were supplied (`--stories`).")
	}
	for _, s := range r.Stories {
		w("- [%s#%d](%s): the [posted review](%s) caught: %s The fixed head was approved %d minutes later; the PR merged %d minutes after opening (opened %s, caught %s, approved %s, merged %s UTC).",
			s.Repo, s.Number, s.URL(), s.ReviewURL, strings.TrimSpace(s.Caught), s.FixMinutes(), s.MergeMinutes(),
			s.OpenedAt.UTC().Format("15:04"), s.CaughtAt.UTC().Format("15:04"), s.FixApprovedAt.UTC().Format("15:04"), s.MergedAt.UTC().Format("2006-01-02 15:04"))
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
	w("## What the README shows and leaves out")
	w("")
	w("The README carries only plain-language cards; this section says which numbers made it and why the rest did not.")
	w("")
	for _, line := range r.readmeDecisions() {
		w("- %s", line)
	}
	w("- **Not a controlled comparison.** Pull requests merged without a Hive review come from other repositories and periods; differences between the two groups may not be caused by review.")
	w("- **Pull requests sent back.** Counts a `changes_requested` verdict on any head before merge. Whether the next head fixed the finding, or a human overrode it, is not verified here.")
	w("- **Prompt injection.** The catch rate is from Hive's own evaluation set, not production traffic, and is not subject to the README minimum base because it is a self-run test and labelled as such on the card.")
	w("- **Post-merge CI pass rate** is drawn only when at least %d%% of reviewed merges carry a recorded check result; a roadmap item is to record check status for every merged head so it can be reported.", int(100*minCICardCoverage))
	w("")
	w("## Methodology")
	w("")
	w("How measured: per-PR evidence bundles the spoke already writes, folded per PR; 95%% Wilson intervals; n < %d is never reported. The README shows only cards whose base is at least %d (`--readme-min-base`), in plain words and without intervals; everything below that threshold, and every interval, is in this report.", MinN, r.MinBase())
	w("")
	w("- **Source.** Only the per-PR evidence bundles the spoke already writes under `/data/evidence` (schema v1), folded per PR with `pkg/evidence.FoldPRs`. No new telemetry.")
	w("- **Cohorts.** A PR is *mediated* if any head carries a Hive verdict or a posted Hive review; otherwise *unmediated*. Backfilled history is unmediated by construction, so the window must start no earlier than the first mediated merge or the comparison is confounded by time.")
	w("- **Revert / change-failure.** Denominator is merged PRs observed at least %d days after merge. A revert is a later PR the evidence linker tagged `reverted_by`; change-failure adds `followed_up_by` fixes.", int(Maturation.Hours()/24))
	w("- **Time-to-merge.** Lower median of merge time minus open time, over merged PRs whose bundle recorded an open time.")
	w("- **CI failure at head.** Share of merged heads with a captured `failure` status among heads with a captured success/failure. The unmediated cohort rarely has a captured status (Hive only records CI for heads it reviews), so its cell reports coverage and is not comparable when coverage is low.")
	w("- **Changes requested.** Share of mediated merged PRs with at least one `changes_requested` verdict on any head before the merge — findings the author had a chance to address pre-merge. Whether the later head fixed the finding, or a human overrode it, is not verified here.")
	w("- **Sent back, then fixed.** Over PRs whose first `changes_requested` verdict falls in the window: the share that pushed a later head which then received an `approve` verdict. Heads are ordered by bundle creation time; a PR counted as *merged on the same head* merged with no later head, and the report says how many of those heads also carried an `approve` from another perspective, which is how the gate let them through. This measures whether the author acted on the reviewer and the fix passed re-review, not whether the shipped code was correct.")
	w("- **Time to fix.** Lower median of first `approve` on a later head minus the first `changes_requested`, over the PRs counted as fixed above.")
	w("- **Outcome measures after review** (reverts, follow-up fixes, reopened issues) need merges observed at least %d days; the reviewed cohort is too young for a base of %d, so they stay in the tables and are not drawn in the README.", int(Maturation.Hours()/24), DefaultReadmeMinBase)
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

// readmeDecisions lists, per README card, whether it was drawn and the base
// it rests on, so the report states every exclusion the README is silent on.
func (r Report) readmeDecisions() []string {
	min := r.MinBase()
	var out []string
	f := r.Followup
	fa := f.FixedAndApproved
	if r.HasCard(CardAccuracy) {
		out = append(out, fmt.Sprintf("**Sent back, then fixed** is shown: %d of %d PRs that received a changes_requested verdict in the window pushed a new head that was then approved (%d pushed a new head at all; %d merged on the same head, %d of those because another perspective approved it; %d were unresolved at observation time).", fa.K, fa.N, f.NewHead, f.MergedSameHead, f.MergedSameHeadApproved, f.Unresolved))
	} else {
		out = append(out, fmt.Sprintf("**Sent back, then fixed** is not shown: base %d is below the README minimum of %d.", fa.N, min))
	}
	ta := f.TimeToApproval
	if r.HasCard(CardAccuracy) && ta.Reported && ta.N >= min {
		out = append(out, fmt.Sprintf("**Time to fix** is shown: %d minutes is the median from the first changes_requested verdict to the first approve verdict on a later head, over %d PRs.", minutes(ta.Value), ta.N))
	} else {
		out = append(out, fmt.Sprintf("**Time to fix** is not shown: base %d is below the README minimum of %d.", ta.N, min))
	}
	cr := r.Mediated.ChangesRequested
	if cr.Reported {
		out = append(out, fmt.Sprintf("**Sent back before merge** (%d of %d reviewed merges) is in the tables but not the README: it counts reviewer objections, not whether the shipped code was right.", cr.K, cr.N))
	}
	if f.NewHead > 0 {
		out = append(out, fmt.Sprintf("**Checks fixed after a change request** is not shown: a check result is captured on both the sent-back head and the next head for %d of %d PRs (%.0f%%), under the %d%% floor; of those, %d went from failing to passing.", f.CIPairs, f.NewHead, 100*f.CICoverage.Rate, int(100*minCICardCoverage), f.CIFixed))
	}
	out = append(out, fmt.Sprintf("**Issues that stayed closed** is not shown: issue reopen records exist only for the newest part of the history and no closure via a reviewed merge has been observed for %d days yet.", int(Maturation.Hours()/24)))
	ci := r.Mediated.CIFailureAtHead
	if r.HasCard(CardChecksGreen) {
		out = append(out, fmt.Sprintf("**Checks passing at merge** is shown: %d of %d reviewed merges failed a recorded check.", ci.K, ci.N))
	} else {
		out = append(out, fmt.Sprintf("**Checks passing at merge** is not shown: a check result is recorded on %d of %d reviewed merges (%.0f%%), under the %d%% floor, so the pass rate would describe a biased subset.", r.Mediated.CICoverage.K, r.Mediated.CICoverage.N, 100*r.Mediated.CICoverage.Rate, int(100*minCICardCoverage)))
	}
	rv := r.Mediated.RevertRate
	out = append(out, fmt.Sprintf("**Revert rate** is not shown: the reviewed base is %d merges observed %d+ days after merge, below the README minimum of %d, and the two groups are not distinguishable on this data.", rv.N, int(Maturation.Hours()/24), min))
	if per, ok := r.Spend.PerMergedPR(); ok {
		out = append(out, fmt.Sprintf("**Cost per merged PR** is shown as an estimate: $%.2f = $%.2f estimated spend / %d hive-attributed merges, %s → %s.", per, r.Spend.USD, r.Spend.MergedPRs, day(r.Spend.WindowStart), day(r.Spend.WindowEnd)))
	} else {
		out = append(out, "**Cost per merged PR** is not shown: no spend sample was supplied or its merged-PR count is zero.")
	}
	m, u := r.Mediated.TimeToMerge, r.Unmediated.TimeToMerge
	if m.Reported && u.Reported {
		out = append(out, fmt.Sprintf("**Time to merge** is shown in the Also strip as %d minutes (typical, i.e. median) for %d reviewed merges, against %d minutes for %d merges without review; the extra minutes are stated as the cost of review.", minutes(m.Value), m.N, minutes(u.Value), u.N))
	}
	return out
}
