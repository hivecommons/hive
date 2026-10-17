package hub

import (
	"fmt"
	"math"
	"strings"
)

// Plain-language findings for the hub evidence view (#12456). Every sentence
// here is derived from an EvidenceView and nothing else; the dashboard's
// evidenceFindingsByKey reproduces the same sentences for a client-side hive /
// ACMM selection and TestEvidenceFindingsParity keeps the two in step. Issue
// numbers and internal flag names never appear in this text: it is what a hub
// admin with no statistics background reads first.

const (
	// evidenceDataSince is the day evidence records started to exist;
	// evidenceRevertsSince the day change-failure links were added. Both are
	// quoted so a low number is read as "young data", not "bad data".
	evidenceDataSince    = "2026-10-08"
	evidenceRevertsSince = "2026-10-15"
	// evidenceUpgradeBuild names, in user terms, the spoke build that reports
	// the fleet panels (outcomes by author kind, write counters, shadow gates).
	evidenceUpgradeBuild = "Oct 16 build"
	// evidenceDash is printed where a rate is below the minimum sample.
	evidenceDash = "—"
)

// evidenceConfidenceRange is one of the spoke card's four reviewer-confidence
// ranges. Spoke heartbeats carry six integer buckets of width 1/6 (bucket i
// covers [i/6, (i+1)/6)), so the hub regroups them: buckets 0–2 are 0.0–0.5
// exactly, and buckets 3, 4 and 5 are the closest sixth-boundaries to the
// card's 0.5 / 0.7 / 0.9 cut-offs. The range printed is the real one.
type evidenceConfidenceRange struct {
	Name  string
	Range string
	From  int // first heartbeat bucket, inclusive
	To    int // last heartbeat bucket, inclusive
}

var evidenceConfidenceRanges = []evidenceConfidenceRange{
	{Name: "Not confident", Range: "0.0–0.5", From: 0, To: 2},
	{Name: "Somewhat confident", Range: "0.5–0.67", From: 3, To: 3},
	{Name: "Confident", Range: "0.67–0.83", From: 4, To: 4},
	{Name: "Very confident", Range: "0.83–1.0", From: 5, To: 5},
}

// Label is the user-facing row label, e.g. "Very confident (0.83–1.0)".
func (r evidenceConfidenceRange) Label() string { return r.Name + " (" + r.Range + ")" }

// EvidenceConfidenceRow is one regrouped confidence range with its pooled
// bad-outcome rate.
type EvidenceConfidenceRow struct {
	evidenceConfidenceRange
	Bad int
	EvidenceRate
}

// evidenceConfidenceRows regroups the six pooled heartbeat buckets into the
// four confidence ranges.
func evidenceConfidenceRows(buckets []EvidenceCalibrationBucket) []EvidenceConfidenceRow {
	out := make([]EvidenceConfidenceRow, 0, len(evidenceConfidenceRanges))
	for _, r := range evidenceConfidenceRanges {
		row := EvidenceConfidenceRow{evidenceConfidenceRange: r}
		n := 0
		for _, b := range buckets {
			if b.Bucket >= r.From && b.Bucket <= r.To {
				n += b.N
				row.Bad += b.Bad
			}
		}
		row.EvidenceRate = newEvidenceRate(row.Bad, n)
		out = append(out, row)
	}
	return out
}

// evidencePctText prints a rate as the view does: one decimal, or a dash
// below the minimum sample.
func evidencePctText(r *float64) string {
	if r == nil || math.IsNaN(*r) || math.IsInf(*r, 0) {
		return evidenceDash
	}
	return fmt.Sprintf("%.1f%%", math.Round(*r*1000)/10)
}

// evidenceRateCount recovers the numerator of a rate (the server only ships
// rate and n). Exact for every rate newEvidenceRate produces.
func evidenceRateCount(r EvidenceRate) int {
	if r.Rate == nil {
		return 0
	}
	return int(math.Round(*r.Rate * float64(r.N)))
}

func evidencePlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// evidenceWindowText is "the last 90d" / "the chosen date range".
func evidenceWindowText(w EvidenceWindow) string {
	if w.Approximate {
		return "the chosen date range"
	}
	if w.Label == "" {
		return "the last 90d"
	}
	return "the " + w.Label
}

func evidenceHiveName(h EvidenceHive) string {
	if h.Name != "" {
		return h.Name
	}
	return h.ID
}

// evidenceLevelName spells an ACMM level for a reader who has never seen the
// scale: only L6 has a short plain name.
func evidenceLevelName(level int) string {
	if level == 6 {
		return "full autonomy (L6)"
	}
	return fmt.Sprintf("autonomy level L%d", level)
}

// evidenceFindingsByKey returns one plain sentence per section key. An empty
// string means the section is not reported by any selected hive and belongs
// in the "Waiting on spokes" list instead.
func evidenceFindingsByKey(v EvidenceView) map[string]string {
	out := map[string]string{}
	win := evidenceWindowText(v.Window)

	// perhive — who reports at all.
	var reportingName string
	for _, h := range v.Hives {
		if h.Reported {
			reportingName = evidenceHiveName(h)
			break
		}
	}
	switch {
	case v.Spokes == 0:
		out["perhive"] = "No hives are selected."
	case v.Reporting == 0:
		out["perhive"] = fmt.Sprintf("None of the %d selected %s has reported evidence for %s yet.", v.Spokes, evidencePlural(v.Spokes, "hive", "hives"), win)
	case v.Reporting == 1 && v.Spokes > 1:
		out["perhive"] = fmt.Sprintf("Only %s reports evidence so far (1 of %d hives).", reportingName, v.Spokes)
	case v.Reporting == v.Spokes && v.Spokes == 1:
		out["perhive"] = fmt.Sprintf("%s reports evidence for %s.", reportingName, win)
	case v.Reporting == v.Spokes:
		out["perhive"] = fmt.Sprintf("All %d selected hives report evidence for %s.", v.Spokes, win)
	default:
		out["perhive"] = fmt.Sprintf("%d of %d selected hives report evidence for %s.", v.Reporting, v.Spokes, win)
	}
	if v.WindowUnreported > 0 {
		out["perhive"] += fmt.Sprintf(" %d %s only the 90-day figures because %s an older build.", v.WindowUnreported,
			evidencePlural(v.WindowUnreported, "reports", "report"), evidencePlural(v.WindowUnreported, "it runs", "they run"))
	}

	// coverage — how much of the merged work has a record.
	cov := v.RecordCoverage
	switch {
	case !v.Window.Coverage:
		out["coverage"] = "Record coverage is only computed for the 90-day window, because the count of merged PRs it is compared against is a 90-day figure."
	case cov.Rate != nil:
		out["coverage"] = fmt.Sprintf("Of the %d merged PRs in the last 90 days across the selected hives, %d (%s) have an evidence record — recording began %s, so this will rise.",
			cov.N, evidenceRateCount(cov), evidencePctText(cov.Rate), evidenceDataSince)
	case cov.N == 0:
		out["coverage"] = "No merged PRs are counted yet, so we cannot say how much of the work has an evidence record."
	default:
		out["coverage"] = fmt.Sprintf("Only %d merged %s counted, fewer than the %d needed to say what share has an evidence record.", cov.N, evidencePlural(cov.N, "PR is", "PRs are"), minEvidenceN)
	}

	// acmm — revert rate at the highest level with enough merges.
	var best *EvidenceACMMLevel
	for i := range v.ByACMMLevel {
		if v.ByACMMLevel[i].RevertRate.Rate != nil && (best == nil || v.ByACMMLevel[i].Level > best.Level) {
			best = &v.ByACMMLevel[i]
		}
	}
	switch {
	case best != nil:
		rev := evidenceRateCount(best.RevertRate)
		tail := fmt.Sprintf("%d (%s) were reverted or needed a follow-up fix within 14 days", rev, evidencePctText(best.RevertRate.Rate))
		if rev == 0 {
			tail = "none were reverted or needed a follow-up fix within 14 days"
		}
		out["acmm"] = fmt.Sprintf("At %s, %d %s merged %d PRs and %s.", evidenceLevelName(best.Level), best.Hives, evidencePlural(best.Hives, "hive", "hives"), best.RevertRate.N, tail)
	case len(v.ByACMMLevel) == 0:
		out["acmm"] = fmt.Sprintf("No selected hive reports merges for %s yet.", win)
	default:
		out["acmm"] = fmt.Sprintf("No autonomy level has %d merged PRs yet, so revert rates cannot be reported.", minEvidenceN)
	}

	// calibration — does confidence track outcomes.
	rows := evidenceConfidenceRows(v.Calibration)
	var low, top *EvidenceConfidenceRow
	for i := range rows {
		if rows[i].Rate != nil {
			if low == nil {
				low = &rows[i]
			}
			top = &rows[i]
		}
	}
	switch {
	case top == nil:
		out["calibration"] = fmt.Sprintf("No confidence range has %d verdicts yet, so we cannot tell whether the reviewer's confidence means anything.", minEvidenceN)
	case low == top:
		out["calibration"] = fmt.Sprintf("Only one confidence range (%s, %d verdicts, %s went wrong) has enough verdicts to measure, so there is nothing to compare it with yet.",
			top.Label(), top.N, evidencePctText(top.Rate))
	default:
		lead := "The reviewer's confidence tracks reality"
		if *top.Rate > *low.Rate {
			lead = "The reviewer's confidence does not track reality yet"
		}
		topPhrase := evidencePctText(top.Rate) + " went wrong"
		if evidenceRateCount(top.EvidenceRate) == 0 {
			topPhrase = "nothing went wrong"
		}
		lowPhrase := evidencePctText(low.Rate) + " did"
		if evidenceRateCount(low.EvidenceRate) == 0 {
			lowPhrase = "none did"
		}
		out["calibration"] = fmt.Sprintf("%s: when %s (%s, %d verdicts) %s; when %s (%s, %d verdicts) %s.",
			lead, strings.ToLower(top.Name), top.Range, top.N, topPhrase, strings.ToLower(low.Name), low.Range, low.N, lowPhrase)
	}

	// outcomes — agent vs human PRs (fleet panel; absent on old spokes).
	if v.OutcomesByAuthorKind != nil {
		byKind := map[string]EvidenceAuthorKindOutcome{}
		for _, o := range v.OutcomesByAuthorKind {
			byKind[o.Kind] = o
		}
		var parts []string
		for _, k := range []struct{ kind, who string }{{"agent", "AI agents"}, {"human", "Humans"}, {"bot", "Bots"}} {
			o, ok := byKind[k.kind]
			if !ok || o.N == 0 {
				continue
			}
			s := fmt.Sprintf("%s wrote %d %s: %d merged", k.who, o.N, evidencePlural(o.N, "PR", "PRs"), o.Merged)
			if o.MergeRate.Rate != nil {
				s += " (" + evidencePctText(o.MergeRate.Rate) + ")"
			}
			s += fmt.Sprintf(" and %d of those %s reverted within 14 days", o.Reverted, evidencePlural(o.Reverted, "was", "were"))
			parts = append(parts, s)
		}
		if len(parts) == 0 {
			out["outcomes"] = fmt.Sprintf("No PRs with a known author kind were recorded for %s.", win)
		} else {
			out["outcomes"] = strings.Join(parts, ". ") + "."
		}
	} else {
		out["outcomes"] = ""
	}

	// writes — guardrail proxy counters (fleet panel).
	if v.WriteCounters != nil {
		att, blk := 0, 0
		for _, t := range v.WriteCounters {
			att += t.Attempted
			blk += t.Blocked
		}
		if att == 0 {
			out["writes"] = "No agent write attempts have been recorded by the guardrail proxy yet."
		} else {
			rate := float64(blk) / float64(att)
			out["writes"] = fmt.Sprintf("Agents attempted %d writes to GitHub through the guardrail proxy and %d (%s) %s blocked; a blocked write never reached GitHub.",
				att, blk, evidencePctText(&rate), evidencePlural(blk, "was", "were"))
		}
	} else {
		out["writes"] = ""
	}

	// family — same vs cross AI family.
	fam := v.FamilyFalseApproveRate
	var fparts []string
	if fam.Same.Rate != nil {
		fparts = append(fparts, fmt.Sprintf("when reviewer and author come from the same AI family the false-approve rate is %s (%d verdicts)", evidencePctText(fam.Same.Rate), fam.Same.N))
	}
	if fam.Cross.Rate != nil {
		fparts = append(fparts, fmt.Sprintf("across different families it is %s (%d verdicts)", evidencePctText(fam.Cross.Rate), fam.Cross.N))
	}
	if fam.Mixed.Rate != nil {
		fparts = append(fparts, fmt.Sprintf("for a mix of both it is %s (%d verdicts)", evidencePctText(fam.Mixed.Rate), fam.Mixed.N))
	}
	if len(fparts) == 0 {
		out["family"] = fmt.Sprintf("No family mix has %d verdicts yet, so same- vs cross-family review cannot be compared.", minEvidenceN)
	} else {
		s := strings.Join(fparts, "; ")
		out["family"] = strings.ToUpper(s[:1]) + s[1:] + "."
	}

	// shadow — what the gates would have done (fleet panel).
	if v.ShadowGate != nil {
		var sparts []string
		for _, g := range []struct {
			name, verb string
			gate       EvidenceShadowGateView
		}{{"The review gate", "held", v.ShadowGate.ReviewGate}, {"Intent verification", "blocked", v.ShadowGate.Intent}} {
			if g.gate.N == 0 {
				continue
			}
			held := g.gate.Held
			ok := held.Merged - held.Reverted
			if ok < 0 {
				ok = 0
			}
			sparts = append(sparts, fmt.Sprintf("%s would have %s %d %s; %d of those merged and stuck (false alarms). It would have passed %d, of which %d %s reverted (misses)",
				g.name, g.verb, held.N, evidencePlural(held.N, "PR", "PRs"), ok, g.gate.Passed.N, g.gate.Passed.Reverted, evidencePlural(g.gate.Passed.Reverted, "was", "were")))
		}
		if len(sparts) == 0 {
			out["shadow"] = "The selected hives have shadow mode on but no gate has evaluated a PR yet."
		} else {
			out["shadow"] = strings.Join(sparts, ". ") + "."
		}
	} else {
		out["shadow"] = ""
	}
	return out
}

// evidenceFindingsOrder is the order of the "What this says" block: who
// reports, how much is covered, then the three headline questions.
var evidenceFindingsOrder = []string{"perhive", "coverage", "acmm", "calibration", "outcomes", "shadow"}

// evidenceFindings is the ordered "What this says" block: between three and
// six sentences, skipping sections no selected hive reports.
func evidenceFindings(v EvidenceView) []string {
	byKey := evidenceFindingsByKey(v)
	out := make([]string, 0, len(evidenceFindingsOrder))
	for _, k := range evidenceFindingsOrder {
		if s := byKey[k]; s != "" {
			out = append(out, s)
		}
	}
	return out
}

// evidenceWaiting lists, in user terms, what the selected hives do not report
// yet and why. Panels and the time-to-merge column are listed when absent
// from the view; the never-emitted CI gate item is always listed.
func evidenceWaiting(v EvidenceView) []string {
	upgrade := "hives report this once upgraded to the " + evidenceUpgradeBuild + " or later"
	var out []string
	if v.OutcomesByAuthorKind == nil {
		out = append(out, "Outcome by author kind — "+upgrade+".")
	}
	if v.WriteCounters == nil {
		out = append(out, "Boundary enforcement (agent writes the guardrail proxy blocked) — "+upgrade+".")
	}
	if v.ShadowGate == nil {
		out = append(out, "Shadow gates — shadow mode is off on every selected hive, or none has reported it yet ("+upgrade+").")
	}
	out = append(out, "CI gate (claim vs verdict) totals — not reported by any hive build yet.")
	if !evidenceHasTimeToMerge(v) {
		out = append(out, evidenceWaitingTimeToMerge)
	}
	if len(v.RevertRateByAuthorKind) == 0 {
		out = append(out, "Revert rate by author kind — "+upgrade+".")
	}
	return out
}

// evidenceWaitingTimeToMerge is the waiting item while no selected level
// reports a median time-to-merge.
const evidenceWaitingTimeToMerge = "Median time-to-merge per autonomy level — no selected hive reports it yet (it needs a build that records when each merged PR was opened)."

func evidenceHasTimeToMerge(v EvidenceView) bool {
	for _, a := range v.ByACMMLevel {
		if a.TimeToMerge != nil {
			return true
		}
	}
	return false
}
