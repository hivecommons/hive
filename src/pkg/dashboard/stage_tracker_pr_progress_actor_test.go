package dashboard

import "testing"

// A lit "PR in progress" chevron names who works the PR with their kind
// icon, the way "Waiting for @x" names its person: "PR in progress ·
// @thatguywob" + the robot for a clanker, the honeycomb for a hive agent,
// the person glyph for a human. The name rides inside the clamped copy
// (truncates like every holder) with the full text in the tooltip; the
// icon sits outside the copy. Nobody known keeps the bare word; an
// inactive column stays icon-only.
func TestStageTrackerPRProgressActorChevronJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+"\n"+jsFunc(t, html, "stageTrackerReturnedAuthor")+stageTrackerRowsFixtureJS+`
const lit = (html) => (/<li class="ham-seg active[^"]*"[^>]*>([\s\S]*?)<\/li>/.exec(strip(html)) || ['', ''])[1];
const litWord = (html) => text(lit(html));
const copyText = (html) => text((/<span class="ham-chevron-copy">([\s\S]*?)<\/span>(?:<svg|<span id=)/.exec(lit(html)) || ['', ''])[1]);
const actorIco = (html) => (/<svg class="ham-ico ham-actor-ico"[^>]*aria-label="([^"]*)"[^>]*>([\s\S]*?)<\/svg>/.exec(lit(html)) || []);
const chevronTip = (html) => text((/<span id="ham-chevron-tip-[^"]*" class="config-tooltip ham-chevron-tip" role="tooltip">([\s\S]*?)<\/span>/.exec(lit(html)) || ['', ''])[1]);
const since = new Date(now - 3 * 3600000).toISOString();
// An issue drawn through its one unclassified covering PR: the PR row
// lights "PR in progress" under the issue (hivecommons/hive#15541); the
// payload carries the PR's author but no holder and no next mover.
const subRow = (prExtra) => stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 10, kind: 'issue', title: 'fix it', url: 'https://x/10', stage: 40, stage_since: since, caption: 'Covered by PR', author: 'reporter', author_kind: 'human', stage_actor: { kind: 'none', why: 'its PR' }, linked_prs: [
  Object.assign({ number: 101, url: 'https://x/101', title: 'fix', stage: 0, stage_since: since, caption: 'PR in progress', stage_actor: { kind: 'none', why: '' } }, prExtra),
] }, stages, ctx);

// Hive-agent author → @login + honeycomb.
let row = subRow({ author: 'thatguywob', author_kind: 'hive' });
assert.strictEqual(litWord(row), 'PR in progress · @thatguywob', litWord(row));
assert.ok(actorIco(row)[2] && actorIco(row)[2].includes(STAGE_TRACKER_ICONS.hive), 'hive author draws the honeycomb: ' + lit(row));
assert.strictEqual(actorIco(row)[1], 'Working the PR: hive @thatguywob');
assert.ok(/<span class="ham-seg-t ham-has-holder">PR in progress<span class="ham-holder"> · @thatguywob<\/span><\/span>/.test(lit(row)), 'the name rides the holder slot inside the copy: ' + lit(row));
assert.strictEqual(copyText(row), 'PR in progress · @thatguywob', 'the icon is outside the clamped copy: ' + lit(row));
assert.ok(chevronTip(row).includes('PR in progress · @thatguywob'), 'the tooltip holds the full text: ' + chevronTip(row));

// Clanker author → robot; human author → the person glyph.
row = subRow({ author: 'thatguywob', author_kind: 'clanker' });
assert.strictEqual(litWord(row), 'PR in progress · @thatguywob', litWord(row));
assert.ok(actorIco(row)[2].includes(STAGE_TRACKER_ICONS.clanker), 'clanker author draws the robot: ' + lit(row));
row = subRow({ author: 'alice', author_kind: 'human' });
assert.strictEqual(litWord(row), 'PR in progress · @alice', litWord(row));
assert.ok(actorIco(row)[2].includes(STAGE_TRACKER_ICONS.human), 'human author draws the person glyph: ' + lit(row));

// A recorded holder still leads and is named once.
row = subRow({ author: 'bob', author_kind: 'clanker', claimant: { kind: 'clanker', login: 'bob', since, source: 'author' } });
assert.strictEqual(litWord(row), "PR in progress · @bob's clanker", litWord(row));
assert.strictEqual((lit(row).match(/ham-actor-ico/g) || []).length, 1, 'one actor icon: ' + lit(row));

// Nobody known (bot author, unlabelled author): the bare word, no icon.
for (const extra of [{ author: 'dependabot[bot]', author_kind: 'bot' }, { author: 'someone', author_kind: '' }, { author: '', author_kind: '' }]) {
  row = subRow(extra);
  assert.strictEqual(litWord(row), 'PR in progress', JSON.stringify(extra) + ': ' + litWord(row));
  assert.ok(!lit(row).includes('ham-actor-ico') && !lit(row).includes('ham-has-holder'), JSON.stringify(extra) + ': ' + lit(row));
}

// The server's stage_actor login names the actor when no holder is recorded.
row = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 11, kind: 'issue', title: 'fix it', url: 'https://x/11', stage: 30, stage_since: since, caption: 'PR in progress', author: 'reporter', author_kind: 'human', stage_actor: { kind: 'hive', login: 'fixer', why: 'fixer is on it' } }, stages, ctx);
assert.strictEqual(litWord(row), 'PR in progress · @fixer', litWord(row));
assert.strictEqual((lit(row).match(/ham-actor-ico/g) || []).length, 1, 'the next-move icon stands, not a second one: ' + lit(row));
// An issue's own author filed the issue, never works a PR: no name without a PR.
row = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 12, kind: 'issue', title: 'fix it', url: 'https://x/12', stage: 30, stage_since: since, caption: 'PR in progress', author: 'reporter', author_kind: 'human', stage_actor: { kind: 'hive', why: 'an agent has a PR under way' } }, stages, ctx);
assert.strictEqual(litWord(row), 'PR in progress', litWord(row));

// Inactive (the PR moved on to CI / Review): the column is its icon alone.
row = subRow({ author: 'thatguywob', author_kind: 'clanker', stage: 50, head: 'h1' });
assert.ok(!text(strip(row)).includes('@thatguywob'), 'a passed PR in progress column carries no name: ' + strip(row));
assert.ok(!/<li class="ham-seg done[^"]*"[^>]*>(?:(?!<\/li>)[\s\S])*ham-holder/.test(strip(row)), 'no holder text on a done column: ' + strip(row));

// Pure helper: the word gate and the fallback order.
assert.strictEqual(stageTrackerPRProgressActor({ kind: 'pr', author: 'alice', author_kind: 'human' }, 'Fixing'), null, 'only the bare word names an actor');
assert.deepStrictEqual(stageTrackerPRProgressActor({ kind: 'pr', author: 'alice', author_kind: 'human' }, 'PR in progress'), { who: '@alice', kind: 'human' });
assert.deepStrictEqual(stageTrackerPRProgressActor({ kind: 'pr', author: 'alice', author_kind: 'human', stage_actor: { kind: 'clanker', login: 'bob' } }, 'PR in progress'), { who: '@bob', kind: 'clanker' }, 'stage_actor login outranks the author');
assert.deepStrictEqual(stageTrackerPRProgressActor({ kind: 'pr', author: 'alice', author_kind: 'human', claimant: { kind: 'hive-agent', agent: 'quality' } }, 'PR in progress'), { who: 'quality', kind: 'hive' }, 'the recorded holder outranks both');
console.log('pr progress actor chevron ok');
`)
}
