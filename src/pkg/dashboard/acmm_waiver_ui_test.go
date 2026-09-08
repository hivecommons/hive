package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A waived criterion counts as passed, so every score, bar and tile reads the
// same as a real detection — that is what the operator asked for. The risk it
// creates is a row showing ✅ above a pattern list none of whose files exist,
// with nothing on screen to resolve the contradiction. These tests pin the
// marker and the justification that resolve it.

// TestACMMWaiverDetailRendersJustification executes the panel's own helper so
// the assertion is about what a reader sees, not about the source text.
func TestACMMWaiverDetailRendersJustification(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the waiver rendering rule was NOT executed by this run; the structure tests below still ran")
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "escapeHtml") + "\n" +
		jsFunc(t, html, "acmmWaiverDetail") + "\n" +
		jsFunc(t, html, "acmmLevelWaivedCriteria") + "\n" +
		jsFunc(t, html, "acmmLevelWaiverMark") + "\n" + acmmWaiverAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "waiver.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("ACMM waiver rendering check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const acmmWaiverAssertions = `
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

// A criterion that was actually detected renders nothing extra.
check('detected criterion gets no waiver block',
  acmmWaiverDetail({ id: 'acmm:x', passed: true }) === '');

const waived = acmmWaiverDetail({
  id: 'acmm:ai-fix-workflow', passed: true, waived: true,
  waiver_satisfied_by: 'hive',
  waiver_reason: 'Removed in PR #118; hive is the sole autonomous path.'
});
check('waived block names where the capability lives', waived.includes('hive'));
check('waived block carries the reason', waived.includes('PR #118'));
check('waived block is marked as a waiver', waived.includes('Waived'));

// The reason is repo-supplied text rendered into the panel: it must be
// escaped. A repo that can inject markup here can inject it into the
// operator's dashboard by committing a file.
const hostile = acmmWaiverDetail({
  waived: true, waiver_satisfied_by: '<img src=x onerror=alert(1)>',
  waiver_reason: '</div><script>alert(2)</script>'
});
check('satisfied_by is escaped', !hostile.includes('<img'));
check('reason is escaped', !hostile.includes('<script>'));

// Missing optional fields must not print "undefined" at the reader.
const bare = acmmWaiverDetail({ waived: true });
check('absent fields do not render undefined', !bare.includes('undefined'));

// ---- the level line ----
// A level whose ratio reads full green but part of which is waived must
// carry the mark. This is the altitude most operators read; without it the
// waiver is only visible two clicks down.
const l4 = { level: 4, name: 'Security-Aware', passed: true, matched: 9, total: 9, detected: 7, threshold: 0.7 };
const sensi = {
  levels: [l4],
  criteria_results: [
    { level: 4, name: 'AI-fix-requested workflow', passed: true, waived: true, waiver_satisfied_by: 'hive' },
    { level: 4, name: 'Automated review application', passed: true, waived: true, waiver_satisfied_by: 'hive' },
    { level: 4, name: 'Nightly compliance', passed: true },
    { level: 3, name: 'Something else', passed: true, waived: true, waiver_satisfied_by: 'hive' },
  ],
};

const mark = acmmLevelWaiverMark(sensi, 4);
check('a level containing waivers is marked', mark.includes('*'));
check('the mark is red', mark.includes('var(--red)'));
check('the mark names both waived criteria', mark.includes('AI-fix-requested workflow') && mark.includes('Automated review application'));
check('the mark counts only this level', !mark.includes('Something else'));
check('a passing level states the rule', mark.includes('never advance a level'));

// A level with no waivers renders nothing, so the asterisk means something.
check('a clean level is unmarked', acmmLevelWaiverMark(sensi, 99) === '');
check('a scope with no criteria is unmarked', acmmLevelWaiverMark(null, 4) === '');

// The reconciliation case: full ratio, still not passed. The tooltip has to
// explain the contradiction the operator is looking at.
const blocked = {
  levels: [{ level: 5, name: 'Semi-Autonomous', passed: false, matched: 5, total: 5, detected: 0, threshold: 0.7 }],
  criteria_results: [
    { level: 5, name: 'A', passed: true, waived: true, waiver_satisfied_by: 'hive' },
  ],
};
const blockedMark = acmmLevelWaiverMark(blocked, 5);
check('a blocked level reports the detected count', blockedMark.includes('0/5 detected'));
check('a blocked level says waivers cannot advance', blockedMark.includes('cannot advance a level'));

if (fails) { console.log(fails + ' waiver rendering assertion(s) failed'); process.exit(1); }
`

// TestACMMCriterionRowMarksWaived pins that the row itself carries the marker.
// Without it the row is a bare ✅ over a pattern list that matches nothing.
func TestACMMCriterionRowMarksWaived(t *testing.T) {
	row := jsFunc(t, indexHTML(t), "acmmCriterionRow")

	for _, want := range []string{
		"c.waived",            // the row branches on the flag at all
		"waivedChip",          // ...and renders the marker
		"acmmWaiverDetail(c)", // ...and the justification in the body
	} {
		if !strings.Contains(row, want) {
			t.Errorf("acmmCriterionRow() does not reference %q — a waived criterion would render as an ordinary pass", want)
		}
	}
}

// TestACMMWaivedCriterionOffersNoGapIssue: the "Open Issue" button is gated on
// !c.passed, and a waived criterion passes. Pinning it keeps a later edit from
// switching the gate to something like "not detected", which would re-file a
// gap issue on every scan for a gap the repo already answered — the same
// unclosable loop the L0 code-style criterion comment warns about.
func TestACMMWaivedCriterionOffersNoGapIssue(t *testing.T) {
	row := jsFunc(t, indexHTML(t), "acmmCriterionRow")

	if !strings.Contains(row, "if (!c.passed) {") {
		t.Error("acmmCriterionRow() no longer gates the gap-issue button on !c.passed; a waived criterion may now re-file an issue on every scan")
	}
}

// TestACMMLevelRenderersMarkWaivers pins that every place a level line is
// drawn asks for the mark. There are three — the per-repo bars inside the
// multi-repo list, the single-repo bars, and the all-criteria dialog — and a
// waiver invisible in one of them is a level that reads clean green depending
// only on which view the operator happened to open.
func TestACMMLevelRenderersMarkWaivers(t *testing.T) {
	html := indexHTML(t)

	if got := strings.Count(html, "acmmLevelWaiverMark("); got < 4 {
		t.Errorf("acmmLevelWaiverMark is called %d time(s): 1 definition + 3 level renderers expected; a level view is unmarked", got)
	}

	for _, fn := range []string{"acmmShowCodebaseDialog", "acmmShowAllCriteriaDialog"} {
		body, ok := jsFuncOptional(html, fn)
		if !ok {
			continue
		}
		if !strings.Contains(body, "acmmLevelWaiverMark(") {
			t.Errorf("%s() draws level lines without the waiver mark", fn)
		}
	}
}

// jsFuncOptional is jsFunc without the fatal, for renderers that may be
// renamed: a missing function is reported by the count check above rather
// than failing this one on a rename alone.
func jsFuncOptional(html, name string) (string, bool) {
	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := strings.Index(html[start:], "{") + start; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start : i+1], true
			}
		}
	}
	return "", false
}
