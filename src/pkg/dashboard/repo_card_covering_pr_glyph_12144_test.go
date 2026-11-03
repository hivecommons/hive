package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// hivecommons/hive#12144: the covering-PR glyph on repo-card issue pills is
// derived from hive's own PR-claim evidence (linked_prs) for actionable AND
// suppressed issues, with a tooltip that distinguishes a closing PR from a
// partial (Refs-only) one, and renders exactly one badge per issue.
func TestRepoCardCoveringPRGlyphFromClaimEvidence(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const prBadge = isHeldIssue ? '' : issueCoveringPRBadge(i);",
		"const prBadge = issueCoveringPRBadge(i);",
		"${prBadge ? repoPillActionCluster([prBadge]) : ''}",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: covering-PR glyph behavior was not executed")
	}
	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\n")
	for _, name := range []string{"esc", "issueCoveringPR", "issueCoveringPRBadge"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
assert.equal(issueCoveringPRBadge({}), '');
assert.equal(issueCoveringPRBadge({ linked_prs: [] }), '');

// Partial (reference) claim: glyph shows, tooltip says partial.
const partial = issueCoveringPRBadge({ number: 12005, linked_prs: [{ number: 12025, state: 'open', url: 'https://github.com/hivecommons/hive/pull/12025', reference: true }] });
assert.match(partial, /title="covered by PR #12025 \(partial\)"/);
assert.match(partial, /href="https:\/\/github.com\/hivecommons\/hive\/pull\/12025"/);
assert.equal((partial.match(/🔗/g) || []).length, 1);

// Closing claim.
const closing = issueCoveringPRBadge({ number: 7, linked_prs: [{ number: 9, state: 'open', url: 'u', closing: true }] });
assert.match(closing, /title="closes via PR #9"/);

// Branch-name claim (neither closing nor reference).
assert.match(issueCoveringPRBadge({ linked_prs: [{ number: 3, state: 'open' }] }), /title="covered by PR #3"/);

// Merged claim.
assert.match(issueCoveringPRBadge({ linked_prs: [{ number: 4, state: 'merged', merged: true }] }), /title="merged via PR #4"/);

// Two PRs: one badge, closing PR primary, count shown, both in the tooltip.
const two = issueCoveringPRBadge({ linked_prs: [{ number: 11, state: 'open', reference: true }, { number: 12, state: 'open', closing: true }] });
assert.equal((two.match(/<a /g) || []).length, 1);
assert.match(two, /🔗×2</);
assert.match(two, /title="closes via PR #12; covered by PR #11 \(partial\)"/);

// Tooltip text is escaped.
assert.doesNotMatch(issueCoveringPRBadge({ linked_prs: [{ number: 5, state: 'open', url: '"><script>' }] }), /"><script>/);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("covering-PR glyph check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
