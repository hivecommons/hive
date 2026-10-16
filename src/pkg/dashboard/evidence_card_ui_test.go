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
// accuracy × model and shadow-gate (#12068) panels.
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
		"/api/evidence/summary?hours=${encodeURIComponent(hours)}",
		"if (repo) url += `&repo=${encodeURIComponent(repo)}`;",
		"if (hours !== prtHours || repo !== prtRepo) return;",
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
		"${bar('om', row.merged, 0)}${bar('oc', row.closed, 1)}${bar('or', row.reverted, 2)}",
		"no PRs with an evidence bundle in this window",
		"Schema v1 bundles record a merge but not a close-without-merge, so closed may read 0.",
		"Reverted means a reverted_by link to a revert PR within 14 d of merge",
		"Open PRs are excluded from the merged and reverted rates.",
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
		"evShadowGateSvg",
		"evClaimVsVerdictSvg",
		"escapeHtml",
	} {
		decl := regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+` + fn + `\s*\(`)
		if !jsDeclaredNear(html, fn, decl) {
			t.Errorf("Evidence card calls %q but it is never declared", fn)
		}
	}
}
