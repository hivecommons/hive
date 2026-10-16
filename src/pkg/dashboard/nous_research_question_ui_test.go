package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The Experiment Lab research question used to be capped at 40ch and
// ellipsised in the section header even though the header had room for it.
// It must now take the remaining header width and wrap instead of clipping.
func TestNousResearchQuestionHeaderIsNotClipped(t *testing.T) {
	html := indexHTML(t)
	rule := regexp.MustCompile(`\.nous-research-question\s*\{[^}]*\}`).FindString(html)
	if rule == "" {
		t.Fatal(".nous-research-question rule missing from index.html")
	}
	for _, banned := range []string{"max-width: 40ch", "text-overflow: ellipsis", "white-space: nowrap", "overflow: hidden"} {
		if strings.Contains(rule, banned) {
			t.Errorf(".nous-research-question still clips the question via %q: %s", banned, rule)
		}
	}
	for _, want := range []string{"flex: 1 1", "min-width: 0", "white-space: normal", "overflow-wrap: anywhere"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".nous-research-question lacks %q so it cannot fill and wrap the header: %s", want, rule)
		}
	}

	ensureBody := jsFunctionBody(t, html, "function ensureSectionCard(sectionId)")
	badgeSelector := regexp.MustCompile(`querySelectorAll\('\[id\$="-badge"\][^']*'\)`).FindString(ensureBody)
	if strings.Contains(badgeSelector, "#nous-research-question") {
		t.Fatal("research question is still migrated into the pill-badge cluster, which forces nowrap/uppercase")
	}
	if !strings.Contains(ensureBody, "querySelectorAll('#nous-research-question')") || !strings.Contains(ensureBody, "badges.insertAdjacentElement('afterend', el)") {
		t.Fatal("research question is not placed as its own flex item after the header badges")
	}

	summaries := jsFunctionBody(t, html, "function refreshDashboardSectionSummaries(data)")
	if strings.Contains(summaries, "nous-research-question") || !strings.Contains(summaries, "setSectionSummary('nous-section', nousSectionSummaryText(") {
		t.Fatal("collapsed section summary must show lab mode/experiment count, not duplicate the research question already in the header")
	}
}

// A stored research question may carry a pasted YAML key and folded-scalar
// marker ("research_question: > ..."). The header shows a muted
// "Research question:" label followed by the full question only.
func TestNousResearchQuestionHeaderRendersFullQuestionWithLabel(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: research question header rendering was not executed")
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

// Minimal DOM double covering what setRq() in renderNous() uses.
function el(tag) {
  return {
    tagName: tag, className: '', children: [], _text: '', title: '', hidden: false,
    get textContent() { return this._text || this.children.map(c => c.textContent).join(''); },
    set textContent(v) { this._text = String(v); this.children = []; },
    appendChild(c) { this.children.push(c); return c; },
  };
}
const document = {
  createElement: el,
  createTextNode: (t) => ({ textContent: String(t) }),
};
const rqEl = el('span');
` + nousSetRqSnippet(t, html) + `
setRq(nousResearchQuestionText('research_question: > ' + question));
assert.equal(rqEl.hidden, false);
assert.equal(rqEl.title, question, 'title attribute keeps the full question as a fallback');
assert.equal(rqEl.children.length, 3);
assert.equal(rqEl.children[0].className, 'nous-rq-label');
assert.equal(rqEl.children[0].textContent, 'Research question:');
assert.equal(rqEl.children[2].className, 'nous-rq-text');
assert.equal(rqEl.children[2].textContent, question, 'header carries the full question text');
assert.equal(rqEl.textContent, 'Research question: ' + question);
assert.doesNotMatch(rqEl.textContent, /research_question|>/, 'raw YAML key / marker must not render');
setRq('');
assert.equal(rqEl.hidden, true);
assert.equal(rqEl.textContent, '');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node research question header rendering failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// nousSetRqSnippet extracts the setRq arrow function from renderNous() so the
// node test exercises the real header rendering code.
func nousSetRqSnippet(t *testing.T, html string) string {
	t.Helper()
	body := jsFunctionBody(t, html, "function renderNous()")
	start := strings.Index(body, "const setRq = (text) => {")
	if start < 0 {
		t.Fatal("renderNous() no longer defines setRq")
	}
	end := strings.Index(body[start:], "\n      };")
	if end < 0 {
		t.Fatal("setRq arrow function end not found")
	}
	snippet := body[start : start+end+len("\n      };")]
	// The real helper refreshes section summaries; the test has no dashboard.
	return strings.Replace(snippet, "if (typeof refreshDashboardSectionSummaries === 'function' && window._lastStatus) refreshDashboardSectionSummaries(window._lastStatus);", "", 1)
}
