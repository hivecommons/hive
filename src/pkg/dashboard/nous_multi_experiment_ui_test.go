package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNousMultiExperimentCardsUI pins hivecommons/hive#12532 in the
// dashboard: running experiments render as Inception-style campaign rows
// (question, scope pills, per-card baseline "N / 672 (≈ 7 days …)" with an ⓘ
// explainer, status pill, edit/pause/resume/archive as icon buttons), "+ New
// experiment" opens an inline form whose scope pills are the ⚙ allow-list
// ("Repos experiments may target"), a 409 scope_conflict renders
// an in-app styled warning (never window.alert/confirm) offering to pause
// the other experiment or narrow the scope, and the collapsed summary
// counts open experiments.
func TestNousMultiExperimentCardsUI(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div class="campaigns-card nous-exp-cards">`,
		`<div class="campaigns-title">Experiments</div>`,
		`data-action="nousOpenNewExperiment"`,
		"case 'nousEditExperiment': nousEditExperiment(el.dataset.arg0); break;",
		"const NOUS_REPO_ALLOWLIST_HEADING = '📂 Repos experiments may target';",
		"const NOUS_REPO_ALLOWLIST_COPY = 'Experiments can only choose repos from this list; none selected = any repo.';",
		`nousLabCardHead('Scope', NOUS_REPO_ALLOWLIST_HEADING, NOUS_REPO_ALLOWLIST_COPY + ' Click a repo to toggle it; changes apply on Save.')`,
		`<details class="faq-q" id="faq-create-experiment">`,
		"<summary>Where do I create an experiment?</summary>",
		`<details class="faq-q" id="faq-edit-experiment">`,
		"<summary>Can I change an experiment after it starts?</summary>",
		"if (r.status === 409 && j.code === 'restart_required') {",
		".nous-exp-icon { min-width: 28px; min-height: 28px;",
		".nous-exp-icon:focus-visible { outline: 2px solid var(--accent);",
		`data-action="nousPauseConflictAndStart"`,
		`data-action="nousNarrowScope"`,
		"case 'nousOpenNewExperiment': nousOpenNewExperiment(); break;",
		"case 'nousGenerateProposals': nousGenerateProposals(el.dataset.arg0); break;",
		"body: JSON.stringify({ mode: 'suggest' }),",
		"Add an experiment: enter a research question and pick the repos it may touch.",
		"Changing the question or scope restarts this experiment: baseline resets to 0/${NOUS_BASELINE_TARGET_DEFAULT} (≈7 days) and hypotheses/proposals so far are archived with the old version. Continue?",
		"confirmLabel: 'Restart experiment',",
		"cancelLabel: 'Keep as is',",
		"case 'nousCampaignAction': nousCampaignAction(el.dataset.arg0, el.dataset.arg1); break;",
		"case 'nousPauseConflictAndStart': nousPauseConflictAndStart(el.dataset.arg0); break;",
		"fetch('/api/nous/campaigns', {",
		"fetch('/api/nous/campaigns/' + encodeURIComponent(id), {",
		"if (r.status === 409 && j.code === 'scope_conflict') {",
		"const NOUS_BASELINE_TARGET_EXPLAINER = '≈ 7 days of observations (4/hour)';",
		`<details class="faq-q" id="faq-multiple-experiments">`,
		"<summary>Can I run more than one experiment at once?</summary>",
		".nous-conflict-warn {",
		".nous-exp-row .campaign-row-title {",
		`.nous-exp-stage[data-loop-idle="1"] .nous-exp-stage-label { color: var(--yellow); }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// kellya's report: the lab told a user with a question and a 100%
	// baseline "No defined experiments yet — switch to Suggest…". That copy
	// is gone; the empty state says what to do and the stage line on each
	// card says what happens next.
	if strings.Contains(html, "No defined experiments yet") || strings.Contains(html, "switch to Suggest after baseline completes") {
		t.Error("index.html still tells the user to go find the mode knob; cards carry the stage and a Generate proposals action instead")
	}
	// The conflict path must be in-app: no native dialogs anywhere in the
	// experiment-card code.
	for _, fn := range []string{"nousSubmitNewExperiment", "nousPauseConflictAndStart", "nousCampaignAction", "nousConflictWarning", "nousGenerateProposals", "nousEditExperiment"} {
		body := extractJSFunction(t, html, fn)
		for _, banned := range []string{"window.alert", "window.confirm", "window.prompt", " alert(", " confirm(", " prompt("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s uses native dialog %q; use the in-app warning / hiveConfirm", fn, banned)
			}
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: experiment card rendering was not executed")
	}
	script := `const assert = require('node:assert/strict');
function escapeHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function formatDashboardDateTime(ts){return new Date(ts).toISOString().slice(0,16);}
` + extractJSFunction(t, html, "nousCampaignDate") + "\n    }\n" +
		extractJSFunction(t, html, "nousResearchQuestionText") + "\n    }\n" +
		extractJSFunction(t, html, "nousProgressBar") + "\n    }\n" +
		extractJSFunction(t, html, "nousRepoPillClass") + "\n    }\n" +
		extractJSFunction(t, html, "nousRepoPillTitle") + "\n    }\n" +
		extractJSFunction(t, html, "nousBaselineTargetTip") + "\n    }\n" +
		extractJSFunction(t, html, "nousBaselineInfo") + "\n    }\n" +
		extractJSFunction(t, html, "nousScopePills") + "\n    }\n" +
		extractJSFunction(t, html, "nousExperimentStage") + "\n    }\n" +
		extractJSFunction(t, html, "nousPrinciplesInfo") + "\n    }\n" +
		extractJSFunction(t, html, "nousFmtDuration") + "\n    }\n" +
		extractJSFunction(t, html, "nousMetricValue") + "\n    }\n" +
		extractJSFunction(t, html, "nousMetricLine") + "\n    }\n" +
		extractJSFunction(t, html, "nousOutputLinks") + "\n    }\n" +
		extractJSFunction(t, html, "nousExperimentProgressRows") + "\n    }\n" +
		extractJSFunction(t, html, "nousAllowedRepos") + "\n    }\n" +
		extractJSFunction(t, html, "nousScopeWarning") + "\n    }\n" +
		extractJSFunction(t, html, "nousExpIconButton") + "\n    }\n" +
		extractJSFunction(t, html, "nousEditIsMaterial") + "\n    }\n" +
		extractJSFunction(t, html, "nousExperimentCard") + "\n    }\n" +
		extractJSFunction(t, html, "nousNewExperimentForm") + "\n    }\n" +
		extractJSFunction(t, html, "nousConflictWarning") + "\n    }\n" +
		extractJSFunction(t, html, "nousExperimentCards") + "\n    }\n" +
		extractJSFunction(t, html, "nousSectionSummaryText") + "\n    }\n" + `
const NOUS_BASELINE_TARGET_DEFAULT = 672;
const NOUS_BASELINE_TARGET_EXPLAINER = '≈ 7 days of observations (4/hour)';
const NOUS_PRINCIPLES_EXPLAINER = 'Principles are durable lessons the lab distils from judged experiments (principles.json). A new lab starts at 0 and the count grows as experiments conclude; none are needed to start an experiment.';
let _nousNewExp = { open: false, conflict: null, pending: false, editId: 0, original: null };

const campaigns = [
  { id: 1, question: 'research_question: > Cut CI time by 20%?', hypothesis: 'shard the slowest package', status: 'active', startedAt: '2026-10-10T10:00:00Z', snapshots: 96, target: 672, percent: 14.3, targetExplainer: '≈ 7 days of observations (4/hour)', experiments: 2, scope: { repos: ['acme/api'], intervention: 'ci', tags: ['ci'], hiveWide: false }, stage: 'baseline', stageLabel: 'collecting baseline 96 / 672', stageDetail: 'Proposals are designed only once the baseline is complete (≈ 7 days of observations (4/hour)).' },
  { id: 2, question: 'Fewer flaky tests?', status: 'paused', startedAt: '2026-10-12T10:00:00Z', snapshots: 900, target: 672, percent: 100, complete: true, scope: { repos: [], hiveWide: true }, stage: 'paused', stageLabel: 'paused' },
  { id: 3, question: 'Old one', status: 'archived', startedAt: '2026-09-01T10:00:00Z', endedAt: '2026-10-01T10:00:00Z', snapshots: 672, scope: { repos: ['acme/web'] } },
];
const status = { mode: 'suggest', campaigns, projectRepos: ['acme/api', 'acme/web'], repoTargets: ['acme/api'] };

// Card list: only running/paused campaigns, one row each, Inception classes.
let list = nousExperimentCards(campaigns, status);
assert.match(list, /class="campaigns-card nous-exp-cards"/);
assert.equal((list.match(/class="campaign-row nous-exp-row"/g) || []).length, 2, 'archived campaigns are not cards');
assert.match(list, /\+ New experiment/);
assert.doesNotMatch(list, /nous-new-exp"/, 'form closed by default');

// Card #1: cleaned question, scope pills, baseline with the 672 explainer and ⓘ,
// edit / pause / archive as icon buttons with aria-label + title.
const c1 = nousExperimentCard(campaigns[0], status);
assert.match(c1, /<div class="campaign-row-title">#1 · Cut CI time by 20%\?<\/div>/);
assert.doesNotMatch(c1, /research_question/);
assert.match(c1, /Hypothesis: shard the slowest package/);
assert.match(c1, /class="campaign-pill nous-exp-pill" data-status="active">active</);
assert.match(c1, /class="campaign-pill nous-scope-pill" title="acme\/api">api</);
assert.match(c1, /nous-scope-pill" title="Intervention kind">ci</);
assert.match(c1, /nous-scope-pill">#ci</);
assert.match(c1, /Baseline 96 \/ 672 \(≈ 7 days of observations \(4\/hour\)\)/);
assert.match(c1, /ⓘ<span id="nous-baseline-tip-1" class="config-tooltip" role="tooltip">672 baseline snapshots ≈ 7 days of observations \(4\/hour\): one hive-state snapshot per governor evaluation cycle, 4 an hour × 24 hours × 7 days/);
assert.match(c1, /role="progressbar" aria-label="Baseline for experiment #1"[^>]*aria-valuenow="14"/);
assert.match(c1, /<button type="button" class="hv-btn btn-icon btn-sm nous-exp-icon nous-exp-icon-edit" data-action="nousEditExperiment" data-arg0="1" aria-label="Edit experiment" title="Edit experiment"><span aria-hidden="true">✎<\/span><\/button>/);
assert.match(c1, /class="hv-btn btn-icon btn-sm nous-exp-icon nous-exp-icon-pause" data-action="nousCampaignAction" data-arg0="1" data-arg1="pause" aria-label="Pause experiment" title="Pause experiment"><span aria-hidden="true">⏸<\/span>/);
assert.match(c1, /class="hv-btn btn-icon btn-sm nous-exp-icon nous-exp-icon-archive" data-action="nousCampaignAction" data-arg0="1" data-arg1="archive" aria-label="Archive experiment" title="Archive experiment"><span aria-hidden="true">🗄<\/span>/);
assert.doesNotMatch(c1, /data-arg1="resume"/);
assert.doesNotMatch(c1, />Pause<|>Archive<|>Edit</, 'row actions are icons, the verb is in aria-label/title');
assert.doesNotMatch(c1, /nous-exp-version/, 'a first version has no lineage line');
assert.doesNotMatch(c1, /nous-exp-scope-warn/, 'acme/api is in the allow-list');
// Scope repo dropped from the allow-list later: muted warning, scope kept.
const c1gone = nousExperimentCard(campaigns[0], Object.assign({}, status, { repoTargets: ['acme/web'] }));
assert.match(c1gone, /<div class="nous-exp-scope-warn text-muted" role="note">acme\/api is no longer in the lab's allow-list \(⚙ Repos experiments may target\)\. This experiment keeps its scope; new experiments cannot choose it\.<\/div>/);
assert.match(c1gone, /nous-scope-pill" title="acme\/api">api</, 'scope pills still show the repo');
assert.equal(nousScopeWarning(campaigns[0], { repoTargets: [] }), '', 'empty allow-list = any repo, nothing to warn about');
assert.match(nousScopeWarning({ scope: { repos: ['a/x', 'a/y'] } }, { repoTargets: ['a/z'] }), /a\/x, a\/y are no longer .* cannot choose them\./);
// Restarted experiment: version line.
const v2 = nousExperimentCard(Object.assign({}, campaigns[0], { id: 7, version: 2, restartedFrom: 1 }), status);
assert.match(v2, /<div class="nous-exp-version text-muted">v2 · restarted from #1<\/div>/);

// Stage line: server-derived label + detail; the kellya scenario (question
// set, baseline 100%, lab in observe) gets ONE primary action.
assert.match(c1, /<div class="nous-exp-stage" data-stage="baseline"><span class="nous-exp-stage-label">collecting baseline 96 \/ 672<\/span><span class="nous-exp-stage-detail">Proposals are designed only once the baseline is complete/);
assert.doesNotMatch(c1, /Generate proposals/);
const ready = { id: 4, question: 'Tell me if I remove /clear from the agent prompt the queue drains faster', status: 'active', startedAt: '2026-10-01T10:00:00Z', snapshots: 874, target: 672, percent: 100, complete: true, scope: { repos: [], hiveWide: true }, stage: 'ready', stageLabel: 'ready — generate proposals', stageDetail: 'Baseline complete. The lab is in Observe mode (data collection only); Generate proposals switches it to Suggest so proposals can be designed for your review.', nextAction: 'generate_proposals' };
const c4 = nousExperimentCard(ready);
assert.match(c4, /data-stage="ready"><span class="nous-exp-stage-label">ready — generate proposals</);
assert.match(c4, /<button class="hv-btn btn-primary btn-sm" data-action="nousGenerateProposals" data-arg0="4"[^>]*>Generate proposals</, 'the one call to action stays a labelled button');
assert.match(c4, /Baseline 672 \/ 672/);
// Once in suggest the server drops nextAction: no button, honest waiting copy.
const waiting = nousExperimentCard(Object.assign({}, ready, { stageLabel: 'ready — generating proposals', stageDetail: 'Baseline complete and the lab is in Suggest mode. Proposals appear here as soon as the experiment loop designs them; nothing more is needed from you.', nextAction: '' }));
assert.doesNotMatch(waiting, /Generate proposals</);
assert.match(waiting, /nothing more is needed from you/);
assert.doesNotMatch(waiting, /data-loop-idle|How to run the experiment loop/);
// #12541: the hive designs no proposals itself. When the server reports the
// external experiment loop idle, the card says so and links the docs
// instead of promising proposals forever.
const idle = nousExperimentCard(Object.assign({}, ready, { stageLabel: 'ready — the experiment loop is not running on this hive', stageDetail: 'Baseline complete and the lab is in Suggest mode, but the experiment loop has written nothing to /var/run/nous/governor in the last 24 evaluation cycles.', nextAction: '', experimentLoopIdle: true }));
assert.match(idle, /<div class="nous-exp-stage" data-stage="ready" data-loop-idle="1"><span class="nous-exp-stage-label">ready — the experiment loop is not running on this hive<\/span>/);
assert.match(idle, /<a class="nous-exp-stage-docs" href="https:\/\/github\.com\/hivecommons\/hive\/blob\/v5\/src\/docs\/dashboard-sections\.md#experiment-lab" target="_blank" rel="noopener noreferrer">How to run the experiment loop<\/a>/);
assert.doesNotMatch(idle, /generating proposals|Generate proposals</);
// Older servers without stage fields: derived locally, never blank.
assert.match(nousExperimentCard({ id: 5, question: 'q', status: 'active', snapshots: 10, target: 672 }), /data-stage="baseline"><span class="nous-exp-stage-label">collecting baseline 10 \/ 672</);
assert.match(nousExperimentCard({ id: 6, question: 'q', status: 'active', snapshots: 700, target: 672, complete: true }), /data-stage="ready"/);

// No "Experiment Progress" empty box: with no proposals the rows render
// nothing — the cards carry stage and next action.
assert.equal(nousExperimentProgressRows([]), '');
assert.equal(nousExperimentProgressRows(null), '');
assert.match(nousExperimentProgressRows([{ id: 'exp-1', title: 'shard tests', phase: 'experiment', progressPct: 40 }]), /nous-exp-row/);

// Principles ⓘ explains what the tile counts and that 0 is fine.
const pinfo = nousPrinciplesInfo();
assert.match(pinfo, /aria-label="What principles are">ⓘ<span id="nous-principles-tip" class="config-tooltip" role="tooltip">Principles are durable lessons the lab distils from judged experiments \(principles\.json\)\. A new lab starts at 0 .* none are needed to start an experiment\.<\/span>/);
assert.match(nousPrinciplesInfo('server copy'), />server copy</);

// Card #2: paused + hive-wide + complete, count capped at the target, Resume icon instead of Pause.
const c2 = nousExperimentCard(campaigns[1]);
assert.match(c2, /data-status="paused">paused</);
assert.match(c2, /nous-scope-pill"[^>]*>hive-wide</);
assert.match(c2, /Baseline 672 \/ 672 \(≈ 7 days of observations \(4\/hour\)\) · complete/);
assert.match(c2, /nous-exp-icon-resume" data-action="nousCampaignAction" data-arg0="2" data-arg1="resume" aria-label="Resume experiment" title="Resume experiment"><span aria-hidden="true">▶</);
assert.doesNotMatch(c2, /data-arg1="pause"/);

// Collapsed summary counts open experiments (not archived, not ledger rows).
assert.equal(nousSectionSummaryText({ status, ledger: [1, 2, 3, 4, 5] }), 'suggest · 2 experiments');
assert.equal(nousSectionSummaryText({ status: { mode: 'observe' }, ledger: [1] }), 'observe · 1 experiment');
assert.equal(nousSectionSummaryText({ status: { status: 'not_configured' } }), '—');

// "+ New experiment" opens the inline form; its scope pills are the ⚙
// allow-list (repoTargets), none preselected (= hive-wide), Start enabled.
_nousNewExp = { open: true, conflict: null, pending: false, editId: 0, original: null };
list = nousExperimentCards(campaigns, status);
assert.match(list, /id="nous-new-exp" role="form" aria-label="New experiment"/);
assert.match(list, /id="nous-ne-question"/);
assert.match(list, /class="nous-config-pill is-off nous-ne-pill" aria-pressed="false" data-repo="acme\/api" data-action="toggleNousRepo"/);
assert.doesNotMatch(list, /data-repo="acme\/web"/, 'acme/web is not in the allow-list so it is not offered');
assert.match(list, /from the lab's allow-list; none selected = hive-wide/);
assert.match(list, /data-action="nousSubmitNewExperiment">Start experiment</);
assert.match(list, /data-action="nousOpenNewExperiment"[^>]*disabled/);
assert.doesNotMatch(list, /nous-conflict-warn|nous-new-exp-title/);
// Empty allow-list = every project repo is offered.
assert.deepEqual(nousAllowedRepos({ projectRepos: ['a/x', 'a/y'], repoTargets: [] }), ['a/x', 'a/y']);
assert.deepEqual(nousAllowedRepos({ projectRepos: ['a/x', 'a/y'], repoTargets: ['a/y'] }), ['a/y']);
assert.deepEqual(nousAllowedRepos({ availableRepos: ['a/x'] }), ['a/x']);
const allRepos = nousExperimentCards(campaigns, Object.assign({}, status, { repoTargets: [] }));
assert.match(allRepos, /data-repo="acme\/api"/);
assert.match(allRepos, /data-repo="acme\/web"/);

// ✎ Edit: the same form pre-filled (question, hypothesis, scope on) with
// "Save changes"; a scoped repo outside the allow-list stays toggleable.
_nousNewExp = { open: true, conflict: null, pending: false, editId: 1, original: { question: 'Cut CI time by 20%?', repos: ['acme/api'] } };
const editList = nousExperimentCards(campaigns, Object.assign({}, status, { repoTargets: ['acme/web'] }));
assert.match(editList, /id="nous-new-exp" role="form" aria-label="Edit experiment #1" data-edit-id="1"/);
assert.match(editList, /<div class="nous-new-exp-title">Edit experiment #1 /);
assert.match(editList, /<textarea id="nous-ne-question"[^>]*>Cut CI time by 20%\?<\/textarea>/);
assert.match(editList, /id="nous-ne-hypothesis"[^>]*value="shard the slowest package"/);
assert.match(editList, /class="nous-config-pill active is-on nous-ne-pill" aria-pressed="true" data-repo="acme\/api"/);
assert.match(editList, /class="nous-config-pill is-off nous-ne-pill" aria-pressed="false" data-repo="acme\/web"/);
assert.match(editList, /data-action="nousSubmitNewExperiment">Save changes</);
// Material vs in-place edits.
const orig = { question: 'Cut CI time by 20%?', repos: ['acme/api'] };
assert.equal(nousEditIsMaterial(orig, { question: 'cut  ci time by 20%?', hypothesis: 'new hyp', repos: ['acme/api'] }), false, 'hypothesis-only edit keeps the baseline');
assert.equal(nousEditIsMaterial(orig, { question: 'Cut CI time by 30%?', repos: ['acme/api'] }), true, 'question change restarts');
assert.equal(nousEditIsMaterial(orig, { question: 'Cut CI time by 20%?', repos: ['acme/api', 'acme/web'] }), true, 'scope change restarts');
assert.equal(nousEditIsMaterial(orig, { question: 'Cut CI time by 20%?', repos: [] }), true, 'going hive-wide restarts');

// 409 scope_conflict renders the in-app warning naming the other
// experiment, the shared repo, the reason, and both remedies.
const conflict = { ok: false, code: 'scope_conflict', shared: ['acme/api'], conflict: campaigns[0] };
_nousNewExp = { open: true, conflict, pending: false };
list = nousExperimentCards(campaigns, status);
const warn = nousConflictWarning(conflict);
assert.ok(list.includes(warn), 'form embeds the warning');
assert.match(warn, /class="nous-conflict-warn" role="alert" data-conflict-id="1"/);
assert.match(warn, /Scope conflict with experiment #1/);
assert.match(warn, /Both would intervene on <strong>acme\/api<\/strong>/);
assert.match(warn, /confound each other/);
assert.match(warn, /data-action="nousPauseConflictAndStart" data-arg0="1"[^>]*>Pause #1 and start</);
assert.match(warn, /data-action="nousNarrowScope"/);
// Hive-wide conflict says so.
assert.match(nousConflictWarning({ conflict: campaigns[1], shared: [] }), /Both would intervene on <strong>hive-wide<\/strong>/);

// Empty state.
assert.match(nousExperimentCards([], { projectRepos: [] }), /Add an experiment: enter a research question and pick the repos it may touch\./);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node experiment card rendering failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
