package dashboard

import (
	"strings"
	"testing"
)

// These tests pin the STRUCTURE of the agent Configuration dialog's single-save
// behavior and its modal scroll containment in the embedded static/index.html.
// The behavior itself is browser JS (fetch, wheel events, overscroll) and cannot
// run here — what these guarantee is that the wiring survives future edits.
// See kubestellar/hive#2766: the Prompt Template tab used to carry its own green
// "Save Template" button next to the dialog's orange footer "Save", and operators
// clicked the wrong one.

// TestPromptTemplateSaveButtonRemoved asserts the separate "Save Template"
// button markup is gone from the Prompt Template tab. Its return would restore
// the two-save-buttons confusion #2766 reports.
func TestPromptTemplateSaveButtonRemoved(t *testing.T) {
	html := indexHTML(t)
	if strings.Contains(html, `>Save Template</button>`) {
		t.Error(`index.html still renders a "Save Template" button — #2766 regressed: ` +
			`the footer Save must be the only save in the agent config dialog`)
	}
	// The tab should instead tell the operator where the save lives.
	if !strings.Contains(html, `Edits are saved by the <strong style="color:var(--text)">Save</strong> button below.`) {
		t.Error("index.html is missing the Prompt Template hint pointing at the footer Save button")
	}
}

// TestFooterSavePersistsPromptTemplate asserts saveConfig() folds the prompt
// template into the dialog's single save: it captures the live editor, treats a
// dirty template as a reason to save even when no `dirty` section changed, and
// awaits the template PUT as part of the same success accounting.
func TestFooterSavePersistsPromptTemplate(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// saveConfig() consults the template alongside the dirty sections.
		"const templateDirty = !!_configState.promptTemplateDirty;",
		"if (Object.keys(dirty).length === 0 && !templateDirty) {",
		// ...and awaits the template persistence, failing the save if it fails.
		"if (templateDirty && !(await savePromptTemplate())) success = false;",
		// Create mode has no agent to PUT to; the template rides the payload.
		"agent.kick_template = _configState.promptTemplateValue;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the footer Save no longer persists the prompt template (#2766)", snippet)
		}
	}
}

// TestPromptTemplateDirtyTracking asserts the editor records its edits on
// _configState (so they survive a tab switch) and that capturePromptTemplate
// snapshots the textarea before saving — the latter is what makes a save issued
// while the Preview pane is showing still commit the edited text.
func TestPromptTemplateDirtyTracking(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function markPromptTemplateDirty(el)",
		"function capturePromptTemplate()",
		`data-input-action="markPromptTemplateDirty"`,
		"_configState.promptTemplateDirty = true;",
		// Re-entering the tab must not clobber a pending edit with the server copy.
		"if (el && !_configState.promptTemplateDirty) { el.value = data.prompt || ''; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — prompt-template edit tracking regressed", snippet)
		}
	}
}

// TestPromptTemplateSaveNoDoubleToastAndKeepsDialogOpen asserts savePromptTemplate
// reports success/failure to its caller rather than raising its own success toast
// (which would double-toast next to "Configuration saved"), and that a failure
// path exists so a failed template save does not silently close the dialog.
func TestPromptTemplateSaveNoDoubleToastAndKeepsDialogOpen(t *testing.T) {
	html := indexHTML(t)
	if strings.Contains(html, "showToast('Template saved'") {
		t.Error(`savePromptTemplate still raises its own "Template saved" toast — ` +
			`the footer Save must emit exactly one success toast`)
	}
	for _, snippet := range []string{
		"showToast((result.data && result.data.error) || 'Failed to save prompt template', 'error');",
		"showToast('Network error saving prompt template: ' + err.message, 'error'); return false;",
		// Reuses the pre-existing endpoint — no new API was invented.
		"return fetch('/api/config/agent/' + agentName + '/prompt', {",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — prompt-template save error handling regressed", snippet)
		}
	}
}

// TestPromptTemplateStateResetOnOpenAndClose asserts the pending template edit is
// cleared both when the dialog opens and when it closes, so Cancel / X / Escape
// discard it and a later dialog never inherits a stale edit.
func TestPromptTemplateStateResetOnOpenAndClose(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"isCreate, initialTabOverride: initialTabOverride || null, promptTemplateDirty: false, promptTemplateValue: '' };",
		"_configState = { type: null, agent: null, tab: null, data: {}, dirty: {}, promptTemplateDirty: false, promptTemplateValue: '' };",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — pending prompt-template edit is not reset with the dialog", snippet)
		}
	}
}

// TestModalScrollContainment asserts overscroll-behavior containment on the
// modal's scrollable surfaces. Without it, scrolling past the end of the prompt
// template textarea chains to the dashboard behind the modal (#2766).
func TestModalScrollContainment(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// The modal body scroll region.
		".config-body {",
		// The CSS rule covering the template editor + preview.
		"#prompt-template-editor,",
		"#prompt-template-preview {",
		// Inline containment on the editor, preview, and variable picker, so
		// the containment holds even if the element is restyled inline.
		"resize:vertical;white-space:pre;tab-size:2;overscroll-behavior:contain;overscroll-behavior-y:contain",
		"display:none;flex:1;min-height:300px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain",
		"max-height:120px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — modal scroll containment regressed (#2766)", snippet)
		}
	}
	// The .config-body rule itself must carry the containment.
	idx := strings.Index(html, ".config-body {")
	if idx < 0 {
		t.Fatal("index.html has no .config-body rule")
	}
	end := strings.Index(html[idx:], "}")
	if end < 0 {
		t.Fatal(".config-body rule is unterminated")
	}
	if !strings.Contains(html[idx:idx+end], "overscroll-behavior: contain;") {
		t.Error(".config-body rule is missing overscroll-behavior: contain — modal body scroll chains to the page")
	}
	// Other bounded overlay surfaces must not hand their wheel delta to the
	// dashboard when they reach an edge (#4805).
	for _, selector := range []string{
		".oc-sidebar {",
		".gh-setup-modal {",
		".acmm-dialog {",
		".config-stats-list {",
		".hive-chat-panel {",
		".hive-chat-messages {",
		".hive-chat-input {",
		".chat-raw-details pre {",
		".nous-config-body,",
		".welcome-body {",
		".gh-auth-overlay, .cred-dialog-overlay, .config-overlay, .hive-dialog-overlay, .nous-config-overlay, .acmm-dialog-overlay {",
	} {
		idx := strings.Index(html, selector)
		if idx < 0 {
			t.Errorf("index.html has no %s rule", selector)
			continue
		}
		end := strings.Index(html[idx:], "}")
		if end < 0 || !strings.Contains(html[idx:idx+end], "overscroll-behavior: contain;") {
			t.Errorf("%s rule is missing overscroll containment (#4805)", selector)
		}
	}
	if !strings.Contains(html, `.nous-config-body [style*="overflow-y:auto"],`) {
		t.Error("KB inline scrollers are missing containment coverage (#4805)")
	}
}

// TestModalBodyScrollLockReused asserts the pre-existing single body scroll-lock
// mechanism is still the one in force. It is MutationObserver-driven, so it
// covers Cancel, X, and Escape without per-modal bookkeeping; a second, competing
// mechanism must not be added alongside it.
func TestModalBodyScrollLockReused(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"(function installModalScrollLock() {",
		"function sync() { document.body.classList.toggle('modal-open', anyOverlayOpen()); }",
		"body.modal-open { overflow: hidden; }",
		// The config overlay is one of the observed overlays.
		"var OVERLAY_SELECTOR = '.config-overlay,",
		"function observeOverlays() {",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the shared modal body scroll lock regressed", snippet)
		}
	}
	if strings.Contains(html, "document.body.style.overflow") {
		t.Error("a modal still manages body.style.overflow directly instead of using the shared modal-open lock")
	}
}
