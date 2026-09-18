package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// A green PR pill means "the sweep would merge this now" (#7478): it is
// painted from the governor's merge-eligible verdict, carried per PR on the
// status snapshot, not re-derived from GitHub's looser mergeable flag. Amber
// is "mergeable on GitHub, sweep still wants something", with the reason in
// the tooltip; dirty / blocked / unknown get no tint. These tests pin the
// plumbing (the verdict reaches the wire beside the PR's own fields) and the
// pill logic (executed under node, including the no-verdict fallback, which
// must land on amber and never green).

func verdictCfg() *config.Config {
	return &config.Config{Project: config.ProjectConfig{Org: "org", Repos: []string{"repo"}}}
}

func TestAttachMergeVerdicts_StampsEachPRAndReachesTheWire(t *testing.T) {
	green := github.PullRequest{Repo: "repo", Number: 1, Title: "green", Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success"}
	amber := github.PullRequest{Repo: "repo", Number: 2, Title: "amber", Mergeable: github.MergeableYes, MergeableState: "unstable", CIStatus: "failure"}
	unseen := github.PullRequest{Repo: "repo", Number: 3, Title: "unseen", Mergeable: github.MergeableYes}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{green, amber, unseen}}}

	payload := &StatusPayload{Repos: buildRepos(verdictCfg(), actionable, governor.State{})}
	AttachMergeVerdicts(payload, map[string]github.MergeVerdict{
		github.MergeVerdictKey(green): {State: github.MergeVerdictEligible, Reason: "the sweep would merge this now"},
		github.MergeVerdictKey(amber): {State: github.MergeVerdictOutstanding, Reason: "CI failing: build"},
	})

	raw, err := json.Marshal(payload.Repos)
	if err != nil {
		t.Fatal(err)
	}
	var repos []struct {
		OpenPrs []map[string]any `json:"openPrs"`
	}
	if err := json.Unmarshal(raw, &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || len(repos[0].OpenPrs) != 3 {
		t.Fatalf("unexpected repo shape: %s", raw)
	}
	byNumber := map[float64]map[string]any{}
	for _, p := range repos[0].OpenPrs {
		byNumber[p["number"].(float64)] = p
	}
	// The PR's own fields are still on the wire under their own names.
	if byNumber[1]["mergeable"] != "yes" || byNumber[1]["title"] != "green" || byNumber[1]["mergeable_state"] != "clean" {
		t.Errorf("embedding lost the PR's own fields: %v", byNumber[1])
	}
	v, _ := byNumber[1]["merge_verdict"].(map[string]any)
	if v["state"] != "eligible" || !strings.Contains(v["reason"].(string), "would merge") {
		t.Errorf("green PR merge_verdict = %v", byNumber[1]["merge_verdict"])
	}
	v, _ = byNumber[2]["merge_verdict"].(map[string]any)
	if v["state"] != "outstanding" || v["reason"] != "CI failing: build" {
		t.Errorf("amber PR merge_verdict = %v", byNumber[2]["merge_verdict"])
	}
	// A PR the classifier did not see carries no verdict at all — the pill
	// then falls back, and the fallback is what the node test pins.
	if _, present := byNumber[3]["merge_verdict"]; present {
		t.Errorf("unseen PR should carry no merge_verdict, got %v", byNumber[3]["merge_verdict"])
	}

	// Nil / empty inputs are no-ops, not panics.
	AttachMergeVerdicts(nil, map[string]github.MergeVerdict{"x": {}})
	AttachMergeVerdicts(payload, nil)
}

// buildRepos now stores FrontendPR values; the mergeable counter must still
// read the verdict through the wrapper (#7471's fix must survive #7478).
func TestOpenPRIsMergeable_ReadsFrontendPR(t *testing.T) {
	yes := FrontendPR{PullRequest: github.PullRequest{Mergeable: github.MergeableYes}}
	no := FrontendPR{PullRequest: github.PullRequest{Mergeable: github.MergeableNo}}
	if !openPRIsMergeable(yes) || !openPRIsMergeable(&yes) {
		t.Error("FrontendPR with mergeable=yes not counted")
	}
	if openPRIsMergeable(no) || openPRIsMergeable(FrontendPR{}) {
		t.Error("FrontendPR with mergeable=no or unknown counted")
	}
	payload := &StatusPayload{
		Agents: []FrontendAgent{{Name: "scanner", StatsConfig: []any{
			map[string]any{"key": "mergeable", "source": "status", "field": "mergeableCount"},
		}}},
		Repos: buildRepos(verdictCfg(), &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
			{Repo: "repo", Number: 1, Mergeable: github.MergeableYes},
			{Repo: "repo", Number: 2, Mergeable: github.MergeableNo},
		}}}, governor.State{}),
	}
	if got := CollectAgentStats(payload)["scanner"]["mergeable"]; got != 1 {
		t.Errorf("mergeableCount through buildRepos = %v, want 1", got)
	}
}

// The pill structure: three states, the verdict decides, and the queue action
// stays gated on GitHub's "no" as #7475 left it.
func TestPRPillMergeVerdictStructure(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function prMergeState(p) {",
		".repo-pr-pill.merge-outstanding { --pill-c: var(--yellow); }",
		"const mergeState = prMergeState(p);",
		`mergeState === 'eligible' ? '<span class="pill-merge-icon">✓</span>'`,
		`mergeState === 'outstanding' ? '<span class="pill-merge-icon">◐</span>'`,
		"const mergeClass = mergeState === 'eligible' ? ' mergeable' : mergeState === 'outstanding' ? ' merge-outstanding' : '';",
		"const notMergeable = p.mergeable === 'no';",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	// The ✓ must not be derivable from GitHub's flag alone any more.
	if strings.Contains(html, "const mergeIcon = mergeable ? '<span class=\"pill-merge-icon\">✓</span>'") {
		t.Error("the pill still lights ✓ from prMergeable(p) — green must come from the sweep verdict")
	}
}

// The pill logic executed under node against every verdict the wire can
// carry, plus the payloads that carry none.
func TestPRPillMergeVerdictBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the merge-verdict pill rule was NOT executed by this run; TestPRPillMergeVerdictStructure still ran")
	}
	html := indexHTML(t)
	script := jsFunc(t, html, "prMergeable") + "\n" +
		jsFunc(t, html, "prMergeState") + "\n" +
		jsFunc(t, html, "prMergeNote") + "\n" + prPillMergeVerdictAssertions

	path := filepath.Join(t.TempDir(), "pill.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("PR pill merge-verdict check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const prPillMergeVerdictAssertions = `
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

// Green only on the sweep's verdict.
check('eligible is green', prMergeState({ mergeable: 'yes', merge_verdict: { state: 'eligible' } }) === 'eligible');
check('outstanding is amber', prMergeState({ mergeable: 'yes', merge_verdict: { state: 'outstanding', reason: 'CI failing: build' } }) === 'outstanding');
check('blocked lights nothing', prMergeState({ mergeable: 'no', merge_verdict: { state: 'blocked', reason: 'has merge conflicts with v4 — needs a rebase' } }) === '');
check('unknown lights nothing', prMergeState({ mergeable: '', merge_verdict: { state: 'unknown' } }) === '');
check('an unrecognised state lights nothing', prMergeState({ mergeable: 'yes', merge_verdict: { state: 'someday' } }) === '');

// The issue's two PRs: unstable + red check + no required set. Amber.
const bluefin1253 = { number: 1253, mergeable: 'yes', mergeable_state: 'unstable', merge_verdict: { state: 'outstanding', reason: 'CI failing: build (GitHub reports it mergeable; declare auto_merge.required_checks for the sweep to treat non-required checks as optional)' } };
check('bluefin#1253 is amber, not green', prMergeState(bluefin1253) === 'outstanding');
check('bluefin#1253 tooltip says not yet eligible', prMergeNote(bluefin1253).includes('not yet eligible'));
check('bluefin#1253 tooltip carries the reason', prMergeNote(bluefin1253).includes('CI failing: build'));
check('bluefin#1253 tooltip does not call it merge eligible', !prMergeNote(bluefin1253).includes('merge eligible'));

// The fallback: no verdict on the wire. GitHub "yes" lands on AMBER, never
// green — the sweep's verdict is exactly what has not been shown.
check('no verdict + yes is amber', prMergeState({ mergeable: 'yes' }) === 'outstanding');
check('no verdict + yes is never green', prMergeState({ mergeable: 'yes', mergeable_state: 'clean' }) !== 'eligible');
check('no verdict + no lights nothing', prMergeState({ mergeable: 'no' }) === '');
check('no verdict + unknown lights nothing', prMergeState({ mergeable: '' }) === '');
check('no verdict + absent lights nothing', prMergeState({}) === '');
check('null PR lights nothing', prMergeState(null) === '');
check('legacy bool true lights nothing', prMergeState({ mergeable: true }) === '');
check('fallback tooltip says the verdict is unavailable', prMergeNote({ mergeable: 'yes' }).includes('sweep verdict not available'));
check('fallback tooltip on unstable warns about non-required checks', prMergeNote({ mergeable: 'yes', mergeable_state: 'unstable' }).includes('non-required checks may be red'));

// Tooltips for the other states.
const green = { mergeable: 'yes', mergeable_state: 'clean', merge_verdict: { state: 'eligible', reason: 'the sweep would merge this now' } };
check('eligible tooltip says merge eligible', prMergeNote(green).includes('merge eligible'));
check('eligible tooltip carries the reason', prMergeNote(green).includes('would merge this now'));
const greenOptionalRed = { mergeable: 'yes', mergeable_state: 'unstable', merge_verdict: { state: 'eligible', reason: 'the sweep would merge this now — only non-required checks are red (playwright)' } };
check('eligible beside a red optional check names it', prMergeNote(greenOptionalRed).includes('playwright'));
check('eligible without a reason still reads sensibly', prMergeNote({ mergeable: 'yes', merge_verdict: { state: 'eligible' } }).includes('would merge this now'));
const dirty = { mergeable: 'no', mergeable_state: 'dirty', merge_verdict: { state: 'blocked', reason: 'has merge conflicts with v4 — needs a rebase' } };
check('blocked tooltip carries the reason', prMergeNote(dirty).includes('merge conflicts with v4'));
check('blocked tooltip does not say eligible', !prMergeNote(dirty).includes('eligible'));
// hivecommons/hive#7515: a "blocked" verdict now carries the sweep's own
// reason (the red required check, the missing review) and the tooltip shows
// it verbatim — GitHub's raw state is not appended on top.
const blocked = { mergeable: 'no', mergeable_state: 'blocked', merge_verdict: { state: 'blocked', reason: 'blocked — CI failing: build, lint' } };
check('blocked tooltip names the failing required checks', prMergeNote(blocked).includes('blocked — CI failing: build, lint'));
check('blocked tooltip does not repeat the raw GitHub state', !prMergeNote(blocked).includes('GitHub state'));
const blockedReview = { mergeable: 'no', mergeable_state: 'blocked', merge_verdict: { state: 'blocked', reason: 'blocked — awaiting review approval' } };
check('blocked tooltip names the missing review', prMergeNote(blockedReview).includes('awaiting review approval'));
check('draft tooltip says what to do', prMergeNote({ mergeable: 'yes', merge_verdict: { state: 'blocked', reason: 'draft — mark ready for review to enter the sweep' } }).includes('mark ready for review'));
check('unknown tooltip says not yet computed', prMergeNote({ mergeable: '', merge_verdict: { state: 'unknown', reason: 'mergeability not yet computed by GitHub — re-checked next tick; CI pending' } }).includes('not yet computed'));
check('no undefined anywhere', ![green, bluefin1253, dirty, blocked, { mergeable: 'yes' }, {}, { merge_verdict: {} }].some(p => prMergeNote(p).includes('undefined')));

if (fails) { console.log(fails + ' check(s) failed'); process.exit(1); }
`
