package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An offline clanker's PRs sat at "PR in progress · @odttlnt's clanker"
// for hours while the engine held them at 085: the write-back gate kept the
// stage label off the fork PR, and the tracker read the label-less item as
// a lifted park and dropped the record. The record now says the labels
// were suppressed and the row keeps its stage; an unclassified clanker PR
// carries its author's presence and a caption naming the next automatic
// step.

func offlineClankerFixture(t *testing.T, now time.Time, lastSeen time.Time) (*Server, *stagelabels.Store, *StatusPayload) {
	t.Helper()
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	s.presenceLookup = func(login string) (automerge.ContributorPresence, bool) {
		if login != "odttlnt" {
			return automerge.ContributorPresence{}, false
		}
		return automerge.ContributorPresence{Registered: true, Online: false, LastSeen: lastSeen}, true
	}
	s.deps = &Dependencies{Config: &config.Config{}}
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 15312, Title: "Decision asked", Author: "odttlnt", HeadSHA: "036b", Labels: []string{"dco-signoff: yes", "size/M", "author/clanker"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 15343, Title: "Unclassified", Author: "odttlnt", HeadSHA: "d15e", Labels: []string{"dco-signoff: yes", "author/clanker"}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 15400, Title: "Human's PR", Author: "zoe", HeadSHA: "aaaa", Labels: []string{"author/human"}}},
		},
	}}
	s.UpdateStatus(status)
	return s, store, status
}

// A 085 record whose labels the gate suppressed is the row's stage.
func TestStageTrackerSuppressedDecisionRecordKeepsStage(t *testing.T) {
	now := time.Date(2026, 10, 29, 20, 30, 0, 0, time.UTC)
	s, store, status := offlineClankerFixture(t, now, now.Add(-3*time.Hour))
	key := stagelabels.Key("hivecommons/hive", 15312)
	store.Observe(key, stagelabels.NeedsDecision, "contributor-lane:decision-requested")
	store.NoteLabelsSuppressed(key, true)
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15312)
	if it.Stage != stagelabels.NeedsDecision.Number() || it.StageSince == nil {
		t.Fatalf("suppressed 085 record: stage %d since %v caption %q, want 085 from the record", it.Stage, it.StageSince, it.Caption)
	}
	if it.Presence != nil {
		t.Fatalf("a row waiting on a person's decision carries no author presence: %+v", it.Presence)
	}
	// The same record with the label write landed and then removed is a
	// lifted park, as before.
	store.NoteLabelsSuppressed(key, false)
	if it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15312); it.Stage == stagelabels.NeedsDecision.Number() {
		t.Fatalf("label-less 085 record with labels not suppressed still parks the row: %+v", it)
	}
}

// An unclassified clanker PR carries its author's presence and the
// caption says what the offline clanker means for it; a human's does not.
func TestStageTrackerUnclassifiedClankerPROfflineCaption(t *testing.T) {
	now := time.Date(2026, 10, 29, 20, 30, 0, 0, time.UTC)
	s, store, status := offlineClankerFixture(t, now, now.Add(-3*time.Hour))
	on, off := true, false
	s.deps.Config.Governor.Labels.Stage.Adoption.Enabled = &on
	tr := s.buildStageTracker(status, store, now)
	it := trackerItem(t, tr, "pr", 15343)
	if it.Stage != 0 {
		t.Fatalf("stage = %d, want unclassified", it.Stage)
	}
	p := it.Presence
	if p == nil || p.Login != "odttlnt" || p.State != presenceStateOffline || p.Kind != presenceKindClanker || p.Next != presenceNextWait || p.AdoptAt != nil {
		t.Fatalf("presence = %+v, want offline clanker, next wait, no adopt-at (the clock runs at 072 only)", p)
	}
	if p.LastSeen == nil || !p.LastSeen.Equal(now.Add(-3*time.Hour)) {
		t.Fatalf("last_seen = %v", p.LastSeen)
	}
	for _, want := range []string{"waiting for the stage-label pass", "@odttlnt's clanker is offline 3h", "reviewed and merged without its author", "hive adopts it only once it is returned", "nothing is pushed for 15m"} {
		if !strings.Contains(it.Caption, want) {
			t.Errorf("caption lacks %q: %s", want, it.Caption)
		}
	}
	if h := trackerItem(t, tr, "pr", 15400); h.Presence != nil || strings.Contains(h.Caption, "clanker") {
		t.Fatalf("human's unclassified PR = presence %+v caption %q, want neither", h.Presence, h.Caption)
	}
	// Adoption off: no promise about adopting.
	s.deps.Config.Governor.Labels.Stage.Adoption.Enabled = &off
	if c := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15343).Caption; strings.Contains(c, "adopts") || !strings.Contains(c, "offline 3h") {
		t.Fatalf("adoption off caption = %q", c)
	}
}

// The chevron's holder gains the clanker's offline state and the title
// carries the next step (node, lifted from index.html).
func TestStageTrackerHolderPresenceJS(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		stageTrackerRowActionsPreamble(t, html),
		stageTrackerPresenceJS(t, html),
		jsFunc(t, html, "stageTrackerHolderPresenceHtml"),
		`
const NOW = Date.parse('2026-10-30T00:30:00Z');
Date.now = () => NOW;
const iso = ms => new Date(ms).toISOString();
const HOUR = 3600000;
const claimant = { kind: 'clanker', login: 'odttlnt', source: 'author' };
const offline = { kind: 'pr', repo: 'hivecommons/hive', number: 15343, stage: 0, author: 'odttlnt', author_kind: 'clanker', claimant,
  presence: { login: 'odttlnt', state: 'offline', kind: 'clanker', since: iso(NOW - 7 * HOUR), last_seen: iso(NOW - 3 * HOUR), next: 'wait' } };
const h = stageTrackerHolderPresenceHtml(offline, claimant, NOW);
assert.ok(/^<span class="ham-holder-presence ham-presence ham-presence-offline ham-presence-clanker" title="Waiting on @odttlnt&#39;s clanker \(offline 3h\) — hive is waiting for them" aria-label="[^"]*" data-ham-presence="offline"> · <i class="ham-presence-dot" aria-hidden="true"><\/i>offline 3h<\/span>$/.test(h), h);
// Online: nothing to flag. Another holder: not theirs. Human: no presence. Unregistered: said so.
assert.strictEqual(stageTrackerHolderPresenceHtml(Object.assign({}, offline, { presence: Object.assign({}, offline.presence, { state: 'online' }) }), claimant, NOW), '');
assert.strictEqual(stageTrackerHolderPresenceHtml(offline, { kind: 'clanker', login: 'karibhou' }, NOW), '');
assert.strictEqual(stageTrackerHolderPresenceHtml(offline, { kind: 'person', login: 'odttlnt' }, NOW), '');
assert.ok(stageTrackerHolderPresenceHtml(Object.assign({}, offline, { presence: Object.assign({}, offline.presence, { state: 'unknown' }) }), claimant, NOW).includes('>not registered</span>'));
assert.strictEqual(stageTrackerHolderPresenceHtml({ kind: 'pr', stage: 0 }, claimant, NOW), '');
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
	for _, want := range []string{
		"stageTrackerHolderPresenceHtml(item, claimant, nowMs)",
		".ham-holder-presence {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
}
