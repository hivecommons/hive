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
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// hivecommons/hive#7896: the Repositories card said "3 PRs" and listed no PR
// pill, because the PRs number is the repo's total open PR count while the
// pills came from the actionable set only — and a hold label (the ACMM level
// gate, a Renovate bump escalated needs-human) moves a PR out of that set.
// The held PRs were exactly the ones waiting on the operator, and the card
// was the one place they could not be seen.
//
// The fix carries held items on the snapshot beside the actionable ones
// (FrontendRepo.HeldPrs / HeldIssues) and draws them in two columns under
// the two numbers, tinted held. These tests pin the plumbing and the pill
// rules; the hold gate itself must be untouched throughout.

func heldCfg() *config.Config {
	return &config.Config{Project: config.ProjectConfig{Org: "Danathar", Repos: []string{"arch-bootc", "sensi"}}}
}

// The issue's own fixture: arch-bootc with three held PRs and three
// actionable issues, sensi with one held PR and nothing else.
func heldActionable() *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "arch-bootc", Number: 313, Title: "issue 313", URL: "https://github.com/Danathar/arch-bootc/issues/313"},
			{Repo: "arch-bootc", Number: 312, Title: "issue 312"},
			{Repo: "arch-bootc", Number: 316, Title: "issue 316"},
		}},
		PRs: github.PRResult{
			Items: nil,
			Held: []github.PullRequest{
				{Repo: "arch-bootc", Number: 315, Title: "level-gated", Author: "hive-app[bot]", Labels: []string{"hold"}, HiveAttributed: true, URL: "https://github.com/Danathar/arch-bootc/pull/315"},
				{Repo: "arch-bootc", Number: 311, Title: "level-gated too", Labels: []string{"hold"}, HiveAttributed: true},
				{Repo: "arch-bootc", Number: 143, Title: "chore(deps): bump", Author: "renovate[bot]", Labels: []string{"hold", "needs-human", "dependencies"}},
				{Repo: "sensi", Number: 9, Title: "sensi held", Labels: []string{"on-hold"}},
			},
		},
		Hold: github.HoldResult{Items: []github.HoldItem{
			{Repo: "arch-bootc", Number: 315, Title: "level-gated", Type: "pr", Labels: []string{"hold"}},
			{Repo: "arch-bootc", Number: 311, Title: "level-gated too", Type: "pr", Labels: []string{"hold"}},
			{Repo: "arch-bootc", Number: 143, Title: "chore(deps): bump", Type: "pr", Labels: []string{"hold", "needs-human", "dependencies"}},
			{Repo: "arch-bootc", Number: 300, Title: "parked issue", Type: "issue", Labels: []string{"hold/review"}, URL: "https://github.com/Danathar/arch-bootc/issues/300"},
			{Repo: "sensi", Number: 9, Title: "sensi held", Type: "pr", Labels: []string{"on-hold"}},
		}},
		TotalByRepo: map[string]github.RepoCounts{
			"arch-bootc": {Issues: 4, PRs: 3},
			"sensi":      {Issues: 0, PRs: 1},
		},
	}
}

func TestBuildRepos_HeldItemsReachTheWireBesideTheActionableOnes(t *testing.T) {
	repos := buildRepos(heldCfg(), heldActionable(), governor.State{})
	raw, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	var wire []struct {
		Full       string           `json:"full"`
		PRs        int              `json:"prs"`
		Issues     int              `json:"issues"`
		OpenPrs    []map[string]any `json:"openPrs"`
		HeldPrs    []map[string]any `json:"heldPrs"`
		Actionable []map[string]any `json:"actionableIssues"`
		HeldIssues []map[string]any `json:"heldIssues"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 2 {
		t.Fatalf("repos = %d, want 2: %s", len(wire), raw)
	}
	arch, sensi := wire[0], wire[1]

	// The count and its column now agree: 3 PRs, 3 pills in the PR column.
	if arch.PRs != 3 || len(arch.HeldPrs) != 3 || len(arch.OpenPrs) != 0 {
		t.Errorf("arch-bootc: prs=%d openPrs=%d heldPrs=%d, want 3 / 0 / 3", arch.PRs, len(arch.OpenPrs), len(arch.HeldPrs))
	}
	if sensi.PRs != 1 || len(sensi.HeldPrs) != 1 || len(sensi.OpenPrs) != 0 {
		t.Errorf("sensi: prs=%d openPrs=%d heldPrs=%d, want 1 / 0 / 1", sensi.PRs, len(sensi.OpenPrs), len(sensi.HeldPrs))
	}
	// Held issues come from Hold.Items, issue entries only — the PR entries
	// there must not be duplicated into the issue column.
	if len(arch.HeldIssues) != 1 || arch.HeldIssues[0]["number"] != float64(300) || arch.HeldIssues[0]["url"] != "https://github.com/Danathar/arch-bootc/issues/300" {
		t.Errorf("arch-bootc heldIssues = %v, want just #300 with its URL", arch.HeldIssues)
	}
	if len(arch.Actionable) != 3 {
		t.Errorf("actionable issues = %d, want 3 (unchanged)", len(arch.Actionable))
	}
	// Each held PR keeps the fields the pill needs: number, title, labels
	// (the tooltip reasons from them), URL and the hive-attribution flag.
	byNumber := map[float64]map[string]any{}
	for _, p := range arch.HeldPrs {
		byNumber[p["number"].(float64)] = p
	}
	if byNumber[143] == nil || byNumber[315] == nil || byNumber[311] == nil {
		t.Fatalf("held PR numbers = %v", byNumber)
	}
	labels, _ := byNumber[143]["labels"].([]any)
	if len(labels) != 3 || labels[1] != "needs-human" {
		t.Errorf("held PR #143 labels = %v", byNumber[143]["labels"])
	}
	if byNumber[315]["url"] != "https://github.com/Danathar/arch-bootc/pull/315" || byNumber[315]["hive_attributed"] != true {
		t.Errorf("held PR #315 lost url / hive_attributed: %v", byNumber[315])
	}
	// Empty columns are empty arrays, never null — the frontend iterates.
	if sensi.HeldIssues == nil || sensi.Actionable == nil || sensi.OpenPrs == nil {
		t.Errorf("sensi empty lists must be [] not null: %s", raw)
	}
	if !strings.Contains(string(raw), `"heldIssues":[]`) {
		t.Errorf("empty heldIssues must serialise as []: %s", raw)
	}
}

// A nil enumeration (first tick, or the fetch failed) still yields empty
// slices, and nothing panics.
func TestBuildRepos_NilActionableHasEmptyHeldLists(t *testing.T) {
	for _, r := range buildRepos(heldCfg(), nil, governor.State{}) {
		if r.HeldPrs == nil || r.HeldIssues == nil || len(r.HeldPrs) != 0 || len(r.HeldIssues) != 0 {
			t.Errorf("%s: held lists = %v / %v, want empty non-nil", r.Name, r.HeldPrs, r.HeldIssues)
		}
	}
}

// The review-links stamp reaches held PRs too: "has the hive already looked
// at this?" is what the human a hold is waiting on wants to know.
func TestAttachReviewLinks_StampsHeldPRs(t *testing.T) {
	payload := &StatusPayload{Repos: buildRepos(heldCfg(), heldActionable(), governor.State{})}
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	AttachReviewLinks(payload, map[string]github.ReviewLink{
		"Danathar/arch-bootc#143": {URL: "https://github.com/Danathar/arch-bootc/pull/143#pullrequestreview-1", State: "commented", Count: 2, At: at},
	})
	var got *FrontendPR
	for _, entry := range payload.Repos[0].HeldPrs {
		fp, ok := entry.(FrontendPR)
		if ok && fp.Number == 143 {
			got = &fp
		}
	}
	if got == nil || got.ReviewURL == "" || got.ReviewCount != 2 || got.ReviewedAt == nil || !got.ReviewedAt.Equal(at) {
		t.Fatalf("held PR #143 review link not stamped: %+v", got)
	}
	// The other held PRs are untouched.
	for _, entry := range payload.Repos[0].HeldPrs {
		if fp, ok := entry.(FrontendPR); ok && fp.Number != 143 && fp.ReviewURL != "" {
			t.Errorf("held PR #%d got a review link it has no ledger entry for", fp.Number)
		}
	}
}

// The card structure: two columns under the two numbers, held pills in
// their column with the held tint, and the held PR block offers no Queue
// auto-merge action.
func TestRepoCardHeldPillStructure(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// The stat row and the pill row share one two-column grid.
		".repo-stats { display: grid; grid-template-columns: 1fr 1fr;",
		".repo-pills { display: grid; grid-template-columns: 1fr 1fr;",
		`<div class="repo-pills"><div class="repo-pill-col repo-pill-col-issues">${issueCol}</div><div class="repo-pill-col repo-pill-col-prs">${prCol}</div></div>`,
		"const issueCol = issuePills + heldIssuePills;",
		"const prCol = prPills + heldPrPills;",
		// Held tint, distinct from every merge-state and from needs-human.
		".repo-issue-pill.held, .repo-pr-pill.held { --pill-c: var(--muted); border-style: dashed; }",
		"const heldPrPills = (r.heldPrs || []).map(p => {",
		"const heldIssuePills = (r.heldIssues || []).map(i => {",
		`<a class="repo-pr-pill held${needsHuman ? ' needs-human' : ''}"`,
		`<a class="repo-issue-pill held"`,
		// The state chip: ⚠ for the escalation kind of hold, ⏸ otherwise.
		`<span class="repo-pr-pill needs-human pill-needs-human-badge pill-icon" title="${esc(heldTip)}"`,
		`<span class="repo-pr-pill held pill-held-badge pill-icon" title="${esc(heldTip)}"`,
		// The tooltip reasons from the labels.
		"function holdLabels(labels) {",
		"function heldReason(item) {",
		// The section-header counter sees held needs-human PRs too.
		"(r.openPrs || []).concat(r.heldPrs || []).filter(p => (p.labels || []).map(l => String(l)).includes('needs-human'))",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	// The old single wrapped row is gone: pills must not fall back to it.
	if strings.Contains(html, `<div class="repo-issues">`) {
		t.Error("index.html still renders the single-row .repo-issues pill container")
	}
	// A held PR pill must never offer Queue auto-merge: a held PR is out of
	// the automated lane by definition. Pin it on the held block's own text.
	start := strings.Index(html, "const heldPrPills = (r.heldPrs || []).map(p => {")
	end := strings.Index(html, "const issueCol = issuePills + heldIssuePills;")
	if start < 0 || end < start {
		t.Fatal("cannot locate the held PR pill block")
	}
	heldBlock := html[start:end]
	for _, forbidden := range []string{"queuePRAutoMerge", "canQueue", "Queue for Hive auto-merge", "planIssue"} {
		if strings.Contains(heldBlock, forbidden) {
			t.Errorf("held pill block offers %q — a held item is out of the automated lane", forbidden)
		}
	}
	// And the actionable PR pill still does (nothing regressed there).
	if !strings.Contains(html, `data-action="queuePRAutoMerge"`) {
		t.Error("the actionable PR pill lost its Queue auto-merge action")
	}
}

// The tooltip rule executed under node: which labels hold an item, and what
// the held note says for each kind of hold the issue names.
func TestRepoCardHeldNoteBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the held-note rule was NOT executed by this run; TestRepoCardHeldPillStructure still ran")
	}
	html := indexHTML(t)
	start := strings.Index(html, "const HOLD_LABEL_SPELLINGS = [")
	if start < 0 {
		t.Fatal("index.html does not define HOLD_LABEL_SPELLINGS")
	}
	spellings := html[start : start+strings.Index(html[start:], "\n")]
	script := spellings + "\n" + jsFunc(t, html, "holdLabels") + "\n" + jsFunc(t, html, "heldReason") + "\n" + heldReasonAssertions

	path := filepath.Join(t.TempDir(), "held.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("held-note check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const heldReasonAssertions = `
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

// Same rule as github.HasHoldLabel: substring, case-insensitive, every
// spelling the server honours.
check('hold', holdLabels(['hold']).join() === 'hold');
check('on-hold', holdLabels(['on-hold']).join() === 'on-hold');
check('hold/review', holdLabels(['hold/review']).join() === 'hold/review');
check('case-insensitive', holdLabels(['HOLD']).join() === 'HOLD');
check('substring match', holdLabels(['needs-hold-x']).join() === 'needs-hold-x');
check('non-hold labels ignored', holdLabels(['bug', 'needs-human', 'dependencies']).length === 0);
check('null labels', holdLabels(null).length === 0);

// The ACMM level gate on an agent's own PR: hold + hive-attributed.
const gated = { labels: ['hold'], hive_attributed: true };
check('gated says on hold', heldReason(gated).startsWith('On hold'));
check('gated names the label', heldReason(gated).includes('label ` + "`hold`" + `'));
check('gated mentions the level gate', heldReason(gated).includes('ACMM level gate'));
check('gated says who releases it', heldReason(gated).includes('the hive releases itself'));
check('gated is not needs-human', !heldReason(gated).includes('needs-human'));

// The Renovate case: hold + needs-human, not an agent PR.
const renovate = { labels: ['hold', 'needs-human', 'dependencies'] };
check('renovate says on hold', heldReason(renovate).startsWith('On hold'));
check('renovate names needs-human', heldReason(renovate).includes('needs-human: automated fix attempts exhausted'));
check('renovate does not claim the level gate', !heldReason(renovate).includes('ACMM level gate'));

// A plain manual hold on a human PR: just the label.
const manual = { labels: ['on-hold'] };
check('manual names its label', heldReason(manual).includes('label ` + "`on-hold`" + `'));
check('manual says what clears it', heldReason(manual).includes('until the hold label is removed'));
check('manual does not claim the level gate', !heldReason(manual).includes('ACMM level gate'));

// A held issue from an older snapshot: no labels at all. Still explains.
check('no labels still says on hold', heldReason({}).startsWith('On hold'));
check('null item does not throw', heldReason(null).startsWith('On hold'));
check('no parentheses of its own', !heldReason(gated).startsWith('('));

if (fails) { console.log(fails + ' failure(s)'); process.exit(1); }
console.log('ok');
`
