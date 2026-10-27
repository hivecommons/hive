package dashboard

import (
	"strings"
	"testing"
)

// TestEvidenceCardOpensMergesPanel pins the Issue opens : PR merges panel: it
// is an evPanel keyed like its server card, reads issue_opens_pr_merges,
// mirrors the server's band thresholds for the explainer, renders the
// headline against the governor's mode band, a per-repo table with a fleet
// total row and a rolling-30d sparkline shaded with the parity band.
func TestEvidenceCardOpensMergesPanel(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"evPanel('opens-merges', 'Issue opens : PR merges', opensMerges.n, evOpensMergesHtml(opensMerges),",
		"const opensMerges = dto.issue_opens_pr_merges && typeof dto.issue_opens_pr_merges === 'object' ? dto.issue_opens_pr_merges : {};",
		"'opens-merges': (dto) => {",
		"const EV_OM_DRAIN_MAX = 1;",
		"const EV_OM_SCOUT_MIN = 1, EV_OM_SCOUT_MAX = 2;",
		"const EV_OM_PARITY_TOLERANCE = 0.25;",
		"const EV_OM_ROLLING_DAYS = 30;",
		"const EV_OM_SPARK_TRAIL_DAYS = 7;",
		"const EV_OM_STATUS = { in_band: 'in band', above: 'backlog growing', below: 'scouting below cadence', no_data: 'no data' };",
		"function evOmRatioText(row) {",
		"function evOmVerdict(row, band) {",
		"function evOmSparkline(days, band) {",
		"function evOpensMergesHtml(om) {",
		"evDetails['opens-merges'] = ",
		"opens per merge</b> · ${escapeHtml(band.mode || 'unknown')} mode expects ${escapeHtml(band.label || '—')} ${escapeHtml(mark)}",
		`<div class="ev-table-wrap"><table class="ev-table ev-om-table"><thead><tr><th>Repository</th><th class="num">Opens</th><th class="num">Merges</th><th class="num">Ratio</th>`,
		"${row(total, 'ev-om-total')}",
		`<rect class="ev-om-band"`,
		`<line class="ev-axis ev-om-parity"`,
		"Descriptive, not causal; the ratio says how the hive’s filing pace compares with its landing pace",
		"Merges follow the Outcome by author kind card",
		"surge / busy expect ≤ ${EV_OM_DRAIN_MAX} opens per merge (drain), quiet / idle expect ${EV_OM_SCOUT_MIN}–${EV_OM_SCOUT_MAX} (scout, bounded)",
		".ev-table tr.ev-om-total td { font-weight:600; color:var(--text); }",
		".ev-om-bar rect.ev-om-above { fill:var(--red); }",
		".ev-svg .ev-om-band { fill:var(--green); opacity:.12; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Issue opens : PR merges panel missing snippet %q", snippet)
		}
	}
	// The panel sits between Work balance and Shadow gates, away from the
	// Issue reopen rate card.
	wb := strings.Index(html, "evPanel('work-balance'")
	om := strings.Index(html, "evPanel('opens-merges'")
	sg := strings.Index(html, "evPanel('shadow-gates'")
	if wb < 0 || om < 0 || sg < 0 || !(wb < om && om < sg) {
		t.Errorf("opens-merges panel must render between work-balance and shadow-gates (wb=%d om=%d sg=%d)", wb, om, sg)
	}
}
