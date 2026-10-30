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

// hivecommons/hive#15051: a row waiting on a person carries `presence` —
// whether their clanker is online / offline / unregistered and whether the
// hive's next move is to wait or to adopt (and when) — derived from the
// same lookup and the same clock the adoption engine uses.

func presenceOf(t *testing.T, s *Server, store *stagelabels.Store, status *StatusPayload, now time.Time, number int) *StageTrackerPresence {
	t.Helper()
	return trackerItem(t, s.buildStageTracker(status, store, now), "pr", number).Presence
}

// Online clanker at 072 after the nudge: green, next=wait, adopt_at at the
// online grace (2h after the first nudge), the same as adoptionDue.
func TestStageTrackerPresenceOnlineWaits(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	seen := now.Add(-2 * time.Minute)
	s, store, status := adoptionFixture(t, now, "", func(login string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{Registered: true, Online: true, LastSeen: seen}, true
	})
	p := presenceOf(t, s, store, status, now, 14524)
	if p == nil {
		t.Fatal("no presence on a 072 row")
	}
	if p.Login != "hanthor" || p.State != "online" || p.Kind != "clanker" || p.Next != "wait" {
		t.Fatalf("presence = %+v", p)
	}
	if p.LastSeen == nil || !p.LastSeen.Equal(seen) {
		t.Fatalf("last_seen = %v", p.LastSeen)
	}
	if p.Since == nil || !p.Since.Equal(now.Add(-26*time.Hour)) {
		t.Fatalf("since (first nudge) = %v", p.Since)
	}
	if p.AdoptAt == nil || !p.AdoptAt.Equal(now.Add(-26*time.Hour).Add(config.DefaultContributorGraceOnline)) {
		t.Fatalf("adopt_at = %v", p.AdoptAt)
	}
	raw, _ := json.Marshal(trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524))
	for _, want := range []string{`"presence":{`, `"login":"hanthor"`, `"state":"online"`, `"kind":"clanker"`, `"next":"wait"`, `"adopt_at":"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}
}

// Offline clanker: red square, next=adopt, adopt_at = last heartbeat (when
// later than the nudge) + after_offline — adoptionDue's offline rule.
func TestStageTrackerPresenceOfflineAdopts(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	seen := now.Add(-3 * time.Hour)
	s, store, status := adoptionFixture(t, now, "", offlinePresence(seen))
	s.deps.Config.Governor.Labels.Stage.Adoption.AfterOffline = "4h"
	p := presenceOf(t, s, store, status, now, 14524)
	if p == nil || p.State != "offline" || p.Kind != "clanker" || p.Next != "adopt" {
		t.Fatalf("presence = %+v", p)
	}
	if p.AdoptAt == nil || !p.AdoptAt.Equal(seen.Add(4*time.Hour)) {
		t.Fatalf("adopt_at = %v, want last_seen+4h", p.AdoptAt)
	}
	// A heartbeat older than the first nudge: the clock counts from the nudge.
	s.presenceLookup = offlinePresence(now.Add(-40 * time.Hour))
	if p = presenceOf(t, s, store, status, now, 14524); p.AdoptAt == nil || !p.AdoptAt.Equal(now.Add(-26*time.Hour).Add(4*time.Hour)) {
		t.Fatalf("adopt_at = %v, want nudge+4h", p.AdoptAt)
	}
	// The adoption text on the same row agrees: offline, no hold.
	if a := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption; a == nil || a.AuthorOnline || a.BlockedBy != "" {
		t.Fatalf("adoption disagrees with presence: %+v", a)
	}
}

// A clanker PR whose author has no profile here: unknown (grey hollow),
// kind clanker, adoption after after_unregistered from the first nudge.
func TestStageTrackerPresenceUnregisteredClanker(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, "", func(string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{}, true
	})
	p := presenceOf(t, s, store, status, now, 14524)
	if p == nil || p.State != "unknown" || p.Kind != "clanker" || p.Next != "adopt" || p.LastSeen != nil {
		t.Fatalf("presence = %+v", p)
	}
	if p.AdoptAt == nil || !p.AdoptAt.Equal(now.Add(-26*time.Hour).Add(config.DefaultAdoptAfterUnregistered)) {
		t.Fatalf("adopt_at = %v", p.AdoptAt)
	}
	// No lookup at all (no contribute hub): unknown too, and still a clanker.
	s.presenceLookup = nil
	if p = presenceOf(t, s, store, status, now, 14524); p == nil || p.State != "unknown" || p.Kind != "clanker" {
		t.Fatalf("presence without a hub = %+v", p)
	}
}

// Held adoption (maintainer_policy ask / never): the engine will not adopt
// on its own, so next stays wait and no adopt_at is projected — the pill's
// "adopt?" text and the dot never disagree.
func TestStageTrackerPresenceHeldAdoptionWaits(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyAsk, offlinePresence(now.Add(-26*time.Hour)))
	store.NoteAdoptionBlocked(stagelabels.Key("hivecommons/hive", 14524), "feed", stagelabels.AdoptBlockedAuthorMaintainer, now.Add(-25*time.Hour))
	p := presenceOf(t, s, store, status, now, 14524)
	if p == nil || p.State != "offline" || p.Next != "wait" || p.AdoptAt != nil {
		t.Fatalf("presence = %+v", p)
	}
	// Adoption switched off: same.
	off := false
	s2, store2, status2 := adoptionFixture(t, now, "", offlinePresence(now.Add(-26*time.Hour)))
	s2.deps.Config.Governor.Labels.Stage.Adoption.Enabled = &off
	if p = presenceOf(t, s2, store2, status2, now, 14524); p == nil || p.Next != "wait" || p.AdoptAt != nil {
		t.Fatalf("presence with adoption off = %+v", p)
	}
}

// A human's PR is never adopted: kind human, state unknown (no clanker),
// next=wait, no adopt_at — on 071 as well as 072, where there is no
// adoption payload at all.
func TestStageTrackerPresenceHumanAuthor(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.presenceLookup = func(string) (automerge.ContributorPresence, bool) { return automerge.ContributorPresence{}, true }
	s.deps = &Dependencies{Config: &config.Config{}}
	clock = now.Add(-3 * time.Hour)
	store.Observe(stagelabels.Key("hivecommons/hive", 14990), stagelabels.NeedsAuthor, "changes-requested")
	store.Observe(stagelabels.Key("hivecommons/hive", 14991), stagelabels.ReturnedToContributor, "check-failing:ci")
	clock = now
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 14990, Title: "Docs", Author: "zoe", HeadSHA: "aaa", Labels: []string{"stage/071-needs-author", "author/human"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14991, Title: "Fix", Author: "zoe", HeadSHA: "bbb", Labels: []string{"stage/072-returned-to-contributor", "author/human"}}},
		},
	}}
	s.UpdateStatus(status)
	for _, n := range []int{14990, 14991} {
		it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", n)
		p := it.Presence
		if p == nil || p.Login != "zoe" || p.State != "unknown" || p.Kind != "human" || p.Next != "wait" || p.AdoptAt != nil {
			t.Fatalf("#%d presence = %+v", n, p)
		}
		if p.Since == nil || !p.Since.Equal(now.Add(-3*time.Hour)) {
			t.Fatalf("#%d since (stage entry) = %v", n, p.Since)
		}
		if it.Adoption != nil {
			t.Fatalf("#%d human PR carries adoption: %+v", n, it.Adoption)
		}
	}
	// A human who does run a registered clanker reads as a clanker.
	s.presenceLookup = func(string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{Registered: true, Online: true, LastSeen: now}, true
	}
	if p := presenceOf(t, s, store, status, now, 14990); p == nil || p.State != "online" || p.Kind != "clanker" || p.Next != "wait" || p.AdoptAt != nil {
		t.Fatalf("registered human presence = %+v", p)
	}
}

// Rows that wait on the hive or a gate carry no presence; an issue never
// does, but its sub-row mirrors the nested PR's.
func TestStageTrackerPresenceOnlyWhereAPersonIsWaitedOn(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.presenceLookup = func(string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{Registered: true, Online: true, LastSeen: now}, true
	}
	s.deps = &Dependencies{Config: &config.Config{}}
	store.Observe(stagelabels.Key("hivecommons/hive", 14989), stagelabels.ReviewPending, "review gate: no review bundle")
	store.Observe(stagelabels.Key("hivecommons/hive", 14992), stagelabels.NeedsAuthor, "changes-requested")
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{github.Issue{Number: 14986, Title: "Issue", Author: "clubanderson", AuthorIsHuman: true, Labels: []string{"stage/040-covered-by-pr"}}},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 14989, Title: "Review", Author: "Danathar", HeadSHA: "ccc", Labels: []string{"stage/050-review-pending", "author/clanker"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14992, Title: "Fix 14986", Author: "Danathar", HeadSHA: "ddd", Labels: []string{"stage/071-needs-author", "author/clanker"}, LinkedIssues: []github.PRLinkedIssue{{Number: 14986}}}},
		},
	}}
	s.UpdateStatus(status)
	tr := s.buildStageTracker(status, store, now)
	if p := trackerItem(t, tr, "pr", 14989).Presence; p != nil {
		t.Fatalf("review-pending row waits on the swarm, not a person: %+v", p)
	}
	pr := trackerItem(t, tr, "pr", 14992)
	if pr.Presence == nil || pr.Presence.Login != "Danathar" || pr.Presence.State != "online" {
		t.Fatalf("071 presence = %+v", pr.Presence)
	}
	issue := trackerItem(t, tr, "issue", 14986)
	if issue.Presence != nil {
		t.Fatalf("issue row carries presence: %+v", issue.Presence)
	}
	var mirrored *StageTrackerPresence
	for _, lp := range issue.LinkedPRs {
		if lp.Number == 14992 {
			mirrored = lp.Presence
		}
	}
	if mirrored == nil || mirrored.Login != "Danathar" || mirrored.State != "online" {
		t.Fatalf("sub-row presence = %+v (linked %+v)", mirrored, issue.LinkedPRs)
	}
}

// stageTrackerPresenceJS lifts the SPA's presence helpers (and the clock
// words they lean on) out of index.html for the node-fed scripts.
func stageTrackerPresenceJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsConstLine(t, html, "STAGE_TRACKER_PRESENCE_STATES"),
		jsConstLine(t, html, "STAGE_TRACKER_PRESENCE_WORDS"),
		jsFunc(t, html, "stageTrackerPresenceOf"),
		jsFunc(t, html, "stageTrackerPresenceGone"),
		jsFunc(t, html, "stageTrackerPresenceAdoptAt"),
		jsFunc(t, html, "stageTrackerPresenceTip"),
		jsFunc(t, html, "stageTrackerPresenceAria"),
		jsFunc(t, html, "stageTrackerPresenceDotHtml"),
		jsFunc(t, html, "stageTrackerPresenceInPillHtml"),
		jsFunc(t, html, "stageTrackerPresenceChipText"),
		jsFunc(t, html, "stageTrackerPresenceChipHtml"),
	}, "\n")
}

// The SPA, fed the live payload shape: the indicator's kind glyph (person /
// robot head), its state class and aria-label, the plain-words tooltip, its place inside the
// adoption pill, the muted waiting pill where there is no adoption line,
// and the popover's presence line. Node runs the lifted helpers on stdin.
func TestStageTrackerPresenceJS(t *testing.T) {
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
		`
const NOW = Date.parse('2026-10-28T14:32:00Z');
Date.now = () => NOW;
const iso = ms => new Date(ms).toISOString();
const MIN = 60000, HOUR = 60 * MIN;
const online = { kind: 'pr', repo: 'hivecommons/hive', number: 14989, stage: 71, author: 'Danathar', author_kind: 'clanker',
  presence: { login: 'Danathar', state: 'online', kind: 'clanker', since: iso(NOW - 3 * HOUR), last_seen: iso(NOW - 2 * MIN), next: 'wait' } };
const offline = { kind: 'pr', repo: 'hivecommons/hive', number: 14524, stage: 72, author: 'hanthor', author_kind: 'clanker',
  adoption: { author: 'hanthor', due: false, nudges: 2, author_online: false, author_registered: true, policy: 'adopt', text: 'waiting on @hanthor (offline 26h, 2 nudges)' },
  presence: { login: 'hanthor', state: 'offline', kind: 'clanker', since: iso(NOW - 26 * HOUR), last_seen: iso(NOW - 26 * HOUR), next: 'adopt', adopt_at: iso(NOW + 38 * MIN) } };
const human = { kind: 'pr', repo: 'hivecommons/hive', number: 15000, stage: 71, author: 'zoe', author_kind: 'human',
  presence: { login: 'zoe', state: 'unknown', kind: 'human', since: iso(NOW - 40 * MIN), next: 'wait' } };
const ask = { kind: 'pr', repo: 'hivecommons/hive', number: 14524, stage: 72, author: 'hanthor', author_kind: 'clanker',
  adoption: { author: 'hanthor', due: true, blocked_by: 'author-is-maintainer', nudges: 2, author_online: false, author_maintainer: true, policy: 'ask', action: 'decide:adopt', text: 'waiting on @hanthor (maintainer, offline 26h) — adopt?' },
  decision_needed: { question: '@hanthor maintains this repository and has been offline for 26h; adopt the PR?', asked_by: 'the hive', since: iso(NOW - 26 * HOUR), blocker: 'maintainer_policy is ask', options: [{ action: 'decide:adopt', label: 'Adopt (hive takes over)', enabled: true }] },
  presence: { login: 'hanthor', state: 'offline', kind: 'clanker', since: iso(NOW - 26 * HOUR), last_seen: iso(NOW - 26 * HOUR), next: 'wait' } };

assert.strictEqual(stageTrackerPresenceTip(online, NOW), "Waiting on @Danathar's clanker (online, last seen 2 min ago) — hive is waiting for them");
assert.match(stageTrackerPresenceTip(offline, NOW), /^Waiting on @hanthor's clanker \(offline 26h\) — hive will adopt at \S+ .*\(in 38m\)$/);
assert.strictEqual(stageTrackerPresenceTip(human, NOW), 'Waiting on @zoe (human) — waiting for a human reply');
assert.strictEqual(stageTrackerPresenceTip(ask, NOW), "Waiting on @hanthor's clanker (offline 26h) — hive is waiting for them", 'held adoption promises no adopt-at');

// Kind glyph + state class + aria per state; role=img so it reads out.
for (const [item, state, kind, word] of [[online, 'online', 'clanker', 'clanker online'], [offline, 'offline', 'clanker', 'clanker offline'], [human, 'unknown', 'human', 'human']]) {
  const dot = stageTrackerPresenceDotHtml(item, NOW);
  assert.ok(dot.includes('class="ham-presence ham-presence-' + state + ' ham-presence-' + kind + '"'), dot);
  assert.ok(dot.includes('role="img"'), dot);
  assert.ok(dot.includes('aria-label="@' + item.presence.login + ': ' + word + ' — '), dot);
  assert.ok(dot.includes('<svg class="ham-ico ham-presence-ico"') && dot.includes(STAGE_TRACKER_ICONS[kind]), 'draws the ' + kind + ' glyph: ' + dot);
  assert.strictEqual(dot.includes('ham-presence-dot'), kind === 'clanker', 'only a clanker carries the state dot: ' + dot);
}
assert.ok(!stageTrackerPresenceDotHtml(human, NOW).includes(STAGE_TRACKER_ICONS.clanker), 'a human is never drawn as a robot');
// Inside the adoption pill, before the @login; the presence line joins the title and aria-label.
const chip = stageTrackerAdoptionChipHtml(offline);
assert.ok(/>waiting on <span class="ham-presence ham-presence-offline ham-presence-clanker"[^>]*><svg [^>]*><\/svg><i class="ham-presence-dot"><\/i><\/span>@hanthor<\/span>$/.test(chip.replace(/<svg ([^>]*)>[\s\S]*?<\/svg>/, '<svg $1></svg>')), chip);
assert.ok(chip.includes("title=\"waiting on @hanthor (offline 26h, 2 nudges) · Waiting on @hanthor&#39;s clanker (offline 26h)"), chip);
assert.ok(chip.includes("aria-label=\"Adoption — waiting on @hanthor (offline 26h, 2 nudges) · Waiting on @hanthor&#39;s clanker ("), chip);
assert.strictEqual(stageTrackerPresenceChipHtml(offline), '', 'one pill, not two');
// The "adopt?" trigger carries the dot in front of its word.
const trig = stageTrackerAdoptionChipHtml(ask);
assert.ok(/<button [^>]*ham-decision-trigger[^>]*><span class="ham-presence ham-presence-offline ham-presence-clanker"[^>]*>[\s\S]*<\/span>adopt\?<\/button>$/.test(trig), trig);
// No adoption line (071, a human): the muted waiting pill carries it.
const w = stageTrackerPresenceChipHtml(online);
assert.ok(/^<span class="ham-badge ham-badge-waiting" title="Waiting on @Danathar&#39;s clanker \(online, last seen 2 min ago\) — hive is waiting for them" aria-label="Waiting on @Danathar&#39;s clanker \([^"]*">waiting on <span class="ham-presence ham-presence-online ham-presence-clanker"[^>]*>[\s\S]*<i class="ham-presence-dot"><\/i><\/span>@Danathar<\/span>$/.test(w), w);
// A human: the person glyph and the handle, nothing else — no dot, no "(human · no clanker)" qualifier.
const hw = stageTrackerPresenceChipHtml(human);
assert.ok(/^<span class="ham-badge ham-badge-waiting" title="Waiting on @zoe \(human\) — waiting for a human reply" aria-label="Waiting on @zoe \(human\) — waiting for a human reply">waiting on <span class="ham-presence ham-presence-unknown ham-presence-human"[^>]*><svg [^>]*>[\s\S]*<\/svg><\/span>@zoe<\/span>$/.test(hw), hw);
assert.ok(!hw.includes('no clanker') && !hw.includes('ham-presence-dot'), hw);
assert.strictEqual(stageTrackerPresenceChipHtml({ kind: 'pr', stage: 71 }), '');
// The Needs-human popover names the presence under the question.
const pop = stageTrackerDecisionPopHtml(ask, [], NOW, '');
assert.ok(/<p class="ham-decision-presence"><span class="ham-presence ham-presence-offline ham-presence-clanker"[^>]*>[\s\S]*?<\/span>Waiting on @hanthor&#39;s clanker \(offline 26h\) — hive is waiting for them<\/p>/.test(pop), pop);
assert.ok(!stageTrackerDecisionPopHtml(Object.assign({}, ask, { presence: null }), [], NOW, '').includes('ham-decision-presence'));
// Sub-rows and the popover's nested-PR lookup carry the field.
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
	for _, want := range []string{
		"presence: lp.presence || null",
		"'Presence: ' + presence",
		".ham-presence-online {",
		"@media (prefers-reduced-motion: reduce) { .ham-presence-online .ham-presence-dot { animation: none; } }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	// The single-cover Object.assign and the shared stageTrackerLinkedPRItem
	// mapper (sub-row, item-by-key and decision lookups) both carry presence.
	if n := strings.Count(html, "presence: lp.presence || null"); n < 2 {
		t.Errorf("linked-PR mappings carrying presence = %d, want the single-cover mapping and stageTrackerLinkedPRItem", n)
	}
	i := strings.Index(html, "function stageTrackerLinkedPRItem(")
	if i < 0 || !strings.Contains(html[i:i+2000], "presence: lp.presence || null") {
		t.Errorf("stageTrackerLinkedPRItem does not carry presence")
	}
}
