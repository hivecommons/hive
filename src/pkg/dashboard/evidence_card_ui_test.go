package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// TestEvidenceCardInThroughputSection pins the Evidence sub-panel (#12064): it
// sits inside the Throughput section below the throughput card, reuses the
// prt-card chrome, follows the Throughput window/repo selector, and reads
// /api/evidence/summary for the outcome-by-author-kind (#12364), calibration,
// accuracy × model, boundary enforcement (#12368) and shadow-gate (#12068) panels.
func TestEvidenceCardInThroughputSection(t *testing.T) {
	html := indexHTML(t)
	section := strings.Index(html, `<div id="pr-throughput-section"`)
	if section < 0 {
		t.Fatal("pr-throughput-section missing")
	}
	end := strings.Index(html[section:], `<div id="advisory-section"`)
	if end < 0 {
		t.Fatal("cannot find end of pr-throughput-section markup")
	}
	markup := html[section : section+end]
	prt := strings.Index(markup, `id="pr-throughput-card"`)
	ev := strings.Index(markup, `<div id="evidence-card" class="prt-card advisory-subsection-card evidence-card"`)
	if prt < 0 || ev < 0 || ev < prt {
		t.Fatalf("evidence-card must sit inside pr-throughput-section below pr-throughput-card (prt=%d ev=%d)", prt, ev)
	}

	for _, snippet := range []string{
		"/api/evidence/summary?${query}",
		"if (repo) url += `&repo=${encodeURIComponent(repo)}`;",
		"if (query !== evWindowQuery() || repo !== prtRepo) return;",
		"async function fetchPRThroughput() {\n      fetchEvidenceSummary();",
		"dto.reviewer_calibration",
		"dto.reviewer_accuracy_by_model",
		"verdicts_model_unknown",
		`<details class="ev-panel" data-ev-panel="${key}"`,
		`n=${Number(n) || 0}`,
		"What this does not show:",
		"'Reviewer calibration'",
		"'Reviewer accuracy × model family'",
		"row.model === 'unknown' ? 'ev-unknown' : ''",
		`<svg class="ev-svg"`,
		".ev-svg .ev-unknown .ev-bar",
		"dto.shadow_gate || {}",
		"evPanel('shadow-gates', 'Shadow gates', shadow.n, evShadowGateSvg(shadow),",
		"no shadow-gate observations in this window",
		"[['held', `would have ${verb}`], ['passed', 'would have passed']]",
		".ev-svg .ev-bar.sr",
		// Panel 1 (#12364): outcome by author kind.
		"dto.outcomes_by_author_kind || {}",
		"evPanel('outcome-by-author', 'Outcome by author kind', outcomeN, evOutcomesSvg(outcomes),",
		"const EV_AUTHOR_KINDS = ['human', 'agent', 'bot'];",
		"reverted: Number(o.reverted_14d) || 0",
		"${bar('om', row.merged, 0)}${bar('oc', row.closed, 1)}${bar('or', row.reverted, 2)}${bar('ou', row.unknown, 3)}",
		"no PRs with an evidence bundle in this window",
		"A live bundle records a merge only when this hive merged, and never a close-without-merge, so a PR reviewed here and then merged by someone else, closed by hand or still open has no recorded outcome",
		"it is shown as unknown and excluded from every rate, not counted as open or as not merged.",
		"Reverted means a reverted_by link to a revert PR within 14 d of merge",
		// Unknown-outcome policy (#12514 follow-up): never a denominator, always a count.
		"prs_outcome_unknown",
		"unknown: Number(o.unknown) || 0",
		"outcome unknown ${row.unknown} (not in any rate)",
		".ev-svg .ev-bar.ou",
		"const EV_CALIB_MIN_BUCKET_N = 5;",
		"thin: known < EV_CALIB_MIN_BUCKET_N, merge: evRate(k && k.merged, known), block: evRate(k && k.blocks, n)",
		"const solid = pts.filter((q) => !q.thin && q.merge != null);",
		".ev-svg .ev-pt.ev-thin",
		"fa: evRate(s.approved_not_merged, approvalsKnown), fb: evRate(s.merged_after_block, blocksKnown)",
		"approved_unknown",
		"blocked_unknown",
		"false-approve (approved, closed without merge)",
		"records_ci_pending",
		"CI still running when captured ${Number(row.ci_pending) || 0}",
		"skipped and neutral checks are green, as the merge gate treats them",
		"${seg('sm', row.merged)}${seg('sr', row.reverted)}${seg('so', row.open)}${seg('su', row.unknown)}",
		".ev-svg .ev-bar.or",
		".ev-legend i.om",
		"dto.claim_vs_verdict",
		"evPanel('claim-vs-verdict', 'Claim vs verdict', claimN, evClaimVsVerdictSvg(claims),",
		"no agent-authored PRs with evidence in this window",
		"['ci_not_green', 'cv-ci', 'CI not green'], ['verdict_not_approve', 'cv-swarm', 'swarm not approve'], ['both', 'cv-both', 'both']",
		"records_empty_ci",
		"Only agent-authored PRs",
		".ev-svg .ev-bar.cv-both",
		// Typography / sizing (#evidence-card font bug): the chart is pinned to
		// its viewBox width so SVG text renders 1:1 at the dashboard's font
		// tokens, and panels tile in a grid instead of each spanning the card.
		"const EV_SVG_W = 320;",
		".ev-svg { display:block; width:320px; max-width:100%; height:auto;",
		".ev-svg text { fill:var(--muted); font-family:var(--font-ui); font-size:var(--fs-2xs); }",
		".ev-svg text.ev-lbl { fill:var(--text); font-size:var(--fs-xs); }",
		".evidence-card { margin-top: var(--sp-4); display:grid; grid-template-columns:repeat(auto-fit, minmax(min(380px, 100%), 1fr));",
		".evidence-card > .prt-head { grid-column:1 / -1;",
		"dto.work_balance",
		"evPanel('work-balance', 'Work balance', balanceN, evWorkBalanceSvg(balance),",
		"no audited issue opens or closes in this window",
		"[['human', 'wb-human', 'opened by humans'], ['agent', 'wb-agent', 'opened by agents'], ['bot', 'wb-bot', 'opened by bots']]",
		"row.throttled ?",
		"throttle_windows_unavailable",
		"Throttle windows not reported by this hive",
		"Issues only, from this hive's audit log",
		".ev-svg .ev-bar.wb-throttle",
		"'work-balance': (dto) => {",
		"const EV_BALANCE_STRONG = 0.8;",
		"const EV_BALANCE_FAIR = 0.5;",
		"evGrade(ratio, openedTotal, EV_BALANCE_STRONG, EV_BALANCE_FAIR, true)",
		// Panel 4 (#12368): boundary enforcement.
		"const counters = dto.write_counters && typeof dto.write_counters === 'object' ? dto.write_counters : null;",
		"const sentinel = dto.sentinel_hits_per_week || {};",
		"evPanel('boundary-enforcement', 'Boundary enforcement', attemptedN, evWriteCountersSvg(counters) + evSentinelSparkline(sentinel),",
		"const EV_WRITE_TIERS = ['ADVISORY', 'ISSUES_ONLY', 'ISSUES_AND_PRS', 'ISSUES_PRS_MERGE'];",
		"Object.entries(c.blocked_by_reason || {})",
		"row.reasons.slice(0, EV_TOP_REASONS)",
		"no write counters yet (the proxy has not recorded an agent write, or the counter file is absent)",
		"no sentinel hits recorded in this window",
		"${bar('wa', row.attempted, 0, row.attempted)}${bar('wb', row.blocked, 1,",
		`aria-label="Sentinel hits per ISO week"`,
		"Write counters are proxy-side observations",
		"Would-block counts from intent verification are not included in these bars",
		"direct writes that bypass the proxy are invisible here",
		".ev-svg .ev-bar.wb",
		".ev-legend i.wa",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence card missing snippet %q", snippet)
		}
	}
	// Every SVG helper must draw at the shared width; a hard-coded 320 would
	// silently drift from the CSS pin if the constant ever changes.
	if m := regexp.MustCompile(`const w = 320\b`).FindString(html); m != "" {
		t.Errorf("ev*Svg helpers must size from EV_SVG_W, found %q", m)
	}
	// Panel order follows the RFC numbering: 1 (outcome) renders first.
	p1 := strings.Index(html, "evPanel('outcome-by-author'")
	p2 := strings.Index(html, "evPanel('calibration'")
	if p1 < 0 || p2 < 0 || p1 > p2 {
		t.Errorf("outcome-by-author panel must render before calibration (p1=%d p2=%d)", p1, p2)
	}
	// Panel order follows the RFC numbering: 4 (boundary) before 5 (claim) before 8 (shadow).
	p4 := strings.Index(html, "evPanel('boundary-enforcement'")
	p5 := strings.Index(html, "evPanel('claim-vs-verdict'")
	p8 := strings.Index(html, "evPanel('shadow-gates'")
	if p4 < 0 || p5 < 0 || p8 < 0 || p4 > p5 || p5 > p8 {
		t.Errorf("boundary-enforcement panel must render before claim-vs-verdict and shadow-gates (p4=%d p5=%d p8=%d)", p4, p5, p8)
	}
}

// TestEvidenceCardUIHasNoUndefinedCallees is the ReferenceError guard for the
// Evidence card helpers.
func TestEvidenceCardUIHasNoUndefinedCallees(t *testing.T) {
	html := indexHTML(t)
	for _, fn := range []string{
		"fetchEvidenceSummary",
		"renderEvidenceSummary",
		"evPanel",
		"evRate",
		"evOutcomesSvg",
		"evCalibrationSvg",
		"evAccuracySvg",
		"evWriteCountersSvg",
		"evSentinelSparkline",
		"evShadowGateSvg",
		"evClaimVsVerdictSvg",
		"evWorkBalanceSvg",
		"escapeHtml",
		"evWindowControls",
		"evWindowLabel",
		"evWindowQuery",
		"evExplainerHtml",
		"toggleEvidenceExplainer",
		"evGrade",
		"evLimitsSentence",
	} {
		decl := regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+` + fn + `\s*\(`)
		if !jsDeclaredNear(html, fn, decl) {
			t.Errorf("Evidence card calls %q but it is never declared", fn)
		}
	}
}

// TestEvidenceCardWindowSelector pins the Evidence card's own window pills
// (24h · 7d · 30d · custom from/to), their localStorage persistence and the
// header scope text that follows the selection (RFC #12062).
func TestEvidenceCardWindowSelector(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const EV_WINDOWS = [[24, '24h'], [168, '7d'], [720, '30d']];",
		"const EV_WINDOW_STORAGE_KEY = 'hive.evidence.window';",
		"const EV_CUSTOM_MAX_DAYS = 3 * 366;",
		"localStorage.setItem(EV_WINDOW_STORAGE_KEY, JSON.stringify(evWindow))",
		"return evWindow.from ? `from=${encodeURIComponent(evWindow.from)}&to=${encodeURIComponent(evWindow.to)}` : `hours=${encodeURIComponent(evWindow.hours)}`;",
		`data-action="setEvidenceWindow" data-arg0="${h}" data-stop="1"`,
		`data-action="toggleEvidenceCustomRange" data-stop="1" aria-expanded="${evCustomOpen ? 'true' : 'false'}" aria-controls="ev-custom-range"`,
		`<div id="ev-custom-range" class="ev-custom-range" role="group"`,
		`<input type="date" name="from"`,
		`<input type="date" name="to"`,
		`data-action="applyEvidenceCustomRange" data-arg-types="t" data-stop="1">Apply</button>`,
		`'"To" must not be before "from".'`,
		"if (evWindow.from) return `${evShortDate(evWindow.from)} – ${evShortDate(evWindow.to)}`;",
		"return `last ${w ? w[1] : evWindow.hours + 'h'}`;",
		"🔬 Evidence · ${escapeHtml(scope)}",
		`class="gov-pr-models-toggle window-pills-toolbar ev-window-controls"`,
		".ev-custom-range input[type=\"date\"]",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence window selector missing snippet %q", snippet)
		}
	}
	// The card no longer follows the throughput pills.
	if strings.Contains(html, "const windowName = PRT_WINDOWS.find(([h]) => h === prtHours);\n      const scope = (dto.all_time") {
		t.Error("Evidence card scope must come from evWindowLabel(), not the throughput window")
	}
}

// TestEvidenceCardExplainers pins the plain-language ⓘ explainer per panel:
// keyboard-accessible toggle (aria-expanded/aria-controls, Esc closes), the
// four fixed headings plus "Why it matters", a live "What ours shows" section
// and the named grade thresholds documented in docs/review-evidence.md.
func TestEvidenceCardExplainers(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const EV_GRADE_MIN_N = 20;",
		"const EV_SHORT_WINDOW_HOURS = 48;",
		"const EV_CALIB_STRONG = 0.8;",
		"const EV_CALIB_FAIR = 0.6;",
		"const EV_ERR_STRONG = 0.10;",
		"const EV_ERR_FAIR = 0.25;",
		"const EV_REVERT_STRONG = 0.05;",
		"const EV_REVERT_FAIR = 0.15;",
		"const EV_SHADOW_FALSE_ALARM_STRONG = 0.25;",
		"const EV_SHADOW_FALSE_ALARM_FAIR = 0.5;",
		"const EV_BLOCKED_STRONG = 0.05;",
		"const EV_BLOCKED_FAIR = 0.15;",
		"const EV_DATA_SINCE = '2026-10-08';",
		"const EV_REVERTS_SINCE = '2026-10-15';",
		"const EV_GRADES = { strong: 'Strong', fair: 'Fair', weak: 'Weak', none: 'Not enough data' };",
		"'outcome-by-author': (dto) => {",
		"calibration: (dto) => {",
		"'accuracy-by-model': (dto) => {",
		"'shadow-gates': (dto) => {",
		"'boundary-enforcement': (dto) => {",
		`<button type="button" class="ev-info" data-action="toggleEvidenceExplainer" data-arg0="${key}" data-stop="1" data-prevent="1" aria-expanded="${open ? 'true' : 'false'}" aria-controls="${id}" aria-label="Explain ${escapeHtml(title)}">ⓘ</button>`,
		`class="ev-explainer" role="region"`,
		"section('What is measured', x.measured)",
		"section('Why it matters', x.matters)",
		"section('How it is measured', x.how)",
		"section('What good looks like', x.good)",
		"section('What ours shows', x.ours)",
		"evGradeLine(x.grade, x.reason)",
		"if (e.key !== 'Escape' || !evExplainOpen.size) return;",
		"is reported as unknown and left out of every rate, never counted as \"not merged\". Live evidence (verdicts, CI, sentinel) has only been recorded since ${EV_DATA_SINCE} and reverts only since ${EV_REVERTS_SINCE}; backfilled records reach further back but carry only author, outcome and dates.",
		"false-approve rate = approvals on PRs known not to have merged ÷ approvals on PRs with a known outcome",
		"merged ÷ verdicts on PRs with a known outcome",
		"CI not green rate = CI not green ÷ PRs whose CI had settled green or red",
		"A well-calibrated reviewer would be near 90–100% in the top bucket.",
		".ev-explainer[hidden] { display:none; }",
		".ev-grade.weak",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence explainer missing snippet %q", snippet)
		}
	}
	// Every panel rendered by renderEvidenceSummary has an explainer entry.
	for _, m := range regexp.MustCompile(`evPanel\('([a-z0-9-]+)'`).FindAllStringSubmatch(html, -1) {
		key := m[1]
		if !strings.Contains(html, "'"+key+"': (dto) => {") && !strings.Contains(html, "\n      "+key+": (dto) => {") {
			t.Errorf("panel %q has no EV_EXPLAINERS entry", key)
		}
	}
}

// TestEvidenceCardWindowTooShortHint pins that a panel with too few samples
// for a grade points at a longer preset window when the summary's
// available_windows says one would hold more PRs, instead of a bare "Not
// enough data"; and that the custom range accepts backfilled history.
func TestEvidenceCardWindowTooShortHint(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const EV_AVAILABLE_KEYS = [['all', 'all time'], ['720', '30d'], ['168', '7d']];",
		"function evWindowHint(dto) {",
		"dto.available_windows",
		"return ` — window too short: ${counts} PRs; switch to ${presets.length ? presets.join(' or ') : 'a custom range'}`;",
		"`needs at least ${EV_GRADE_MIN_N} merged PRs${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} known-outcome verdicts in the top bucket${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} known-outcome verdicts${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} stopped-and-merged PRs${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} agent PRs with captured checks${evWindowHint(dto)}`",
		`<div class="ev-note ev-window-hint">No PRs opened in this window${escapeHtml(windowHint)}.</div>`,
		"by the PR\\u2019s open time, not when its record was written",
		"PRs are placed in the window by their open time, never by when a record was written.",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence window hint missing snippet %q", snippet)
		}
	}
	for _, stale := range []string{
		"Closed-without-merge is not recorded yet, so a closed PR still shows as open.",
		"so closed may read 0",
		"a PR that is still open counts as not merged",
		"an approval on a still-open PR counts as a false approve",
		"reads as open",
	} {
		if strings.Contains(html, stale) {
			t.Errorf("stale closed-state copy still present: %q", stale)
		}
	}
}
