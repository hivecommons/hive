package dashboard

import (
	"strings"
	"testing"
)

// TestPromptTemplateTabScrollContainment pins #4850: the Prompt Template tab
// scroll pane must contain overscroll so wheel/touch at its edge does not chain
// into the dashboard behind the agent config modal. The editor/preview already
// carried #2766 containment; the gap was the tab wrapper itself when
// Import-from-repo expansion made the whole column taller than .config-body.
func TestPromptTemplateTabScrollContainment(t *testing.T) {
	html := indexHTML(t)

	tabTag := `id="prompt-template-tab" style="`
	idx := strings.Index(html, tabTag)
	if idx < 0 {
		t.Fatal(`index.html no longer renders #prompt-template-tab with an inline style — update this test alongside the markup (#4850)`)
	}
	styleEnd := strings.Index(html[idx+len(tabTag):], `"`)
	if styleEnd < 0 {
		t.Fatal("#prompt-template-tab inline style is unterminated")
	}
	style := html[idx+len(tabTag) : idx+len(tabTag)+styleEnd]
	for _, want := range []string{
		"overflow-y:auto",
		"overscroll-behavior:contain",
		"overscroll-behavior-y:contain",
		"min-height:0",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("#prompt-template-tab inline style is missing %q — tab scroll chains to the dashboard (#4850); style: %s", want, style)
		}
	}

	// Nested surfaces that operators actually wheel over must still contain.
	for _, snippet := range []string{
		"overscroll-behavior:contain;overscroll-behavior-y:contain\" placeholder=\"Enter your agent's kick template here.",
		`class="config-pre config-pre-fill" style="display:none;flex:1;min-height:300px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain"`,
		`max-height:120px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing Prompt Template nested containment %q (#4850)", snippet)
		}
	}
}

// TestAgentConfigSiblingTabScrollContainment audits sibling tab scroll panes in
// the same agent config dialog (#4850): Export YAML, Import paste, and Prior
// Prompts shell must carry the same overscroll containment convention.
func TestAgentConfigSiblingTabScrollContainment(t *testing.T) {
	html := indexHTML(t)

	for _, tc := range []struct {
		name    string
		snippet string
	}{
		{
			"export tab outer",
			`flex-direction:column;height:100%;min-height:0;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain">'
        + '<p style="font-size:0.75rem;color:var(--muted);margin-bottom:8px;flex-shrink:0">Portable agent definition`,
		},
		{
			"export yaml textarea",
			`tab-size:2;overscroll-behavior:contain;overscroll-behavior-y:contain">Loading...</textarea>`,
		},
		{
			"import paste textarea",
			`id="import-paste-input" rows="20" style="width:100%;font-family:var(--font-mono);font-size:0.75rem;resize:vertical;min-height:300px;overscroll-behavior:contain;overscroll-behavior-y:contain"`,
		},
		{
			"prior prompts shell outer",
			"Prompts sent to this agent — fully expanded, all variables resolved and pipeline data injected. Newest first.",
		},
	} {
		if !strings.Contains(html, tc.snippet) {
			t.Errorf("index.html is missing sibling-tab scroll containment for %s (#4850): %q", tc.name, tc.snippet)
		}
	}
	// Prior Prompts outer must carry the same tab-level containment as Prompt Template.
	phMarker := "Prompts sent to this agent — fully expanded"
	phIdx := strings.Index(html, phMarker)
	if phIdx < 0 {
		t.Fatal("prior prompts blurb missing")
	}
	// Walk back to the opening style= of the shell div (within ~400 chars).
	start := phIdx - 400
	if start < 0 {
		start = 0
	}
	window := html[start:phIdx]
	if !strings.Contains(window, "overscroll-behavior:contain") || !strings.Contains(window, "min-height:0") {
		t.Error("Prior Prompts shell outer is missing overscroll containment / min-height:0 (#4850)")
	}

	// .config-pre and .config-stats-list own-rule containment (not only the
	// multi-selector list) so regrouping selectors cannot drop them.
	for _, rule := range []string{".config-pre {", ".config-stats-list {"} {
		idx := strings.Index(html, rule)
		if idx < 0 {
			t.Errorf("index.html has no %q rule", rule)
			continue
		}
		end := strings.Index(html[idx:], "}")
		if end < 0 {
			t.Errorf("%q rule is unterminated", rule)
			continue
		}
		body := html[idx : idx+end]
		if !strings.Contains(body, "overscroll-behavior") {
			t.Errorf("%q rule is missing overscroll-behavior containment (#4850)", rule)
		}
	}
}
