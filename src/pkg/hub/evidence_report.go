package hub

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Downloadable plain-language evidence report (#12456). GET
// /api/hub/evidence/report renders the same EvidenceView the drill-down shows
// — same window, same hive / ACMM selection — as Markdown (canonical) or a
// standalone HTML page, server-side, so the report reconciles with the modal
// exactly. Read-only like everything else under /api/hub/evidence: nothing
// here is consulted by any merge decision.

const (
	evidenceReportPath = "/api/hub/evidence/report"
	// evidenceReportDocURL is the only link the report carries (footer).
	evidenceReportDocURL    = "https://github.com/hivecommons/hive/blob/v5/src/docs/review-evidence.md"
	evidenceReportDocAnchor = "#how-to-read-the-hub-evidence-view"

	// Grade thresholds, mirrored from dashboard.html's EVIDENCE_* constants
	// and documented in docs/review-evidence.md.
	evidenceCoverageStrong         = 0.8
	evidenceCoverageFair           = 0.4
	evidenceErrStrong              = 0.10
	evidenceErrFair                = 0.25
	evidenceRevertStrong           = 0.05
	evidenceRevertFair             = 0.15
	evidenceShadowFalseAlarmStrong = 0.25
	evidenceShadowFalseAlarmFair   = 0.5
	evidenceReportingStrong        = 0.8
	evidenceReportingFair          = 0.4
)

// evidenceGrade is one section's grade and the sentence that justifies it.
type evidenceGrade struct {
	Section string
	Grade   string // Strong | Fair | Weak | Not enough data | Not graded
	Basis   string
}

func evidenceGradeLow(r EvidenceRate, strong, fair float64) string {
	if r.Rate == nil || r.N < minEvidenceN {
		return "Not enough data"
	}
	switch {
	case *r.Rate <= strong:
		return "Strong"
	case *r.Rate <= fair:
		return "Fair"
	}
	return "Weak"
}

func evidenceGradeHigh(r EvidenceRate, strong, fair float64) string {
	if r.Rate == nil || r.N < minEvidenceN {
		return "Not enough data"
	}
	switch {
	case *r.Rate >= strong:
		return "Strong"
	case *r.Rate >= fair:
		return "Fair"
	}
	return "Weak"
}

func evidencePctConst(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

// evidenceGrades grades every section by the same rules as the modal's
// "How to read our numbers" paragraphs.
func evidenceGrades(v EvidenceView) []evidenceGrade {
	var out []evidenceGrade
	lowBasis := func(what string, strong, fair float64) string {
		return fmt.Sprintf("%s; Strong at or below %s, Fair at or below %s, otherwise Weak.", what, evidencePctConst(strong), evidencePctConst(fair))
	}
	highBasis := func(what string, strong, fair float64) string {
		return fmt.Sprintf("%s; Strong at or above %s, Fair at or above %s, otherwise Weak.", what, evidencePctConst(strong), evidencePctConst(fair))
	}

	covGrade := "Not enough data"
	if v.Window.Coverage {
		covGrade = evidenceGradeHigh(v.RecordCoverage, evidenceCoverageStrong, evidenceCoverageFair)
	}
	out = append(out, evidenceGrade{"Evidence coverage", covGrade, highBasis("Share of merged PRs (90 days) that have an evidence record", evidenceCoverageStrong, evidenceCoverageFair)})

	rows := evidenceConfidenceRows(v.Calibration)
	calGrade, measurable := "Not enough data", 0
	var top *EvidenceConfidenceRow
	for i := range rows {
		if rows[i].Rate != nil {
			measurable++
			top = &rows[i]
		}
	}
	if measurable >= 2 {
		calGrade = evidenceGradeLow(top.EvidenceRate, evidenceErrStrong, evidenceErrFair)
	}
	out = append(out, evidenceGrade{"Reviewer confidence", calGrade, lowBasis("Share of verdicts that went wrong in the most-confident range with enough verdicts (needs two measurable ranges)", evidenceErrStrong, evidenceErrFair)})

	outGrade := "Not enough data"
	if v.OutcomesByAuthorKind != nil {
		var pick *EvidenceAuthorKindOutcome
		for i := range v.OutcomesByAuthorKind {
			k := v.OutcomesByAuthorKind[i].Kind
			if k == "agent" || (k == "human" && pick == nil) {
				pick = &v.OutcomesByAuthorKind[i]
			}
		}
		if pick != nil {
			outGrade = evidenceGradeLow(pick.RevertRate, evidenceRevertStrong, evidenceRevertFair)
		}
	}
	out = append(out, evidenceGrade{"Outcome by author kind", outGrade, lowBasis("Revert rate of PRs written by AI agents (humans if no agent PRs)", evidenceRevertStrong, evidenceRevertFair)})

	var best *EvidenceACMMLevel
	for i := range v.ByACMMLevel {
		if v.ByACMMLevel[i].RevertRate.Rate != nil && (best == nil || v.ByACMMLevel[i].Level > best.Level) {
			best = &v.ByACMMLevel[i]
		}
	}
	acmmGrade := "Not enough data"
	if best != nil {
		acmmGrade = evidenceGradeLow(best.RevertRate, evidenceRevertStrong, evidenceRevertFair)
	}
	out = append(out, evidenceGrade{"Autonomy level × outcome", acmmGrade, lowBasis("Revert rate at the highest autonomy level with enough merged PRs", evidenceRevertStrong, evidenceRevertFair)})

	out = append(out, evidenceGrade{"Boundary enforcement", "Not graded", "There is no \"good\" block rate: zero blocks with many attempts means agents stayed inside their lane, many blocks means the guardrail did its job."})

	fam := v.FamilyFalseApproveRate
	biggest := fam.Same
	for _, r := range []EvidenceRate{fam.Cross, fam.Mixed} {
		if r.N > biggest.N {
			biggest = r
		}
	}
	out = append(out, evidenceGrade{"Same- vs cross-family review", evidenceGradeLow(biggest, evidenceErrStrong, evidenceErrFair), lowBasis("False-approve rate of the family mix with the most verdicts", evidenceErrStrong, evidenceErrFair)})

	shareGrade := "Not enough data"
	if v.Spokes > 0 {
		share := float64(v.Reporting) / float64(v.Spokes)
		switch {
		case share >= evidenceReportingStrong:
			shareGrade = "Strong"
		case share >= evidenceReportingFair:
			shareGrade = "Fair"
		default:
			shareGrade = "Weak"
		}
	}
	out = append(out, evidenceGrade{"Per-hive evidence", shareGrade, highBasis("Share of selected hives reporting for the window (no minimum sample)", evidenceReportingStrong, evidenceReportingFair)})

	shadowGrade := "Not enough data"
	if v.ShadowGate != nil {
		var gate *EvidenceShadowGateView
		for _, g := range []*EvidenceShadowGateView{&v.ShadowGate.ReviewGate, &v.ShadowGate.Intent} {
			if g.Held.N > 0 && (gate == nil || g.Held.N > gate.Held.N) {
				gate = g
			}
		}
		if gate != nil {
			ok := gate.Held.Merged - gate.Held.Reverted
			if ok < 0 {
				ok = 0
			}
			shadowGrade = evidenceGradeLow(newEvidenceRate(ok, gate.Held.N), evidenceShadowFalseAlarmStrong, evidenceShadowFalseAlarmFair)
		}
	}
	out = append(out, evidenceGrade{"Shadow gates", shadowGrade, lowBasis("False-alarm rate (PRs a gate would have held that merged and stuck) of the gate with the most held PRs", evidenceShadowFalseAlarmStrong, evidenceShadowFalseAlarmFair)})
	return out
}

// evidenceReportTable is a Markdown/HTML-neutral table.
type evidenceReportTable struct {
	Head []string
	Rows [][]string
}

// evidenceReportSection is one heading with prose, bullets and/or a table.
type evidenceReportSection struct {
	Title   string
	Paras   []string
	Bullets []string
	Table   *evidenceReportTable
	// Sub are nested sections (rendered one heading level deeper).
	Sub []evidenceReportSection
}

// evidenceReport is the whole document; both renderers walk it.
type evidenceReport struct {
	Title    string
	Meta     []string
	Sections []evidenceReportSection
	Footer   string
}

// evidenceRateCell is "rate of n (margin lo–hi)" or "n cases, below the
// minimum sample".
func evidenceRateCell(r EvidenceRate) string {
	if r.Rate == nil {
		return fmt.Sprintf("%s (%d, below the minimum of %d)", evidenceDash, r.N, minEvidenceN)
	}
	return fmt.Sprintf("%s of %d (margin %s – %s)", evidencePctText(r.Rate), r.N, evidencePctText(r.Lo), evidencePctText(r.Hi))
}

// evidenceReportWindowDates returns the from/to dates (YYYY-MM-DD) the window
// covers, for the report meta line and the download filename.
func evidenceReportWindowDates(w EvidenceWindow, now time.Time) (string, string) {
	if w.From != "" {
		from, _ := parseEvidenceTime(w.From)
		to := now
		if w.To != "" {
			to, _ = parseEvidenceTime(w.To)
		}
		return from.Format("2006-01-02"), to.Format("2006-01-02")
	}
	hours := w.Hours
	if hours <= 0 {
		hours = evidenceDefaultWindowHours
	}
	return now.Add(-time.Duration(hours) * time.Hour).Format("2006-01-02"), now.Format("2006-01-02")
}

// buildEvidenceReport turns a view into the report document. scope is the
// hive selection in words ("whole fleet", "hive alpha", …).
func buildEvidenceReport(v EvidenceView, scope string, now time.Time) evidenceReport {
	from, to := evidenceReportWindowDates(v.Window, now)
	win := evidenceWindowText(v.Window)
	byKey := evidenceFindingsByKey(v)
	rep := evidenceReport{
		Title: "Hive evidence report — " + scope,
		Meta: []string{
			"Scope: " + scope,
			fmt.Sprintf("Window: %s (%s to %s)%s", win, from, to, map[bool]string{true: ", served from the smallest reported window that covers it", false: ""}[v.Window.Approximate]),
			"Generated: " + now.UTC().Format("2006-01-02 15:04 UTC"),
		},
	}

	rep.Sections = append(rep.Sections, evidenceReportSection{Title: "Summary", Paras: []string{"What the numbers below say, in plain words. Every figure comes from the hives' own reports; nothing is estimated."}, Bullets: evidenceFindings(v)})

	rep.Sections = append(rep.Sections, evidenceReportSection{Title: "What we measured and why", Paras: []string{
		"Evidence coverage — of all pull requests GitHub says were merged in the last 90 days, how many have an evidence record: a saved file of the AI reviews, their verdicts and what happened to the PR afterwards. Every other number only counts PRs that have a record, so low coverage means the rest describes a small slice of the work.",
		"Reviewer confidence — when the AI reviewer says it is very sure, is it right more often than when it is unsure? Each verdict is filed under a confidence range and we count how often that range's calls went wrong. A \"wrong call\" is a false approve (approved, but the PR was never merged or was reverted) or a false block (blocked, but merged anyway after a human override).",
		"Outcome by author kind — for PRs written by humans, by AI agents and by bots: how many merged, how many were reverted after merging, and how many are still open. This is the basic question behind letting agents write code.",
		"Autonomy level × outcome — the revert rate grouped by the autonomy level a hive runs at, from L1 (advice only) to L6 (merges on green CI without a human). If more autonomy came with more reverts, that would be the signal to slow down.",
		"Boundary enforcement — every write (push, comment, label, merge…) an agent tried to make through the guardrail proxy, and how many the proxy refused. A blocked write never reached GitHub.",
		"Same- vs cross-family review — the false-approve rate split by whether the reviewing AI comes from the same model family as the author's AI. A reviewer from the same family may share the author's blind spots.",
		"Per-hive evidence — each hive on its own, so a single hive going wrong is not hidden by the totals.",
		"Shadow gates — what the review gate and intent verification would have done (hold or pass a PR) if they were enforcing, set against what actually happened. Shadow mode only records; nothing was held or blocked anywhere.",
	}})

	numbers := evidenceReportSection{Title: "The numbers"}
	addNum := func(title, key string, t *evidenceReportTable, extra ...string) {
		sec := evidenceReportSection{Title: title, Table: t}
		if f := byKey[key]; f != "" {
			sec.Paras = append(sec.Paras, f)
		}
		sec.Paras = append(sec.Paras, extra...)
		numbers.Sub = append(numbers.Sub, sec)
	}

	if v.Window.Coverage {
		t := &evidenceReportTable{Head: []string{"Scope", "Merged PRs", "With an evidence record"}}
		t.Rows = append(t.Rows, []string{"All selected hives", fmt.Sprint(v.RecordCoverage.N), evidenceRateCell(v.RecordCoverage)})
		if len(v.Hives) > 1 {
			for _, h := range v.Hives {
				t.Rows = append(t.Rows, []string{evidenceHiveName(h), fmt.Sprint(h.RecordCoverage.N), evidenceRateCell(h.RecordCoverage)})
			}
		}
		addNum("Evidence coverage", "coverage", t)
	} else {
		addNum("Evidence coverage", "coverage", nil)
	}

	ct := &evidenceReportTable{Head: []string{"Reviewer confidence", "Verdicts", "Went wrong"}}
	for _, r := range evidenceConfidenceRows(v.Calibration) {
		ct.Rows = append(ct.Rows, []string{r.Label(), fmt.Sprint(r.N), evidenceRateCell(r.EvidenceRate)})
	}
	addNum("Reviewer confidence", "calibration", ct)

	if v.OutcomesByAuthorKind != nil {
		t := &evidenceReportTable{Head: []string{"Author kind", "Hives", "PRs", "Merged", "Reverted within 14 days"}}
		for _, o := range v.OutcomesByAuthorKind {
			t.Rows = append(t.Rows, []string{o.Kind, fmt.Sprint(o.Hives), fmt.Sprint(o.N), evidenceRateCell(o.MergeRate), evidenceRateCell(o.RevertRate)})
		}
		addNum("Outcome by author kind", "outcomes", t)
	}

	at := &evidenceReportTable{Head: []string{"Autonomy level", "Hives", "Merged PRs", "Reverted within 14 days"}}
	for _, a := range v.ByACMMLevel {
		at.Rows = append(at.Rows, []string{fmt.Sprintf("L%d", a.Level), fmt.Sprint(a.Hives), fmt.Sprint(a.Merged90d), evidenceRateCell(a.RevertRate)})
	}
	addNum("Autonomy level × outcome", "acmm", at)

	if v.WriteCounters != nil {
		t := &evidenceReportTable{Head: []string{"Intent tier", "Hives", "Attempted", "Blocked"}}
		for _, w := range v.WriteCounters {
			t.Rows = append(t.Rows, []string{w.Tier, fmt.Sprint(w.Hives), fmt.Sprint(w.Attempted), evidenceRateCell(w.BlockRate)})
		}
		addNum("Boundary enforcement", "writes", t, "Totals run since each hive's proxy started, not over the window.")
	}

	ft := &evidenceReportTable{Head: []string{"Family mix", "Verdicts", "False approve"}}
	for _, m := range []struct {
		k string
		r EvidenceRate
	}{{"same family", v.FamilyFalseApproveRate.Same}, {"different families", v.FamilyFalseApproveRate.Cross}, {"mix of both", v.FamilyFalseApproveRate.Mixed}} {
		ft.Rows = append(ft.Rows, []string{m.k, fmt.Sprint(m.r.N), evidenceRateCell(m.r)})
	}
	addNum("Same- vs cross-family review", "family", ft)

	pt := &evidenceReportTable{Head: []string{"Hive", "Autonomy", "Family mix", "Verdicts", "False approve", "False block", "Merged PRs", "Reverted within 14 days"}}
	for _, h := range v.Hives {
		if !h.Reported {
			why := "no evidence reported"
			if h.WindowUnreported {
				why = "not reported for this window (older build)"
			}
			pt.Rows = append(pt.Rows, []string{evidenceHiveName(h), fmt.Sprintf("L%d", h.ACMMLevel), why, "", "", "", "", ""})
			continue
		}
		pt.Rows = append(pt.Rows, []string{evidenceHiveName(h), fmt.Sprintf("L%d", h.ACMMLevel), h.FamilyMix, fmt.Sprint(h.Verdicts90d),
			evidenceRateCell(h.FalseApproveRate), evidenceRateCell(h.FalseBlockRate), fmt.Sprint(h.Merged90d), evidenceRateCell(h.RevertRate)})
	}
	addNum("Per-hive evidence", "perhive", pt)

	if v.ShadowGate != nil {
		t := &evidenceReportTable{Head: []string{"Gate", "Decision", "PRs", "Merged", "Not merged", "Reverted within 14 days"}}
		for _, g := range []struct {
			name, verb string
			gate       EvidenceShadowGateView
		}{{"Review gate", "would have held", v.ShadowGate.ReviewGate}, {"Intent verification", "would have blocked", v.ShadowGate.Intent}} {
			if g.gate.N == 0 {
				continue
			}
			for _, side := range []struct {
				label string
				o     EvidenceShadowOutcomeRate
			}{{g.verb, g.gate.Held}, {"would have passed", g.gate.Passed}} {
				t.Rows = append(t.Rows, []string{g.name, side.label, fmt.Sprint(side.o.N), fmt.Sprint(side.o.Merged), fmt.Sprint(side.o.Open), evidenceRateCell(side.o.RevertRate)})
			}
		}
		addNum("Shadow gates", "shadow", t, fmt.Sprintf("%d %s shadow gates. Shadow mode only records; no merge was held or blocked anywhere.", v.ShadowGate.Hives, evidencePlural(v.ShadowGate.Hives, "hive reports", "hives report")))
	}
	rep.Sections = append(rep.Sections, numbers)

	rep.Sections = append(rep.Sections, evidenceReportSection{Title: "How to read these", Bullets: []string{
		fmt.Sprintf("Margin (95%%) — the range after a percentage. With this many cases, the true value is very likely inside that range; fewer cases mean a wider range. Below %d cases we print \"%s\" instead of a percentage, because a rate from so few cases is mostly luck.", minEvidenceN, evidenceDash),
		"Merged is not the same as correct. A merged PR may still have been a bad change that nobody caught.",
		"Open PRs are not failures; they count as \"not merged\" until they merge. Closed-without-merge is not recorded, so it also counts as not merged.",
		"Evidence records only exist since " + evidenceDataSince + ", and reverts have only been linked to their PR since " + evidenceRevertsSince + ". Older PRs are simply not in the data.",
		"Backfilled records (created for PRs that merged before recording began) carry no AI verdicts, so they add to coverage but not to the reviewer-confidence numbers.",
		"Nothing in this report is read by anything that decides whether a PR may merge. It records; it does not gate.",
	}})

	rep.Sections = append(rep.Sections, evidenceReportSection{Title: "What is not reported yet and why", Bullets: evidenceWaiting(v)})

	gt := &evidenceReportTable{Head: []string{"Section", "Grade", "How the grade is decided"}}
	for _, g := range evidenceGrades(v) {
		gt.Rows = append(gt.Rows, []string{g.Section, g.Grade, g.Basis})
	}
	rep.Sections = append(rep.Sections, evidenceReportSection{Title: "Grade per section",
		Paras: []string{"\"Not enough data\" means the graded rate is below the minimum sample; \"Not graded\" means there is no good or bad value for that section."}, Table: gt})

	rep.Footer = "How to read the hub evidence view: " + evidenceReportDocURL + evidenceReportDocAnchor
	return rep
}

// ---- Markdown ------------------------------------------------------------

// evidenceMDText escapes operator text (hive names) so it cannot inject
// Markdown HTML; evidenceMDCell additionally keeps table pipes literal.
func evidenceMDText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "<", "\\<"), "\n", " ")
}

func evidenceMDCell(s string) string {
	return strings.ReplaceAll(evidenceMDText(s), "|", "\\|")
}

func evidenceMDTable(b *strings.Builder, t *evidenceReportTable) {
	if t == nil {
		return
	}
	b.WriteString("| " + strings.Join(mapStrings(t.Head, evidenceMDCell), " | ") + " |\n")
	b.WriteString("|" + strings.Repeat("---|", len(t.Head)) + "\n")
	for _, r := range t.Rows {
		b.WriteString("| " + strings.Join(mapStrings(r, evidenceMDCell), " | ") + " |\n")
	}
	b.WriteString("\n")
}

func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

func evidenceMDSection(b *strings.Builder, s evidenceReportSection, level int) {
	b.WriteString(strings.Repeat("#", level) + " " + evidenceMDText(s.Title) + "\n\n")
	for _, p := range s.Paras {
		b.WriteString(evidenceMDText(p) + "\n\n")
	}
	for _, l := range s.Bullets {
		b.WriteString("- " + evidenceMDText(l) + "\n")
	}
	if len(s.Bullets) > 0 {
		b.WriteString("\n")
	}
	evidenceMDTable(b, s.Table)
	for _, sub := range s.Sub {
		evidenceMDSection(b, sub, level+1)
	}
}

// renderEvidenceReportMarkdown is the canonical report format.
func renderEvidenceReportMarkdown(rep evidenceReport) string {
	var b strings.Builder
	b.WriteString("# " + evidenceMDText(rep.Title) + "\n\n")
	for _, m := range rep.Meta {
		b.WriteString("- " + evidenceMDText(m) + "\n")
	}
	b.WriteString("\n")
	for _, s := range rep.Sections {
		evidenceMDSection(&b, s, 2)
	}
	b.WriteString("---\n\n" + rep.Footer + "\n")
	return b.String()
}

// ---- HTML ----------------------------------------------------------------

// evidenceReportCSS uses the dashboard's token names with their light-theme
// values inlined, so the page stands alone with no external assets.
const evidenceReportCSS = `:root{--bg:#f6f8fa;--surface:#ffffff;--text:#1f2328;--muted:#656d76;--border:#d0d7de;--accent:#0969da;--sp-2:.5rem;--sp-4:1rem;--sp-6:1.5rem;--r-lg:12px;--fs-sm:.9rem}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--surface:#161b22;--text:#e6edf3;--muted:#8d96a0;--border:#30363d;--accent:#58a6ff}}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
main{max-width:960px;margin:0 auto;padding:var(--sp-6);background:var(--surface)}
h1{color:var(--accent);margin-top:0}h2{border-bottom:1px solid var(--border);padding-bottom:var(--sp-2);margin-top:var(--sp-6)}h3{margin-top:var(--sp-6)}
table{border-collapse:collapse;width:100%;font-size:var(--fs-sm);margin:var(--sp-2) 0 var(--sp-4)}th,td{border:1px solid var(--border);padding:var(--sp-2);text-align:left;vertical-align:top}th{background:var(--bg)}
.meta{color:var(--muted);font-size:var(--fs-sm);padding-left:0;list-style:none}footer{margin-top:var(--sp-6);padding-top:var(--sp-4);border-top:1px solid var(--border);color:var(--muted);font-size:var(--fs-sm)}a{color:var(--accent)}`

func evidenceHTMLSection(b *strings.Builder, s evidenceReportSection, level int) {
	tag := fmt.Sprintf("h%d", level)
	b.WriteString("<" + tag + ">" + html.EscapeString(s.Title) + "</" + tag + ">\n")
	for _, p := range s.Paras {
		b.WriteString("<p>" + html.EscapeString(p) + "</p>\n")
	}
	if len(s.Bullets) > 0 {
		b.WriteString("<ul>\n")
		for _, l := range s.Bullets {
			b.WriteString("<li>" + html.EscapeString(l) + "</li>\n")
		}
		b.WriteString("</ul>\n")
	}
	if t := s.Table; t != nil {
		b.WriteString("<table><thead><tr>")
		for _, h := range t.Head {
			b.WriteString("<th>" + html.EscapeString(h) + "</th>")
		}
		b.WriteString("</tr></thead><tbody>\n")
		for _, r := range t.Rows {
			b.WriteString("<tr>")
			for _, c := range r {
				b.WriteString("<td>" + html.EscapeString(c) + "</td>")
			}
			b.WriteString("</tr>\n")
		}
		b.WriteString("</tbody></table>\n")
	}
	for _, sub := range s.Sub {
		evidenceHTMLSection(b, sub, level+1)
	}
}

// renderEvidenceReportHTML wraps the report in a minimal standalone page.
func renderEvidenceReportHTML(rep evidenceReport) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>" +
		html.EscapeString(rep.Title) + "</title><style>" + evidenceReportCSS + "</style></head><body><main>\n")
	b.WriteString("<h1>" + html.EscapeString(rep.Title) + "</h1>\n<ul class=\"meta\">\n")
	for _, m := range rep.Meta {
		b.WriteString("<li>" + html.EscapeString(m) + "</li>\n")
	}
	b.WriteString("</ul>\n")
	for _, s := range rep.Sections {
		evidenceHTMLSection(&b, s, 2)
	}
	b.WriteString("<footer>How to read the hub evidence view: <a href=\"" + evidenceReportDocURL + evidenceReportDocAnchor + "\" rel=\"noopener noreferrer\">review-evidence.md</a></footer>\n</main></body></html>\n")
	return b.String()
}

// ---- Handler -------------------------------------------------------------

// evidenceReportScope words the selection for the title and filename slug.
func evidenceReportScope(q url.Values, hives []RegistryEntry) (scope, slug string) {
	var ids []string
	for _, raw := range q["hive"] {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}
	level := strings.TrimSpace(q.Get("level"))
	switch {
	case len(ids) == 1:
		name := ids[0]
		for _, h := range hives {
			if h.ID == ids[0] && h.Name != "" {
				name = h.Name
			}
		}
		scope, slug = "hive "+name, ids[0]
	case len(ids) > 1:
		scope, slug = fmt.Sprintf("%d selected hives", len(ids)), "selection"
	case level != "":
		scope, slug = "hives at autonomy level L"+level, "level-"+level
	default:
		scope, slug = "whole fleet", "fleet"
	}
	if level != "" && len(ids) > 0 {
		scope += " at autonomy level L" + level
	}
	return scope, evidenceFilenameSlug(slug)
}

// evidenceFilenameSlug keeps a Content-Disposition filename to a safe set.
func evidenceFilenameSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "fleet"
	}
	return out
}

// handleEvidenceReport serves GET /api/hub/evidence/report (admin only):
// the drill-down's view for the same window / hive / level query, rendered
// as Markdown (default, format=md) or a standalone HTML page (format=html).
// download=1 sends it as an attachment; otherwise it renders inline.
func (s *HubServer) handleEvidenceReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now().UTC()
	win, err := parseEvidenceWindow(q, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "md"
	}
	if format != "md" && format != "html" {
		http.Error(w, "format must be md or html", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	entries := filterEvidenceRegistry(s.registry.Hives, q)
	scope, slug := evidenceReportScope(q, s.registry.Hives)
	view := buildEvidenceViewWindow(entries, win)
	s.mu.RUnlock()

	rep := buildEvidenceReport(view, scope, now)
	from, to := evidenceReportWindowDates(view.Window, now)
	var body, ctype string
	if format == "html" {
		body, ctype = renderEvidenceReportHTML(rep), "text/html; charset=utf-8"
	} else {
		body, ctype = renderEvidenceReportMarkdown(rep), "text/markdown; charset=utf-8"
	}
	filename := fmt.Sprintf("hive-evidence-%s-%s-%s.%s", slug, from, to, format)
	disposition := "inline"
	if q.Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, filename))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}
