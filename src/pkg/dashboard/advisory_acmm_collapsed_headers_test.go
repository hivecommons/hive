package dashboard

import (
	"strings"
	"testing"
)

func TestAdvisoryCollapsedHeaderUsesWeeklyAdvicePills(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"advisory-collapsed-advice",
		"advice-collapsed-pill",
		"advice-pill-impact",
		"function advisoryCollapsedRecommendations()",
		"function advisoryCollapsedImpact(rec)",
		"function openAdvisoryAdvicePill(index)",
		"ev.target.closest('button,a,input,select,textarea,[role=\"button\"]')",
		"Nothing to do — queue healthy",
		`data-action="openAdvisoryAdvicePill"`,
		`id="hive-advice-rec-${i}"`,
		"_lastHiveAdvice = dto;",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("advisory collapsed advice contract missing %q", want)
		}
	}
	if strings.Contains(jsFunctionBody(t, html, "function visualSectionSummary(sectionId, html, title)"), "sectionId === 'advisory-section' || sectionId === 'hive-advice-section'") {
		t.Fatal("advisory section still shares the old dot/count/sparkline collapsed renderer")
	}
}

func TestACMMCollapsedHeaderUsesReadableStatusPill(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"acmm-collapsed-pill",
		"acmm-pill-detail",
		"function acmmCollapsedDimensions(data)",
		"function acmmCollapsedEvalHtml(text)",
		"criteria_results",
		"levels.map",
		"`L${Number.isFinite(level) ? level : '—'} · ${detail}`",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("ACMM collapsed status contract missing %q", want)
		}
	}
	if strings.Contains(jsFunctionBody(t, html, "function visualSectionSummary(sectionId, html, title)"), "sectionId === 'acmm-eval-section' || sectionId === 'acmm-reco-section'") {
		t.Fatal("ACMM Eval section still shares the old seven-level mini dial renderer")
	}
}
