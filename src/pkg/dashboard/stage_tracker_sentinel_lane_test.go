package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// stageTrackerSentinelJS is the sentinel-alert surface the row builders
// depend on (constants + the two pure helpers), for every node harness
// that runs stageTrackerSegments / stageTrackerChipsHtml.
func stageTrackerSentinelJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_SENTINEL_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_SENTINEL_CHIP"),
		jsConstLine(t, html, "STAGE_TRACKER_SENTINEL_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_SENTINEL_TIP_FALLBACK"),
		jsConstLine(t, html, "STAGE_TRACKER_SHORT_SHA"),
		jsFunc(t, html, "stageTrackerSentinelTip"),
		jsFunc(t, html, "stageTrackerSentinelInLane"),
		jsFunc(t, html, "stageTrackerSentinelChipHtml"),
	}, "\n")
}

func writeSentinelBundle(t *testing.T, root, repo string, number int, head string, findings []evidence.SentinelFinding) {
	t.Helper()
	path := github.ReviewEvidencePath(root, repo, number, head)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := evidence.Bundle{SchemaVersion: evidence.SchemaVersion, Repo: repo, Number: number, HeadSHA: head, Sentinel: findings}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStageTrackerSentinelFlag pins the snapshot side: a PR wearing the
// configured sentinel label carries sentinel_alert (label only: no count,
// no reason); a PR whose current head's evidence bundle records findings
// carries it too, with the count and the first finding's summary; a PR
// with neither does not; an issue never does; and a webhook label patch
// flips the flag on the next build without a governor cycle.
func TestStageTrackerSentinelFlag(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	prev := github.ReviewEvidenceRoot
	root := filepath.Join(t.TempDir(), "evidence")
	github.ReviewEvidenceRoot = root
	t.Cleanup(func() { github.ReviewEvidenceRoot = prev })

	s, store := stageTrackerFixture(t, now)
	const headEvidence = "feedface0123456789abcdef0123456789abcdef"
	const headClean = "0badf00d0123456789abcdef0123456789abcdef"
	writeSentinelBundle(t, root, "acme/console", 110, headEvidence, []evidence.SentinelFinding{
		{Rule: "sensitive_path", Summary: "touches OWNERS", Paths: []string{"OWNERS"}},
		{Rule: "workflow_permissions", Summary: "widens workflow permissions"},
	})
	// A bundle for a superseded head says nothing about the current one.
	writeSentinelBundle(t, root, "acme/console", 111, "0ld0ld0ld", []evidence.SentinelFinding{{Rule: "test_removal", Summary: "deletes tests"}})
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		// Label only, parked on a person.
		FrontendPR{PullRequest: github.PullRequest{Number: 109, Title: "Widen perms", Author: "mallory", HeadSHA: headClean, Labels: []string{"stage/085-needs-decision", config.DefaultSentinelLabel}}},
		// Evidence only (a trusted author's notice: comment, no label), in review.
		FrontendPR{PullRequest: github.PullRequest{Number: 110, Title: "Owners tweak", Author: "pat", HeadSHA: headEvidence, Labels: []string{"stage/050-review-pending"}}},
		// Stale evidence on an older head: clean.
		FrontendPR{PullRequest: github.PullRequest{Number: 111, Title: "Re-pushed", Author: "pat", HeadSHA: headClean, Labels: []string{"stage/050-review-pending"}}},
	)
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	labelled := trackerItem(t, resp, "pr", 109)
	if !labelled.SentinelAlert || labelled.SentinelFindings != 0 || labelled.SentinelReason != "" {
		t.Fatalf("label-only PR 109 = alert %v findings %d reason %q", labelled.SentinelAlert, labelled.SentinelFindings, labelled.SentinelReason)
	}
	if labelled.Stage != trackerNumberNeedsDecision {
		t.Fatalf("PR 109 stage = %d, want 085", labelled.Stage)
	}
	evid := trackerItem(t, resp, "pr", 110)
	if !evid.SentinelAlert || evid.SentinelFindings != 2 || evid.SentinelReason != "touches OWNERS (+1 more)" || evid.Head != headEvidence {
		t.Fatalf("evidence PR 110 = alert %v findings %d reason %q head %q", evid.SentinelAlert, evid.SentinelFindings, evid.SentinelReason, evid.Head)
	}
	if stale := trackerItem(t, resp, "pr", 111); stale.SentinelAlert || stale.SentinelFindings != 0 {
		t.Fatalf("a bundle for a superseded head must not flag PR 111: %+v", stale)
	}
	for _, it := range resp.Items {
		if it.Kind == "issue" && it.SentinelAlert {
			t.Fatalf("issue #%d must never carry sentinel_alert", it.Number)
		}
		if it.Kind == "pr" && it.Number != 109 && it.Number != 110 && it.SentinelAlert {
			t.Fatalf("PR #%d flagged without label or evidence", it.Number)
		}
	}
	// The wire shape: sentinel_alert only when set, the count and reason
	// beside it, and nothing on a clean row.
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sentinel_alert":true,"sentinel_findings":2,"sentinel_reason":"touches OWNERS (+1 more)"`) {
		t.Fatalf("payload lacks the evidence-backed sentinel fields: %s", raw)
	}
	if strings.Count(string(raw), `"sentinel_alert":true`) != 2 {
		t.Fatalf("exactly two rows carry sentinel_alert: %s", raw)
	}

	// A nested PR mirrors its row's alert onto the issue's linked_prs entry.
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 112, Title: "Covers 13", Author: "mallory", HeadSHA: headClean, Labels: []string{"stage/085-needs-decision", config.DefaultSentinelLabel},
			LinkedIssues: []github.PRLinkedIssue{{Number: 13}}}},
	)
	s.statusMu.Unlock()
	resp = s.buildStageTracker(s.status, store, now)
	issue := trackerItem(t, resp, "issue", 13)
	var nested *StageTrackerLinkedPR
	for i := range issue.LinkedPRs {
		if issue.LinkedPRs[i].Number == 112 {
			nested = &issue.LinkedPRs[i]
		}
	}
	if nested == nil || !nested.SentinelAlert || nested.Head != headClean {
		t.Fatalf("issue 13 linked_prs must mirror PR 112's alert: %+v", issue.LinkedPRs)
	}

	// Webhook: the label comes off → flag drops; goes on → flag returns.
	// The head the re-read reports is kept so evidence follows the revision.
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 109, IsPR: true, State: "open", Title: "Widen perms", Labels: []string{"stage/085-needs-decision"}, HeadSHA: headClean, UpdatedAt: now},
		{Repo: "acme/console", Number: 111, IsPR: true, State: "open", Title: "Re-pushed", Labels: []string{"stage/050-review-pending", config.DefaultSentinelLabel}, HeadSHA: headClean, UpdatedAt: now},
		{Repo: "acme/console", Number: 110, IsPR: true, State: "open", Title: "Owners tweak", Labels: []string{"stage/050-review-pending"}, HeadSHA: headClean, UpdatedAt: now},
	})
	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	after := s.buildStageTracker(cur, store, now)
	if got := trackerItem(t, after, "pr", 109); got.SentinelAlert {
		t.Fatalf("label removed by webhook must clear sentinel_alert: %+v", got)
	}
	if got := trackerItem(t, after, "pr", 111); !got.SentinelAlert {
		t.Fatalf("label added by webhook must set sentinel_alert: %+v", got)
	}
	if got := trackerItem(t, after, "pr", 110); got.SentinelAlert || got.Head != headClean {
		t.Fatalf("a new head without findings (and no label) clears the evidence-backed alert: alert %v head %q", got.SentinelAlert, got.Head)
	}
}

// TestStageTrackerSentinelConfiguredLabel: the flag follows sentinel.label,
// not the default name.
func TestStageTrackerSentinelConfiguredLabel(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Sentinel: config.SentinelConfig{Label: "security-hold"}}}
	if alert, _, _ := s.stageTrackerSentinel("acme/console", 1, "", []string{"Security-Hold"}); !alert {
		t.Fatal("configured label (case-insensitive) must flag")
	}
	if alert, _, _ := s.stageTrackerSentinel("acme/console", 1, "", []string{config.DefaultSentinelLabel}); alert {
		t.Fatal("the default label is not the configured one")
	}
	if alert, n, reason := s.stageTrackerSentinel("acme/console", 1, "", nil); alert || n != 0 || reason != "" {
		t.Fatal("no label, no head → clean")
	}
}

// TestStageTrackerSentinelLaneWiring pins the markup and CSS surface: the
// constants, the chevron class, the chip class, the Decide… preface and
// that the new CSS names tokens only (ADR-0018).
func TestStageTrackerSentinelLaneWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const STAGE_TRACKER_SENTINEL_WORD = 'Sentinel alert · needs human';`,
		`const STAGE_TRACKER_SENTINEL_CHIP = 'sentinel';`,
		`const STAGE_TRACKER_SENTINEL_TIP = 'Sentinel flagged security-relevant changes';`,
		`function stageTrackerSentinelTip(item)`,
		`function stageTrackerSentinelInLane(item)`,
		`function stageTrackerSentinelChipHtml(item)`,
		`function stageTrackerSentinelPreface(items)`,
		`sentinel ? 'ham-sentinel' : ''`,
		`.ham-badge-sentinel { color: var(--status-error);`,
		`.ham-seg.active.ham-sentinel { background: var(--status-error); color: var(--surface-0); }`,
		`const sentinel = id.indexOf('decide:') === 0 ? stageTrackerSentinelPreface(items) : '';`,
		`message: preface + 'Post "/lgtm" and "/approve" on '`,
		`confirmLabel: 'Post /lgtm + /approve', danger: !!sentinel`,
		`label: preface + 'What should the hive change on '`,
		`if (item.sentinel_alert) lines.push(stageTrackerSentinelTip(item));`,
		`sentinel_alert: !!lp.sentinel_alert, sentinel_reason: lp.sentinel_reason || '', head: lp.head || ''`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing sentinel lane wiring %q", want)
		}
	}
	start := strings.Index(html, "/* Sentinel-flagged PR, #13535 follow-up")
	end := strings.Index(html, ".ham-strip { list-style: none;")
	if start < 0 || end < start {
		t.Fatal("sentinel CSS block not found")
	}
	// Tokens only: the block's single '#' is the issue reference in its comment.
	if css := html[start:end]; strings.Count(css, "#") != 1 || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("sentinel CSS must use tokens only, found a literal colour:\n%s", css)
	}
}

// TestStageTrackerSentinelLaneText runs the row builders under node: a PR
// parked at 085 with sentinel_alert reads "Sentinel alert · needs human"
// in the lit chevron (class ham-sentinel, aria-label + title carrying the
// reason at the head's short sha), the caption line says the same, no
// chip is added there; on any other lane a `sentinel` chip sits in the
// chip area beside the modifier pill with the same tooltip; the generic
// tooltip stands in without a reason; an issue riding one covering PR
// inherits the lane word, a nested sub-row carries its own; a clean row
// has none of it; the Decide… preface names each flagged PR; and the
// keyed reconcile swaps the row in place (same key, no enter animation)
// when the flag flips.
func TestStageTrackerSentinelLaneText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sentinel lane text was NOT executed by this run")
	}
	html := indexHTML(t)
	script := stageTrackerRepoGroupingHarness(t, html) + strings.Join([]string{
		jsFunc(t, html, "stageTrackerSentinelPreface"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_MAX_ROWS"),
		jsConstLine(t, html, "STAGE_TRACKER_EXIT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ENTER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_HEADER_KEY"),
		"var _stageTrackerRowSrc = new WeakMap();",
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerRowSrcKey"),
		jsFunc(t, html, "stageTrackerRowHasMove"),
		jsFunc(t, html, "stageTrackerRowAnimating"),
		jsFunc(t, html, "stageTrackerRowNode"),
		jsFunc(t, html, "stageTrackerRowSrcOf"),
		jsFunc(t, html, "stageTrackerRowMotionOn"),
		jsFunc(t, html, "stageTrackerRowExit"),
		jsFunc(t, html, "stageTrackerRowEnter"),
		jsFunc(t, html, "stageTrackerReconcile"),
	}, "\n") + `
const ctx = { nowMs: now };
const HEAD = 'feedface0123456789abcdef0123456789abcdef';
const flagged = { sentinel_alert: true, sentinel_reason: 'touches OWNERS (+1 more)', head: HEAD };
const TIP = 'Sentinel flagged security-relevant changes at feedfac: touches OWNERS (+1 more)';

// Pure helpers.
assert.strictEqual(stageTrackerSentinelTip(P('o/r', 1, 85, flagged)), TIP);
assert.strictEqual(stageTrackerSentinelTip(P('o/r', 1, 85, { sentinel_alert: true, head: HEAD })), 'Sentinel flagged security-relevant changes at feedfac: ' + STAGE_TRACKER_SENTINEL_TIP_FALLBACK, 'no reason → generic');
assert.strictEqual(stageTrackerSentinelTip(P('o/r', 1, 85, { sentinel_alert: true })), 'Sentinel flagged security-relevant changes: ' + STAGE_TRACKER_SENTINEL_TIP_FALLBACK, 'no head → no sha');
assert.strictEqual(stageTrackerSentinelTip(P('o/r', 1, 85)), '', 'clean → nothing');
assert.ok(stageTrackerSentinelInLane(P('o/r', 1, 85, flagged)));
assert.ok(!stageTrackerSentinelInLane(P('o/r', 1, 50, flagged)), 'CI / Review is not the human-decision lane');
assert.ok(!stageTrackerSentinelInLane(I('o/r', 1, 85, flagged)), 'an issue parked at 085 on its own is not the merge gate');
assert.ok(stageTrackerSentinelInLane(Object.assign(I('o/r', 1, 85, flagged), { via_pr: true })), 'an issue riding its PR is');

// 085 + sentinel: the lane word itself names the alert.
const parked = stageTrackerRowHtml(P('o/r', 109, 85, flagged), stages, ctx);
const lit = /<li class="ham-seg active detoured ham-sentinel"([^>]*)>([\s\S]*?)<\/li>/.exec(parked);
assert.ok(lit, 'the lit merge-gate chevron carries ham-sentinel: ' + parked);
assert.ok(lit[2].includes('<span class="ham-detour">Sentinel alert · needs human</span>'), 'lane text: ' + lit[2]);
assert.ok(!lit[2].includes('Needs human decision'), 'the plain word is replaced, not appended');
assert.ok(lit[1].includes('title="' + TIP + ' — '), 'the chevron tooltip leads with the reason: ' + lit[1]);
assert.ok(lit[1].includes('aria-current="step"'));
assert.ok(lit[1].includes('aria-label="Sentinel alert · needs human — ' + TIP + '"'), 'aria-label carries the wording: ' + lit[1]);
assert.ok(lit[2].includes('aria-label="Next move: human"'), 'the 👤 actor icon stays');
assert.ok(parked.includes('<p class="ham-current-stage">Needs human decision · Sentinel alert · needs human · Next move: human</p>'), 'the compact caption says it too: ' + parked);
assert.ok(!parked.includes('ham-badge-sentinel'), 'no chip when the lane word already says it');
assert.ok(parked.includes(esc(TIP)), 'the PR link tooltip carries the sentinel line');

// Other lanes: chip beside the modifier pill, same tooltip.
const review = stageTrackerRowHtml(P('o/r', 110, 50, Object.assign({ modifiers: ['needs-rebase'] }, flagged)), stages, ctx);
assert.ok(review.includes('<span class="ham-detour">') === false && review.includes('<span class="ham-seg-t">CI / Review</span>'), 'lane word unchanged off the merge gate');
const chips = /<span class="ham-badges">([\s\S]*?)<\/span><\/li>/.exec(review);
assert.ok(chips, review);
assert.ok(/^<span class="ham-badge ham-badge-sentinel" title="[^"]+" aria-label="Sentinel alert — [^"]+">sentinel<\/span><span class="ham-badge ham-badge-mod ham-mod-needs-rebase">needs rebase<\/span>$/.test(chips[1]), 'sentinel chip leads the chip area: ' + chips[1]);
assert.ok(chips[1].includes('title="' + TIP + '"'), chips[1]);
assert.ok(!review.includes('ham-sentinel"'), 'the chevron is not tinted off the merge gate');
const noMods = stageTrackerRowHtml(P('o/r', 110, 50, flagged), stages, ctx);
assert.ok(noMods.includes('ham-badge-sentinel') && !noMods.includes('ham-badge-mod'), 'chip alone when there are no modifiers');
const generic = stageTrackerRowHtml(P('o/r', 110, 71, { sentinel_alert: true }), stages, ctx);
assert.ok(generic.includes('title="Sentinel flagged security-relevant changes: ' + esc(STAGE_TRACKER_SENTINEL_TIP_FALLBACK) + '"'), 'generic tooltip without a reason: ' + generic);

// Clean rows carry none of it.
const clean = stageTrackerRowHtml(P('o/r', 104, 85), stages, ctx);
assert.ok(clean.includes('<span class="ham-detour">Needs human decision</span>') && !clean.includes('entinel'), clean);
assert.ok(!stageTrackerRowHtml(P('o/r', 1, 50, { modifiers: ['needs-rebase'] }), stages, ctx).includes('entinel'));

// An issue with one covering PR rides that PR's lane word; several covering
// PRs put the alert on the flagged sub-row only.
const single = stageTrackerRowHtml(I('o/r', 13, 85, { linked_prs: [Object.assign({ number: 112, stage: 85, title: 'Covers 13' }, flagged)] }), stages, ctx);
assert.ok(single.includes('<span class="ham-detour">Sentinel alert · needs human</span>') && !single.includes('ham-badge-sentinel'), single);
const multi = stageTrackerRowHtml(I('o/r', 14, 20, { linked_prs: [Object.assign({ number: 115, stage: 85, title: 'A' }, flagged), { number: 116, stage: 50, title: 'B' }] }), stages, ctx);
const subs = multi.split('<div class="ham-sub"').slice(1);
assert.strictEqual(subs.length, 2);
assert.ok(subs[0].includes('Sentinel alert · needs human') && subs[0].includes('data-ham-sub-key="o/r/pr#115"'), subs[0]);
assert.ok(!subs[1].includes('entinel'), subs[1]);
assert.ok(!multi.slice(0, multi.indexOf('<div class="ham-sub"')).includes('entinel'), 'the issue line itself is clean');

// Decide… preface: one line per flagged PR, nothing for clean ones.
assert.strictEqual(stageTrackerSentinelPreface([P('o/r', 109, 85, flagged), P('o/r', 104, 85)]), '⚠ o/r#109 — ' + TIP);
assert.strictEqual(stageTrackerSentinelPreface([P('o/r', 104, 85)]), '');
assert.strictEqual(stageTrackerSentinelPreface([]), '');

// Keyed reconcile: the flag flipping swaps the row under the same key with
// no enter animation (no flash), and a clean refresh keeps the node.
let tnow = 0; const timers = [];
global.setTimeout = (fn, ms) => { timers.push({ at: tnow + (ms || 0), fn }); return timers.length; };
function classes() { const set = new Set(); return { set, contains: c => set.has(c), add(...cs) { cs.forEach(c => set.add(c)); }, remove(...cs) { cs.forEach(c => set.delete(c)); } }; }
function base() {
  return { parentNode: null, children: [], dataset: {}, classList: classes(), offsetHeight: 24,
    style: { props: {}, setProperty(k, v) { this.props[k] = v; } },
    addEventListener() {}, removeEventListener() {},
    remove() { if (this.parentNode) { const c = this.parentNode.children; c.splice(c.indexOf(this), 1); this.parentNode = null; } },
    insertBefore(n, ref) { n.remove(); const i = ref ? this.children.indexOf(ref) : this.children.length; this.children.splice(i, 0, n); n.parentNode = this; },
    replaceChild(n, old) { const i = this.children.indexOf(old); n.remove(); this.children[i] = n; n.parentNode = this; old.parentNode = null; },
    querySelector() { return null; } };
}
global.document = { hidden: false, createElement(tag) { const t = { content: {} }; Object.defineProperty(t, 'innerHTML', { set(v) { const n = base(); n.html = v; t.content.firstElementChild = n; } }); return t; } };
const el = base();
const row = it => ({ key: stageTrackerItemKey(it), html: stageTrackerRowHtml(it, stages, ctx) });
const opts = { effect: 'wash', hidden: false };
stageTrackerReconcile(el, [row(P('o/r', 109, 85))], opts);
const first = el.children[0];
assert.ok(!first.html.includes('entinel'));
let r = stageTrackerReconcile(el, [row(P('o/r', 109, 85))], opts);
assert.deepStrictEqual(r, { kept: 1, replaced: 0, entered: 0, exited: 0, deferred: 0 });
r = stageTrackerReconcile(el, [row(P('o/r', 109, 85, flagged))], opts);
assert.deepStrictEqual(r, { kept: 0, replaced: 1, entered: 0, exited: 0, deferred: 0 }, 'flag on → same key, swapped in place');
assert.strictEqual(el.children.length, 1);
assert.notStrictEqual(el.children[0], first);
assert.ok(el.children[0].html.includes('Sentinel alert · needs human'));
assert.ok(!el.children[0].classList.contains('ham-entering'), 'a replaced row does not fade in');
r = stageTrackerReconcile(el, [row(P('o/r', 109, 85))], opts);
assert.deepStrictEqual(r, { kept: 0, replaced: 1, entered: 0, exited: 0, deferred: 0 }, 'flag off → swapped back');
assert.ok(!el.children[0].html.includes('entinel'));
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node sentinel lane fixture failed: %v\n%s", err, out)
	}
}
