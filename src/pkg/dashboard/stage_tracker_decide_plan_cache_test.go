package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Let hive decide answers fast and says why a person is wanted
// (hivecommons/hive#15773): the plan endpoint shares one answer between the
// prefetch and the click, reports its timing, and every human move carries
// a short human_reason.

// The prefetch fired when the row's options are revealed and the click
// that follows get one plan: the second request is a cache hit with the
// same body, and both say so in Server-Timing. Another head is a new plan.
func TestDelegatedDecisionPlanCacheSharesPrefetch(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	body := `{"type":"pr","reason":"needs-decision","caption":"needs a decision","head":"abc1234"}`
	merger := map[string]string{"X-Hive-Role": "merger"}
	prefetch := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger", delegatedPlanPrefetchHeader: "1"})
	if prefetch.Code != http.StatusOK {
		t.Fatalf("prefetch code=%d body=%s", prefetch.Code, prefetch.Body.String())
	}
	if got := prefetch.Header().Get("Server-Timing"); !strings.HasPrefix(got, `decide_plan;desc="miss";dur=`) {
		t.Fatalf("prefetch Server-Timing = %q", got)
	}
	click := postDelegatedPlan(t, srv, 8, "writer", body, merger)
	if click.Code != http.StatusOK {
		t.Fatalf("click code=%d body=%s", click.Code, click.Body.String())
	}
	if got := click.Header().Get("Server-Timing"); !strings.HasPrefix(got, `decide_plan;desc="hit";dur=`) {
		t.Fatalf("click Server-Timing = %q, want the prefetched plan", got)
	}
	if click.Body.String() != prefetch.Body.String() {
		t.Fatalf("the click must see the prefetched plan:\n%s\n%s", prefetch.Body.String(), click.Body.String())
	}
	// A push (another head) or another role is a fresh plan.
	pushed := postDelegatedPlan(t, srv, 8, "writer", strings.Replace(body, "abc1234", "def5678", 1), merger)
	if got := pushed.Header().Get("Server-Timing"); !strings.Contains(got, `desc="miss"`) {
		t.Fatalf("new head Server-Timing = %q", got)
	}
	if got := postDelegatedPlan(t, srv, 8, "writer", body, nil).Header().Get("Server-Timing"); strings.Contains(got, `desc="hit"`) {
		t.Fatalf("another role Server-Timing = %q", got)
	}
	// Starting the decision drops the item's cached previews.
	key := delegatedPlanCacheKey("testorg/testrepo", 8, "pr", true, delegatedDecisionRequest{Type: "pr", Reason: "needs-decision", Caption: "needs a decision", Head: "abc1234"})
	if _, ok := srv.delegatedJobs.plans.get(key, time.Now()); !ok {
		t.Fatal("the plan was not cached under its (repo, number, type, head, request) key")
	}
	postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	if _, ok := srv.delegatedJobs.plans.get(key, time.Now()); ok {
		t.Fatal("a recorded decision must drop the item's cached previews")
	}
}

// The cache is bounded: entries expire after the TTL, the oldest goes when
// the cap is reached, and forget drops one item's entries only.
func TestDelegatedPlanCacheBounded(t *testing.T) {
	var c delegatedPlanCache
	now := time.Date(2026, 10, 31, 7, 0, 0, 0, time.UTC)
	key := func(n int) string {
		return delegatedPlanCacheKey("o/r", n, "pr", false, delegatedDecisionRequest{Head: "h"})
	}
	c.put(key(1), &delegatedDecisionPlan{Number: 1}, now)
	if p, ok := c.get(key(1), now.Add(delegatedPlanCacheTTL-time.Second)); !ok || p.Number != 1 {
		t.Fatalf("fresh entry: %+v %v", p, ok)
	}
	if _, ok := c.get(key(1), now.Add(delegatedPlanCacheTTL)); ok {
		t.Fatal("an entry past the TTL must not be served")
	}
	for n := 1; n <= delegatedPlanCacheMax+5; n++ {
		c.put(key(n), &delegatedDecisionPlan{Number: n}, now.Add(time.Duration(n)*time.Millisecond))
	}
	if len(c.entries) != delegatedPlanCacheMax {
		t.Fatalf("cache holds %d entries, cap %d", len(c.entries), delegatedPlanCacheMax)
	}
	at := now.Add(time.Second)
	if _, ok := c.get(key(1), at); ok {
		t.Fatal("the oldest entry must go first")
	}
	last := delegatedPlanCacheMax + 5
	if _, ok := c.get(key(last), at); !ok {
		t.Fatal("the newest entry must stay")
	}
	c.forget("O/R", last)
	if _, ok := c.get(key(last), at); ok {
		t.Fatal("forget must drop the item's entries")
	}
	if _, ok := c.get(key(last-1), at); !ok {
		t.Fatal("forget must keep other items' entries")
	}
	if !strings.HasPrefix(key(1), delegatedJobKey("o/r", 1)+"\x00") || strings.HasPrefix(key(10), delegatedJobKey("o/r", 1)+"\x00") {
		t.Fatal("#1's prefix must not match #10")
	}
	if got := delegatedPlanServerTiming("hit", 1500*time.Microsecond); got != `decide_plan;desc="hit";dur=1.5` {
		t.Fatalf("Server-Timing metric = %q", got)
	}
}

// The app-wide Server-Timing wrapper adds its metric beside the handler's
// own instead of overwriting it.
func TestServerTimingKeepsHandlerMetrics(t *testing.T) {
	s := &Server{}
	h := s.withServerTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Server-Timing", delegatedPlanServerTiming("miss", 2*time.Millisecond))
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/repos/o/r/items/1/decide-delegated/plan", nil))
	got := strings.Join(rec.Header().Values("Server-Timing"), ", ")
	if !strings.Contains(got, `decide_plan;desc="miss";dur=2.0`) || !strings.Contains(got, "app;dur=") {
		t.Fatalf("Server-Timing = %q, want both the plan and the app metric", got)
	}
}

// No 👤 marker ships without a reason: every stage, author kind and
// modifier the actor derivation reads yields a human_reason of at most
// 40 characters exactly when the next move is a person's.
func TestStageTrackerNoHumanMarkerWithoutReason(t *testing.T) {
	now := time.Date(2026, 10, 31, 7, 0, 0, 0, time.UTC)
	check := func(name string, a *StageTrackerActor) {
		t.Helper()
		if a == nil {
			return
		}
		if a.Kind != StageActorHuman {
			if a.HumanReason != "" {
				t.Errorf("%s: kind %q carries human_reason %q", name, a.Kind, a.HumanReason)
			}
			return
		}
		if strings.TrimSpace(a.HumanReason) == "" {
			t.Errorf("%s: 👤 without a reason (why %q)", name, a.Why)
		}
		if n := utf8.RuneCountInString(a.HumanReason); n > stageTrackerHumanReasonMax {
			t.Errorf("%s: human_reason %q is %d chars, max %d", name, a.HumanReason, n, stageTrackerHumanReasonMax)
		}
	}
	longQuestion := "Keep #1 and close #2, or keep #2 and close #1, given both touch the release pipeline?"
	for _, d := range stagelabels.Table {
		for _, kind := range []string{"pr", "issue"} {
			for _, author := range []struct{ login, kind string }{{"pat", authorKindHuman}, {"hive-app[bot]", authorKindHive}, {"arteeh", authorKindClanker}, {"github-actions[bot]", ""}, {"", ""}} {
				for _, mods := range [][]string{nil, {modDraft}, {modBlocked}, {modHold}} {
					for _, reason := range []string{"", "contributor-lane:workflow-path", "requires_human:high-risk", "workflow-files"} {
						it := StageTrackerItem{Repo: "o/r", Kind: kind, Number: 1, Stage: d.Number, Author: author.login, AuthorKind: author.kind, ClaimantKind: author.kind, Modifiers: mods, Reason: reason, DecisionNeeded: &StageTrackerDecision{Question: longQuestion}}
						it.HumanMergeReason, _, _ = stageTrackerHumanMergeReason(reason)
						check(d.Slug+"/"+kind+"/"+author.login+"/"+strings.Join(mods, ",")+"/"+reason, stageTrackerStageActor(it, nil, now))
					}
				}
			}
		}
	}
	superseded := StageTrackerItem{Kind: "pr", Number: 2, Stage: trackerNumberFixing, Author: "pat", AuthorKind: authorKindHuman, Superseded: &github.ReviewQueueSuperseded{CloserPR: 15759}}
	if a := stageTrackerStageActor(superseded, nil, now); a.HumanReason != "superseded by #15759" {
		t.Errorf("superseded human_reason = %q", a.HumanReason)
	}
	// The issue's own examples: an 085 PR says what is owed, a 090 PR the
	// merge reason word, and an issue riding the 085 PR names that PR.
	pr := StageTrackerItem{Repo: "o/r", Kind: "pr", Number: 15759, Stage: trackerNumberNeedsDecision, Reason: "contributor-lane:workflow-path", AuthorKind: authorKindHive}
	pr.StageActor = stageTrackerStageActor(pr, nil, now)
	if want := stageTrackerClipReason(stageTrackerDecisionWhy(pr)); pr.StageActor.HumanReason != want || want == "" {
		t.Errorf("085 human_reason = %q", pr.StageActor.HumanReason)
	}
	merge := StageTrackerItem{Repo: "o/r", Kind: "pr", Number: 15760, Stage: trackerNumberReadyForHuman, Reason: "contributor-lane:workflow-path"}
	merge.HumanMergeReason, _, _ = stageTrackerHumanMergeReason(merge.Reason)
	if a := stageTrackerStageActor(merge, nil, now); a.HumanReason != "workflow files" {
		t.Errorf("090 human_reason = %q (code %q)", a.HumanReason, merge.HumanMergeReason)
	}
	issue := StageTrackerItem{Repo: "o/r", Kind: "issue", Number: 15503, Stage: trackerNumberPRInProgress, Actor: "its PR", LinkedPRs: []StageTrackerLinkedPR{{Number: 15759, DrawnUnder: 15502}}}
	issue.StageActor = &StageTrackerActor{Kind: StageActorHive, Why: "an agent has a PR under way; not yet opened"}
	items := []StageTrackerItem{pr, issue}
	stageTrackerResolveActors(items)
	if a := items[1].StageActor; a.Kind != StageActorHuman || a.HumanReason != "#15759 needs a decision" || a.Why != pr.StageActor.Why {
		t.Errorf("riding issue actor = %+v", a)
	}
	check("riding issue", items[1].StageActor)
	if got := stageTrackerClipReason(strings.Repeat("word ", 20)); utf8.RuneCountInString(got) > stageTrackerHumanReasonMax || !strings.HasSuffix(got, "…") {
		t.Errorf("clip = %q", got)
	}
	for i := range items {
		if items[i].StageActor != nil && items[i].StageActor.Kind == StageActorHuman && items[i].StageActor.HumanReason == "" {
			t.Errorf("item %s has a human marker without a reason", strconv.Itoa(items[i].Number))
		}
	}
}

// A covered issue whose PR is parked on a decision says it waits there.
func TestStageTrackerCoveredIssueCaptionWaitsOnPRDecision(t *testing.T) {
	it := StageTrackerItem{Kind: "issue", Number: 15503, Stage: trackerNumberCoveredByPR, LinkedPRs: []StageTrackerLinkedPR{{Number: 15759, State: "open", Stage: trackerNumberNeedsDecision, Caption: "A human decision is owed — workflow path"}}}
	stageTrackerLinkedPRProgress(&it)
	if it.Caption != "Covered by PR #15759 — waiting for a decision on the PR" {
		t.Fatalf("caption = %q", it.Caption)
	}
	other := StageTrackerItem{Kind: "issue", Number: 15504, Stage: trackerNumberCoveredByPR, LinkedPRs: []StageTrackerLinkedPR{{Number: 15761, State: "open", Stage: trackerNumberReviewPending, Caption: "Review swarm requested"}}}
	stageTrackerLinkedPRProgress(&other)
	if other.Caption != "Covered by PR #15761 — Review swarm requested" {
		t.Fatalf("a PR not waiting on a decision keeps its caption: %q", other.Caption)
	}
}

// The preview opens at once as a skeleton — Cancel works, Let hive do
// that waits — and fills when the plan lands; a failed plan reads inline
// with Retry; a second click while one preview is up opens nothing; the
// prefetch fired when the row's options are revealed serves the click.
func TestStageTrackerDelegatePreviewSkeletonPrefetch(t *testing.T) {
	html := indexHTML(t)
	dialog := jsFunc(t, html, "stageTrackerDelegatePlanDialog")
	for _, banned := range []string{"window.confirm", "window.alert", "window.prompt", "confirm(", "alert("} {
		if strings.Contains(dialog, banned) {
			t.Errorf("the preview must use the in-app dialog, found %q", banned)
		}
	}
	for _, want := range []string{
		"hiveWireDialog(overlay, resolve, null, '.primary', '.cancel',",
		"if (loading || view) return;",
		"primary.disabled = false;",
		".ham-plan-retry",
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("stageTrackerDelegatePlanDialog lacks %q", want)
		}
	}
	for _, want := range []string{
		"@media (prefers-reduced-motion: reduce) { .ham-plan-skeleton-line { animation: none; } }",
		".ham-decision-pending {",
		".ham-human-reason {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+"\n"+jsFunc(t, html, "stageTrackerMenuOffersDelegate")+`
(async () => {
  const item = { repo: 'o/r', kind: 'issue', number: 4, url: 'u4', stage: 45, title: 'Pick a release cadence' };
  // The skeleton: title, subject, "Asking the hive…", Cancel enabled and
  // focused, Let hive do that disabled, the merge button hidden.
  const sk = stageTrackerDelegatePlanSkeletonHtml(stageTrackerDelegatePlanSubject([item]), '', 'Asking the hive…');
  assert.ok(sk.includes('Let hive decide') && sk.includes('o/r#4 · Pick a release cadence') && sk.includes('Asking the hive…'), sk);
  assert.ok(sk.includes('role="status" aria-live="polite"'), sk);
  assert.strictEqual((sk.match(/ham-plan-skeleton-line/g) || []).length, STAGE_TRACKER_PLAN_SKELETON_LINES);
  assert.ok(/<button[^>]*class="[^"]*cancel"[^>]*data-initial-focus>Cancel<\/button>/.test(sk) && !/cancel"[^>]*disabled/.test(sk), 'Cancel is enabled and focused: ' + sk);
  assert.ok(/<button[^>]*primary" disabled aria-disabled="true">Let hive do that<\/button>/.test(sk), 'Let hive do that waits for the plan: ' + sk);
  assert.ok(/ham-plan-merge" hidden>/.test(sk), sk);
  const err = stageTrackerDelegatePlanErrorContent('o/r#4', [{ ref: 'o/r#4', error: 'HTTP 502' }]);
  assert.ok(err.includes('role="alert"') && err.includes('Could not preview o/r#4 (HTTP 502)') && err.includes('ham-plan-retry">Retry</button>'), err);
  assert.strictEqual(stageTrackerDelegatePlanSubject([item, item]), '2 items');

  const plan = { would: 'approve', summary: 'Approve o/r#4.', why: 'The question has one sane answer.', consequences: ['o/r#4 moves on'], alternatives: [] };
  const posts = [];
  let reply = () => ({ ok: true, status: 200, json: async () => plan });
  global.fetch = async (url, init) => { posts.push({ url, prefetch: !!(init && init.headers && init.headers['X-Hive-Prefetch']) }); return reply(); };

  // Revealing the options prefetches once; the click takes that answer.
  assert.strictEqual(stageTrackerPrefetchDelegatePlan(item), 1);
  assert.strictEqual(stageTrackerPrefetchDelegatePlan(item), 0, 'one prefetch per item while it is fresh');
  dialogs.length = 0;
  assert.deepStrictEqual(await stageTrackerActionPayload([item], 'decide:delegate'), { chosen: 'Let hive do that · previewed approve' });
  assert.deepStrictEqual(posts.map(p => [p.url, p.prefetch]), [['/api/repos/o/r/items/4/decide-delegated/plan', true]], 'the click reuses the prefetch');
  assert.deepStrictEqual(dialogs.map(d => d.kind), ['plan']);
  // A prefetched answer serves one click; the next asks again.
  posts.length = 0; dialogs.length = 0;
  await stageTrackerActionPayload([item], 'decide:delegate');
  assert.deepStrictEqual(posts.map(p => p.prefetch), [false]);
  // A stale prefetch is not used.
  posts.length = 0;
  stageTrackerPrefetchDelegatePlan(item, Date.now() - STAGE_TRACKER_PLAN_PREFETCH_MS - 1);
  await stageTrackerActionPayload([item], 'decide:delegate');
  assert.deepStrictEqual(posts.map(p => p.prefetch), [true, false]);

  // No double-fire: while the first preview is up a second click is a no-op.
  posts.length = 0; dialogs.length = 0;
  let release; planGate = new Promise(r => { release = r; });
  const first = stageTrackerActionPayload([item], 'decide:delegate');
  await Promise.resolve();
  assert.deepStrictEqual(dialogs.map(d => d.kind), ['plan-skeleton'], 'the skeleton opens before the plan lands');
  assert.strictEqual(await stageTrackerActionPayload([item], 'decide:delegate'), null, 'the second click opens nothing');
  assert.strictEqual(dialogs.length, 1);
  release(); planGate = null;
  assert.deepStrictEqual(await first, { chosen: 'Let hive do that · previewed approve' });
  assert.strictEqual(posts.length, 1);
  // Once closed, the next click opens a fresh preview.
  dialogs.length = 0;
  assert.ok(await stageTrackerActionPayload([item], 'decide:delegate'));

  // A failed plan reads inline (no toast) and runs nothing.
  dialogs.length = 0;
  reply = () => ({ ok: false, status: 502, json: async () => ({ error: 'GitHub timed out' }) });
  assert.strictEqual(await stageTrackerActionPayload([item], 'decide:delegate'), null);
  assert.deepStrictEqual(dialogs.map(d => d.kind), ['plan-error']);
  assert.ok(dialogs[0].cfg.html.includes('Could not preview o/r#4 (GitHub timed out)') && dialogs[0].cfg.html.includes('Retry'), dialogs[0].cfg.html);

  // Only an enabled Let hive decide (or one in a submenu) prefetches from the menu.
  assert.strictEqual(stageTrackerMenuOffersDelegate([{ id: 'decide:approve' }, { id: 'more', submenu: [{ id: 'decide:delegate' }] }]), true);
  assert.strictEqual(stageTrackerMenuOffersDelegate([{ id: 'decide:delegate', enabled: false }]), false);
  assert.strictEqual(stageTrackerMenuOffersDelegate(null), false);
})().catch(e => { console.error(e); process.exit(1); });
`)
}
