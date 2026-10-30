package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15784: a PR row in the CI / Review column carries `push`
// — the wait for the author's push (with its age and a stale mark) or the
// push that just landed (sha, commit link, what it queued) — from the
// stage record alone. The chevron keeps the stage; the pill carries the
// push; the hive caption says what is queued.

func TestStageTrackerPushFor(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	it := StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: 42, Author: "zoe", Head: "bbb2222", URL: "https://github.com/hivecommons/hive/pull/42"}
	waiting := stagelabels.Record{Stage: stagelabels.NeedsAuthor, WaitingPushSince: now.Add(-38 * time.Minute), ActivityHead: "aaa1111"}
	pushedAt := now.Add(-2 * time.Minute)
	pushed := stagelabels.Record{Stage: stagelabels.NeedsAuthor, Reason: "changes-requested", WaitingPushSince: now.Add(-38 * time.Minute), ActivityHead: "aaa1111", LastPushAt: pushedAt, LastPushSha: "bbb2222"}
	cases := []struct {
		name    string
		it      StageTrackerItem
		stage   stagelabels.Stage
		rec     stagelabels.Record
		hasRec  bool
		stale   time.Duration
		wantNil bool
		state   string
		since   time.Time
		sha     string
		url     string
		next    string
		isStale bool
	}{
		{name: "issue row", it: StageTrackerItem{Kind: "issue", Number: 1}, stage: stagelabels.NeedsAuthor, rec: waiting, hasRec: true, wantNil: true},
		{name: "no record", it: it, stage: stagelabels.NeedsAuthor, wantNil: true},
		{name: "outside the column", it: it, stage: stagelabels.AwaitingMergeAdmission, rec: pushed, hasRec: true, wantNil: true},
		{name: "050 with nothing pushed has no pill", it: it, stage: stagelabels.ReviewPending, rec: stagelabels.Record{Stage: stagelabels.ReviewPending, ActivityHead: "bbb2222"}, hasRec: true, wantNil: true},
		{name: "waiting, fresh", it: it, stage: stagelabels.NeedsAuthor, rec: waiting, hasRec: true, stale: 4 * time.Hour, state: "waiting", since: now.Add(-38 * time.Minute)},
		{name: "waiting past the stale threshold", it: it, stage: stagelabels.ReturnedToContributor, rec: stagelabels.Record{Stage: stagelabels.ReturnedToContributor, WaitingPushSince: now.Add(-5 * time.Hour)}, hasRec: true, stale: 4 * time.Hour, state: "waiting", since: now.Add(-5 * time.Hour), isStale: true},
		{name: "stale disabled", it: it, stage: stagelabels.NeedsAuthor, rec: stagelabels.Record{Stage: stagelabels.NeedsAuthor, WaitingPushSince: now.Add(-50 * time.Hour)}, hasRec: true, stale: 0, state: "waiting", since: now.Add(-50 * time.Hour)},
		{name: "pushed while still 071 (sweep not yet through)", it: it, stage: stagelabels.NeedsAuthor, rec: pushed, hasRec: true, stale: 4 * time.Hour, state: "pushed", since: pushedAt, sha: "bbb2222", url: "https://github.com/hivecommons/hive/commit/bbb2222", next: "re-review"},
		{name: "pushed, re-staged to 050, checks running", it: it, stage: stagelabels.ReviewPending, rec: stagelabels.Record{Stage: stagelabels.ReviewPending, Reason: "ci-pending", ActivityHead: "bbb2222", LastPushAt: pushedAt, LastPushSha: "bbb2222", WaitingOnHive: true}, hasRec: true, state: "pushed", since: pushedAt, sha: "bbb2222", url: "https://github.com/hivecommons/hive/commit/bbb2222", next: "re-review"},
		{name: "pushed on an adopted row, hive working it", it: it, stage: stagelabels.AdoptedByHive, rec: stagelabels.Record{Stage: stagelabels.AdoptedByHive, Reason: "adopted:ci-red", ActivityHead: "bbb2222", LastPushAt: pushedAt, LastPushSha: "bbb2222"}, hasRec: true, state: "pushed", since: pushedAt, sha: "bbb2222", url: "https://github.com/hivecommons/hive/commit/bbb2222", next: "ci"},
		{name: "pushed, enumerate has not seen the head yet", it: it, stage: stagelabels.Reviewed, rec: stagelabels.Record{Stage: stagelabels.Reviewed, Reason: "approved:ci-pending", ActivityHead: "aaa1111", LastPushAt: pushedAt, LastPushSha: "bbb2222"}, hasRec: true, state: "pushed", since: pushedAt, sha: "bbb2222", url: "https://github.com/hivecommons/hive/commit/bbb2222", next: "ci"},
		{name: "pushed, then the head moved on: the pill goes", it: StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: 42, Author: "zoe", Head: "ccc3333", URL: "https://github.com/hivecommons/hive/pull/42"}, stage: stagelabels.ReviewPending, rec: stagelabels.Record{Stage: stagelabels.ReviewPending, Reason: "ci-pending", ActivityHead: "bbb2222", LastPushAt: pushedAt, LastPushSha: "bbb2222", WaitingOnHive: true}, hasRec: true, wantNil: true},
		{name: "pushed, the list still shows the head pushed over", it: StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: 42, Author: "zoe", Head: "aaa1111", URL: "https://github.com/hivecommons/hive/pull/42"}, stage: stagelabels.NeedsAuthor, rec: pushed, hasRec: true, state: "pushed", since: pushedAt, sha: "bbb2222", url: "https://github.com/hivecommons/hive/commit/bbb2222", next: "re-review"},
		{name: "pushed head settled: the pill goes", it: it, stage: stagelabels.Reviewed, rec: stagelabels.Record{Stage: stagelabels.Reviewed, Reason: "approved", ActivityHead: "bbb2222", LastPushAt: pushedAt, LastPushSha: "bbb2222"}, hasRec: true, wantNil: true},
		{name: "push before the wait began is history", it: it, stage: stagelabels.NeedsAuthor, rec: stagelabels.Record{Stage: stagelabels.NeedsAuthor, WaitingPushSince: now.Add(-10 * time.Minute), LastPushAt: now.Add(-20 * time.Minute), LastPushSha: "aaa1111"}, hasRec: true, stale: 4 * time.Hour, state: "waiting", since: now.Add(-10 * time.Minute)},
		{name: "no commit link without a PR URL", it: StageTrackerItem{Kind: "pr", Number: 42, Author: "zoe", Head: "bbb2222"}, stage: stagelabels.NeedsAuthor, rec: pushed, hasRec: true, state: "pushed", since: pushedAt, sha: "bbb2222", next: "re-review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerPushFor(tc.it, tc.stage, tc.rec, tc.hasRec, tc.stale, now)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want a push field, got nil")
			}
			if got.State != tc.state || got.Login != tc.it.Author || got.Sha != tc.sha || got.URL != tc.url || got.Next != tc.next || got.Stale != tc.isStale {
				t.Fatalf("push = %+v", got)
			}
			if got.Since == nil || !got.Since.Equal(tc.since) {
				t.Fatalf("since = %v, want %v", got.Since, tc.since)
			}
		})
	}
	// The hive line names the push's age; nothing for a wait.
	if c := stageTrackerPushCaption(stageTrackerPushFor(it, stagelabels.NeedsAuthor, pushed, true, 0, now), now); c != "push seen 2m ago → CI/re-review queued" {
		t.Fatalf("caption = %q", c)
	}
	if c := stageTrackerPushCaption(stageTrackerPushFor(it, stagelabels.NeedsAuthor, waiting, true, 0, now), now); c != "" {
		t.Fatalf("waiting caption = %q", c)
	}
}

// Through the tracker, with the webhook re-read booking the push: the 071
// row drops its presence marker and rework word, carries the push, and
// its hive note dates from the push; a plain 050 row with the same push
// carries it too; the nested copy under the issue mirrors it.
func TestStageTrackerPushPayload(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now.Add(-3 * time.Hour)
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.presenceLookup = func(string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{Registered: true, Online: true, LastSeen: now}, true
	}
	cfg := &config.Config{}
	cfg.Dashboard.StageTracker.WaitingPushStaleH = 2
	s.deps = &Dependencies{Config: cfg}
	k71, k50, k72 := stagelabels.Key("hivecommons/hive", 14990), stagelabels.Key("hivecommons/hive", 14992), stagelabels.Key("hivecommons/hive", 14993)
	store.Observe(k72, stagelabels.ReturnedToContributor, "check-failing:ci")
	store.NoteActivity(k72, stagelabels.Activity{Head: "eee", Comments: -1, Reviews: -1})
	store.Observe(k50, stagelabels.ReviewPending, "ci-pending")
	store.NoteActivity(k50, stagelabels.Activity{Head: "ccc", Comments: -1, Reviews: -1, WaitingOnHive: true})
	clock = now.Add(-time.Hour)
	store.Observe(k71, stagelabels.NeedsAuthor, "changes-requested")
	store.NoteActivity(k71, stagelabels.Activity{Head: "aaa", Comments: -1, Reviews: -1})
	clock = now
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 14990, Title: "Docs", Author: "zoe", HeadSHA: "aaa", URL: "https://github.com/hivecommons/hive/pull/14990", UpdatedAt: now.Add(-4 * time.Hour), Labels: []string{"stage/071-needs-author", "author/human"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14992, Title: "Other", Author: "zoe", HeadSHA: "ccc", URL: "https://github.com/hivecommons/hive/pull/14992", Labels: []string{"stage/050-review-pending"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14993, Title: "Clanker", Author: "kai", HeadSHA: "eee", URL: "https://github.com/hivecommons/hive/pull/14993", Labels: []string{"stage/072-returned-to-contributor", "author/clanker"}}},
		},
	}}
	s.UpdateStatus(status)

	// Before any push: the 071 and 072 rows wait (the 072 one past the
	// 2 h knob), the 050 row carries nothing.
	tr := s.buildStageTracker(s.status, store, now)
	if p := trackerItem(t, tr, "pr", 14990).Push; p == nil || p.State != "waiting" || p.Login != "zoe" || p.Stale || p.Since == nil || !p.Since.Equal(now.Add(-time.Hour)) {
		t.Fatalf("071 before push = %+v", p)
	}
	if p := trackerItem(t, tr, "pr", 14993).Push; p == nil || p.State != "waiting" || !p.Stale {
		t.Fatalf("072 before push = %+v (want stale past the 2h knob)", p)
	}
	if it := trackerItem(t, tr, "pr", 14990); it.Presence == nil || it.Rework == nil {
		t.Fatalf("071 before push must keep its presence and rework: %+v %+v", it.Presence, it.Rework)
	}
	if p := trackerItem(t, tr, "pr", 14992).Push; p != nil {
		t.Fatalf("050 before push = %+v", p)
	}

	// The synchronize re-reads land: new heads on 14990 and 14992, the
	// same head on 14993.
	fetched := now.Add(-2 * time.Minute)
	if n := s.notePushedSnapshots([]github.WebhookItemSnapshot{
		{Repo: "hivecommons/hive", Number: 14990, IsPR: true, State: "open", Title: "Docs", HeadSHA: "bbb", FetchedAt: fetched},
		{Repo: "hivecommons/hive", Number: 14992, IsPR: true, State: "open", Title: "Other", HeadSHA: "ddd", FetchedAt: fetched},
		{Repo: "hivecommons/hive", Number: 14993, IsPR: true, State: "open", Title: "Clanker", HeadSHA: "eee", FetchedAt: fetched},
		{Repo: "hivecommons/hive", Number: 7, IsPR: false, State: "open", Title: "An issue"},
	}); n != 2 {
		t.Fatalf("booked %d pushes, want 2", n)
	}
	tr = s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, tr, "pr", 14990)
	p := it.Push
	if p == nil || p.State != "pushed" || p.Login != "zoe" || p.Sha != "bbb" || p.URL != "https://github.com/hivecommons/hive/commit/bbb" || p.Next != "re-review" || p.Since == nil || !p.Since.Equal(fetched) {
		t.Fatalf("071 after push = %+v", p)
	}
	if it.Presence != nil || it.Rework != nil || it.Adoption != nil {
		t.Fatalf("a pushed row no longer waits on its author: presence=%+v rework=%+v adoption=%+v", it.Presence, it.Rework, it.Adoption)
	}
	if it.StageActor == nil || it.StageActor.Kind != StageActorHive || !strings.Contains(it.StageActor.Why, "@zoe pushed ") {
		t.Fatalf("a pushed 071 row is the hive's again: actor=%+v", it.StageActor)
	}
	if it.Caption != "push seen 2m ago → CI/re-review queued" || it.NoteChangedAt == nil || !it.NoteChangedAt.Equal(fetched) {
		t.Fatalf("071 caption = %q note_changed_at = %v", it.Caption, it.NoteChangedAt)
	}
	if p := trackerItem(t, tr, "pr", 14992).Push; p == nil || p.State != "pushed" || p.Sha != "ddd" || p.Next != "re-review" {
		t.Fatalf("050 after push = %+v", p)
	}
	if p := trackerItem(t, tr, "pr", 14993).Push; p == nil || p.State != "waiting" {
		t.Fatalf("072 re-read of the same head must keep waiting: %+v", p)
	}
	raw, _ := json.Marshal(it)
	for _, want := range []string{`"push":{`, `"state":"pushed"`, `"login":"zoe"`, `"sha":"bbb"`, `"url":"https://github.com/hivecommons/hive/commit/bbb"`, `"next":"re-review"`, `"since":"2026-10-30T09:58:00Z"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}
}

func TestStageTrackerWaitingPushStaleKnob(t *testing.T) {
	cfg := &config.Config{}
	if (*config.Config)(nil).StageTrackerWaitingPushStale() != 4*time.Hour || cfg.StageTrackerWaitingPushStale() != 4*time.Hour {
		t.Fatal("default dashboard.stage_tracker.waiting_push_stale_h is 4 hours")
	}
	cfg.Dashboard.StageTracker.WaitingPushStaleH = -1
	if cfg.StageTrackerWaitingPushStale() != 0 {
		t.Fatal("a negative waiting_push_stale_h disables the stale mark")
	}
	cfg.Dashboard.StageTracker.WaitingPushStaleH = 6
	if cfg.StageTrackerWaitingPushStale() != 6*time.Hour {
		t.Fatal("waiting_push_stale_h is read in hours")
	}
	s := newTestServer()
	if s.stageTrackerWaitingPushStale() != 4*time.Hour {
		t.Fatal("a server without config uses the default")
	}
	s.deps = &Dependencies{Config: cfg}
	if s.stageTrackerWaitingPushStale() != 6*time.Hour {
		t.Fatal("the server reads the configured knob")
	}
}

// stageTrackerPushJS lifts the SPA's push-progress helpers (the pill, the
// lineage suffix, the hover line) for the node harnesses.
func stageTrackerPushJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_PUSH_NEXT"),
		jsFunc(t, html, "stageTrackerPushOf"),
		jsFunc(t, html, "stageTrackerPushedBack"),
		jsFunc(t, html, "stageTrackerPushAge"),
		jsFunc(t, html, "stageTrackerPushSha"),
		jsFunc(t, html, "stageTrackerPushWaitHtml"),
		jsFunc(t, html, "stageTrackerPushChipText"),
		jsFunc(t, html, "stageTrackerPushOnLineage"),
		jsFunc(t, html, "stageTrackerPushLineageSuffix"),
		jsFunc(t, html, "stageTrackerPushTip"),
		jsFunc(t, html, "stageTrackerPushChipHtml"),
	}, "\n")
}

// TestStageTrackerPushJS: the SPA fed the live `push` shape. Waiting: the
// presence pill ends with the wait's age, amber once stale. Pushed: the
// presence pill gives way to "@zoe pushed abc1234 · 2m" linking to the
// commit; on an adopted row the lineage chip says "· pushed 2m ago"
// instead and no pill of its own is drawn; the hover line names what the
// push queued. Without the field nothing changes.
func TestStageTrackerPushJS(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		stageTrackerRowActionsPreamble(t, html),
		jsFunc(t, html, "stageTrackerAdoptionTip"),
		jsFunc(t, html, "stageTrackerAdoptOffered"),
		jsFunc(t, html, "stageTrackerAdoptionChipText"),
		jsFunc(t, html, "stageTrackerAdoptionChipHtml"),
		jsFunc(t, html, "stageTrackerDecisionOf"),
		jsFunc(t, html, "stageTrackerDecisionTriggerHtml"),
		jsFunc(t, html, "stageTrackerDecisionPopHtml"),
		stageTrackerPresenceJS(t, html),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerLineageText"),
		jsConstBlock(t, html, "STAGE_TRACKER_LINEAGE_REASON_TIP"),
		stageTrackerPushJS(t, html),
		`
const NOW = Date.parse('2026-10-28T14:32:00Z');
Date.now = () => NOW;
const iso = ms => new Date(ms).toISOString();
const MIN = 60000, HOUR = 60 * MIN;
const base = { kind: 'pr', repo: 'hivecommons/hive', number: 15780, stage: 71, author: 'zoe', author_kind: 'human', url: 'https://github.com/hivecommons/hive/pull/15780',
  presence: { login: 'zoe', state: 'online', kind: 'human', since: iso(NOW - 38 * MIN), last_seen: iso(NOW - 2 * MIN), next: 'wait' } };
const w = (push) => Object.assign({}, base, { push });
const waiting = w({ state: 'waiting', login: 'zoe', since: iso(NOW - 38 * MIN) });
const stale = w({ state: 'waiting', login: 'zoe', since: iso(NOW - 5 * HOUR), stale: true });
const pushed = w({ state: 'pushed', login: 'zoe', since: iso(NOW - 2 * MIN), sha: 'abc1234def', url: 'https://github.com/hivecommons/hive/commit/abc1234def', next: 'ci' });
const rereview = w({ state: 'pushed', login: 'zoe', since: iso(NOW - 2 * MIN), sha: 'abc1234def', url: 'https://github.com/hivecommons/hive/commit/abc1234def', next: 're-review' });

// Without the field the pill is exactly what it was.
assert.strictEqual(stageTrackerPushOf(base), null);
assert.strictEqual(stageTrackerPushedBack(base), false);
assert.strictEqual(stageTrackerPushWaitHtml(base, NOW), '');
assert.strictEqual(stageTrackerPushChipHtml(base, NOW), '');
assert.strictEqual(stageTrackerPushTip(base, NOW), '');
assert.ok(/>waiting on <span class="ham-presence[\s\S]*<\/span>@zoe to push<\/span>$/.test(stageTrackerPresenceChipHtml(base)), stageTrackerPresenceChipHtml(base));

// Waiting: the wait's age rides last on the presence pill; amber once stale.
assert.strictEqual(stageTrackerPushedBack(waiting), false);
assert.strictEqual(stageTrackerPushWaitHtml(waiting, NOW), ' · <span class="ham-push-age">38m</span>');
assert.strictEqual(stageTrackerPushWaitHtml(stale, NOW), ' · <span class="ham-push-age ham-push-stale">5h</span>');
const wchip = stageTrackerPresenceChipHtml(waiting);
assert.ok(/@zoe to push · <span class="ham-push-age">38m<\/span><\/span>$/.test(wchip), wchip);
assert.ok(wchip.includes('waiting for @zoe to push since 38m ago ('), wchip);
assert.strictEqual(stageTrackerPushChipHtml(waiting, NOW), '', 'no second pill while waiting');
assert.match(stageTrackerPushTip(stale, NOW), /^waiting for @zoe to push since 5h ago \(.*\) · no push for a long time$/);

// Pushed: the ball is back with the hive — the pill names the push and links the commit.
assert.strictEqual(stageTrackerPushedBack(pushed), true);
assert.strictEqual(stageTrackerPushChipText(pushed, NOW), '@zoe pushed abc1234 · 2m');
const pchip = stageTrackerPushChipHtml(pushed, NOW);
assert.match(pchip, /^<a class="ham-badge ham-badge-waiting ham-badge-push" title="@zoe pushed abc1234 2m ago \([^)]*\) · CI running" aria-label="@zoe pushed abc1234 2m ago \([^)]*\) · CI running" href="https:\/\/github\.com\/hivecommons\/hive\/commit\/abc1234def" target="_blank" rel="noopener">@zoe pushed abc1234 · 2m<\/a>$/);
assert.match(stageTrackerPushTip(rereview, NOW), / · re-review queued$/);
assert.strictEqual(stageTrackerPushWaitHtml(pushed, NOW), '');
// No URL: a span, same words.
assert.strictEqual(stageTrackerPushChipHtml(w({ state: 'pushed', since: iso(NOW - 2 * MIN), sha: 'abc1234' }), NOW).replace(/ title="[^"]*" aria-label="[^"]*"/, ''), '<span class="ham-badge ham-badge-waiting ham-badge-push">pushed abc1234 · 2m</span>');

// Adopted row: the lineage chip carries the push; no pill of its own.
const adopted = Object.assign({}, pushed, { stage: 74, lineage: { kind: 'adopted', from_number: 15516, from_author: 'karibhou', at: iso(NOW - 3 * HOUR) } });
assert.strictEqual(stageTrackerLineageText(Object.assign({}, adopted, { push: null }), NOW), "adopted from @karibhou's #15516 · 3h");
assert.strictEqual(stageTrackerLineageText(adopted, NOW), "adopted from @karibhou's #15516 · pushed 2m ago");
assert.strictEqual(stageTrackerPushChipHtml(adopted, NOW), '');
// A superseded lineage is not the adopted kind: the pill stays.
const sup = Object.assign({}, pushed, { lineage: { kind: 'superseded', to_number: 9, at: iso(NOW - HOUR) } });
assert.strictEqual(stageTrackerLineageText(sup, NOW), 'superseded by #9 · 1h');
assert.ok(stageTrackerPushChipHtml(sup, NOW).includes('@zoe pushed abc1234'));
// Issues never carry it.
assert.strictEqual(stageTrackerPushOf(Object.assign({}, pushed, { kind: 'issue' })), null);
`}, "\n")
	runStageTrackerRowActionsScript(t, script)
}

// TestStageTrackerPushChevronJS: on a 071 / 072 row the chevron reads
// "Needs @zoe 👤" while the hive waits; once the push lands it returns to
// the column's own word — stage only, the pill carries the push.
func TestStageTrackerPushChevronJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+"\n"+stageTrackerPushJS(t, html)+stageTrackerRowsFixtureJS+`
Date.now = () => now;
const litWord = (html) => text((/<li class="ham-seg active[^"]*"[^>]*>([\s\S]*?)<\/li>/.exec(strip(html)) || ['', ''])[1]);
const since = new Date(now - 3 * 3600000).toISOString();
const row = (extra) => stageTrackerRowHtml(Object.assign({ repo: 'hivecommons/hive', number: 8, kind: 'pr', title: 'docs', url: 'https://x/8', stage: 71, stage_since: since, caption: 'Needs author', author: 'zoe', author_kind: 'human', stage_actor: { kind: 'human', login: 'zoe', why: '@zoe must act' } }, extra || {}), stages, ctx);
const waiting = row({ push: { state: 'waiting', login: 'zoe', since } });
assert.ok(/^Needs @zoe\b/.test(litWord(waiting)), litWord(waiting));
const pushed = row({ stage_actor: { kind: 'hive', why: '@zoe pushed abc1234 — CI runs on the new head' }, push: { state: 'pushed', login: 'zoe', since: new Date(now - 120000).toISOString(), sha: 'abc1234', url: 'https://x/commit/abc1234', next: 'ci' } });
assert.ok(/^CI \/ Review\b/.test(litWord(pushed)), litWord(pushed));
assert.ok(!/ham-actor-ico[^>]*aria-label="Next move: human"/.test(pushed), 'the hive carries it: no person glyph on the chevron');
assert.ok(!/Needs @zoe/.test(litWord(pushed)), litWord(pushed));
assert.ok(pushed.includes('>@zoe pushed abc1234 · 2m</a>'), pushed);
assert.ok(pushed.includes('Push: @zoe pushed abc1234 2m ago'), 'hover line');
// 072 the same.
const back = row({ stage: 72, caption: 'Returned to contributor', push: { state: 'pushed', login: 'zoe', since: new Date(now - 120000).toISOString(), sha: 'abc1234', next: 're-review' } });
assert.ok(/^CI \/ Review\b/.test(litWord(back)), litWord(back));
console.log('push chevron ok');
`)
}
