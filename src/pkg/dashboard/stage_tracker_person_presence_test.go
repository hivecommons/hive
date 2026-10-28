package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerPersonPresence(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, _ := adoptionFixture(t, now, "adopt", nil)
	rec, _ := store.Get(stagelabels.Key("hivecommons/hive", 14524))
	start := rec.NudgeStart()
	for _, tc := range []struct {
		name, kind, state, next string
		p                       automerge.ContributorPresence
		available               bool
		stage                   int
		deadline                time.Time
	}{
		{"online", "clanker", "online", "adopt", automerge.ContributorPresence{Registered: true, Online: true, LastSeen: now}, true, 72, start.Add(2 * time.Hour)},
		{"offline", "clanker", "offline", "adopt", automerge.ContributorPresence{Registered: true, LastSeen: now.Add(-5 * time.Minute)}, true, 72, now.Add(10 * time.Minute)},
		{"unregistered", "clanker", "unknown", "adopt", automerge.ContributorPresence{}, true, 72, start.Add(15 * time.Minute)},
		{"unavailable", "clanker", "unknown", "adopt", automerge.ContributorPresence{}, false, 72, start.Add(15 * time.Minute)},
		{"human", "human", "unknown", "wait", automerge.ContributorPresence{}, true, 72, time.Time{}},
		{"needs-author", "clanker", "online", "wait", automerge.ContributorPresence{Registered: true, Online: true}, true, 71, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := StageTrackerItem{Kind: "pr", Author: "hanthor", AuthorKind: tc.kind, Stage: tc.stage, Head: "feed"}
			lookup := func(string) (automerge.ContributorPresence, bool) { return tc.p, tc.available }
			got := s.stageTrackerPersonPresence(it, rec, true, lookup)
			if got == nil || got.Login != "hanthor" || got.Kind != tc.kind || got.State != tc.state || got.Next != tc.next || got.Available != tc.available {
				t.Fatalf("presence = %+v", got)
			}
			if tc.deadline.IsZero() {
				if got.AdoptAt != nil {
					t.Fatalf("unexpected deadline: %v", got.AdoptAt)
				}
			} else if got.AdoptAt == nil || !got.AdoptAt.Equal(tc.deadline) {
				t.Fatalf("deadline = %v, want %v", got.AdoptAt, tc.deadline)
			}
			// Before a nudge or after the head moves there must be no promise of adoption.
			it.Head = "new-head"
			if p := s.stageTrackerPersonPresence(it, rec, true, lookup); p.AdoptAt != nil || p.Next != "wait" {
				t.Fatalf("stale-head presence = %+v", p)
			}
			if p := s.stageTrackerPersonPresence(it, stagelabels.Record{}, false, lookup); p == nil || p.AdoptAt != nil {
				t.Fatalf("pre-nudge presence = %+v", p)
			}
		})
	}
	it := StageTrackerItem{Kind: "pr", Author: "hanthor", AuthorKind: "clanker", Stage: 72, Head: "feed"}
	off := false
	s.deps.Config.Governor.Labels.Stage.Adoption.Enabled = &off
	if got := s.stageTrackerPersonPresence(it, rec, true, nil); got.Next != "wait" || got.AdoptAt != nil {
		t.Fatalf("disabled: %+v", got)
	}
	s.deps.Config.Governor.Labels.Stage.Adoption.Enabled = nil
	it.Labels = []string{"hold"}
	if got := s.stageTrackerPersonPresence(it, rec, true, nil); got.Next != "wait" {
		t.Fatalf("held: %+v", got)
	}
	it.Labels = nil
	s.deps.Config.Governor.Labels.Stage.Adoption.AfterOffline = "45m"
	got := s.stageTrackerPersonPresence(it, rec, true, offlinePresence(now))
	if got.AdoptAt == nil || !got.AdoptAt.Equal(now.Add(45*time.Minute)) {
		t.Fatalf("configured deadline: %+v", got)
	}
	store.NoteAdoptionBlocked(stagelabels.Key("hivecommons/hive", 14524), "feed", stagelabels.AdoptBlockedAuthorMaintainer, now)
	blocked, _ := store.Get(stagelabels.Key("hivecommons/hive", 14524))
	if got := s.stageTrackerPersonPresence(it, blocked, true, offlinePresence(now)); got.Next != "wait" || got.AdoptAt != nil {
		t.Fatalf("blocked adoption: %+v", got)
	}
	it.Kind = "issue"
	if got := s.stageTrackerPersonPresence(it, rec, true, nil); got != nil {
		t.Fatalf("issue presence = %+v", got)
	}
}

func TestStageTrackerPresenceSharesSnapshot(t *testing.T) {
	calls := 0
	lookup := cachedStageTrackerPresence(func(string) (automerge.ContributorPresence, bool) {
		calls++
		return automerge.ContributorPresence{Registered: true, Online: calls == 1}, true
	})
	first, _ := lookup("hanthor")
	second, _ := lookup("hanthor")
	if calls != 1 || first != second {
		t.Fatalf("inconsistent snapshot: %v %v, calls=%d", first, second, calls)
	}
}

func TestStageTrackerPersonPresencePayload(t *testing.T) {
	now := time.Now()
	s, store, status := adoptionFixture(t, now, "adopt", offlinePresence(now))
	status.Repos[0].ActionableIssues = []any{github.Issue{Number: 15051, Title: "Covered", Author: "zoe", AuthorIsHuman: true, LinkedPRs: []github.IssueLinkedPR{{Number: 14524, State: "open"}}}}
	response := s.buildStageTracker(status, store, now)
	it := trackerItem(t, response, "pr", 14524)
	if it.Presence == nil || it.Presence.State != "offline" || it.Presence.AdoptAt == nil {
		t.Fatalf("missing presence: %+v", it)
	}
	issue := trackerItem(t, response, "issue", 15051)
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Presence != it.Presence {
		t.Fatalf("sub-row lost presence: %+v", issue.LinkedPRs)
	}
}

func TestStageTrackerPersonPresenceJS(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		stageTrackerRowActionsPreamble(t, html),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerPresenceTip"),
		jsFunc(t, html, "stageTrackerPresenceHtml"),
		jsFunc(t, html, "stageTrackerAdoptionTip"),
		jsFunc(t, html, "stageTrackerAdoptOffered"),
		jsFunc(t, html, "stageTrackerAdoptionChipText"),
		jsFunc(t, html, "stageTrackerAdoptionChipHtml"),
		`
const now = Date.parse('2026-10-28T16:00:00Z');
const row = {kind:'pr', presence:{login:'zoe', kind:'clanker', state:'online', available:true, next:'wait', last_seen:'2026-10-28T15:58:00Z'}};
assert.ok(stageTrackerPresenceTip(row, now).includes("@zoe's clanker: online, last seen 2m ago"));
assert.ok(stageTrackerPresenceHtml(row, now).includes('ham-presence-online'));
assert.ok(stageTrackerPresenceHtml(row, now).includes('aria-label='));
assert.ok(stageTrackerAdoptionChipHtml(row).includes('waiting on @zoe'));
row.presence.state = 'offline';
row.presence.since = '2026-10-27T14:00:00Z';
row.presence.next = 'adopt';
row.presence.adopt_at = '2026-10-28T16:38:00Z';
assert.ok(stageTrackerPresenceHtml(row, now).includes('ham-presence-offline'));
assert.ok(stageTrackerPresenceTip(row, now).includes('offline 26h'));
assert.ok(stageTrackerPresenceTip(row, now).includes('hive will adopt at'));
assert.ok(stageTrackerPresenceTip(row, now).includes('(in 38m)'));
assert.ok(stageTrackerPresenceTip(row, now + 3600000).includes('adoption due'));
row.presence = {login:'zoe', kind:'human', state:'unknown', available:true, next:'wait'};
assert.ok(stageTrackerPresenceTip(row, now).includes('no clanker registered — waiting for a human reply'));
assert.ok(stageTrackerPresenceHtml(row, now).includes('ham-presence-unknown'));
assert.ok(stageTrackerAdoptionChipHtml(row).includes('human · no clanker'));
row.presence.available = false;
assert.ok(stageTrackerPresenceTip(row, now).includes('presence unavailable'));
assert.ok(!stageTrackerPresenceTip(row, now).includes('no clanker registered'));
assert.strictEqual(stageTrackerPresenceHtml({}, now), '');
row.presence.login = '<unsafe>';
assert.ok(!stageTrackerPresenceHtml(row, now).includes('<unsafe>'));
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
	for _, want := range []string{"presence: lp.presence", "stageTrackerPresenceHtml(item, nowMs)", ".ham-presence-online", ".ham-presence-offline", ".ham-presence-unknown", "prefers-reduced-motion: reduce"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}
