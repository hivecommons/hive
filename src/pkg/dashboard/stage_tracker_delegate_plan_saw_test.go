package dashboard

import (
	"strings"
	"testing"
)

// The "Let hive decide" preview keeps its visible budget (one sentence, one
// why, two "then" items) and puts what the rules read under Details: "What
// hive saw" with each fact marked by its effect, the rule that decided and
// its whole reason, and each ruled-out option with the fact that ruled it
// out (owner: "more information in that pop up dialog, specifically why
// hive would do what it wants to do").
func TestStageTrackerDelegatePlanDetailsShowWhatHiveSaw(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{`.ham-plan-saw {`, `.ham-plan-fact[data-state="blocks"] .ham-plan-fact-mark {`, `.ham-plan-sr {`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const visibleOf = html => stageTrackerDelegatePlanVisibleText(html);
const detailsOf = html => (html.match(/<details[\s\S]*?<\/details>/) || [''])[0];
const base = { ref: 'o/r#15219', number: 15219, type: 'pr', title: 'fix the thing', would: 'adopt-supersede', rule: 'contributor-offline',
  summary: 'Re-open #15219 as hive\'s own PR with the same commits and merge that; close #15219 as superseded.', why: 'Their clanker is offline.',
  consequences: ['Hive opens its copy of #15219, lets it soak, and merges it.', '#15219 is closed as superseded; the author keeps the credit.'],
  ladder: { decision: 'contributor-offline', reason: 'offline', ruled_out: ['already-fixed: no merged PR has closed this PR\'s issue', 'mergeable-as-is: conflicting, CI not green'] },
  mergeable_as_is: false, alternatives: [] };
const withSaw = Object.assign({}, base, {
  saw: [
    { label: 'Author', value: '@Karibhou, not trusted', state: 'info' },
    { label: 'CI', value: 'not green', state: 'blocks' },
    { label: 'Sign-off', value: 'signed off', state: 'ok' },
    { label: 'Question for a person', value: '<img src=x onerror=alert(1)>', state: 'weird' },
  ],
  rule_plain: 'the contributor is offline; adopted in supersede mode',
  rationale: 'the author\'s clanker hasn\'t been connected since the nudge; adoption in supersede mode keeps the work moving',
  why_not: [{ option: 'Close as already fixed', because: 'no merged PR has closed this PR\'s issue' }, { option: 'Approve as it is', because: 'conflicting, CI not green' }],
});

// The visible part is exactly what it was without the new fields.
const before = stageTrackerDelegatePlanHtml(stageTrackerDelegatePlanView([base], ''));
const after = stageTrackerDelegatePlanHtml(stageTrackerDelegatePlanView([withSaw], ''));
assert.strictEqual(visibleOf(after), visibleOf(before), 'the visible budget is unchanged');
assert.ok(!visibleOf(after).includes('What hive saw'), 'What hive saw is under Details');

const details = detailsOf(after);
assert.ok(details.includes('<div class="ham-plan-label">What hive saw</div>'), details);
assert.ok(details.includes('<li class="ham-plan-fact" data-state="blocks"><span class="ham-plan-fact-mark" aria-hidden="true">✗</span> <span class="ham-plan-fact-label">CI</span> not green<span class="ham-plan-sr"> (holds it back)</span></li>'), details);
assert.ok(details.includes('data-state="ok"><span class="ham-plan-fact-mark" aria-hidden="true">✓</span> <span class="ham-plan-fact-label">Sign-off</span> signed off<span class="ham-plan-sr"> (fine)</span>'), details);
assert.ok(details.includes('data-state="info"><span class="ham-plan-fact-mark" aria-hidden="true">•</span> <span class="ham-plan-fact-label">Author</span> @Karibhou, not trusted</li>'), details);
// An unknown state renders as info, and every value is escaped.
assert.ok(details.includes('data-state="info"><span class="ham-plan-fact-mark" aria-hidden="true">•</span> <span class="ham-plan-fact-label">Question for a person</span> &lt;img src=x onerror=alert(1)&gt;</li>'), details);
assert.ok(!after.includes('<img'), 'a fact value is never markup');
assert.ok(details.includes('<p class="ham-plan-rule"><span class="ham-plan-label">Rule</span> the contributor is offline; adopted in supersede mode</p>'), details);
assert.ok(details.includes('<p class="ham-plan-rationale">the author&#39;s clanker') || details.includes('<p class="ham-plan-rationale">the author&#x27;s clanker') || details.includes("<p class=\"ham-plan-rationale\">the author's clanker"), details);
// Ruled-out options read as choices with their reason, not rung slugs.
assert.ok(details.includes('<li>Approve as it is — conflicting, CI not green</li>') && !details.includes('mergeable-as-is:'), details);
assert.ok(details.indexOf('What hive saw') < details.indexOf('Why not the others') && details.indexOf('Why not the others') < details.indexOf('Rails:'), 'order: what hive saw, why not the others, rails');

// A plan from an older server (no why_not) keeps the raw ruled-out lines.
assert.ok(detailsOf(before).includes('mergeable-as-is: conflicting, CI not green') && !detailsOf(before).includes('What hive saw'), detailsOf(before));

// A batch labels each item's facts with its reference.
const batch = stageTrackerDelegatePlanHtml(stageTrackerDelegatePlanView([withSaw, Object.assign({}, withSaw, { ref: 'o/r#9', number: 9 })], ''));
assert.ok(detailsOf(batch).includes('What hive saw · o/r#15219') && detailsOf(batch).includes('What hive saw · o/r#9'), detailsOf(batch));
`)
}
