package dashboard

import (
	"strings"
	"testing"
)

func TestRepoCardActionBands9019SharedTableAndLabels(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "const ACTION_BANDS =")
	if start < 0 {
		t.Fatal("index.html missing shared ACTION_BANDS table")
	}
	end := strings.Index(html[start:], "function issueTip(")
	if end < 0 {
		t.Fatal("could not locate end of action band code")
	}
	bandCode := html[start : start+end]
	for _, want := range []string{
		"const ACTION_BANDS =",
		"key: 'unclaimed', label: 'Unclaimed', shortLabel: 'unclaimed'",
		"key: 'in-progress', label: 'Claimed', shortLabel: 'claimed'",
		"key: 'needs-triage', label: 'Needs triage', shortLabel: 'triage'",
		"key: 'waiting', label: 'Needs human', shortLabel: 'needs human'",
		"key: 'done', label: 'Confirm & close', shortLabel: 'close?'",
		"key: 'waiting', label: 'Needs human', shortLabel: 'needs human'",
		"human_acknowledged",
		"labels.has('approved-direction')",
		"else if (matches.needsTriage) band = 'needs-triage'",
	} {
		if !strings.Contains(bandCode, want) {
			t.Errorf("band code missing %q", want)
		}
	}
	for _, old := range []string{"Likely done", "Agent-filed", "Waiting on human", "return 'Ready'", "band = 'agent-filed'", "band = 'ready'"} {
		if strings.Contains(bandCode, old) {
			t.Errorf("band code still contains old band label/key %q", old)
		}
	}
}

func TestRepoCardActionBands9019RenderersUseSharedRules(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"ACTION_BANDS.issue.map(b => {",
		"ACTION_BANDS.pr.map(b => `<span class=\"repo-pr-band-title\" title=\"${esc(actionBandRule('pr', b.key))}\">${esc(b.label)}</span>`)",
		"return actionBandDef('pr', band).label;",
		"const label = actionBandDef('issue', band).label;",
		"return actionBandDef('issue', band).shortLabel;",
		"rule: actionBandRule('issue', band)",
		"rule: actionBandRule('pr', band)",
		"<title>${esc(s.rule)}</title>",
		"<div class=\"overview-chart-legend-row\" title=\"${esc(s.rule)}\">",
		"<div class=\"repo-issue-band-title\" title=\"${esc(g.rule)}\">",
		"<div class=\"repo-pr-band-title\" title=\"${esc(g.rule)}\">",
		"const key = info.band;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("shared action band wiring missing %q", want)
		}
	}
}

func TestRepoCardActionBands9019NoRawColourLiteralsInBandStyles(t *testing.T) {
	html := indexHTML(t)
	styleEnd := strings.Index(html, "</style>")
	if styleEnd < 0 {
		t.Fatal("index.html missing style end")
	}
	for _, line := range strings.Split(html[:styleEnd], "\n") {
		if !strings.Contains(line, "repo-issue-pill.") && !strings.Contains(line, "overview-issue-") && !strings.Contains(line, "overview-pr-") {
			continue
		}
		if strings.Contains(line, "#") || strings.Contains(line, "rgb(") || strings.Contains(line, "rgba(") || strings.Contains(line, "hsl(") {
			t.Errorf("band style should use colour tokens, got raw colour literal in %q", strings.TrimSpace(line))
		}
	}
}
