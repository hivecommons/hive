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
		jsConstLine(t, html, "STAGE_TRACKER_DEFERRED_NOTE"),
		jsFunc(t, html, "stageTrackerDeferredMods"),
		jsFunc(t, html, "stageTrackerSentinelTip"),
		jsFunc(t, html, "stageTrackerSentinelInLane"),
		jsFunc(t, html, "stageTrackerSentinelAsSuffix"),
		jsFunc(t, html, "stageTrackerReturnedAuthor"),
		jsFunc(t, html, "stageTrackerToolingSuffix"),
		jsFunc(t, html, "stageTrackerReturnedWord"),
		jsFunc(t, html, "stageTrackerDirectionCarrier"),
		jsFunc(t, html, "stageTrackerSentinelChipHtml"),
	}, "\n")
}

// stageTrackerManualMergeJS is the 090 manual-merge surface (the stage
// number, the word and its builder) and the 085 unauthorized one every
// harness that runs stageTrackerSegments needs beside the sentinel one.
func stageTrackerManualMergeJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_READY_FOR_HUMAN_MERGE"),
		jsConstLine(t, html, "STAGE_TRACKER_MANUAL_MERGE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_BREAKING_CHANGE_CODE"),
		jsConstLine(t, html, "STAGE_TRACKER_READY_FOR_HUMAN_MERGE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MERGE_REASON_WORD"),
		jsFunc(t, html, "stageTrackerHumanMergeReasonWord"),
		jsFunc(t, html, "stageTrackerManualMergeWord"),
		jsFunc(t, html, "stageTrackerReadyForHumanWord"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_GATED"),
		jsConstLine(t, html, "STAGE_TRACKER_UNAUTHORIZED_WORD"),
		jsFunc(t, html, "stageTrackerUnauthorizedWord"),
		jsConstLine(t, html, "STAGE_TRACKER_ADOPTED_BY_HIVE"),
		jsFunc(t, html, "stageTrackerAdoptedWord"),
	}, "\n")
}

func writeSentinelBundle(t *testing.T, root, repo string, number int, head string, findings []evidence.SentinelFinding, actions ...evidence.Action) {
	t.Helper()
	path := github.ReviewEvidencePath(root, repo, number, head)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := evidence.Bundle{SchemaVersion: evidence.SchemaVersion, Repo: repo, Number: number, HeadSHA: head, Sentinel: findings, HumanActions: actions}
	if err := evidence.Seal(&b, nil); err != nil {
		t.Fatal(err)
	}
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
	const headCleared = "c1ea4ed00123456789abcdef0123456789abcdef"
	writeSentinelBundle(t, root, "acme/console", 110, headEvidence, []evidence.SentinelFinding{
		{Rule: "sensitive_path", Summary: "touches OWNERS", Paths: []string{"OWNERS"}, Notice: true},
		{Rule: "workflow_permissions", Summary: "widens workflow permissions", Notice: true},
	})
	// A bundle for a superseded head says nothing about the current one.
	writeSentinelBundle(t, root, "acme/console", 111, "0ld0ld0ld", []evidence.SentinelFinding{{Rule: "test_removal", Summary: "deletes tests"}})
	// A blocking alert on the current head whose label a maintainer removed:
	// the sweep only re-applies it on a new head, so the row is not flagged.
	writeSentinelBundle(t, root, "acme/console", 120, headCleared, []evidence.SentinelFinding{{Rule: "sensitive_path", Summary: "touches a workflow"}})
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		// Label only, parked on a person.
		FrontendPR{PullRequest: github.PullRequest{Number: 109, Title: "Widen perms", Author: "mallory", HeadSHA: headClean, Labels: []string{"stage/085-needs-decision", config.DefaultSentinelLabel}}},
		// Evidence only (a trusted author's notice: comment, no label), in review.
		FrontendPR{PullRequest: github.PullRequest{Number: 110, Title: "Owners tweak", Author: "pat", HeadSHA: headEvidence, Labels: []string{"stage/050-review-pending"}}},
		// Stale evidence on an older head: clean.
		FrontendPR{PullRequest: github.PullRequest{Number: 111, Title: "Re-pushed", Author: "pat", HeadSHA: headClean, Labels: []string{"stage/050-review-pending"}}},
		// Alert evidence on the current head, label removed by a maintainer.
		FrontendPR{PullRequest: github.PullRequest{Number: 120, Title: "Bump action", Author: "renovate[bot]", HeadSHA: headCleared, Labels: []string{"stage/085-needs-decision"}}},
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
	// Cleared: no alert, but the findings still fill the tooltip (#14579).
	if cleared := trackerItem(t, resp, "pr", 120); cleared.SentinelAlert || cleared.SentinelFindings != 1 || cleared.SentinelReason != "touches a workflow" {
		t.Fatalf("an alert a maintainer cleared on this head must not flag PR 120: %+v", cleared)
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

// TestStageTrackerSentinelLabelClearsBlockingBundle pins the fix for the
// stale "Sentinel alert · needs human" pill: a blocking finding (the
// sweep applied the label) whose label a maintainer has since removed is
// cleared — alert follows the live label, the bundle only fills the count
// and reason — while a notice-only finding (trusted author, no label ever)
// keeps flagging on the bundle alone, and a notice-only finding beside a
// blocking one still flags.
func TestStageTrackerSentinelLabelClearsBlockingBundle(t *testing.T) {
	prev := github.ReviewEvidenceRoot
	root := filepath.Join(t.TempDir(), "evidence")
	github.ReviewEvidenceRoot = root
	t.Cleanup(func() { github.ReviewEvidenceRoot = prev })
	s := newTestServer()
	verifier := &github.ReviewEvidenceVerifier{}
	const head = "d7fde650a3992abad2a2a2c55076ac18afeb5540"
	blocking := []evidence.SentinelFinding{
		{Rule: "sensitive_path", Summary: "changes 1 sensitive path(s)", Paths: []string{".github/workflows/mutation.yml"}},
		{Rule: "workflow_permissions", Summary: "widens workflow permissions"},
	}
	notice := []evidence.SentinelFinding{{Rule: "sensitive_path", Summary: "touches OWNERS", Notice: true}}
	mixed := []evidence.SentinelFinding{blocking[0], notice[0]}

	ack := evidence.Action{Actor: "clubanderson", Kind: evidence.ActionSentinelAcknowledged, Detail: "approved from the Hive dashboard", At: time.Date(2026, 10, 28, 12, 50, 0, 0, time.UTC)}
	other := evidence.Action{Actor: "clubanderson", Kind: "label_removed", At: ack.At}

	tests := []struct {
		name         string
		number       int
		findings     []evidence.SentinelFinding
		actions      []evidence.Action
		labels       []string
		wantAlert    bool
		wantFindings int
		wantReason   string
	}{
		// The owner's Approve acknowledged the findings on this head
		// (hivecommons/crux#30 had no label, so nothing else could clear it).
		{name: "notice-only bundle acknowledged by owner approval", number: 406, findings: notice, actions: []evidence.Action{other, ack}, labels: []string{"stage/085-needs-decision"}, wantAlert: false, wantFindings: 1, wantReason: "touches OWNERS — acknowledged by @clubanderson"},
		{name: "blocking bundle acknowledged, label still on", number: 407, findings: blocking, actions: []evidence.Action{ack}, labels: []string{config.DefaultSentinelLabel}, wantAlert: true, wantFindings: 2, wantReason: "changes 1 sensitive path(s) (+1 more) — acknowledged by @clubanderson"},
		{name: "unrelated action does not acknowledge", number: 408, findings: notice, actions: []evidence.Action{other}, labels: nil, wantAlert: true, wantFindings: 1, wantReason: "touches OWNERS"},
		{name: "blocking bundle, label still on", number: 401, findings: blocking, labels: []string{"lgtm", config.DefaultSentinelLabel}, wantAlert: true, wantFindings: 2, wantReason: "changes 1 sensitive path(s) (+1 more)"},
		{name: "blocking bundle, label removed by maintainer", number: 402, findings: blocking, labels: []string{"lgtm", "stage/085-needs-decision"}, wantAlert: false, wantFindings: 2, wantReason: "changes 1 sensitive path(s) (+1 more)"},
		{name: "notice-only bundle, never labelled", number: 403, findings: notice, labels: []string{"stage/050-review-pending"}, wantAlert: true, wantFindings: 1, wantReason: "touches OWNERS"},
		{name: "notice beside blocking, label removed", number: 404, findings: mixed, labels: nil, wantAlert: true, wantFindings: 2, wantReason: "changes 1 sensitive path(s) (+1 more)"},
		{name: "no bundle, no label", number: 405, labels: []string{"lgtm"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.findings != nil {
				writeSentinelBundle(t, root, "acme/console", tt.number, head, tt.findings, tt.actions...)
			}
			alert, findings, reason := s.stageTrackerSentinel(verifier, "acme/console", tt.number, head, tt.labels)
			if alert != tt.wantAlert || findings != tt.wantFindings || reason != tt.wantReason {
				t.Fatalf("stageTrackerSentinel = alert %v findings %d reason %q, want %v %d %q", alert, findings, reason, tt.wantAlert, tt.wantFindings, tt.wantReason)
			}
		})
	}
}

// TestStageTrackerSentinelConfiguredLabel: the flag follows sentinel.label,
// not the default name.
func TestStageTrackerSentinelConfiguredLabel(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Sentinel: config.SentinelConfig{Label: "security-hold"}}}
	if alert, _, _ := s.stageTrackerSentinel(nil, "acme/console", 1, "", []string{"Security-Hold"}); !alert {
		t.Fatal("configured label (case-insensitive) must flag")
	}
	if alert, _, _ := s.stageTrackerSentinel(nil, "acme/console", 1, "", []string{config.DefaultSentinelLabel}); alert {
		t.Fatal("the default label is not the configured one")
	}
	if alert, n, reason := s.stageTrackerSentinel(nil, "acme/console", 1, "", nil); alert || n != 0 || reason != "" {
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
		`sentinel || sentinelSuffix ? 'ham-sentinel' : ''`,
		`.ham-badge-sentinel { color: var(--status-error);`,
		`.ham-seg.active.ham-sentinel { background: var(--status-error); color: var(--surface-0); }`,
		`const sentinel = id.indexOf('decide:') === 0 ? stageTrackerSentinelPreface(items) : '';`,
		`message: preface + 'Post "/lgtm" and "/approve" on '`,
		`confirmLabel: 'Post /lgtm + /approve', danger: !!sentinel`,
		`label: preface + 'What should the hive change on '`,
		`if (item.sentinel_alert && deferred.indexOf(STAGE_TRACKER_SENTINEL_CHIP) < 0) lines.push(stageTrackerSentinelTip(item));`,
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
assert.ok(generic.includes('Sentinel flagged security-relevant changes: ' + esc(STAGE_TRACKER_SENTINEL_TIP_FALLBACK)), 'generic tooltip without a reason: ' + generic);

// 071/072 + sentinel (hivecommons/hive#15159): the chevron keeps the
// routing word — the human already acted — with the alert as a suffix,
// never "needs human"; the chip is folded into the word.
assert.strictEqual(stageTrackerReturnedWord({ stage: 72, kind: 'pr', author: 'thatguywob', author_kind: 'clanker' }, 'Returned to contributor'), "Returned to @thatguywob's clanker");
assert.strictEqual(stageTrackerReturnedWord({ stage: 72, kind: 'pr', author: 'bee', author_kind: 'hive' }, 'Returned to contributor'), "Returned to @bee's hive");
// An issue row drawn through its one covering PR names the PR's author,
// not the issue's (hive's hive: #15133 was filed by hivecommons-hive[bot];
// its PR #15307 is @thatguywob's clanker's).
const viaPR = { stage: 72, kind: 'issue', via_pr: true, number: 15133, author: 'hivecommons-hive[bot]', author_kind: 'hive', linked_prs: [{ number: 15307, author: 'thatguywob', author_kind: 'clanker', stage: 72 }] };
assert.strictEqual(stageTrackerReturnedWord(viaPR, 'Returned to contributor'), "Returned to @thatguywob's clanker");
assert.strictEqual(stageTrackerReturnedWord(Object.assign({}, viaPR, { stage: 71 }), 'Needs author'), 'Needs @thatguywob');
// A PR drawn under another issue is not this row's; only the issue's own
// author is left to name.
assert.strictEqual(stageTrackerReturnedWord(Object.assign({}, viaPR, { linked_prs: [Object.assign({}, viaPR.linked_prs[0], { drawn_under: 15100 })] }), 'Returned to contributor'), "Returned to @hivecommons-hive[bot]'s hive");
// A row the sweep marked superseded says so instead of "Returned to":
// nothing is owed by the author; a maintainer closes it (#15303 / #15304).
assert.strictEqual(stageTrackerReturnedWord({ stage: 72, kind: 'pr', author: 'thatguywob', author_kind: 'clanker', superseded: { reason: 'superseded', closer_pr: 15304, closer_repo: 'hive' } }, 'Returned to contributor'), 'Superseded by #15304 · decide');
assert.strictEqual(stageTrackerReturnedWord(Object.assign({ stage: 72, kind: 'pr', author: 'thatguywob', author_kind: 'clanker', superseded: { closer_pr: 15304 } }, flagged), 'x'), 'Superseded by #15304 · decide · sentinel');
assert.strictEqual(stageTrackerReturnedWord({ stage: 72, kind: 'pr', author: 'thatguywob', author_kind: 'clanker', superseded: true }, 'Returned to contributor'), "Returned to @thatguywob's clanker", 'a bare nested flag names no closer');
assert.strictEqual(stageTrackerReturnedWord({ stage: 72, kind: 'pr', author: 'sam', author_kind: 'human' }, 'Returned to contributor'), 'Returned to @sam');
assert.strictEqual(stageTrackerReturnedWord({ stage: 71, kind: 'pr', author: 'sam' }, 'Needs author'), 'Needs @sam');
assert.strictEqual(stageTrackerReturnedWord({ stage: 72, kind: 'pr' }, 'Returned to contributor'), 'Returned to contributor', 'no author → stage word');
assert.strictEqual(stageTrackerReturnedWord(Object.assign({ stage: 72, kind: 'pr', author: 'thatguywob', author_kind: 'clanker' }, flagged), 'x'), "Returned to @thatguywob's clanker · sentinel");
assert.ok(stageTrackerSentinelAsSuffix(P('o/r', 1, 72, flagged)) && stageTrackerSentinelAsSuffix(P('o/r', 1, 71, flagged)));
assert.ok(!stageTrackerSentinelAsSuffix(P('o/r', 1, 85, flagged)) && !stageTrackerSentinelAsSuffix(I('o/r', 1, 72, flagged)));
const returned = stageTrackerRowHtml(P('o/r', 15159, 72, Object.assign({ author: 'thatguywob', author_kind: 'clanker' }, flagged)), stages, ctx);
const retLit = /<li class="ham-seg active detoured ham-sentinel"([^>]*)>([\s\S]*?)<\/li>/.exec(returned);
assert.ok(retLit, 'the lit 072 chevron is tinted: ' + returned);
assert.ok(retLit[2].includes("<span class=\"ham-detour\">Returned to @thatguywob&#39;s clanker · sentinel</span>"), 'routing word with sentinel suffix: ' + retLit[2]);
assert.ok(!returned.includes('needs human') && !returned.includes('Needs human'), 'no needs-human wording at 072: ' + returned);
assert.ok(retLit[1].includes("aria-label=\"Returned to @thatguywob&#39;s clanker · sentinel — " + TIP + '"'), 'aria names the routing: ' + retLit[1]);
assert.ok(retLit[1].includes(' — ' + TIP), 'the tooltip carries the reason: ' + retLit[1]);
assert.ok(!returned.includes('ham-badge-sentinel'), 'no chip when the word carries the suffix');
const needsAuthor = stageTrackerRowHtml(P('o/r', 15160, 71, Object.assign({ author: 'sam', author_kind: 'human' }, flagged)), stages, ctx);
assert.ok(needsAuthor.includes('<span class="ham-detour">Needs @sam · sentinel</span>') && !needsAuthor.includes('needs human'), needsAuthor);
const returnedClean = stageTrackerRowHtml(P('o/r', 15161, 72, { author: 'thatguywob', author_kind: 'clanker' }), stages, ctx);
assert.ok(returnedClean.includes("<span class=\"ham-detour\">Returned to @thatguywob&#39;s clanker</span>") && !returnedClean.includes('ham-sentinel') && !returnedClean.includes('entinel'), returnedClean);
// 085 still says it in full.
assert.ok(stageTrackerRowHtml(P('o/r', 15162, 85, flagged), stages, ctx).includes('<span class="ham-detour">Sentinel alert · needs human</span>'));
// Give direction on a clanker PR is carried to the contributor's clanker,
// not to the hive's own fixer.
assert.strictEqual(stageTrackerDirectionCarrier([{ kind: 'pr', author: 'thatguywob', author_kind: 'clanker' }]), "@thatguywob's clanker");
assert.strictEqual(stageTrackerDirectionCarrier([{ kind: 'pr', author: '@a', author_kind: 'clanker' }, { kind: 'pr', author: 'b', author_kind: 'clanker' }]), "the contributor's clanker");
assert.strictEqual(stageTrackerDirectionCarrier([{ kind: 'pr', author: 'x', author_kind: 'clanker' }, { kind: 'pr', author: 'hive[bot]', author_kind: 'hive' }]), 'the author agent');
assert.strictEqual(stageTrackerDirectionCarrier([]), 'the author agent');

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

// 090 under a manual-merge rule a trusted merger approved: the merge-gate
// chevron says what is owed and who approved, with no sentinel word left.
assert.strictEqual(stageTrackerManualMergeWord({ human_approver: 'clubanderson' }), 'Awaiting human merge · approved by @clubanderson');
assert.strictEqual(stageTrackerManualMergeWord({}), 'Awaiting human merge');
const manual = stageTrackerRowHtml(P('o/r', 401, 90, { manual_merge: true, human_approver: 'clubanderson', reason: 'contributor-lane:workflow-path:human-approved@clubanderson' }), stages, ctx);
assert.ok(manual.includes('<span class="ham-detour">Awaiting human merge · approved by @clubanderson</span>'), 'manual-merge 090 lane word: ' + manual);
assert.ok(!manual.includes('entinel') && !manual.includes('Needs human decision'), manual);
const plain090 = stageTrackerRowHtml(P('o/r', 402, 90, { reason: 'untrusted-author-role' }), stages, ctx);
assert.ok(!plain090.includes('Awaiting human merge') && plain090.includes('<span class="ham-detour">Ready for human merge</span>'), 'a trust-blocked 090 keeps the stage word: ' + plain090);

// Every 090 word says WHY a person merges (hivecommons/hive#14658): the
// payload's human_merge_reason code becomes the word's tail, its
// human_merge_reason_text leads the lit chevron's tooltip, and a nested
// linked PR carries the same.
assert.strictEqual(stageTrackerHumanMergeReasonWord({ human_merge_reason: 'workflow-files' }), 'workflow files');
assert.strictEqual(stageTrackerHumanMergeReasonWord({ human_merge_reason: 'some-new-code' }), 'some new code');
assert.strictEqual(stageTrackerHumanMergeReasonWord({}), '');
assert.strictEqual(stageTrackerManualMergeWord({ human_approver: 'clubanderson', human_merge_reason: 'workflow-files' }), 'Awaiting human merge · workflow files · approved by @clubanderson');
const whyTip = 'Changes .github/workflows/** — the hive never merges workflow changes; click Merge on GitHub';
const why = stageTrackerRowHtml(P('o/r', 401, 90, { manual_merge: true, human_approver: 'clubanderson', human_merge_reason: 'workflow-files', human_merge_reason_text: whyTip }), stages, ctx);
assert.ok(why.includes('<span class="ham-detour">Awaiting human merge · workflow files · approved by @clubanderson</span>'), 'manual-merge 090 word carries the reason: ' + why);
assert.ok(why.includes('title="Awaiting human merge · workflow files · approved by @clubanderson — ' + whyTip), 'the lit 090 chevron tooltip leads with the reason: ' + why);
const whyPlain = stageTrackerRowHtml(P('o/r', 402, 90, { human_merge_reason: 'untrusted-author', human_merge_reason_text: 'No lane can vouch for the author' }), stages, ctx);
assert.ok(whyPlain.includes('<span class="ham-detour">Ready for human merge · untrusted author</span>') && whyPlain.includes('title="Ready for human merge · untrusted author — No lane can vouch for the author'), 'plain 090 word carries the reason: ' + whyPlain);
const whyNested = stageTrackerRowHtml(I('o/r', 400, 90, { linked_prs: [{ number: 401, stage: 90, title: 'Shard', manual_merge: true, human_approver: 'clubanderson', human_merge_reason: 'workflow-files', human_merge_reason_text: whyTip }] }), stages, ctx);
assert.ok(whyNested.includes('Awaiting human merge · workflow files · approved by @clubanderson'), 'nested 090 PR carries the reason: ' + whyNested);
// The breaking-change rule (hivecommons/hive#15442): the stage's own word
// leads and the chip says "human merge"; an approval changes nothing.
assert.strictEqual(stageTrackerManualMergeWord({ manual_merge: true, human_merge_reason: 'breaking-change' }), 'Ready for human merge · human merge');
const breaking = stageTrackerRowHtml(P('o/r', 403, 90, { manual_merge: true, human_merge_reason: 'breaking-change', human_merge_reason_text: 'Breaking change — owner merges' }), stages, ctx);
assert.ok(breaking.includes('<span class="ham-detour">Ready for human merge · human merge</span>') && !breaking.includes('Awaiting human merge'), 'breaking-change 090 word: ' + breaking);

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
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node sentinel lane fixture failed: %v\n%s", err, out)
	}
}
