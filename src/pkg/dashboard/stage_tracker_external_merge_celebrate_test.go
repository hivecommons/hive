package dashboard

import (
	"strings"
	"testing"
)

// TestStageTrackerExternalMergeCelebrateNode (hivecommons/hive#16658): a
// row that leaves the payload is celebrated on any merged terminal fact,
// whoever merged it — the hive's own sweep (bee + "merged by 🐝 hive") and
// a maintainer's `gh pr merge --admin` (bee + "merged by @clubanderson")
// alike; a closed-not-merged exit draws only the quiet info toast; and a
// close the server later corrects to a merge still celebrates once.
func TestStageTrackerExternalMergeCelebrateNode(t *testing.T) {
	html := indexHTML(t)
	js := strings.Replace(stageTrackerCelebrateJS(t, html), stageTrackerMergingHoldOffJS, stageTrackerMergingHoldJS(t, html), 1)
	runStageTrackerRowActionsScript(t, js+stageTrackerCelebrateFixture+`
const iso = ms => new Date(ms).toISOString();
const srv = (all, today) => ({ all_time: all, today, total_7d: today, total_30d: today, recent: [], tz: 'UTC', day_start: iso(stageTrackerLocalMidnight(clock)) });
const pr = (num, extra) => Object.assign({ repo: 'o/r', kind: 'pr', number: num, stage: 50, title: 'fix ' + num }, extra || {});
const merged = (num, by) => ({ repo: 'o/r', number: num, merged_at: iso(clock), merged_by: by });
const poll = d => { _stageTracker = d; stageTrackerNoteMerges(d); };
const lastBanner = () => banners.children[banners.children.length - 1].innerHTML;
// Banners time out across the ticks below, so count the ones made, not the ones still up.
let made = 0; const _banner = stageTrackerMergeBanner; stageTrackerMergeBanner = (...a) => { made++; return _banner(...a); };
panel.appendChild(row('o/r/pr#16644')); panel.appendChild(row('o/r/pr#16645')); panel.appendChild(row('o/r/pr#16646')); panel.appendChild(row('o/r/pr#16647'));
poll({ items: [pr(16644), pr(16645), pr(16646), pr(16647)], merged: srv(10, 1) });
// #16645: the hive's own merge → bee + banner naming the hive.
tick(10000);
poll({ items: [pr(16644), pr(16646), pr(16647)], recently_merged: [merged(16645, 'hive')], merged: srv(11, 2) });
tick(0);
assert.strictEqual(made, 1, 'hive merge: one banner');
assert.ok(lastBanner().includes('#16645</span> merged by <span class="merge-banner-by">🐝 hive</span>'), lastBanner());
assert.strictEqual(flights().length, 1, 'hive merge: one bee');
flights()[0]._ev.animationend();
// #16644: merged by hand (gh pr merge --admin --squash), no audit line
// yet — recently_merged carries it from the ledger with the merger → the
// same bee + banner, naming @clubanderson.
tick(10000);
poll({ items: [pr(16646), pr(16647)], recently_merged: [merged(16644, 'clubanderson'), merged(16645, 'hive')], merged: srv(12, 3) });
tick(0);
assert.strictEqual(made, 2, 'external merge: one banner');
assert.ok(lastBanner().includes('#16644</span> merged by <span class="merge-banner-by">@clubanderson</span>'), lastBanner());
assert.strictEqual(flights().length, 1, 'external merge: one bee');
assert.deepStrictEqual(toasts, [], 'a merge is never a closed toast');
flights()[0]._ev.animationend();
// #16646: closed, not merged → one quiet info toast, no bee, no banner.
tick(10000);
poll({ items: [pr(16647)], recently_closed: [{ repo: 'o/r', number: 16646, closed_at: iso(clock) }], recently_merged: [merged(16644, 'clubanderson'), merged(16645, 'hive')], merged: srv(12, 3) });
tick(0);
assert.deepStrictEqual(toasts, [['info', '#16646 closed · closed without merging']]);
assert.strictEqual(made, 2, 'no banner for a close'); assert.strictEqual(flights().length, 0, 'no bee for a close');
// #16647: a bot's comment on the just-merged PR read as "closed" first;
// the next poll carries the merge → the celebration still fires, once.
tick(10000);
poll({ items: [], recently_closed: [{ repo: 'o/r', number: 16647, closed_at: iso(clock) }], recently_merged: [merged(16644, 'clubanderson'), merged(16645, 'hive')], merged: srv(12, 3) });
tick(0);
assert.strictEqual(toasts.length, 2); assert.strictEqual(toasts[1][1], '#16647 closed · closed without merging');
assert.strictEqual(made, 2); assert.strictEqual(flights().length, 0);
tick(10000);
poll({ items: [], recently_merged: [merged(16647, 'clubanderson'), merged(16644, 'clubanderson'), merged(16645, 'hive')], merged: srv(13, 4) });
tick(0);
assert.strictEqual(made, 3, 'a close corrected to a merge celebrates');
assert.ok(lastBanner().includes('#16647</span> merged by <span class="merge-banner-by">@clubanderson</span>'), lastBanner());
assert.strictEqual(flights().length, 1);
flights()[0]._ev.animationend();
tick(10000);
poll({ items: [], recently_merged: [merged(16647, 'clubanderson'), merged(16644, 'clubanderson'), merged(16645, 'hive')], merged: srv(13, 4) });
tick(0);
assert.strictEqual(made, 3, 'never twice'); assert.strictEqual(flights().length, 0);
assert.strictEqual(toasts.length, 2);
console.log('ok');
`)
}
