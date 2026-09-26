package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGovernorNextRunDoesNotRenderPastTimestamp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the governor next-run rendering rule was NOT executed")
	}
	html := indexHTML(t)
	for _, snippet := range []string{
		"function formatGovernorNextRun(",
		"formatGovernorNextRun(gov)",
		"due now",
		"overdue ${formatStatusPayloadAge(overdueMs)}",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "${escapeHtml(gov.nextKick || '—')}") {
		t.Fatal("NEXT RUN tile still renders gov.nextKick directly")
	}

	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\n")
	script.WriteString(jsFunc(t, html, "formatStatusPayloadAge") + "\n")
	script.WriteString(jsFunc(t, html, "formatGovernorNextRun") + "\n")
	script.WriteString(`
const now = Date.parse('2026-09-25T16:15:00Z');
assert.equal(formatGovernorNextRun({
  nextKick: '9/25 12:11 PM EDT',
  nextKickAt: '2026-09-25T16:11:00Z'
}, now), 'overdue 4m');
assert.equal(formatGovernorNextRun({
  nextKick: '9/25 12:14 PM EDT',
  nextKickAt: '2026-09-25T16:14:30Z'
}, now), 'due now');
assert.equal(formatGovernorNextRun({
  nextKick: '9/25 12:16 PM EDT',
  nextKickAt: '2026-09-25T16:16:00Z'
}, now), '9/25 12:16 PM EDT');
assert.equal(formatGovernorNextRun({}, now), '—');
console.log('ok');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("governor next-run check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
