package dashboard

import (
	"strings"
	"testing"
)

func TestStageTrackerExternalMergeUsesVerifiedTerminalModels(t *testing.T) {
	html := indexHTML(t)
	for _, wire := range []string{
		"stageTrackerTerminalObserve(d, fresh, nowMs)",
		"stageTrackerTerminalView(held, nowMs)",
		"stageTrackerTerminalEvents(nowMs)",
		"nowMs, it => stageTrackerRowHtml(it, stages, ctx)",
	} {
		if !strings.Contains(html, wire) {
			t.Fatalf("missing production terminal-model seam: %s", wire)
		}
	}
	functions := []string{
		"stageTrackerMergeState", "stageTrackerCarryOpen", "stageTrackerNewlyMerged",
		"stageTrackerPRHosts", "stageTrackerRowPRKeys", "stageTrackerCelebrateRowKey",
		"stageTrackerTerminalObserve", "stageTrackerTerminalItem", "stageTrackerTerminalView",
		"stageTrackerTerminalPinnedItem", "stageTrackerTerminalEvents", "stageTrackerPinnedRows",
		"stageTrackerMergedPRKnown", "stageTrackerItemsForKeys", "stageTrackerItemByKey",
		"stageTrackerPRParked", "stageTrackerRowActions", "stageTrackerActionRequest", "repoParts",
	}
	var lifted []string
	for _, name := range functions {
		lifted = append(lifted, jsFunc(t, html, name))
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+stageTrackerRowsFixtureJS+
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_PIN_MS")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_DECISION_STAGES")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_ALL_MIN")+"\n"+strings.Join(lifted, "\n")+`
const document = {getElementById:()=>null};
var _stageTrackerCelebrating = {}, _stageTrackerTerminalRows = {}, _stageTrackerTerminalFacts = {};
var _stageTrackerMergeState = {}, _stageTrackerPRHost = {}, _stageTracker = null;
let releases = [], actionNow = now;
Date.now = () => actionNow;
function stageTrackerScheduleRelease(delay){ releases.push(delay); }
const advance = (data, at=now) => {
  const next = stageTrackerCarryOpen(_stageTrackerMergeState, stageTrackerMergeState(data, _stageTrackerMergeState));
  const fresh = stageTrackerNewlyMerged(_stageTrackerMergeState, next);
  _stageTrackerMergeState = next;
  Object.assign(_stageTrackerPRHost, stageTrackerPRHosts(data));
  stageTrackerTerminalObserve(data, fresh, at);
  stageTrackerTerminalEvents(at);
  return {fresh, view:stageTrackerTerminalView(data, at)};
};
const reset = () => { _stageTrackerCelebrating={}; _stageTrackerTerminalRows={}; _stageTrackerTerminalFacts={};
  _stageTrackerMergeState={}; _stageTrackerPRHost={}; _stageTrackerEvents={}; };
const repo = 'hivecommons/hive', number = 16008, key=repo+'/pr#'+number;
const pr = {repo, number, kind:'pr', stage:30, title:'Externally merged change',
  url:'https://github.com/'+repo+'/pull/'+number, manual_merge:true, human_merge_reason:'stale manual decision',
  head:'ed329668506eec69238b0badde9540f83598e3ff', author:'clubanderson', author_kind:'human'};
const record = {repo, number, merged_by:'clubanderson', merged_at:'2026-10-31T11:11:04Z'};
const merged = {items:[], merged:{recent:[record]}};
advance({items:[pr]});
let result = advance(merged);
assert.deepStrictEqual(result.fresh,[key], 'no earlier queue observation is required');
assert.strictEqual(pr.stage,30, 'source models are immutable');
let terminal = stageTrackerTerminalPinnedItem(key,now);
assert.strictEqual(terminal.stage,110);
assert.strictEqual(terminal.merge_gate,null);
assert.strictEqual(terminal.merge_train,null);
assert.strictEqual(terminal.preparation,null);
assert.strictEqual(terminal.manual_merge,false);
assert.strictEqual(terminal.human_merge_reason,'');
assert.strictEqual(stageTrackerPRParked(terminal),false);
assert.strictEqual(terminal.merged_by,'clubanderson');
assert.strictEqual(terminal.stage_actor.kind,'none');
const render = it=>stageTrackerRowHtml(it,stages,{...ctx,events:_stageTrackerEvents});
const pinned = stageTrackerPinnedRows({querySelector:()=>null},[],now,render);
assert.strictEqual(pinned.length,1, 'last known model renders even after the old DOM node vanished');
assert.ok(pinned[0].html.includes('active cap merged'), pinned[0].html);
assert.ok(text(pinned[0].html).includes('Merged PR #16008 by @clubanderson'), text(pinned[0].html));
assert.ok(!text(pinned[0].html).includes('by hive'));
_stageTracker={items:[pr]};
assert.strictEqual(stageTrackerItemsForKeys([key])[0].stage,110,'selection resolves the terminal view, not stale raw decisions');
assert.deepStrictEqual(stageTrackerRowActions(pr,{}).map(a=>a.id),['open','copy'],'terminal menus keep navigation only');
for (const action of ['hold','retry','needs-human','decide:approve','decide:merge-now','decide:delegate']) {
  assert.strictEqual(stageTrackerActionRequest(pr,action),null,'captured stale action cannot write after canonical merge: '+action);
}
const until = _stageTrackerCelebrating[key].until, eventAt = _stageTrackerEvents[key].at;
result = advance({items:[pr]},now+100);
assert.deepStrictEqual(result.fresh,[]);
assert.strictEqual(result.view.items[0].stage,110,'stale open refresh cannot regress a verified merged fact');
assert.strictEqual(_stageTrackerCelebrating[key].until,until,'no duplicate pin extension');
assert.strictEqual(_stageTrackerEvents[key].at,eventAt,'no duplicate event clock');
assert.strictEqual(stageTrackerTerminalView({items:[pr]},until+1).items.length,0,'expired stale open rows do not resurrect');
advance({items:[]},until+STAGE_TRACKER_CELEBRATE_PIN_MS+1);
assert.strictEqual(Object.keys(_stageTrackerTerminalRows).length,0,'dropped row models are bounded');
assert.strictEqual(Object.keys(_stageTrackerTerminalFacts).length,0,'actor metadata is bounded with row models');
reset(); advance({items:[{...pr,nested_under:500}]});
advance(merged);
assert.strictEqual(stageTrackerTerminalPinnedItem(key,now).stage,110,'a root PR whose issue is absent uses its actual retained row, not an unavailable host');

reset();
const host={repo:'hivecommons/console',kind:'issue',number:500,stage:40,title:'Open related issue',
  linked_prs:[{...pr, repo:undefined,url:'https://github.com/'+repo+'/pull/'+number,drawn_under:500}]};
const other={...pr,repo:'another/hive'};
advance({items:[host,other]});
assert.strictEqual(_stageTrackerMergeState[key],false,'a linked-only canonical identity is observed');
assert.strictEqual(stageTrackerRowPRKeys(host),key);
result=advance({items:[{...host,linked_prs:[]},other],merged:{recent:[record]}});
const issue=result.view.items.find(it=>it.number===500), lp=issue.linked_prs[0];
assert.strictEqual(issue.stage,40,'a related PR merge does not claim that its issue closed');
assert.strictEqual(lp.stage,110);
assert.strictEqual(stageTrackerLinkedPROpen(lp,issue),false,'terminal visibility never restores open-action semantics');
assert.strictEqual(stageTrackerLinkedSupersededGone(lp,issue),false,'render-only terminal exception');
assert.strictEqual(result.view.items.find(it=>it.repo==='another/hive').stage,30,'same number in another repo stays open');
const inline=render(issue);
assert.ok(inline.includes('active cap merged'),inline);
assert.ok(text(inline).includes('by @clubanderson'),text(inline));
assert.strictEqual(stageTrackerPRHosts({items:[other,host]})[key],'hivecommons/console/issue#500');
const sibling={...pr,number:16099,stage:50,url:'https://github.com/'+repo+'/pull/16099'};
const multi={...host,linked_prs:[host.linked_prs[0],sibling]};
reset(); advance({items:[multi]});
result=advance({items:[{...multi,linked_prs:[sibling]}],merged:{recent:[record]}});
const nested=result.view.items[0];
assert.strictEqual(nested.linked_prs.length,2,'retain the merged child without duplicating siblings');
assert.strictEqual(nested.linked_prs.find(p=>p.number===16099).stage,50);
const nestedHtml=render(nested);
assert.ok(nestedHtml.includes('data-ham-sub-key="'+key+'"'),nestedHtml);
assert.ok(nestedHtml.includes('active cap merged'),nestedHtml);
assert.ok(text(nestedHtml).includes('by @clubanderson'),text(nestedHtml));
_stageTracker={items:[multi]};
assert.strictEqual(stageTrackerItemsForKeys([key])[0].state,'closed','linked selection resolves the same canonical terminal fact');
assert.strictEqual(stageTrackerActionRequest(multi,'decide:delegate-all'),null,'stale parent bulk decisions omit merged children');
assert.strictEqual(stageTrackerActionRequest(multi,'hold').body.type,'issue','a child merge does not disable legitimate issue actions');
const siblingKey=repo+'/pr#16099', siblingRecord={repo,number:16099,merged_by:'another-maintainer'};
const firstSiblingUntil=_stageTrackerTerminalFacts[key].until, firstSiblingAt=_stageTrackerEvents[key].at;
advance({items:[multi],merged:{recent:[record,siblingRecord]}},now+500);
assert.strictEqual(_stageTrackerTerminalFacts[key].until,firstSiblingUntil,'later sibling never renews the earlier pin');
assert.strictEqual(_stageTrackerEvents[key].at,firstSiblingAt,'later sibling never rewrites the earlier event clock');
assert.strictEqual(_stageTrackerEvents[siblingKey].at,now+500);
const siblingAfter=stageTrackerTerminalView({items:[multi]},firstSiblingUntil+1).items[0];
assert.deepStrictEqual(siblingAfter.linked_prs.map(p=>p.number),[16099],'each sibling leaves at its own deadline');
releases=[];
stageTrackerTerminalEvents(firstSiblingUntil+1);
assert.ok(releases.includes(_stageTrackerTerminalFacts[siblingKey].until-firstSiblingUntil-1),'next sibling deadline is rearmed');
releases=[];
stageTrackerPinnedRows({querySelector:()=>null},[{key:stageTrackerItemKey(multi),html:'existing'}],firstSiblingUntil+1,render);
assert.ok(releases.length>0,'an already-rendered host still rearms its release');

reset(); advance({items:[pr]});
advance({items:[],merged:{recent:[{repo,number}]}});
assert.ok(!_stageTrackerEvents[key].text.includes('by '),'unknown actor is not hive or the PR author');
const firstUntil=_stageTrackerCelebrating[key].until, firstAt=_stageTrackerEvents[key].at;
advance(merged,now+100);
assert.ok(_stageTrackerEvents[key].text.includes('by @clubanderson'),'verified actor enrichment updates the same terminal event');
assert.strictEqual(_stageTrackerCelebrating[key].until,firstUntil);
assert.strictEqual(_stageTrackerEvents[key].at,firstAt);
assert.ok(releases.length>0,'uses the existing pin/release mechanism');
reset(); advance({items:[pr]}); result=advance({items:[]});
assert.deepStrictEqual(result.fresh,[],'vanishing is not merge proof');
assert.strictEqual(Object.keys(_stageTrackerCelebrating).length,0);
const delayedAt=now+3*STAGE_TRACKER_CELEBRATE_PIN_MS;
advance({items:[]},delayedAt);
assert.ok(_stageTrackerTerminalRows[key],'unsettled dropped models survive the visual pin duration without being displayed');
assert.strictEqual(stageTrackerPinnedRows({querySelector:()=>null},[],delayedAt,render).length,0);
advance(merged,delayedAt+1);
assert.strictEqual(stageTrackerTerminalPinnedItem(key,delayedAt+1).stage,110,'late canonical evidence still has a terminal row model');
reset(); advance({items:[host]});
advance({items:[{...host,linked_prs:[]}]},delayedAt);
assert.strictEqual(stageTrackerTerminalView({items:[{...host,linked_prs:[]}]},delayedAt).items[0].linked_prs.length,0,'unresolved missing children are not reintroduced as visible work');
result=advance({items:[{...host,linked_prs:[]}],merged:{recent:[record]}},delayedAt+1);
assert.strictEqual(result.view.items[0].linked_prs[0].stage,110,'late linked evidence survives an intervening empty-child refresh');
reset(); advance({items:[multi]});
advance({items:[{...multi,linked_prs:[]}]},now+100);
advance(merged,delayedAt);
assert.deepStrictEqual(stageTrackerTerminalPinnedItem(stageTrackerItemKey(multi),delayedAt).linked_prs.map(p=>p.number),[number],
  'an absent pinned host renders the verified terminal child, never unresolved history-only siblings');
reset();
for(let i=0;i<STAGE_TRACKER_MERGED_LIST_MAX+5;i++) {
  advance({items:[{...pr,number:17000+i}]},now+i);
  advance({items:[]},now+i);
}
assert.strictEqual(Object.keys(_stageTrackerTerminalRows).length,STAGE_TRACKER_MERGED_LIST_MAX,'unsettled detached rows have a hard history bound');
reset();
for(let i=0;i<STAGE_TRACKER_MERGED_LIST_MAX+5;i++) {
  advance({items:[{...host,linked_prs:[{...sibling,number:17000+i}]}]},now+i);
}
const issueKey=stageTrackerItemKey(host);
assert.ok(_stageTrackerTerminalRows[issueKey].item.linked_prs.length<=STAGE_TRACKER_MERGED_LIST_MAX+1,'unsettled missing children are bounded alongside current children');
`)
}
