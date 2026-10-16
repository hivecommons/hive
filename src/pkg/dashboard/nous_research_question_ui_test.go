package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// hivecommons/hive#12498 put the research question in the Experiment Lab
// section header; hivecommons/hive#12532 moved it onto the experiment cards
// (several experiments, several questions) and returned the header to the
// shape every other section has: title + ⓘ/⚙ + the collapsed
// "mode · N experiments" summary. Nothing may put a question back there.
func TestNousResearchQuestionIsNotInSectionHeader(t *testing.T) {
	html := indexHTML(t)
	for _, banned := range []string{"nous-research-question", "nous-rq-label", "nous-rq-text", "const setRq"} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html still references %q: the research question belongs on the experiment cards, not the section header", banned)
		}
	}
	header := regexp.MustCompile(`<h2[^>]*data-arg0="nous-section"[^>]*>.*?</h2>`).FindString(html)
	if header == "" {
		t.Fatal("Experiment Lab section header not found")
	}
	if !strings.Contains(header, "🧪 Experiment Lab</h2>") {
		t.Fatalf("Experiment Lab header must end with the title and nothing else: %s", header)
	}
	summaries := jsFunctionBody(t, html, "function refreshDashboardSectionSummaries(data)")
	if !strings.Contains(summaries, "setSectionSummary('nous-section', nousSectionSummaryText(") {
		t.Fatal("collapsed section summary must keep showing lab mode · experiment count")
	}
	ensureBody := jsFunctionBody(t, html, "function ensureSectionCard(sectionId)")
	if strings.Contains(ensureBody, "nous-research-question") {
		t.Fatal("ensureSectionCard still migrates a header research question element")
	}
	// The question renders on each card, cleaned of pasted YAML markers.
	card := extractJSFunction(t, html, "nousExperimentCard")
	if !strings.Contains(card, "nousResearchQuestionText(c.question)") || !strings.Contains(card, `class="campaign-row-title"`) {
		t.Fatalf("nousExperimentCard must render the cleaned question as the Inception-style row title: %s", card)
	}
}

// A stored research question may carry a pasted YAML key and folded-scalar
// marker ("research_question: > ..."). Cards show only the question.
func TestNousResearchQuestionTextStripsYAMLMarkers(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: research question normalisation was not executed")
	}
	script := `const assert = require('node:assert/strict');
` + jsFunctionBody(t, html, "function nousResearchQuestionText(raw)") + `
const question = 'Can prefilter gates be tightened to reduce false-positive holds without raising the median time-to-merge for first-time contributors across all monitored repositories?';
assert.equal(nousResearchQuestionText('research_question: > ' + question), question);
assert.equal(nousResearchQuestionText('> ' + question), question);
assert.equal(nousResearchQuestionText('>' + question), question);
assert.equal(nousResearchQuestionText('Research Question: |\n  ' + question + '\n'), question);
assert.equal(nousResearchQuestionText(question), question);
assert.equal(nousResearchQuestionText(''), '');
assert.equal(nousResearchQuestionText(null), '');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node research question normalisation failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
