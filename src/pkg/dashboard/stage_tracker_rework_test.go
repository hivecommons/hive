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

// hivecommons/hive#15280: a PR row returned to its author (071 / 072)
// carries `rework` — whether the request has been picked up: requested,
// acknowledged (activity on the PR after the request), in_progress (the
// clanker's relay claimed the fix) or pushed (the head moved; 050 next
// sweep) — from the stage record, the PR list and the hub fix queue the
// tracker already has. No extra GitHub calls.

func TestStageTrackerReworkFor(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	requested := now.Add(-time.Hour)
	tp := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	pr := func(head string, updated *time.Time) StageTrackerItem {
		return StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: 42, Head: head, UpdatedAt: updated}
	}
	nudged := stagelabels.Record{Stage: stagelabels.ReturnedToContributor, EnteredAt: requested.Add(-time.Minute), NudgeHead: "aaa", FirstNudgeAt: requested, LastNudgeAt: requested, NudgeCount: 1}
	none := func(string, string, int) (StageTrackerFixWork, bool) { return StageTrackerFixWork{}, false }
	inflight := func(login, repo string, n int) (StageTrackerFixWork, bool) {
		if login != "zoe" || repo != "hivecommons/hive" || n != 42 {
			return StageTrackerFixWork{}, false
		}
		return StageTrackerFixWork{InFlight: true, DispatchedAt: now.Add(-5 * time.Minute), Mode: "spawn"}, true
	}
	queued := func(login, repo string, n int) (StageTrackerFixWork, bool) {
		return StageTrackerFixWork{Queued: true, QueuedAt: now.Add(-10 * time.Minute), Ahead: 2}, login == "zoe"
	}
	rerouted := func(login, repo string, n int) (StageTrackerFixWork, bool) {
		return StageTrackerFixWork{InFlight: true, DispatchedAt: now.Add(-2 * time.Minute), Mode: "preempt"}, login == "kai"
	}

	cases := []struct {
		name      string
		it        StageTrackerItem
		login     string
		stage     stagelabels.Stage
		rec       stagelabels.Record
		hasRec    bool
		fixWork   func(string, string, int) (StageTrackerFixWork, bool)
		wantNil   bool
		state     string
		evidence  string
		actor     string
		since     time.Time
		ahead     int
		mode      string
		head      string
		reqAtFrom time.Time
	}{
		{name: "not a returned stage", it: pr("aaa", nil), login: "zoe", stage: stagelabels.ReviewPending, rec: nudged, hasRec: true, fixWork: none, wantNil: true},
		{name: "nobody waited on", it: pr("aaa", nil), login: "", stage: stagelabels.NeedsAuthor, rec: nudged, hasRec: true, fixWork: none, wantNil: true},
		{name: "issue row", it: StageTrackerItem{Kind: "issue", Repo: "hivecommons/hive", Number: 42}, login: "zoe", stage: stagelabels.NeedsAuthor, rec: nudged, hasRec: true, fixWork: none, wantNil: true},
		{name: "no record", it: pr("aaa", nil), login: "zoe", stage: stagelabels.NeedsAuthor, hasRec: false, fixWork: none, wantNil: true},
		{name: "requested, nothing moved", it: pr("aaa", tp(-90*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: none,
			state: "requested", actor: "zoe", since: requested, reqAtFrom: requested},
		{name: "hive's own nudge comment is not an ack", it: pr("aaa", tp(-59*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: none,
			state: "requested", actor: "zoe", since: requested, reqAtFrom: requested},
		{name: "071 without a nudge dates from stage entry", it: pr("aaa", nil), login: "zoe", stage: stagelabels.NeedsAuthor,
			rec: stagelabels.Record{Stage: stagelabels.NeedsAuthor, EnteredAt: now.Add(-20 * time.Minute)}, hasRec: true, fixWork: nil,
			state: "requested", actor: "zoe", since: now.Add(-20 * time.Minute), reqAtFrom: now.Add(-20 * time.Minute)},
		{name: "activity after the request acknowledges", it: pr("aaa", tp(-30*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: none,
			state: "acknowledged", evidence: "activity", actor: "zoe", since: now.Add(-30 * time.Minute), reqAtFrom: requested},
		{name: "activity after a later re-nudge still counts from the re-nudge", it: pr("aaa", tp(-30*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor,
			rec: stagelabels.Record{Stage: stagelabels.ReturnedToContributor, EnteredAt: requested, NudgeHead: "aaa", FirstNudgeAt: requested, LastNudgeAt: now.Add(-29 * time.Minute), NudgeCount: 2}, hasRec: true, fixWork: none,
			state: "requested", actor: "zoe", since: requested, reqAtFrom: requested},
		{name: "future updated_at is clock skew, not an ack", it: pr("aaa", tp(10*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: none,
			state: "requested", actor: "zoe", since: requested, reqAtFrom: requested},
		{name: "queued in the clanker's hub queue", it: pr("aaa", nil), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: queued,
			state: "requested", evidence: "queued", actor: "zoe", since: now.Add(-10 * time.Minute), ahead: 2, reqAtFrom: requested},
		{name: "clanker claimed the fix", it: pr("aaa", tp(-30*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: inflight,
			state: "in_progress", evidence: "claim", actor: "zoe", since: now.Add(-5 * time.Minute), mode: "spawn", reqAtFrom: requested},
		{name: "rerouted fix claimed by the other clanker", it: pr("aaa", nil), login: "zoe", stage: stagelabels.ReturnedToContributor,
			rec: stagelabels.Record{Stage: stagelabels.ReturnedToContributor, EnteredAt: requested, NudgeHead: "aaa", FirstNudgeAt: requested, LastNudgeAt: requested, NudgeCount: 1, RerouteTo: "kai", RerouteHead: "aaa"}, hasRec: true, fixWork: rerouted,
			state: "in_progress", evidence: "claim", actor: "kai", since: now.Add(-2 * time.Minute), mode: "preempt", reqAtFrom: requested},
		{name: "stale reroute on an older head is ignored", it: pr("aaa", nil), login: "zoe", stage: stagelabels.ReturnedToContributor,
			rec: stagelabels.Record{Stage: stagelabels.ReturnedToContributor, EnteredAt: requested, NudgeHead: "aaa", FirstNudgeAt: requested, LastNudgeAt: requested, NudgeCount: 1, RerouteTo: "kai", RerouteHead: "000"}, hasRec: true, fixWork: rerouted,
			state: "requested", actor: "zoe", since: requested, reqAtFrom: requested},
		{name: "new head pushed", it: pr("bbb", tp(-3*time.Minute)), login: "zoe", stage: stagelabels.ReturnedToContributor, rec: nudged, hasRec: true, fixWork: inflight,
			state: "pushed", evidence: "commit", actor: "zoe", since: now.Add(-3 * time.Minute), head: "bbb", reqAtFrom: requested},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerReworkFor(tc.it, tc.login, tc.stage, tc.rec, tc.hasRec, tc.fixWork, now)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("rework = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("rework = nil")
			}
			if got.State != tc.state || got.Evidence != tc.evidence || got.Actor != tc.actor || got.Ahead != tc.ahead || got.Mode != tc.mode || got.Head != tc.head {
				t.Fatalf("rework = %+v", got)
			}
			if got.RequestedAt == nil || !got.RequestedAt.Equal(tc.reqAtFrom) {
				t.Fatalf("requested_at = %v, want %v", got.RequestedAt, tc.reqAtFrom)
			}
			if got.Since == nil || !got.Since.Equal(tc.since) {
				t.Fatalf("since = %v, want %v", got.Since, tc.since)
			}
		})
	}
}

// Through the tracker: the field rides on the PR row, dated from the nudge
// the contributor engine recorded.
func TestStageTrackerReworkPayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.presenceLookup = func(string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{Registered: true, Online: true, LastSeen: now}, true
	}
	s.deps = &Dependencies{Config: &config.Config{}}
	s.fixWorkLookup = func(login, repo string, n int) (StageTrackerFixWork, bool) {
		if login == "zoe" && n == 14991 {
			return StageTrackerFixWork{InFlight: true, DispatchedAt: now.Add(-4 * time.Minute), Mode: "spawn"}, true
		}
		return StageTrackerFixWork{}, false
	}
	clock = now.Add(-3 * time.Hour)
	store.Observe(stagelabels.Key("hivecommons/hive", 14990), stagelabels.NeedsAuthor, "changes-requested")
	store.Observe(stagelabels.Key("hivecommons/hive", 14991), stagelabels.ReturnedToContributor, "check-failing:ci")
	store.MarkNudged(stagelabels.Key("hivecommons/hive", 14991), "bbb", 1, true, now.Add(-2*time.Hour))
	clock = now
	upd := now.Add(-90 * time.Minute)
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 14990, Title: "Docs", Author: "zoe", HeadSHA: "aaa", UpdatedAt: now.Add(-4 * time.Hour), Labels: []string{"stage/071-needs-author", "author/human"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14991, Title: "Fix", Author: "zoe", HeadSHA: "bbb", UpdatedAt: upd, Labels: []string{"stage/072-returned-to-contributor"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14992, Title: "Other", Author: "zoe", HeadSHA: "ccc", Labels: []string{"stage/050-review-pending"}}},
		},
	}}
	s.UpdateStatus(status)
	tr := s.buildStageTracker(status, store, now)
	if r := trackerItem(t, tr, "pr", 14990).Rework; r == nil || r.State != "requested" || r.Actor != "zoe" || r.RequestedAt == nil || !r.RequestedAt.Equal(now.Add(-3*time.Hour)) {
		t.Fatalf("071 rework = %+v", r)
	}
	it := trackerItem(t, tr, "pr", 14991)
	if r := it.Rework; r == nil || r.State != "in_progress" || r.Evidence != "claim" || r.Mode != "spawn" || r.RequestedAt == nil || !r.RequestedAt.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("072 rework = %+v", r)
	}
	if r := trackerItem(t, tr, "pr", 14992).Rework; r != nil {
		t.Fatalf("050 row carries rework: %+v", r)
	}
	raw, _ := json.Marshal(it)
	for _, want := range []string{`"rework":{`, `"state":"in_progress"`, `"evidence":"claim"`, `"mode":"spawn"`, `"requested_at":"2026-10-28T14:00:00Z"`, `"since":"2026-10-28T15:56:00Z"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}
}

func TestStageTrackerReworkJS(t *testing.T) {
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
		jsConstLine(t, html, "STAGE_TRACKER_REWORK_WORDS"),
		jsFunc(t, html, "stageTrackerReworkOf"),
		jsFunc(t, html, "stageTrackerReworkWord"),
		jsFunc(t, html, "stageTrackerReworkSuffix"),
		jsFunc(t, html, "stageTrackerReworkTip"),
		jsFunc(t, html, "stageTrackerReworkJoinTip"),
		jsConstBlock(t, html, "STAGE_TRACKER_EVENT_MOD_WORDS"),
		jsFunc(t, html, "stageTrackerStateMap"),
		jsFunc(t, html, "stageTrackerStageFell"),
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_COVERED_BY_PR"),
		jsFunc(t, html, "stageTrackerEventText"),
		jsConstLine(t, html, "STAGE_TRACKER_REWORK_EVENT_WORDS"),
		jsFunc(t, html, "stageTrackerReworkEventText"),
		`
const NOW = Date.parse('2026-10-28T14:32:00Z');
Date.now = () => NOW;
const iso = ms => new Date(ms).toISOString();
const MIN = 60000, HOUR = 60 * MIN;
const base = { kind: 'pr', repo: 'hivecommons/hive', number: 14989, stage: 71, author: 'Danathar', author_kind: 'clanker',
  presence: { login: 'Danathar', state: 'online', kind: 'clanker', since: iso(NOW - 3 * HOUR), last_seen: iso(NOW - 2 * MIN), next: 'wait' } };
const w = (rework) => Object.assign({}, base, { rework });
const requested = w({ state: 'requested', actor: 'Danathar', requested_at: iso(NOW - HOUR), since: iso(NOW - HOUR) });
const queued = w({ state: 'requested', actor: 'Danathar', requested_at: iso(NOW - HOUR), since: iso(NOW - 10 * MIN), evidence: 'queued', ahead: 2 });
const acked = w({ state: 'acknowledged', actor: 'Danathar', requested_at: iso(NOW - HOUR), since: iso(NOW - 20 * MIN), evidence: 'activity' });
const working = w({ state: 'in_progress', actor: 'Danathar', requested_at: iso(NOW - HOUR), since: iso(NOW - 5 * MIN), evidence: 'claim', mode: 'spawn' });
const pushed = w({ state: 'pushed', actor: 'Danathar', requested_at: iso(NOW - HOUR), since: iso(NOW - 1 * MIN), evidence: 'commit', head: 'bbb' });

// Without the field the pill is exactly what it was (hivecommons/hive#15051 tests).
assert.strictEqual(stageTrackerPresenceChipText(base), 'waiting on @Danathar');
assert.strictEqual(stageTrackerReworkTip(base, NOW), '');
assert.strictEqual(stageTrackerReworkJoinTip('x', base, NOW), 'x');
// The state word rides on the pill text after the login; the title says since when.
assert.strictEqual(stageTrackerPresenceChipText(requested), 'waiting on @Danathar · no response');
assert.strictEqual(stageTrackerPresenceChipText(queued), 'waiting on @Danathar · queued');
assert.strictEqual(stageTrackerPresenceChipText(acked), 'waiting on @Danathar · acknowledged');
assert.strictEqual(stageTrackerPresenceChipText(working), 'waiting on @Danathar · working');
assert.strictEqual(stageTrackerPresenceChipText(pushed), 'waiting on @Danathar · re-review pending');
assert.match(stageTrackerReworkTip(requested, NOW), /^rework requested 1 hour ago \(\S+ .*\) · no response from @Danathar yet$/);
assert.match(stageTrackerReworkTip(queued, NOW), /^rework queued for @Danathar's clanker · 2 ahead · queued 10 min ago \(/);
assert.match(stageTrackerReworkTip(acked, NOW), /^@Danathar acknowledged · activity on the PR 20 min ago \(/);
assert.match(stageTrackerReworkTip(working, NOW), /^@Danathar's clanker picked up the rework 5 min ago \(.*\) \(spawn\)$/);
assert.match(stageTrackerReworkTip(pushed, NOW), /^@Danathar pushed a new head 1 min ago \(.*\) · re-review pending$/);
// The pill keeps its class, dot and login; title and aria carry the rework line after the presence line.
const chip = stageTrackerPresenceChipHtml(acked);
assert.ok(/^<span class="ham-badge ham-badge-waiting" title="Waiting on @Danathar&#39;s clanker \(online, last seen 2 min ago\) — hive is waiting for them · @Danathar acknowledged · activity on the PR 20 min ago[^"]*" aria-label="Waiting on @Danathar&#39;s clanker[^"]*acknowledged[^"]*">waiting on <span class="ham-presence ham-presence-online ham-presence-clanker"[^>]*>[\s\S]*<\/span>@Danathar · acknowledged<\/span>$/.test(chip), chip);
// Same on the 072 adoption pill.
const adopt = Object.assign({}, working, { stage: 72, adoption: { author: 'Danathar', due: false, nudges: 1, author_online: true, author_registered: true, policy: 'adopt', text: 'waiting on @Danathar (online, 1 nudge)' } });
const achip = stageTrackerAdoptionChipHtml(adopt);
assert.ok(/@Danathar · working<\/span>$/.test(achip), achip);
assert.ok(achip.includes("picked up the rework 5 min ago"), achip);
assert.strictEqual(stageTrackerAdoptionChipText(Object.assign({}, adopt, { adoption: Object.assign({}, adopt.adoption, { blocked_by: 'x' }) })), 'not adopted', 'a blocked adoption keeps its own word');
// Event line: the stage does not change, the pickup does.
const states = (items) => stageTrackerStateMap(items);
const before = states([requested]), after = states([acked]);
const key = Object.keys(after)[0];
assert.strictEqual(stageTrackerEventText(before[key], after[key], []), '@Danathar acknowledged the rework request');
assert.strictEqual(stageTrackerEventText(states([acked])[key], states([working])[key], []), "@Danathar's clanker picked up the rework");
assert.strictEqual(stageTrackerEventText(states([working])[key], states([pushed])[key], []), '@Danathar pushed a new head — re-review pending');
assert.strictEqual(stageTrackerEventText(states([acked])[key], states([acked])[key], []), '', 'no change, no line');
assert.strictEqual(stageTrackerEventText(states([base])[key], states([requested])[key], []), '', 'the request itself is the stage move, already said');
// Nested linked PRs carry it too.
const issue = { kind: 'issue', repo: 'hivecommons/hive', number: 7, stage: 72, linked_prs: [{ number: 14989, stage: 72, rework: acked.rework }] };
const lk = Object.keys(states([issue])).find(k => k.endsWith('#14989'));
assert.strictEqual(states([issue])[lk].rework, 'acknowledged');
`}, "\n")
	runStageTrackerRowActionsScript(t, script)
}
