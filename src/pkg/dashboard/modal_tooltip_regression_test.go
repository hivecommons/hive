package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestTabbedModalShellsStayFixedSize11268(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name      string
		shell     string
		body      string
		strip     string
		stripWant []string
		wants     []string
		bodyWant  []string
	}{
		{
			name:     "agent and governor config dialog",
			shell:    ".config-modal",
			body:     ".config-body",
			strip:    ".config-tabs",
			wants:    []string{"height: min(84vh, 760px)", "display: flex", "flex-direction: column", "overflow: hidden"},
			bodyWant: []string{"flex: 1 1 auto", "min-height: 0", "overflow: auto"},
		},
		{
			name:     "feedback dialog",
			shell:    ".feedback-modal",
			body:     ".feedback-body > [role=\"tabpanel\"]",
			strip:    ".feedback-tabs",
			wants:    []string{"height: min(82vh, 720px)", "display:flex", "flex-direction:column", "overflow: hidden"},
			bodyWant: []string{"flex:1 1 auto", "min-height:0", "overflow:auto"},
		},
		{
			name:     "strategy lab and knowledge modal shell",
			shell:    ".nous-config-dialog",
			body:     ".nous-config-dialog > .nous-config-body",
			strip:    ".nous-config-output-modes",
			wants:    []string{"height: min(82vh, 760px)", "display: flex", "flex-direction: column", "overflow: hidden"},
			bodyWant: []string{"flex: 1 1 auto", "min-height:0", "overflow: auto"},
		},
		{
			name:     "knowledge integrations modal",
			shell:    ".kb-integrations-modal",
			body:     ".kb-integrations-main",
			strip:    ".kb-integrations-sidebar",
			wants:    []string{"height: 100%", "min-height:0", "overflow: hidden"},
			bodyWant: []string{"min-height:0", "overflow: auto"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell := cssRule(t, html, tc.shell)
			for _, want := range tc.wants {
				if !strings.Contains(shell, want) {
					t.Fatalf("%s shell must keep a fixed flex-sized box; missing %q in %s", tc.name, want, shell)
				}
			}
			body := cssRule(t, html, tc.body)
			for _, want := range tc.bodyWant {
				if !strings.Contains(body, want) {
					t.Fatalf("%s body must absorb overflow internally; missing %q in %s", tc.name, want, body)
				}
			}
			strip := cssRule(t, html, tc.strip)
			for _, want := range tc.stripWant {
				if !strings.Contains(strip, want) {
					t.Fatalf("%s tab strip must not resize with tab content; missing %q in %s", tc.name, want, strip)
				}
			}
		})
	}
}

func TestTabbedModalPanelsDoNotUseInlineHeights11268(t *testing.T) {
	html := indexHTML(t)
	inlinePanelHeight := regexp.MustCompile(`(?is)<(?:section|div)[^>]+role=["']tabpanel["'][^>]+style=["'][^"']*height`)
	if inlinePanelHeight.MatchString(html) {
		t.Fatal("tab panels must not carry inline height styles that can override the fixed modal shell")
	}
}

func TestHelpTooltipsUseBodyFixedPortal11269(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const CONFIG_TOOLTIP_EDGE_PAD_PX = 8",
		"const CONFIG_TOOLTIP_GAP_PX = 6",
		"portal.id = 'config-tooltip-portal'",
		"document.body.appendChild(portal)",
		"const rect = info.getBoundingClientRect()",
		"window.addEventListener('scroll', () => { if (_configTooltipTrigger) positionConfigTooltip(_configTooltipTrigger); }, true)",
		"window.addEventListener('resize', () => { if (_configTooltipTrigger) positionConfigTooltip(_configTooltipTrigger); })",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("tooltip anchoring regression guard missing %q", want)
		}
	}
	portalRule := cssRule(t, html, ".config-tooltip-portal")
	for _, want := range []string{"display: block", "position: fixed"} {
		if !strings.Contains(portalRule, want) {
			t.Fatalf("tooltip portal must be body-level fixed positioning, missing %q in %s", want, portalRule)
		}
	}
	if strings.Contains(jsFunc(t, html, "positionConfigTooltip"), "pageX") || strings.Contains(jsFunc(t, html, "positionConfigTooltip"), "offsetParent") {
		t.Fatal("tooltip positioning must use trigger viewport rects, not page coordinates or offset parents")
	}
}
