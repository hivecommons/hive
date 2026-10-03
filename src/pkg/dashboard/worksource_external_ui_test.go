package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// TestWorkSourceExternalPickerUIHasNoUndefinedCallees is the ADR-0020
// "Dashboard terminology" guard for #10174: the Settings → Work source
// type-picker offers "External provider (HTTP)" wired to
// work_source.type=external, its config block is rendered with the ADR's
// exact field set, and every function the block's inputs dispatch to is
// actually defined in the inline script — the renderAll()/hiveToast()
// ReferenceError class of bug (see linear_agent_ui_test.go).
func TestWorkSourceExternalPickerUIHasNoUndefinedCallees(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	for _, snippet := range []string{
		// The ADR's exact wording for the type-picker option.
		`'External provider (HTTP)'`,
		`['external', 'External provider (HTTP)']`,
		// The external config block's field set (ADR-0020 "Configuration").
		`data-arg0="external.name"`,
		`data-arg0="external.display_name"`,
		`data-arg0="external.base_url"`,
		`data-arg0="external.auth_token"`,
		`data-arg0="external.ca_bundle"`,
		`data-change-action="wsExternalReposChanged"`,
		`data-change-action="wsExternalHoldLabelsChanged"`,
		`data-change-action="wsExternalTimeoutChanged"`,
		// The per-source dropped-item counter.
		`ext.dropped_items`,
		// The run-detail source badge prefers display_name over source_type.
		`issue.source_display_name || sourceKind`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// Hive dashboards must never use native browser dialogs for this panel.
	extBlockStart := strings.Index(html, "${type === 'external' ?")
	extBlockEnd := strings.Index(html[extBlockStart+1:], "${type === 'linear' ?")
	if extBlockStart < 0 || extBlockEnd < 0 {
		t.Fatal("could not locate the external work-source config block in index.html")
	}
	extBlock := html[extBlockStart : extBlockStart+1+extBlockEnd]
	for _, dialog := range []string{"window.prompt(", "window.alert(", "window.confirm("} {
		if strings.Contains(extBlock, dialog) {
			t.Errorf("external work-source block uses native dialog %q; use an in-app component", dialog)
		}
	}

	for _, fn := range []string{
		"wsExternalReposChanged", "wsExternalHoldLabelsChanged", "wsExternalTimeoutChanged",
		"markWorkSourceDirty", "renderGovWorkSource", "esc",
	} {
		// Accept plain functions, const/let/var bindings and entries in the
		// data-change-action dispatch table (`name: function (event, A) {`).
		defined := regexp.MustCompile(`(?:function\s+` + regexp.QuoteMeta(fn) + `\s*\(|(?:const|let|var)\s+` + regexp.QuoteMeta(fn) + `\s*=|\b` + regexp.QuoteMeta(fn) + `\s*:\s*function\s*\()`)
		if !defined.MatchString(html) {
			t.Errorf("index.html calls %s() from the external work-source UI but never defines it", fn)
		}
	}
}
