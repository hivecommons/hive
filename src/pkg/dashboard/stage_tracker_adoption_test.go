package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14524: a maintainer's clanker PR sat at 072 for 26h,
// nudged twice, its hive offline, while the row read a bare "Returned to
// contributor". The tracker now carries `adoption{}` and says why in plain
// words; under maintainer_policy: ask the row offers Adopt.

func adoptionFixture(t *testing.T, now time.Time, policy string, presence func(string) (automerge.ContributorPresence, bool)) (*Server, *stagelabels.Store, *StatusPayload) {
	t.Helper()
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.presenceLookup = presence
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Governor.Labels.Stage.Adoption.MaintainerPolicy = policy
	key := stagelabels.Key("hivecommons/hive", 14524)
	clock = now.Add(-26 * time.Hour)
	store.Observe(key, stagelabels.ReturnedToContributor, "check-failing:ratchet")
	store.MarkNudged(key, "feed", 1, false, clock)
	clock = now.Add(-20 * time.Hour)
	store.MarkNudged(key, "feed", 2, false, clock)
	clock = now
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 14524, Title: "Big change", Author: "hanthor", HeadSHA: "feed", Labels: []string{"stage/072-returned-to-contributor", "author/clanker", "role/contributor"}}}},
	}}
	s.UpdateStatus(status)
	return s, store, status
}

func offlinePresence(lastSeen time.Time) func(string) (automerge.ContributorPresence, bool) {
	return func(login string) (automerge.ContributorPresence, bool) {
		if login != "hanthor" {
			return automerge.ContributorPresence{}, false
		}
		return automerge.ContributorPresence{Registered: true, Online: false, LastSeen: lastSeen}, true
	}
}

func TestStageTrackerAdoptionAskOffersAdopt(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyAsk, offlinePresence(now.Add(-26*time.Hour)))
	store.NoteAdoptionBlocked(stagelabels.Key("hivecommons/hive", 14524), "feed", stagelabels.AdoptBlockedAuthorMaintainer, now.Add(-25*time.Hour))
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	a := it.Adoption
	if a == nil {
		t.Fatalf("no adoption payload: %+v", it)
	}
	if !a.Due || a.BlockedBy != "author-is-maintainer" || a.Nudges != 2 || a.AuthorOnline || !a.AuthorRegistered || !a.AuthorMaintainer || a.Policy != "ask" || a.Action != "decide:adopt" {
		t.Fatalf("adoption = %+v", a)
	}
	if a.Since == nil || !a.Since.Equal(now.Add(-26*time.Hour)) {
		t.Fatalf("since = %v", a.Since)
	}
	if a.Text != "waiting on @hanthor (maintainer, offline 26h) — adopt?" {
		t.Fatalf("text = %q", a.Text)
	}
	if it.Caption != "Returned to @hanthor — a required check is failing (ratchet) — waiting on @hanthor (maintainer, offline 26h) — adopt?" && !strings.HasSuffix(it.Caption, " — waiting on @hanthor (maintainer, offline 26h) — adopt?") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if strings.Contains(it.Caption, "must act") {
		t.Fatalf("caption keeps the bare clause: %q", it.Caption)
	}
	// The ask hold is a decision a person owes: decision_needed carries the
	// question and leads with Adopt under the same action id (#14995).
	d := it.DecisionNeeded
	if d == nil || len(d.Options) == 0 || d.Options[0].Action != "decide:adopt" || !d.Options[0].Enabled {
		t.Fatalf("decision_needed = %+v", d)
	}
	if !strings.HasPrefix(d.Question, "@hanthor maintains this repository and has been offline for 26h; adopt the PR?") || d.BlockerReason != "adoption:author-is-maintainer" || d.Since == nil || !d.Since.Equal(now.Add(-26*time.Hour)) {
		t.Fatalf("decision question = %q reason=%q since=%v", d.Question, d.BlockerReason, d.Since)
	}
	// The wire shape the SPA reads.
	raw, _ := json.Marshal(it)
	for _, want := range []string{`"adoption":{`, `"due":true`, `"blocked_by":"author-is-maintainer"`, `"nudges":2`, `"author_online":false`, `"action":"decide:adopt"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}
}

func TestStageTrackerAdoptionNeverSaysSo(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(time.Time{}))
	store.NoteAdoptionBlocked(stagelabels.Key("hivecommons/hive", 14524), "feed", stagelabels.AdoptBlockedAuthorMaintainer, now.Add(-25*time.Hour))
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524)
	if a := it.Adoption; a == nil || a.Action != "" || a.Text != "waiting on @hanthor (maintainer, offline 26h) — not adopted: maintainer_policy is never" {
		t.Fatalf("adoption = %+v", a)
	}
	if it.DecisionNeeded != nil {
		t.Fatalf("never is not a person's decision: %+v", it.DecisionNeeded)
	}
}

// hivecommons/hive#15692: the engine only reads the author's repository
// permission once adoption is due (automerge_contributor_adopt.go), so a
// never-policy row that has not reached that moment yet has no
// AdoptionBlocked record — nothing distinguishes it, in the dashboard's
// own data, from a row the hive will truly auto-adopt. Before this fix the
// row read "hive adopts it in Nm" right up to the due moment, then flipped
// to "not adopted: maintainer_policy is never" for a maintainer's PR. The
// row now names the exception instead of promising an outcome it cannot
// back for every author.
func TestStageTrackerAdoptionNeverHedgesBeforeDue(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyNever, offlinePresence(now.Add(-3*time.Hour)))
	a := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption
	if a == nil || a.Due || a.BlockedBy != "" || a.Action != "" || a.Policy != "never" || !a.AutoDue {
		t.Fatalf("adoption = %+v", a)
	}
	if a.Text != "waiting on @hanthor (offline 3h, 2 nudges) — hive adopts it on the next sweep unless the author maintains this repository" {
		t.Fatalf("text = %q", a.Text)
	}
	// Nudged 13 minutes ago, offline: the pending countdown carries the
	// same hedge.
	store2 := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("hivecommons/hive", 14524)
	store2.Observe(key, stagelabels.ReturnedToContributor, "conflicting")
	store2.MarkNudged(key, "feed", 1, false, now.Add(-13*time.Minute))
	s.stageStore = store2
	s.presenceLookup = offlinePresence(now.Add(-13 * time.Minute))
	if a = trackerItem(t, s.buildStageTracker(status, store2, now), "pr", 14524).Adoption; a == nil || a.AutoDue || a.AutoAdoptAt == nil || !a.AutoAdoptAt.Equal(now.Add(2*time.Minute)) || a.Text != "waiting on @hanthor (offline 13m, 1 nudge) — hive adopts it in 2m unless a new head lands or the author maintains this repository" {
		t.Fatalf("pending text = %+v", a)
	}
}

// hivecommons/hive#15742: ask reads the author's permission at the same due
// moment as never, so its countdown hedges too, saying what ask does.
func TestStageTrackerAdoptionAskHedgesBeforeDue(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyAsk, offlinePresence(now.Add(-3*time.Hour)))
	a := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption
	if a == nil || a.Due || a.BlockedBy != "" || a.Action != "" || a.Policy != "ask" || !a.AutoDue {
		t.Fatalf("adoption = %+v", a)
	}
	if a.Text != "waiting on @hanthor (offline 3h, 2 nudges) — hive adopts it on the next sweep unless the author maintains this repository, then it asks" {
		t.Fatalf("text = %q", a.Text)
	}
	store2 := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("hivecommons/hive", 14524)
	store2.Observe(key, stagelabels.ReturnedToContributor, "conflicting")
	store2.MarkNudged(key, "feed", 1, false, now.Add(-13*time.Minute))
	s.stageStore = store2
	s.presenceLookup = offlinePresence(now.Add(-13 * time.Minute))
	if a = trackerItem(t, s.buildStageTracker(status, store2, now), "pr", 14524).Adoption; a == nil || a.AutoDue || a.AutoAdoptAt == nil || !a.AutoAdoptAt.Equal(now.Add(2*time.Minute)) || a.Text != "waiting on @hanthor (offline 13m, 1 nudge) — hive adopts it in 2m unless a new head lands or the author maintains this repository, then it asks" {
		t.Fatalf("pending text = %+v", a)
	}
}

// hivecommons/hive#15742: the hedge clause per maintainer policy.
func TestStageTrackerAdoptionHedgeByPolicy(t *testing.T) {
	for _, tc := range []struct{ policy, want string }{
		{config.AdoptionMaintainerPolicyNever, " unless the author maintains this repository"},
		{config.AdoptionMaintainerPolicyAsk, " unless the author maintains this repository, then it asks"},
		{config.AdoptionMaintainerPolicyAdopt, ""},
	} {
		if got := stageTrackerAdoptionNeverHedge(StageTrackerAdoption{Policy: tc.policy}, " unless"); got != tc.want {
			t.Errorf("policy %q: hedge = %q, want %q", tc.policy, got, tc.want)
		}
	}
}

// adopt (default): nothing is held, so the row reports the wait, the
// nudges, the presence and — since the engine's own predicate
// (automerge.AutoAdoption) is what the row reads — that the hive adopts
// on its next sweep; no Adopt click is asked for.
func TestStageTrackerAdoptionDefaultReportsWait(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, "", offlinePresence(now.Add(-3*time.Hour)))
	a := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption
	if a == nil || a.Due || a.BlockedBy != "" || a.Action != "" || a.Policy != "adopt" || !a.AutoDue {
		t.Fatalf("adoption = %+v", a)
	}
	if a.Text != "waiting on @hanthor (offline 3h, 2 nudges) — hive adopts it on the next sweep" {
		t.Fatalf("text = %q", a.Text)
	}
	// Online author: no offline span; 26h past the first nudge is past the
	// 2h online grace too.
	s.presenceLookup = func(string) (automerge.ContributorPresence, bool) {
		return automerge.ContributorPresence{Registered: true, Online: true}, true
	}
	if a = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption; a == nil || a.Text != "waiting on @hanthor (online, 2 nudges) — hive adopts it on the next sweep" {
		t.Fatalf("online text = %+v", a)
	}
	// Nudged 13 minutes ago, offline: the row says when the hive adopts.
	store2 := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("hivecommons/hive", 14524)
	store2.Observe(key, stagelabels.ReturnedToContributor, "conflicting")
	store2.MarkNudged(key, "feed", 1, false, now.Add(-13*time.Minute))
	s.stageStore = store2
	s.presenceLookup = offlinePresence(now.Add(-13 * time.Minute))
	if a = trackerItem(t, s.buildStageTracker(status, store2, now), "pr", 14524).Adoption; a == nil || a.AutoDue || a.AutoAdoptAt == nil || !a.AutoAdoptAt.Equal(now.Add(2*time.Minute)) || a.Text != "waiting on @hanthor (offline 13m, 1 nudge) — hive adopts it in 2m unless a new head lands" {
		t.Fatalf("pending text = %+v", a)
	}
}

// A stale blocked mark (head moved) and an adopted record carry nothing.
func TestStageTrackerAdoptionStaleAndAdopted(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status := adoptionFixture(t, now, config.AdoptionMaintainerPolicyAsk, offlinePresence(time.Time{}))
	key := stagelabels.Key("hivecommons/hive", 14524)
	store.NoteAdoptionBlocked(key, "old", stagelabels.AdoptBlockedAuthorMaintainer, now.Add(-time.Hour))
	if a := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption; a == nil || a.Due || a.Action != "" {
		t.Fatalf("stale head still blocks: %+v", a)
	}
	store.MarkAdopted(key, "feed", 14977, now)
	if a := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14524).Adoption; a != nil {
		t.Fatalf("adopted record still carries adoption: %+v", a)
	}
}

func TestStageTrackerAdoptionOfflineFor(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	for _, tc := range []struct {
		a    StageTrackerAdoption
		want string
	}{
		{StageTrackerAdoption{Since: at(26 * time.Hour)}, " 26h"},
		{StageTrackerAdoption{Since: at(26 * time.Hour), AuthorLastSeen: at(40 * time.Minute)}, " 40m"},
		{StageTrackerAdoption{Since: at(26 * time.Hour), AuthorLastSeen: at(3 * 24 * time.Hour)}, " 26h"},
		{StageTrackerAdoption{Since: at(5 * 24 * time.Hour)}, " 5d"},
		{StageTrackerAdoption{Since: at(10 * time.Second)}, " 1m"},
		{StageTrackerAdoption{}, ""},
	} {
		if got := stageTrackerAdoptionOfflineFor(tc.a, now); got != tc.want {
			t.Errorf("%+v: got %q want %q", tc.a, got, tc.want)
		}
	}
}

func TestStageTrackerAdoptionCaption(t *testing.T) {
	a := &StageTrackerAdoption{Text: "waiting on @x (offline 2h)"}
	for in, want := range map[string]string{
		"Returned to @x — review swarm requested changes — @x must act (1 nudge sent)": "Returned to @x — review swarm requested changes — waiting on @x (offline 2h)",
		"Returned to @x — @x must act": "Returned to @x — waiting on @x (offline 2h)",
		"Returned to @x":               "Returned to @x — waiting on @x (offline 2h)",
	} {
		if got := stageTrackerAdoptionCaption(in, a); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if got := stageTrackerAdoptionCaption("Returned to @x — @x must act", nil); got != "Returned to @x — @x must act" {
		t.Errorf("nil adoption changed the caption: %q", got)
	}
}

// POST …/items/N/decision {"decision":"adopt"}: PR only, needs adoption
// enabled, posts the attributed note, sets adoption/supersede, audits.
func TestRepoItemDecisionAdopt(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv := newRepoItemDecisionServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.MaintainerPolicy = config.AdoptionMaintainerPolicyAsk

	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"adopt","type":"issue"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("issue adopt: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := postRepoItemDecision(t, srv, 8, "reader", `{"decision":"adopt","type":"pr"}`); w.Code != http.StatusForbidden {
		t.Fatalf("reader adopt: code=%d body=%s", w.Code, w.Body.String())
	}
	off := false
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = &off
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"adopt","type":"pr"}`); w.Code != http.StatusConflict {
		t.Fatalf("adoption disabled: code=%d body=%s", w.Code, w.Body.String())
	}
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = nil
	if len(mock.added) != 0 || len(mock.comments) != 0 {
		t.Fatalf("refused requests mutated GitHub: added=%v comments=%v", mock.added, mock.comments)
	}

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"adopt","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("adopt: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Decision != "adopt" || resp.Status != "adoption-queued" || resp.Label != delegatedecision.LabelAdoptSupersede {
		t.Fatalf("resp = %+v", resp)
	}
	if strings.Join(mock.added, ",") != delegatedecision.LabelAdoptSupersede {
		t.Fatalf("added = %v", mock.added)
	}
	if len(mock.comments) != 1 || !strings.HasPrefix(mock.comments[0], "<!-- hive:adoption-requested by=writer -->") || !strings.Contains(mock.comments[0], "@writer asked the hive to adopt") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if acts := decisionAuditActions(srv); len(acts) == 0 || acts[0] != auditRepoItemDecisionAdopt {
		t.Fatalf("audit = %v", acts)
	}
}

// governor.labels.stage.adoption.maintainer_policy round-trips through the
// Labels tab and rejects unknown values before touching the config.
func TestGovernorLabelsAdoptionMaintainerPolicyRoundTrip(t *testing.T) {
	s := covApiServer(t)
	for _, bad := range []any{"always", "ASK ME", 7, true} {
		if rec := doPut(s, "/api/config/governor/labels", map[string]any{"stage_adoption_maintainer_policy": bad}); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d — %s", bad, rec.Code, rec.Body.String())
		}
	}
	if got := s.deps.Config.Governor.Labels.Stage.Adoption.MaintainerPolicy; got != "" {
		t.Fatalf("a rejected PUT changed the policy to %q", got)
	}
	var got map[string]any
	if err := json.Unmarshal(doGet(s, "/api/config/governor").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["stageAdoptionMaintainerPolicy"] != "adopt" {
		t.Fatalf("default stageAdoptionMaintainerPolicy = %v, want adopt", got["stageAdoptionMaintainerPolicy"])
	}
	if rec := doPut(s, "/api/config/governor/labels", map[string]any{"stage_adoption_maintainer_policy": " Ask "}); rec.Code != http.StatusOK {
		t.Fatalf("labels PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Labels.Stage.Adoption.MaintainerPolicy; got != "ask" {
		t.Fatalf("policy = %q, want ask", got)
	}
	if err := json.Unmarshal(doGet(s, "/api/config/governor").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["stageAdoptionMaintainerPolicy"] != "ask" {
		t.Fatalf("stageAdoptionMaintainerPolicy after PUT = %v", got["stageAdoptionMaintainerPolicy"])
	}

	html := indexHTML(t)
	labels := extractJSFunction(t, html, "renderGovLabels")
	for _, want := range []string{
		"yamlHint('governor.labels.stage.adoption.maintainer_policy')",
		`id="cfg-stage-adoption-maintainer-policy" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_adoption_maintainer_policy" data-arg-types="s,s,v"`,
		"_configState.data.stageAdoptionMaintainerPolicy || 'adopt'",
		"['adopt',", "['ask',", "['never',",
	} {
		if !strings.Contains(labels, want) {
			t.Errorf("renderGovLabels lacks %q", want)
		}
	}
	if !strings.Contains(html, "key: 'governor.labels.stage.adoption.maintainer_policy'") {
		t.Error("settings search index has no entry for governor.labels.stage.adoption.maintainer_policy")
	}
}

// The SPA's pure helpers: the chip text/tooltip, the Adopt row action
// (only when the payload offers it, only with write), and the confirm.
func TestStageTrackerAdoptionJS(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		stageTrackerRowActionsPreamble(t, html),
		jsFunc(t, html, "stageTrackerAdoptionTip"),
		jsFunc(t, html, "stageTrackerAdoptOffered"),
		jsFunc(t, html, "stageTrackerAdoptionChipText"),
		jsFunc(t, html, "stageTrackerAdoptionChipHtml"),
		stageTrackerPresenceJS(t, html),
		`
const ask = { kind: 'pr', repo: 'hivecommons/hive', number: 14524, stage: 72, stage_label: 'stage/072-returned-to-contributor', author: 'hanthor', labels: [],
  adoption: { author: 'hanthor', due: true, blocked_by: 'author-is-maintainer', nudges: 2, author_online: false, author_maintainer: true, policy: 'ask', action: 'decide:adopt', text: 'waiting on @hanthor (maintainer, offline 26h) — adopt?' } };
const plain = { kind: 'pr', repo: 'hivecommons/hive', number: 1, stage: 72, stage_label: 'stage/072-returned-to-contributor', author: 'zoe', labels: [],
  adoption: { author: 'zoe', due: false, nudges: 1, author_online: true, policy: 'adopt', text: 'waiting on @zoe (online, 1 nudge)' } };
const none = { kind: 'pr', repo: 'hivecommons/hive', number: 2, stage: 72, stage_label: 'stage/072-returned-to-contributor', author: 'zoe', labels: [] };
assert.strictEqual(stageTrackerAdoptionTip(ask), 'waiting on @hanthor (maintainer, offline 26h) — adopt?');
assert.strictEqual(stageTrackerAdoptionTip(none), '');
assert.strictEqual(stageTrackerAdoptOffered(ask), true);
assert.strictEqual(stageTrackerAdoptOffered(plain), false);
assert.strictEqual(stageTrackerAdoptionChipText(ask), 'adopt?');
assert.strictEqual(stageTrackerAdoptionChipText(plain), 'waiting on @zoe');
assert.ok(stageTrackerAdoptionChipHtml(ask).includes('ham-badge-adoption-ask'));
assert.ok(stageTrackerAdoptionChipHtml(ask).includes('offline 26h'));
assert.strictEqual(stageTrackerAdoptionChipHtml(none), '');
const ids = (item, ctx) => stageTrackerRowActions(item, ctx).map(a => a.id);
const w = { write: true, user: 'owner' };
assert.ok(ids(ask, w).includes('decide:adopt'), 'ask row offers Adopt: ' + ids(ask, w));
assert.ok(!ids(plain, w).includes('decide:adopt'), 'plain 072 row must not offer Adopt');
assert.ok(!ids(none, w).includes('decide:adopt'));
const ro = stageTrackerRowActions(ask, { write: false, user: '' }).find(a => a.id === 'decide:adopt');
assert.ok(ro && ro.enabled === false && ro.why, 'read-only Adopt is listed but disabled');
assert.strictEqual(STAGE_TRACKER_ACTION_LABELS['decide:adopt'], 'Adopt (hive takes over)');
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
	for _, want := range []string{
		"case 'decide:adopt': {",
		"'Adoption: ' + adoption",
		".ham-badge-adoption {",
		"adoption: lp.adoption",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
}

// The tracker endpoint serialises adoption on the nested PR of an issue
// row too (the issue carrying its covering PR).
func TestStageTrackerAdoptionEndpointShape(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, _ := adoptionFixture(t, now, config.AdoptionMaintainerPolicyAsk, offlinePresence(now.Add(-26*time.Hour)))
	store.NoteAdoptionBlocked(stagelabels.Key("hivecommons/hive", 14524), "feed", stagelabels.AdoptBlockedAuthorMaintainer, now.Add(-25*time.Hour))
	rr := httptest.NewRecorder()
	s.handleStageTracker(rr, httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"blocked_by":"author-is-maintainer"`) {
		t.Fatalf("endpoint body lacks adoption: %s", rr.Body.String())
	}
}

// hivecommons/hive#15202: an owner's / author-human PR the engine refused to
// adopt says so on its row, with no Adopt offer and no nudge clock needed.
func TestStageTrackerAdoptionPersonAuthoredSaysNotAdopted(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	key := stagelabels.Key("hivecommons/hive", 15157)
	store.Observe(key, stagelabels.NeedsDecision, "contributor-lane:decision-requested")
	store.NoteAdoptionBlocked(key, "cafe", stagelabels.AdoptBlockedAuthorPerson, now.Add(-2*time.Minute))
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 15157, Title: "Owner change", Author: "clubanderson", HeadSHA: "cafe", Labels: []string{"stage/085-needs-decision", "author/human", "role/trusted"}}}},
	}}
	s.UpdateStatus(status)
	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15157)
	a := it.Adoption
	if a == nil || !a.Due || a.BlockedBy != stagelabels.AdoptBlockedAuthorPerson || a.Action != "" {
		t.Fatalf("adoption = %+v", a)
	}
	if a.Text != "@clubanderson pushed this PR themselves — not adopted: the hive asks them, not adopts" {
		t.Fatalf("text = %q", a.Text)
	}
	if !strings.Contains(it.Caption, "not adopted") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if d := it.DecisionNeeded; d != nil {
		for _, o := range d.Options {
			if o.Action == decisionActionAdopt {
				t.Fatalf("adopt offered on a person's PR: %+v", d)
			}
		}
	}
}

// hivecommons/hive#15159: a clanker-authored 072 row names whose clanker
// the PR went back to and who sent it; the "@x must act" tail — a person's
// turn — goes.
func TestStageTrackerReturnedToHiveCaption(t *testing.T) {
	cases := []struct {
		caption    string
		requesters []string
		want       string
	}{
		{"Returned to @thatguywob — a maintainer requested changes — @thatguywob must act", []string{"clubanderson"}, "Returned to @thatguywob's clanker — @clubanderson requested changes"},
		{"Returned to @thatguywob — a maintainer requested changes — @thatguywob must act", nil, "Returned to @thatguywob's clanker — a maintainer requested changes"},
		{"Returned to @thatguywob — a maintainer requested changes — @thatguywob must act (2 nudges sent)", []string{"a", "b"}, "Returned to @thatguywob's clanker — @a and @b requested changes (2 nudges sent)"},
		{"Returned to @thatguywob — review swarm requested changes — @thatguywob must act", []string{"clubanderson"}, "Returned to @thatguywob's clanker — review swarm requested changes"},
		{"Returned to @thatguywob — review swarm requested changes — waiting on @thatguywob (offline 2h)", nil, "Returned to @thatguywob's clanker — review swarm requested changes — waiting on @thatguywob (offline 2h)"},
		{"Returned to contributor", nil, "Returned to contributor"},
	}
	for _, tc := range cases {
		if got := stageTrackerReturnedToHiveCaption(tc.caption, "thatguywob", tc.requesters); got != tc.want {
			t.Errorf("%q → %q, want %q", tc.caption, got, tc.want)
		}
	}
	if got := stageTrackerReturnedToHiveCaption("Returned to @x — @x must act", "", nil); got != "Returned to @x — @x must act" {
		t.Errorf("no author changed the caption: %q", got)
	}
}

// hive#15565/#15566/#15609 and #15312: PRs by an offline clanker sat at 072
// with "waiting on @Karibhou (offline 13m, 1 nudge)" until a person clicked
// Adopt. The row now says when the hive adopts, offers Adopt now only as an
// override, and the fork-not-editable row names its blocker.
func TestStageTrackerAutoAdoptionExampleCaptions(t *testing.T) {
	now := time.Date(2026, 10, 29, 22, 45, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	nudged := now.Add(-13 * time.Minute)
	s.presenceLookup = func(login string) (automerge.ContributorPresence, bool) {
		// One heartbeat a minute after the nudge: acknowledged, then gone.
		return automerge.ContributorPresence{Registered: true, Online: false, LastSeen: nudged.Add(time.Minute)}, true
	}
	rows := []struct {
		number int
		reason string
		author string
		want   string
	}{
		{15565, "conflicting", "Karibhou", "conflicting"},
		{15566, "contributor-lane:changes-requested", "Karibhou", "changes requested"},
		{15609, "optional-check-failing:overlayfs-exec-guard", "Karibhou", "overlayfs-exec-guard"},
		{15312, "fork-not-editable", "odttlnt", "fork not editable by hive"},
	}
	status := minimalPayload()
	status.HiveID = "hive"
	var prs []any
	for _, r := range rows {
		key := stagelabels.Key("hivecommons/hive", r.number)
		store.Observe(key, stagelabels.ReturnedToContributor, r.reason)
		store.MarkNudged(key, "feed", 1, false, nudged)
		prs = append(prs, FrontendPR{PullRequest: github.PullRequest{Number: r.number, Title: "Change", Author: r.author, HeadSHA: "feed", Labels: []string{"stage/072-returned-to-contributor", "author/clanker", "role/contributor"}}})
	}
	status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive", OpenPrs: prs}}
	s.UpdateStatus(status)

	for _, r := range rows {
		it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", r.number)
		wantTail := "waiting on @" + r.author + " (offline 12m, 1 nudge) — hive adopts it in 3m unless a new head lands"
		if !strings.Contains(it.Caption, r.want) || !strings.HasSuffix(it.Caption, wantTail) {
			t.Fatalf("#%d caption = %q, want %q … %q", r.number, it.Caption, r.want, wantTail)
		}
		if strings.Contains(it.Caption, "No stage recorded yet") {
			t.Fatalf("#%d caption keeps the empty-stage clause: %q", r.number, it.Caption)
		}
		if it.Adoption == nil || it.Adoption.AutoDue || it.Adoption.AutoAdoptAt == nil || !it.Adoption.AutoAdoptAt.Equal(nudged.Add(16*time.Minute)) {
			t.Fatalf("#%d adoption = %+v", r.number, it.Adoption)
		}
		if it.Takeover == nil || !it.Takeover.AdoptNow || it.Takeover.GraceElapsed || it.Takeover.AutoAdoptAt == nil {
			t.Fatalf("#%d takeover = %+v", r.number, it.Takeover)
		}
	}
	// Three minutes on: the sweep's clock has run out; the row says so.
	later := nudged.Add(16 * time.Minute)
	for _, r := range rows {
		it := trackerItem(t, s.buildStageTracker(status, store, later), "pr", r.number)
		if !strings.HasSuffix(it.Caption, "— hive adopts it on the next sweep") || it.Adoption == nil || !it.Adoption.AutoDue || it.Takeover == nil || !it.Takeover.GraceElapsed {
			t.Fatalf("#%d at +16m: caption=%q adoption=%+v takeover=%+v", r.number, it.Caption, it.Adoption, it.Takeover)
		}
	}
}
