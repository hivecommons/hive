package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

// The tracker names who holds a claimed item — agent, clanker owner or
// person — from the worker ledger, the claim marker, an assignee or the
// busy agent, in that order; nothing names nobody (hivecommons/hive#15422).
func TestStageTrackerIssueClaimant(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-3 * time.Hour)
	claimedAt := now.Add(-20 * time.Minute)
	kickedAt := now.Add(-5 * time.Minute)
	clankerOwners := func(login string) bool { return strings.EqualFold(login, "bob") }
	cases := []struct {
		name string
		in   stageTrackerClaimInputs
		want *StageTrackerClaimant
	}{
		{"ledger agent from another hive", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "quality", Kind: claims.KindAgent, Hive: "hosted-ibm-alchemy", ClaimedAt: claimedAt}, HiveID: "hosted-ibm-main", Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "quality", Hive: "hosted-ibm-alchemy", Since: &claimedAt, Source: claimantSourceClaimLedger}},
		{"ledger agent from this hive drops the hive", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "quality", Kind: claims.KindAgent, Hive: "hosted-ibm-main", ClaimedAt: claimedAt}, HiveID: "hosted-ibm-main"},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "quality", Since: &claimedAt, Source: claimantSourceClaimLedger}},
		{"ledger contributor is a clanker", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "bob", Kind: claims.KindContributor, ClaimedAt: claimedAt}},
			&StageTrackerClaimant{Kind: claimantKindClanker, Login: "bob", Since: &claimedAt, Source: claimantSourceClaimLedger}},
		{"ledger human is a person", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "alice", Kind: claims.KindHuman, ClaimedAt: claimedAt}},
			&StageTrackerClaimant{Kind: claimantKindPerson, Login: "alice", Since: &claimedAt, Source: claimantSourceClaimLedger}},
		{"marker with fleet fingerprint", stageTrackerClaimInputs{MarkerKind: "agent", MarkerHolder: "fixer", MarkerHive: "hosted-ibm-alchemy", HiveID: "local", Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "fixer", Hive: "hosted-ibm-alchemy", Since: &since, Source: claimantSourceClaimComment}},
		{"assignee who owns a clanker", stageTrackerClaimInputs{MarkerHolder: "Bob", Since: &since, IsClankerOwner: clankerOwners},
			&StageTrackerClaimant{Kind: claimantKindClanker, Login: "Bob", Since: &since, Source: claimantSourceAssignee}},
		{"plain assignee is a person", stageTrackerClaimInputs{Assignees: []string{"alice"}, Since: &since, IsClankerOwner: clankerOwners},
			&StageTrackerClaimant{Kind: claimantKindPerson, Login: "alice", Since: &since, Source: claimantSourceAssignee}},
		{"kick history fallback", stageTrackerClaimInputs{Worker: &stageTrackerWorker{Agent: "fixer", At: kickedAt}, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "fixer", Since: &kickedAt, Source: claimantSourceKickHistory}},
		{"ledger beats assignee", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "quality", Kind: claims.KindAgent, ClaimedAt: claimedAt}, Assignees: []string{"alice"}},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "quality", Since: &claimedAt, Source: claimantSourceClaimLedger}},
		{"nobody", stageTrackerClaimInputs{Since: &since}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerIssueClaimant(tc.in)
			if !claimantEqual(got, tc.want) {
				t.Fatalf("got %+v (since %v), want %+v", got, claimantSince(got), tc.want)
			}
		})
	}
}

func TestStageTrackerPRClaimant(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	kickedAt := now.Add(-time.Minute)
	cases := []struct {
		name string
		in   stageTrackerClaimInputs
		want *StageTrackerClaimant
	}{
		{"hive PR names the agent on it", stageTrackerClaimInputs{Author: "hive-bot[bot]", AuthorKind: authorKindHive, Labels: []string{"agent/quality"}, Worker: &stageTrackerWorker{Agent: "fixer", At: kickedAt}, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "fixer", Since: &kickedAt, Source: claimantSourceKickHistory}},
		{"hive PR names its lane label", stageTrackerClaimInputs{Author: "hive-bot[bot]", AuthorKind: authorKindHive, Labels: []string{"stage/070-fixing", "agent/quality"}, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "quality", Since: &since, Source: claimantSourceAuthor}},
		{"hive PR without a lane falls back to the login", stageTrackerClaimInputs{Author: "hive-bot[bot]", AuthorKind: authorKindHive, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Login: "hive-bot[bot]", Since: &since, Source: claimantSourceAuthor}},
		{"clanker PR", stageTrackerClaimInputs{Author: "bob", AuthorKind: authorKindClanker, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindClanker, Login: "bob", Since: &since, Source: claimantSourceAuthor}},
		{"human PR", stageTrackerClaimInputs{Author: "alice", AuthorKind: authorKindHuman, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindPerson, Login: "alice", Since: &since, Source: claimantSourceAuthor}},
		{"foreign bot", stageTrackerClaimInputs{Author: "dependabot[bot]", AuthorKind: "", Since: &since}, nil},
		// PR in progress rows (hivecommons/hive#15541): the attribution
		// trailer names the agent; a live ledger claim beats the author.
		{"hive PR names its attribution trailer agent", stageTrackerClaimInputs{Author: "hive-bot[bot]", AuthorKind: authorKindHive, HiveAgent: "scanner", Labels: []string{"agent/quality"}, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "scanner", Since: &since, Source: claimantSourceAuthor}},
		{"ledger claim on the PR beats the author", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "bob", Kind: claims.KindContributor, ClaimedAt: kickedAt}, Author: "hive-bot[bot]", AuthorKind: authorKindHive, HiveAgent: "scanner", Since: &since},
			&StageTrackerClaimant{Kind: claimantKindClanker, Login: "bob", Since: &kickedAt, Source: claimantSourceClaimLedger}},
		{"ledger agent from another hive on the PR", stageTrackerClaimInputs{Ledger: &claims.Claim{Holder: "fixer", Kind: claims.KindAgent, Hive: "hosted-ibm-alchemy", ClaimedAt: kickedAt}, HiveID: "hosted-ibm-main", Author: "alice", AuthorKind: authorKindHuman, Since: &since},
			&StageTrackerClaimant{Kind: claimantKindHiveAgent, Agent: "fixer", Hive: "hosted-ibm-alchemy", Since: &kickedAt, Source: claimantSourceClaimLedger}},
		{"unknown author kind with no claim", stageTrackerClaimInputs{Author: "someone", Since: &since}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageTrackerPRClaimant(tc.in); !claimantEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStageTrackerClaimantStage(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		stage int
		want  bool
	}{
		{"issue", trackerNumberClaimed, true}, {"issue", trackerNumberPRInProgress, true}, {"issue", trackerNumberActionable, false}, {"issue", trackerNumberCoveredByPR, false},
		{"pr", trackerNumberReviewPending, true}, {"pr", trackerNumberFixing, true}, {"pr", trackerNumberReturned, true}, {"pr", trackerNumberAdopted, true},
		{"pr", trackerNumberAwaitingMerge, false}, {"pr", trackerNumberNeedsDecision, false},
		// A PR before classification or at 030/040 lights "PR in progress" under its issue and names its holder (hivecommons/hive#15541).
		{"pr", 0, true}, {"pr", trackerNumberPRInProgress, true}, {"pr", trackerNumberCoveredByPR, true},
	} {
		if got := stageTrackerClaimantStage(tc.kind, tc.stage); got != tc.want {
			t.Errorf("%s stage %d: got %v, want %v", tc.kind, tc.stage, got, tc.want)
		}
	}
}

func claimantSince(c *StageTrackerClaimant) time.Time {
	if c == nil || c.Since == nil {
		return time.Time{}
	}
	return *c.Since
}

func claimantEqual(a, b *StageTrackerClaimant) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Kind == b.Kind && a.Login == b.Login && a.Agent == b.Agent && a.Hive == b.Hive && a.Source == b.Source && claimantSince(a).Equal(claimantSince(b))
}

// The chevron carries one short holder token after the stage word and the
// hover one "Held by:" line; an unknown holder adds nothing.
func TestStageTrackerClaimantChevronJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+stageTrackerRowsFixtureJS+`
const litWord = (html) => text((/<li class="ham-seg active[^"]*"[^>]*>([\s\S]*?)<\/li>/.exec(strip(html)) || ['', ''])[1]);
const litTitle = (html) => (/<li class="ham-seg active[^"]*"[^>]*title="([^"]*)"/.exec(strip(html)) || [])[1] || '';
const heldLine = (html) => (/Held by: .*$/.exec(litTitle(html)) || [''])[0].replace(/&#39;/g, "'");
const since = new Date(now - 3 * 3600000).toISOString();
const issue = (n, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'fix it', url: 'https://x/' + n, stage: 20, stage_since: since, caption: 'Claimed', author: 'alice', stage_actor: { kind: 'hive', why: 'holds the live claim' } }, extra || {});

// Hive agent from this hive: lane name only.
let row = stageTrackerRowHtml(issue(1, { claimant: { kind: 'hive-agent', agent: 'quality', since, source: 'claim-ledger' } }), stages, ctx);
assert.strictEqual(litWord(row), 'Claimed · quality', litWord(row));
assert.strictEqual(heldLine(row), 'Held by: hive agent quality since 3h ago (claim ledger)', litTitle(row));
assert.ok(strip(row).includes('class="ham-ico ham-actor-ico"'), 'the actor icon stays');

// Hive agent from another hive: lane@short-hive on the chevron, the full hive in the hover.
row = stageTrackerRowHtml(issue(2, { claimant: { kind: 'hive-agent', agent: 'quality', hive: 'hosted-ibm-alchemy', since, source: 'claim-comment' } }), stages, ctx);
assert.strictEqual(litWord(row), 'Claimed · quality@alchemy', litWord(row));
assert.ok(heldLine(row).startsWith('Held by: hive agent quality@hosted-ibm-alchemy since 3h ago'), litTitle(row));

// Clanker: the owner's login plus "'s clanker".
row = stageTrackerRowHtml(issue(3, { stage_actor: { kind: 'clanker', why: 'holds the live claim' }, claimant: { kind: 'clanker', login: 'bob', since, source: 'assignee' } }), stages, ctx);
assert.strictEqual(litWord(row), "Claimed · @bob's clanker", litWord(row));
assert.ok(heldLine(row).startsWith("Held by: @bob's clanker since 3h ago (assignee)"), litTitle(row));

// Person: @login.
row = stageTrackerRowHtml(issue(4, { stage_actor: { kind: 'human', why: 'holds the live claim' }, claimant: { kind: 'person', login: 'alice', since, source: 'assignee' } }), stages, ctx);
assert.strictEqual(litWord(row), 'Claimed · @alice', litWord(row));
assert.ok(heldLine(row).startsWith('Held by: @alice since 3h ago (assignee)'), litTitle(row));

// Kick history on PR in progress: the agent and the source.
row = stageTrackerRowHtml(issue(5, { stage: 30, caption: 'PR in progress', claimant: { kind: 'hive-agent', agent: 'fixer', since: new Date(now - 30000).toISOString(), source: 'kick-history' } }), stages, ctx);
assert.strictEqual(litWord(row), 'PR in progress · fixer', litWord(row));
assert.strictEqual(heldLine(row), 'Held by: hive agent fixer since just now (kick history)', litTitle(row));

// Unknown: nothing added, no slug or "unknown" leaks.
row = stageTrackerRowHtml(issue(6), stages, ctx);
assert.strictEqual(litWord(row), 'Claimed', litWord(row));
assert.strictEqual(heldLine(row), '', litTitle(row));
row = stageTrackerRowHtml(issue(7, { claimant: { kind: 'person', source: 'assignee' } }), stages, ctx);
assert.strictEqual(litWord(row), 'Claimed', 'a claimant without an identity adds nothing: ' + litWord(row));
assert.ok(!/hive-agent|kick-history|claim-comment|unknown/i.test(text(row)), 'no slug leakage: ' + text(row));

// A returned-to-contributor PR names its author on the detour word.
const pr = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 8, kind: 'pr', title: 'docs', url: 'https://x/8', stage: 72, stage_since: since, caption: 'Returned', author: 'bob', author_kind: 'clanker', stage_actor: { kind: 'clanker', login: 'bob', why: '@bob must act' }, claimant: { kind: 'clanker', login: 'bob', since, source: 'author' } }, stages, ctx);
assert.ok(litWord(pr).endsWith("@bob's clanker"), litWord(pr));
assert.strictEqual((litWord(pr).match(/@bob's clanker/g) || []).length, 1, 'the detour word names the holder once: ' + litWord(pr));
assert.ok(heldLine(pr).startsWith("Held by: @bob's clanker since 3h ago (PR author)"), litTitle(pr));

// PR in progress (hivecommons/hive#15541): an unclassified PR under its
// issue names who is working it, with the holder's own kind icon, exactly
// like Claimed; nobody known keeps the bare word.
const subRow = (prExtra) => stageTrackerRowHtml(issue(10, { stage: 40, caption: 'Covered by PR', stage_actor: { kind: 'none', why: 'its PR' }, linked_prs: [
  Object.assign({ number: 101, url: 'https://x/101', title: 'fix', stage: 0, stage_since: since, caption: 'PR in progress', stage_actor: { kind: 'none', why: '' } }, prExtra),
] }), stages, ctx);
let sub = subRow({ author: 'hive-bot', author_kind: 'hive', claimant: { kind: 'hive-agent', agent: 'scanner', since, source: 'author' } });
assert.strictEqual(litWord(sub), 'PR in progress · scanner', litWord(sub));
assert.ok(heldLine(sub).startsWith('Held by: hive agent scanner since 3h ago (PR author)'), litTitle(sub));
assert.ok(strip(sub).includes('class="ham-ico ham-actor-ico"'), 'the holder draws an icon when the next mover is nobody: ' + strip(sub));
assert.ok(/ham-seg-t ham-has-holder/.test(strip(sub)), 'the word carries the one-line holder class: ' + strip(sub));
const icoOf = (html) => { const m = /<li class="ham-seg active[^"]*"[^>]*>([\s\S]*?)<\/li>/.exec(strip(html)); return m ? m[1] : ''; };
const clankerRow = subRow({ author: 'bob', author_kind: 'clanker', claimant: { kind: 'clanker', login: 'bob', since, source: 'author' } });
const humanRow = subRow({ author: 'alice', author_kind: 'human', claimant: { kind: 'person', login: 'alice', since, source: 'author' } });
assert.strictEqual(litWord(clankerRow), "PR in progress · @bob's clanker", litWord(clankerRow));
assert.strictEqual(litWord(humanRow), 'PR in progress · @alice', litWord(humanRow));
const icoOnly = (html) => icoOf(html).replace(/<span class="ham-seg-t[\s\S]*?<\/span>/, '');
assert.ok(icoOnly(clankerRow).includes('ham-actor-ico') && icoOnly(humanRow).includes('ham-actor-ico'), 'all three holder kinds draw an icon');
assert.ok(icoOnly(sub) !== icoOnly(clankerRow) && icoOnly(clankerRow) !== icoOnly(humanRow), 'hive, clanker and person icons differ');
sub = subRow({ author: 'dependabot[bot]', author_kind: 'bot' });
assert.strictEqual(litWord(sub), 'PR in progress', 'nobody known keeps the bare word: ' + litWord(sub));
assert.ok(!strip(sub).includes('ham-actor-ico'), 'no icon without a holder');
assert.ok(!strip(sub).includes('ham-has-holder'), 'no holder class without a holder');

// The covering-PR sub-row carries its PR's holder.
const covered = stageTrackerRowHtml(issue(9, { stage: 40, caption: 'Covered by PR', stage_actor: { kind: 'hive', why: 'its PR' }, linked_prs: [
  { number: 91, url: 'https://x/91', title: 'fix', author: 'hive-bot', author_kind: 'hive', stage: 70, stage_since: since, caption: 'Fixing', stage_actor: { kind: 'hive', login: 'fixer', why: 'fixer is on it' }, claimant: { kind: 'hive-agent', agent: 'fixer', since, source: 'kick-history' } },
  { number: 92, url: 'https://x/92', title: 'alt', author: 'alice', author_kind: 'human', stage: 50, stage_since: since, caption: 'CI', stage_actor: { kind: 'hive', why: 'review' }, claimant: { kind: 'person', login: 'alice', since, source: 'author' } },
] }), stages, ctx);
const subWords = strips(covered).slice(1).map(s => text((/<li class="ham-seg active[^"]*"[^>]*>([\s\S]*?)<\/li>/.exec(s) || ['', ''])[1]));
assert.deepStrictEqual(subWords, ['Fixing · fixer', 'CI / Review · @alice'], JSON.stringify(subWords));
console.log('claimant chevron ok');
`)
}
