package dashboard

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// featuresTabRenderers are the JS template functions that, together, produce
// everything an operator sees on Settings -> Features. renderGovReplanSection
// is included because renderGovFeatures interpolates it: the Stall Replan
// block renders on the Features tab even though it lives in its own function,
// and scoping the guard to renderGovFeatures alone would leave that half of
// the tab unprotected.
var featuresTabRenderers = []string{
	"renderGovFeatures",
	"renderGovReplanSection",
}

// featuresTabControl matches the two shapes a labelled control takes in the
// Settings markup: a toggle's caption span, and a field's label element.
//
// Section headings (the uppercase muted divs) are deliberately NOT matched.
// They carry no structural marker that distinguishes them from any other
// styled div, so pinning them would mean matching on inline CSS -- a guard
// that breaks the first time someone adjusts a margin. Several headings do
// carry help text; this test simply does not claim to enforce that.
var featuresTabControl = regexp.MustCompile(`(?s)<span class="config-toggle-label">.*?</span>\s*\n|<label>.*?</label>`)

var featuresTagPattern = regexp.MustCompile(`<[^>]+>`)

// extractJSFunction returns the source of the named JS function as it appears
// in index.html, from its `function name(` declaration to the closing brace at
// the same indentation. It fails the test if the function cannot be located,
// so a rename turns into a clear failure here rather than a guard that
// silently starts checking nothing.
func extractJSFunction(t *testing.T, html, name string) string {
	t.Helper()

	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s( not found in index.html -- was it renamed? "+
			"This guard must be repointed, not deleted.", name)
	}
	rest := html[start:]
	end := strings.Index(rest, "\n    }\n")
	if end < 0 {
		t.Fatalf("could not find the closing brace of %s in index.html", name)
	}
	return rest[:end]
}

// TestFeaturesTabControlsAllHaveTooltips enforces the invariant behind #7256:
// every labelled control on Settings -> Features must carry an (i) help
// affordance.
//
// The Features tab is a grab-bag of unrelated, non-obvious capabilities --
// OTLP export, the retro quality loop, the token mint, auto-plan, the review
// gate -- and before this guard only 5 of ~13 controls explained themselves.
// "Retro loop enabled" in particular gave an operator no way to tell what
// enabling it did or whether it would spend tokens.
//
// This is written as a coverage invariant over the whole tab rather than a pin
// on the controls that were missing, so a newly added feature cannot land
// without help text. That is the actual requirement; asserting the presence of
// seven specific tooltips would be satisfied by the current file and by
// nothing else.
func TestFeaturesTabControlsAllHaveTooltips(t *testing.T) {
	html := indexHTML(t)

	var missing []string
	total := 0
	for _, fn := range featuresTabRenderers {
		body := extractJSFunction(t, html, fn)
		for _, control := range featuresTabControl.FindAllString(body, -1) {
			total++
			if strings.Contains(control, "config-info") {
				continue
			}
			text := strings.TrimSpace(featuresTagPattern.ReplaceAllString(control, ""))
			missing = append(missing, fmt.Sprintf("%s: %q", fn, text))
		}
	}

	// Fail closed. If the extraction ever stops matching controls -- a markup
	// refactor, a changed class name -- every control is vacuously covered and
	// the guard passes while protecting nothing. #7247 taught this the hard
	// way: a rule nothing exercises looks exactly like a rule that holds.
	if total < 20 {
		t.Fatalf("only %d controls matched across %v; the extraction has "+
			"probably drifted from the markup, which would make this guard "+
			"vacuous", total, featuresTabRenderers)
	}

	if len(missing) > 0 {
		t.Errorf("%d Features-tab control(s) have no (i) tooltip (#7256). "+
			"Every control on this tab needs one -- add a "+
			`<span class="config-info">i<span class="config-tooltip">...</span></span> `+
			"inside the label, sourced from the config doc-comment:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestRetroTooltipExplainsTokenCost pins the one piece of tooltip *content*
// that is a safety property rather than a nicety.
//
// The retro loop is deterministic and free until an Analysis model is set, at
// which point it starts calling a model on every reconstructed trajectory.
// An operator enabling the loop needs to know that the cost is opt-in and
// where the opt-in lives, so this asserts both tooltips actually say so
// instead of merely existing.
func TestRetroTooltipExplainsTokenCost(t *testing.T) {
	html := indexHTML(t)
	body := extractJSFunction(t, html, "renderGovFeatures")

	retro := controlTooltip(t, body, "Retro loop enabled")
	if !strings.Contains(retro, "no model is called") || !strings.Contains(retro, "Analysis model") {
		t.Errorf("the Retro loop tooltip must say that no model is called "+
			"until an Analysis model is set, so enabling the loop is not "+
			"mistaken for a token spend; got:\n%s", retro)
	}

	analysis := controlTooltip(t, body, "Analysis model")
	if !strings.Contains(analysis, "spends tokens") {
		t.Errorf("the Analysis model tooltip must identify itself as the only "+
			"part of the retro loop that spends tokens; got:\n%s", analysis)
	}
}

func TestFormalVerificationFeatureTooltipAndACMMGate(t *testing.T) {
	html := indexHTML(t)
	body := extractJSFunction(t, html, "renderGovFeatures")

	tooltip := controlTooltip(t, body, "Formal verification (quality lane)")
	for _, want := range []string{
		"OFF by default",
		"effective only at ACMM L5 and L6",
		"formal/&lt;subsystem&gt;/",
		"reporting-only CI job",
		"one deduplicated issue per violated property",
		"src/docs/formal-verification.md",
	} {
		if !strings.Contains(tooltip, want) {
			t.Errorf("formal verification tooltip missing %q; got:\n%s", want, tooltip)
		}
	}
	if !strings.Contains(body, `data-section="features" data-key="formalEnabled"`) {
		t.Fatal("Formal verification toggle must save features.formalEnabled")
	}
	if !strings.Contains(body, "f.formalAvailable === true") || !strings.Contains(body, "Requires ACMM L") {
		t.Fatal("Formal verification toggle no longer renders the ACMM availability gate")
	}
	if !strings.Contains(body, "formalAvailable && f.formalEnabled") {
		t.Fatal("Formal verification toggle must not render on below the ACMM gate")
	}
}

// controlTooltip returns the tooltip text attached to the control whose
// caption starts with the given label.
func controlTooltip(t *testing.T, body, label string) string {
	t.Helper()

	idx := strings.Index(body, label)
	if idx < 0 {
		t.Fatalf("control %q not found on the Features tab", label)
	}
	rest := body[idx:]
	open := strings.Index(rest, `<span class="config-tooltip">`)
	if open < 0 {
		t.Fatalf("control %q has no tooltip", label)
	}
	rest = rest[open+len(`<span class="config-tooltip">`):]
	end := strings.Index(rest, "</span>")
	if end < 0 {
		t.Fatalf("unterminated tooltip on control %q", label)
	}
	return rest[:end]
}
