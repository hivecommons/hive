package dashboard

import (
	"strings"
	"testing"
)

// TestStageTrackerNativeQueueRendering runs the forge-owned merge queue
// panel's pure renderers from the generated index.html in node
// (hivecommons/hive#18702): overview first, one status line per branch,
// caveats once, finished groups folded, calm age for a normal rotation delay,
// warnings only for real problems, and the honesty rules of
// src/docs/native-merge-queue.md.
func TestStageTrackerNativeQueueRendering(t *testing.T) {
	html := indexHTML(t)
	var js strings.Builder
	// Mirrors the page's esc(): a non-string renders as '' so an unwrapped
	// number in the markup fails these assertions instead of passing by luck.
	js.WriteString("const assert = require('assert'); function esc(s) { if (typeof s !== 'string') return ''; return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;'); }\n")
	for _, name := range []string{"NATIVE_QUEUE_SECOND_MS", "NATIVE_QUEUE_MINUTE_S", "NATIVE_QUEUE_SCOPES_PER_PASS", "NATIVE_QUEUE_OVERDUE_ROTATIONS", "NATIVE_QUEUE_FRESH_INTERVALS", "NATIVE_QUEUE_SHA_SHORT", "NATIVE_QUEUE_HELP"} {
		js.WriteString(jsConstLine(t, html, name) + "\n")
	}
	for _, name := range []string{"stageTrackerNativeAgeMs", "stageTrackerNativeAge", "stageTrackerNativeChecked", "stageTrackerNativeLink", "stageTrackerNativeIdentities", "stageTrackerNativeWhere", "stageTrackerNativePrLink", "stageTrackerNativeMember", "stageTrackerNativeEntryWord", "stageTrackerNativeCiWord", "stageTrackerNativeCandidateLive", "stageTrackerNativeScopeFacts", "stageTrackerNativeScopeLine", "stageTrackerNativeHelpMark", "stageTrackerNativeScopeName", "stageTrackerNativeBadges", "stageTrackerNativeScopeDetails", "stageTrackerNativeScopeRow", "stageTrackerNativeQueueHtml", "stageTrackerNativeQueueSync"} {
		js.WriteString(jsFunc(t, html, name) + "\n")
	}
	js.WriteString(`
const now = Date.parse('2026-11-17T13:00:00Z');
const FRESH = 360; // 2 x 3-minute interval, as the Go side sends it
const ago = m => new Date(now - m * 60000).toISOString();
const head801 = 'a'.repeat(40), head802 = 'b'.repeat(40), group = 'c'.repeat(40), gone = 'd'.repeat(40);
const page = [{kind:'pr', repo:'acme/chairlift', number:801, head:head801, base_ref:'main'}];
const all = page.concat([{kind:'pr', repo:'acme/chairlift', number:802, head:head802, base_ref:'main'}]);
const liveGroup = {sha:group, ref:'refs/heads/gh-readonly-queue/main/pr-801-' + group, membership:'proven', members:[{pr:801, head:head801}], ci:'pending', checks_known:true,
  checks:[{name:'Tests Passed', status:'in_progress', url:'https://github.com/acme/chairlift/actions/runs/1/job/2'}]};
const finished = sha => ({sha, ref:'refs/heads/gh-readonly-queue/main/pr-7-' + sha, membership:'unknown', members:[], ci:'unknown', checks:[], checks_known:false, expired:true, reason:'Synthetic ref unavailable or changed; lifecycle outcome unknown.'});
const fresh = {provider:'github-native', repo:'acme/chairlift', base:'main', state:'observed', complete:true, entries_complete:true, observed_at:ago(2), stale:false, fresh_for_s:FRESH,
  entries:[{id:'e1', position:1, pr:801, head:head801, state:'AWAITING_CHECKS', enqueued_at:ago(12)}, {id:'e2', position:2, pr:802, head:head802, state:'QUEUED', enqueued_at:ago(3)}],
  candidates:[liveGroup, finished(gone), finished('e'.repeat(40)), finished('f'.repeat(40))], outcomes:[{kind:'merged', pr:790, merge_sha:'9'.repeat(40), observed_at:ago(40), source:'signed-pull-request-merged-webhook'}]};

// A fresh queue with entries: the status line answers queued / front / state / wait.
let html = stageTrackerNativeQueueHtml([fresh], all, page, now, '');
assert.ok(html.includes('GitHub merge queues'), html);
assert.ok(html.includes('2 PRs queued on 1 branch') && html.includes('1 group testing') && html.includes('last checked 2m ago'), html);
assert.ok(html.includes('acme/chairlift → main'), html);
assert.ok(html.includes('2 PRs queued · front #801 awaiting checks, in queue 12m · testing: checks running'), html);
assert.ok(html.includes('checked 2m ago') && !html.includes('STALE'), html);
// Finished groups are counted as history, never drawn as live candidates.
assert.strictEqual((html.match(/Test group/g) || []).length, 1, html);
assert.ok(html.includes('3 finished groups (outcome not observed)'), html);
assert.ok(!html.includes(gone.slice(0, 12)) && !html.includes('Validation candidate'), html);
assert.ok(!/merged|failed/.test(html.split('finished groups')[1].split('</div>')[0]), 'finished groups claim no outcome');
// Caveats appear once, in the single help mark (its text is in the aria-label
// and the tooltip span, so the sentence occurs twice), not per queue or card.
assert.strictEqual((html.match(/ham-native-help"/g) || []).length, 1, html);
assert.strictEqual((html.match(/never from a PR number/g) || []).length, 2, html);
assert.ok(!html.includes('never inferred from FIFO') && !html.includes('Queue references remain visible across row filters'), html);
assert.ok(!html.includes('policy unavailable'), html);
// Details on demand: entries, the live group with its own SHA's checks, receipts.
assert.ok(html.includes('<details class="ham-native-details"><summary>2 entries, 1 test group, 3 finished, 1 receipt</summary>'), html);
assert.ok(html.includes('on this page') && html.includes('on another page'), html);
assert.ok(html.includes('href="https://github.com/acme/chairlift/pull/801"') && html.includes('href="https://github.com/acme/chairlift/actions/runs/1/job/2"'), html);
assert.ok(html.includes('Test group <code title="' + group + '">' + group.slice(0, 12) + '</code> · checks running'), html);
assert.ok(html.includes('merged') && html.includes('signed-pull-request-merged-webhook'), html);
// Built from the dashboard's own parts. A fresh, problem-free queue carries
// no badge at all; the badge cases are asserted below.
for (const cls of ['ham-train-head', 'ham-train-chip', 'ham-merged-list', 'ham-merged-meta', 'config-info']) assert.ok(html.includes(cls), cls);
assert.ok(!html.includes('ham-badge'), 'fresh queue with no problem shows no badge');
assert.ok(!html.includes('nq-'), 'one-off nq-* look is gone');

// Past the stale window with a normal rotation delay: calm age, no alarm, no empty claim.
const rotation = {...fresh, observed_at:ago(9), stale:true, entries:[], candidates:[], outcomes:[]};
html = stageTrackerNativeQueueHtml([rotation, {...rotation, repo:'acme/dakota', observed_at:ago(13)}, {...rotation, repo:'acme/finpilot', observed_at:ago(5)}], [], [], now, '');
assert.ok(html.includes('nothing queued on 3 branches') && html.includes('last checked 5m ago'), html);
assert.ok(html.includes('3 branches with nothing queued'), html);
assert.ok(html.includes('nothing queued at last check') && html.includes('checked 9m ago') && html.includes('checked 13m ago'), html);
assert.ok(!html.includes('STALE') && !html.includes('ham-native-badge-problem') && !html.includes('needs attention'), html);
// The empty-but-calm panel is a head line plus one folded line.
assert.ok(html.includes('<details class="ham-native-details ham-native-folded">'), html);
assert.strictEqual((html.match(/<ol /g) || []).length, 0, 'no active rows when every branch is calm');
// Stale with an entry: the row says when it was checked and marks the re-check quietly.
html = stageTrackerNativeQueueHtml([{...fresh, observed_at:ago(9), stale:true}], all, page, now, '');
assert.ok(html.includes('awaiting re-check') && html.includes('ham-native-badge-notice') && !html.includes('ham-native-badge-problem'), html);

// A failed refresh still stands out.
const failed = {...rotation, state:'unavailable', reason:'Refresh unavailable; retained observations are not current queue evidence.', observed_at:ago(20)};
html = stageTrackerNativeQueueHtml([failed], [], [], now, '');
assert.ok(html.includes('ham-badge ham-native-badge-problem">refresh failed') && html.includes('ham-native-row-problem'), html);
assert.ok(html.includes('1 branch needs attention') && html.includes('queue unknown; last refresh failed'), html);
const auth = {...failed, reason:'GitHub App installation authentication is unavailable; queue evidence cannot be refreshed.'};
assert.ok(stageTrackerNativeQueueHtml([auth], [], [], now, '').includes('GitHub App auth unavailable'));
// Evidence far older than the rotation explains is a problem, not a delay.
const overdue = {...rotation, observed_at:ago(60)};
html = stageTrackerNativeQueueHtml([overdue, rotation], [], [], now, '');
assert.ok(html.includes('not checked for 60m') && html.includes('ham-native-badge-problem'), html);
// Ten branches rotate slower: 13m is a delay there, not a problem.
const ten = Array.from({length:10}, (_, i) => ({...rotation, repo:'acme/r' + i, observed_at:ago(13)}));
assert.ok(!stageTrackerNativeQueueHtml(ten, [], [], now, '').includes('ham-native-badge-problem'));
// Failed candidate checks stand out; unknown and unreadable never read as passing.
html = stageTrackerNativeQueueHtml([{...fresh, candidates:[{...liveGroup, ci:'failed'}]}], all, page, now, '');
assert.ok(html.includes('checks failed') && html.includes('ham-native-badge-problem'), html);
assert.strictEqual(stageTrackerNativeCiWord({ci:'unknown', checks_known:false}), 'required checks unknown (policy unreadable)');
assert.strictEqual(stageTrackerNativeCiWord({ci:'unavailable', checks_known:false}), 'checks unreadable');
assert.strictEqual(stageTrackerNativeCiWord({ci:'pending', checks_known:true}), 'checks running');
assert.ok(!stageTrackerNativeCiWord({ci:'unknown', checks_known:true}).includes('passed'));
// Membership is never inferred: an unproven live group says so.
html = stageTrackerNativeQueueHtml([{...fresh, candidates:[{...liveGroup, membership:'unknown', members:[]}]}], all, page, now, '');
assert.ok(html.includes('members not proven'), html);
// Older servers send no expired flag: the reason sentence still folds the group.
assert.strictEqual(stageTrackerNativeCandidateLive({...finished(gone), expired:undefined}), false);
assert.strictEqual(stageTrackerNativeCandidateLive(liveGroup), true);

// Scopes not yet visited are "not checked yet", never "nothing queued".
const unknown = {provider:'github-native', repo:'acme/new', base:'main', state:'unknown', stale:true, reason:'No authoritative snapshot for this scope yet; bounded background reconciliation rotates across scopes.'};
html = stageTrackerNativeQueueHtml([rotation, unknown], [], [], now, '');
assert.ok(html.includes('nothing queued on 1 of 2 branches'), html);
assert.ok(html.includes('1 branch with nothing queued, 1 branch not checked yet'), html);
assert.ok(html.includes('waiting for its first check') && html.includes('not checked yet'), html);
assert.ok(!html.includes('ham-native-badge-problem'), html);
// A branch without a queue is calm too.
assert.ok(stageTrackerNativeQueueHtml([{...rotation, state:'not-enabled'}], [], [], now, '').includes('no merge queue on this branch'));
// An incomplete entry list says so instead of implying a full order.
html = stageTrackerNativeQueueHtml([{...fresh, complete:false, entries_complete:false, reason:'Queue exceeds bounded page; order and membership coverage are partial.'}], all, page, now, '');
assert.ok(html.includes('(order partial)') && html.includes('order and membership coverage are partial'), html);

// GitHub's own entry states in plain words; unknown slugs are not invented.
assert.strictEqual(stageTrackerNativeEntryWord('AWAITING_CHECKS'), 'awaiting checks');
assert.strictEqual(stageTrackerNativeEntryWord('MERGEABLE'), 'ready to merge');
assert.strictEqual(stageTrackerNativeEntryWord('UNMERGEABLE'), 'cannot merge');
assert.strictEqual(stageTrackerNativeEntryWord(''), 'state unknown');
assert.strictEqual(stageTrackerNativeEntryWord('SOME_NEW_STATE'), 'some new state');

// Repo filter applies; unscoped placeholders survive it; no scopes renders nothing.
assert.strictEqual(stageTrackerNativeQueueHtml([fresh], all, page, now, 'other/repo'), '');
assert.ok(stageTrackerNativeQueueHtml([fresh, {...rotation, repo:'other/repo'}], all, page, now, 'ACME/chairlift').includes('acme/chairlift'));
assert.ok(!stageTrackerNativeQueueHtml([fresh, {...rotation, repo:'other/repo'}], all, page, now, 'acme/chairlift').includes('other/repo'));
assert.ok(stageTrackerNativeQueueHtml([{provider:'github-native', state:'unknown', stale:true, reason:'No authoritative queue snapshot yet.'}], [], [], now, 'x/y').includes('scope not configured'));
assert.strictEqual(stageTrackerNativeQueueHtml([], [], [], now, ''), '');

// Escaping: every value passes through esc(); only https links become anchors.
const hostile = {...fresh, repo:'<script>alert(1)</script>', base:'"><img src=x>', entries:[{id:'e', position:1, pr:1, head:'<b>', state:'<i>', enqueued_at:ago(1)}],
  candidates:[{...liveGroup, sha:'<sha>', membership:'unknown', members:[], checks:[{name:'<check>', status:'<s>', url:'javascript:alert(1)'}]}],
  outcomes:[{kind:'<k>', candidate_sha:'<c>', observed_at:ago(1), source:'<src>'}], reason:'<r>', complete:false};
html = stageTrackerNativeQueueHtml([hostile], [], [], now, '');
assert.ok(!html.includes('<script>') && !html.includes('<img') && !html.includes('<b>') && !html.includes('<i>') && !html.includes('<sha>') && !html.includes('<check>') && !html.includes('<k>') && !html.includes('<src>') && !html.includes('<r>'), html);
assert.ok(!html.includes('javascript:'), html);
assert.strictEqual(stageTrackerNativeLink('http://example.com/x', 'x'), 'x');

// Sync: the mount hides when there is nothing to show and fills otherwise.
let root = {hidden:true, innerHTML:''};
const document = {getElementById:() => root};
function setIfChanged(el, value) { el.innerHTML = value; }
stageTrackerNativeQueueSync({integration_queues:[fresh], items:all}, page.map(item => ({item})), now, '');
assert.strictEqual(root.hidden, false);
assert.ok(root.innerHTML.includes('front #801'));
stageTrackerNativeQueueSync(null, [], now, '');
assert.strictEqual(root.hidden, true);
`)
	runStageTrackerRowActionsScript(t, js.String())
}

// TestStageTrackerNativeQueueWiring: the mount point popout.css styles for
// display and TV modes is still there, now dressed in the tracker's own
// classes, and the panel CSS is tokens only.
func TestStageTrackerNativeQueueWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div id="activity-monitor-native-queue" class="ham-native-rail" role="region" aria-label="GitHub merge queues" hidden></div>`,
		`body.display-density-tv #activity-monitor-native-queue`,
		`.ham-native-rail {`,
		`.ham-badge.ham-native-badge-problem {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "nq-rail") || strings.Contains(html, ".nq-") {
		t.Error("one-off nq-* classes remain in the page")
	}
	start := strings.Index(html, "/* ---- Forge-owned merge queues rail")
	end := strings.Index(html[start:], ".ham-native-details > summary:hover")
	if start < 0 || end < 0 {
		t.Fatal("native queue CSS block not found")
	}
	css := html[start : start+end]
	for _, literal := range []string{"#", "rgb(", "rgba(", "hsl("} {
		if strings.Contains(strings.ReplaceAll(css, "hivecommons/hive#18702", ""), literal) {
			t.Errorf("native queue CSS must use tokens only, found %q in:\n%s", literal, css)
		}
	}
}
