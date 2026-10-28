package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Part C of the "your turn" spec (hivecommons/hive#11507, sub-issue #11566):
// the ☐ marker, its menu, the inbox strip, the section counter, the L5/L6
// card collapse and the Parked / Gated wording. The browser only renders the
// server's decision, so these tests feed it payloads the server really emits
// (statusWithOverviewBands over decideRepoOwnership) and execute the page's own
// functions under node. Like the other node-driven dashboard tests they skip,
// loudly, when node is not installed.

var (
	jsDeclRe = regexp.MustCompile(`(?m)^    (?:async )?function ([A-Za-z_]\w*)\(`)
	jsCallRe = regexp.MustCompile(`\b([A-Za-z_]\w*)\(`)
	jsArgRe  = regexp.MustCompile(`[(,]\s*([A-Za-z_]\w*)\s*[,)]`)
)

// yourTurnPage is the once-per-process analysis of index.html that every
// your-turn script assembly shares: the set of top-level function names and
// the source of each hoisted const. The markup is an embedded, immutable file,
// so the analysis cannot go stale between tests, and doing it per test cost
// several seconds of regexp scanning over a multi-megabyte page under -race —
// enough, across the 11566/11569/11716 tests, to push pkg/dashboard past the
// 10-minute `go test` budget on CI (#12163).
type yourTurnPageAnalysis struct {
	html    string
	defined map[string]bool
	consts  []string
}

var (
	yourTurnPageOnce sync.Once
	yourTurnPageData yourTurnPageAnalysis
)

func yourTurnPage(t *testing.T) *yourTurnPageAnalysis {
	t.Helper()
	html := indexHTML(t)
	yourTurnPageOnce.Do(func() {
		defined := map[string]bool{}
		for _, m := range jsDeclRe.FindAllStringSubmatch(html, -1) {
			defined[m[1]] = true
		}
		type positioned struct {
			at  int
			src string
		}
		var consts []positioned
		for _, name := range yourTurnPageConsts {
			if at, src := jsConst(html, name); src != "" {
				consts = append(consts, positioned{at, src})
			}
		}
		sort.Slice(consts, func(i, j int) bool { return consts[i].at < consts[j].at })
		srcs := make([]string, 0, len(consts))
		for _, c := range consts {
			srcs = append(srcs, c.src)
		}
		yourTurnPageData = yourTurnPageAnalysis{html: html, defined: defined, consts: srcs}
	})
	return &yourTurnPageData
}

// jsClosure returns the page functions reachable from roots (by name, so
// functions passed as callbacks count), except those the harness stubs.
// defined is the page's top-level function census from yourTurnPage.
func jsClosure(t *testing.T, html string, defined, skip map[string]bool, roots ...string) string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	var visit func(name string)
	visit = func(name string) {
		if seen[name] || skip[name] {
			return
		}
		seen[name] = true
		src := jsFunc(t, html, name)
		// Calls, and functions handed over as callbacks (.map(fn)).
		for _, re := range []*regexp.Regexp{jsCallRe, jsArgRe} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				if defined[m[1]] && m[1] != name {
					visit(m[1])
				}
			}
		}
		out = append(out, src)
	}
	for _, root := range roots {
		visit(root)
	}
	return strings.Join(out, "\n")
}

// jsConst returns the top-level `const NAME = …;` statement, or "" when the
// page has none. A plain substring scan, not a multiline-anchored regexp: the
// page is over 3 MB and this runs once per hoisted const.
func jsConst(html, name string) (int, string) {
	needle := "\n    const " + name
	start := -1
	for from := 0; from < len(html); {
		i := strings.Index(html[from:], needle)
		if i < 0 {
			break
		}
		end := from + i + len(needle)
		if end >= len(html) || !isJSIdentByte(html[end]) {
			start = from + i + 1
			break
		}
		from = end
	}
	if start < 0 {
		return -1, ""
	}
	depth := 0
	var quote byte
	for i := start; i < len(html); i++ {
		c := html[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ';':
			if depth == 0 {
				return start, html[start : i+1]
			}
		}
	}
	return -1, ""
}

func isJSIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

var yourTurnPageConsts = []string{
	"HOLD_LABEL_SPELLINGS", "PR_BAND_ORDER", "REPO_ISSUE_BAND_DEFAULTS", "MS_PER_DAY", "OVERVIEW_PROJECT_FILTERS",
	"PILL_TITLE_BASE_LEN", "PILL_TITLE_MAX_LEN", "PILL_TITLE_MIN_VISIBLE", "PLANNING_MIN_ACMM_LEVEL",
	"REPO_CARD_DEFAULT_W", "REPO_CARD_MAX_W", "REPO_CARD_MIN_W", "REPO_CARD_WIDTH_KEY_PREFIX", "REPO_MERGE_LANE_TTL_MS",
	"REPO_ORDER_KEY", "REPO_PILL_FILTER_KEY_PREFIX", "REPO_LEGEND_COLLAPSED_KEY_PREFIX", "OVERVIEW_ISSUE_BAND_ORDER",
	"YOUR_TURN_GLYPH", "YOUR_TURN_VERB_ORDER", "YOUR_TURN_COLLAPSE_MIN_LEVEL", "YOUR_TURN_EXPANDED_KEY_PREFIX",
	"OVERVIEW_REPOS_KEY", "repoHoldPermissionCache", "REPO_HEALTH_TTL_MS", "repoHealthCache",
}

// yourTurnPageScript assembles a node script that holds the page's own
// functions (the closure of roots) over a small fake browser. prelude and body
// are the test's own JavaScript.
func yourTurnPageScript(t *testing.T, skip map[string]bool, body string, roots ...string) string {
	t.Helper()
	page := yourTurnPage(t)
	// The label-filter strip (hf*, #12172) lives inside every repo card but
	// is its own DOM-free module with its own node test; stub it here so the
	// your-turn closure does not pull in its store and editor state.
	skipAll := map[string]bool{"hfRepoSectionHtml": true, "hfRenderEditor": true, "hfRenderPinned": true, "hfRenderCrossRepo": true}
	for k, v := range skip {
		skipAll[k] = v
	}
	var script strings.Builder
	script.WriteString(yourTurnFakeBrowser)
	script.WriteString("function hfRepoSectionHtml() { return ''; } function hfRenderEditor() {} function hfRenderPinned() {} function hfRenderCrossRepo() {}\n")
	for _, src := range page.consts {
		script.WriteString(src)
		script.WriteByte('\n')
	}
	script.WriteString(jsClosure(t, page.html, page.defined, skipAll, roots...))
	script.WriteString("\n")
	script.WriteString(body)
	return script.String()
}

func runYourTurnNode(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the your-turn UI was NOT executed by this run")
	}
	path := filepath.Join(t.TempDir(), "your-turn.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("your-turn UI check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// yourTurnFakeBrowser is the smallest browser the page's functions need: a
// document whose elements keep their innerHTML, a localStorage, and a focus
// pointer. Menu items are recovered from the markup the page itself builds.
const yourTurnFakeBrowser = `
const assert = require('node:assert/strict');
let activeEl = null;
const els = {};
const listeners = { document: {}, window: {} };
const markers = [];
function parseMenuItems(html) {
  const items = [];
  const re = /<(button|a)\b([^>]*\brole="menuitem"[^>]*)>/g;
  let m;
  while ((m = re.exec(html))) {
    const attrs = {};
    m[2].replace(/([a-z-]+)="([^"]*)"/g, (_, k, v) => { attrs[k] = v.replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&'); });
    const dataset = {};
    Object.keys(attrs).forEach(k => { if (k.startsWith('data-')) dataset[k.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = attrs[k]; });
    const item = { tag: m[1], attrs, dataset, focus() { activeEl = this; }, closest(sel) { return sel === '.yt-item' ? this : null; }, disabled: false };
    items.push(item);
  }
  return items;
}
function mkEl(id, tag) {
  const el = {
    id, tag: tag || 'div', textContent: '', style: {}, dataset: {}, className: '', hidden: false, clientWidth: 1200, title: '',
    attrs: {}, listeners: {}, connected: false, offsetWidth: 220, offsetHeight: 120, items: [], _html: '',
    set innerHTML(h) { this._html = h; this.items = h.includes('role="menuitem"') ? parseMenuItems(h) : []; }, get innerHTML() { return this._html; },
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }, removeAttribute(k) { delete this.attrs[k]; },
    addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
    focus() { activeEl = this; },
    remove() { this.connected = false; const i = bodyChildren.indexOf(this); if (i >= 0) bodyChildren.splice(i, 1); },
    contains(n) { return n === this || this.items.includes(n); },
    getBoundingClientRect() { return { left: 10, top: 10, bottom: 30 }; },
    querySelectorAll(sel) { return sel === '[role="menuitem"]' ? this.items : []; },
    querySelector() { return null; },
    closest(sel) { return this.classes && this.classes.some(c => sel === '.' + c) ? this : null; },
    get isConnected() { return this.connected; }
  };
  return el;
}
const bodyChildren = [];
const document = {
  getElementById(id) { if (id === 'yt-tip') return bodyChildren.find(e => e.id === 'yt-tip') || null; return els[id] || (els[id] = mkEl(id)); },
  createElement(tag) { return mkEl('', tag); },
  addEventListener(type, fn) { (listeners.document[type] = listeners.document[type] || []).push(fn); },
  removeEventListener(type, fn) { listeners.document[type] = (listeners.document[type] || []).filter(f => f !== fn); },
  querySelectorAll(sel) { return sel === '.yt-menu-btn' ? markers.slice() : (sel === '.yt-menu-btn[aria-describedby]' ? markers.filter(m => 'aria-describedby' in m.attrs) : []); },
  get activeElement() { return activeEl; },
  body: { appendChild(el) { el.connected = true; bodyChildren.push(el); } },
  documentElement: mkEl('html')
};
const window = {
  _lastStatus: null, _hiveRole: 'owner', _hiveUser: 'op', _githubBaseUrl: 'https://github.com', innerWidth: 1280, innerHeight: 800,
  addEventListener(type, fn) { (listeners.window[type] = listeners.window[type] || []).push(fn); },
  removeEventListener(type, fn) { listeners.window[type] = (listeners.window[type] || []).filter(f => f !== fn); }
};
const _store = {};
const localStorage = { getItem: k => (k in _store ? _store[k] : null), setItem: (k, v) => { _store[k] = String(v); }, removeItem: k => { delete _store[k]; } };
const MS_PER_HOUR = 3600000, MS_PER_MIN = 60000;
const location = { search: '', hash: '' };
function getComputedStyle() { return { getPropertyValue() { return ''; } }; }
const historyData = []; function getHistoryTimes() { return []; } function sparkSvg() { return ''; }
let _yourTurnMenu = null;
let _repoResizeDrag = null, _repoOrderDrag = null, _overviewProjectFilterKey = '', _overviewProjectFilterCount = null, _overviewLastRepos = [];
function marker(repo, kind, number, variant, tip) {
  const el = mkEl('marker-' + variant + '-' + repo + number, 'button');
  el.dataset = { ytRepo: repo, ytKind: kind, ytNumber: String(number), ytVariant: variant, ytTip: tip };
  el.classes = ['yt-menu-btn'];
  el.connected = true;
  markers.push(el);
  return el;
}
`

// ownership fixtures --------------------------------------------------------

// yourTurnStatus11566 builds the status payload the server serves for repos at
// the given hive level, by the same path /api/status takes: the part A
// decisions, then statusWithOverviewBands. levels overrides the effective
// level of single repositories.
func yourTurnStatus11566(t *testing.T, hiveLevel int, levels map[string]int, repos []FrontendRepo) map[string]any {
	t.Helper()
	env := ownershipEnv11565(hiveLevel)
	env.repoLevel = func(repo string) int { return levels[repo] }
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	s := newTestServer()
	status := &StatusPayload{HiveID: "h", ACMMLevel: hiveLevel, AdmissionLabels: []string{"triage-ok"}, Repos: repos}
	raw, err := json.Marshal(s.statusWithOverviewBands(status, ownershipNow11565))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func yourTurnJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func ago11566(d time.Duration) time.Time { return ownershipNow11565.Add(-d) }

// ladderRepos11566 is the scenario of the "one ownership signal" acceptance
// item on an L5 hive: a parked issue, a level-held agent PR, and a hive
// needs-human PR still inside the adjudicator wait, plus a person's
// needs-human PR (your turn) older than the hive one, and ordinary rows.
func ladderRepos11566() []FrontendRepo {
	return []FrontendRepo{{
		Name: "r", Full: "o/r",
		ActionableIssues: []any{
			github.Issue{Repo: "o/r", Number: 20, Title: "plain ready issue", Labels: []string{}, CreatedAt: ago11566(48 * time.Hour), UpdatedAt: ago11566(2 * time.Hour)},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/r", Number: 1, Title: "parked decision", Labels: []string{"needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: ago11566(30 * 24 * time.Hour), UpdatedAt: ago11566(20 * 24 * time.Hour)},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 3, Title: "hive escalation", HiveAgent: "scanner", Labels: []string{"needs-human"}, CreatedAt: ago11566(5 * time.Hour), UpdatedAt: ago11566(5 * time.Hour)}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 6, Title: "person escalation", Author: "alice", Labels: []string{"needs-human"}, CreatedAt: ago11566(3 * time.Hour), UpdatedAt: ago11566(3 * time.Hour)}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 4, Title: "ordinary open pr", Author: "bob", Labels: []string{}, CreatedAt: ago11566(time.Hour), UpdatedAt: ago11566(time.Hour), LinkedIssues: []github.PRLinkedIssue{{Number: 1, Repo: "o/r", State: "open"}}}},
		},
		HeldPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "o/r", Number: 2, Title: "level held agent pr", HiveAgent: "scanner", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourceLevelGate}, CreatedAt: ago11566(4 * time.Hour), UpdatedAt: ago11566(4 * time.Hour), ReviewState: "approved", ReviewURL: "https://github.com/o/r/pull/2#pullrequestreview-9"}},
		},
	}}
}

// Go side ---------------------------------------------------------------------

func TestBandTitlesParkedAndGated11566(t *testing.T) {
	cfg := config.DashboardIssueBandsConfig{}
	issue := issueBandSpec("waiting", cfg)
	pr := prBandSpec("waiting", cfg)
	if issue.Label != "Parked" || pr.Label != "Gated" {
		t.Fatalf("waiting band titles = %q / %q, want Parked / Gated", issue.Label, pr.Label)
	}
	for _, spec := range append(IssueBandSpecs(cfg), PRBandSpecs(cfg)...) {
		for _, text := range []string{spec.Label, spec.Short, spec.Rule} {
			if banned := regexp.MustCompile(`(?i)needs human|need human review|needs you`).FindString(text); banned != "" {
				t.Fatalf("band %q says %q: %q", spec.Key, banned, text)
			}
		}
	}
	if got := strings.Join(issueBandOrder, ","); got != "ready,in-progress,agent-filed,waiting,done" {
		t.Fatalf("issue band order changed: %s", got)
	}
	if got := strings.Join(prBandOrder, ","); got != "waiting,eligible,blocked,in-review,open,draft" {
		t.Fatalf("PR band order changed: %s", got)
	}
	now := ownershipNow11565
	info := IssueBand(github.Issue{Repo: "o/r", Number: 1, Labels: []string{"needs-human"}}, false, cfg, now)
	for _, signal := range info.Signals {
		if strings.Contains(signal.Label, "human review") {
			t.Fatalf("issue signal %q describes ownership", signal.Label)
		}
	}
	prInfo := PRBand(github.PullRequest{Repo: "o/r", Number: 1, Labels: []string{"needs-human"}}, false, cfg, now)
	if prInfo.Band != "waiting" {
		t.Fatalf("needs-human PR band = %q, want waiting (Gated)", prInfo.Band)
	}
	for _, signal := range prInfo.Signals {
		if strings.Contains(signal.Label, "human review") {
			t.Fatalf("PR signal %q describes ownership", signal.Label)
		}
	}
}

func TestFrontendRepoCarriesEffectiveLevel11566(t *testing.T) {
	five, three := 5, 3
	cfg := &config.Config{ACMMLevel: &five, Project: config.ProjectConfig{Org: "o", RepoPolicies: []config.RepoPolicy{{Repo: "o/low", ACMMLevel: &three}}}}
	repos := []FrontendRepo{{Name: "high", Full: "o/high"}, {Name: "low", Full: "o/low"}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, newOwnershipEnv(cfg, nil, "", ownershipNow11565, nil))
	if repos[0].ACMMLevel != 5 || repos[1].ACMMLevel != 3 {
		t.Fatalf("effective levels = %d, %d; want 5 (hive) and 3 (override on an L5 hive)", repos[0].ACMMLevel, repos[1].ACMMLevel)
	}
	for i, repo := range repos {
		if want := cfg.EffectiveACMMLevelForRepo(repo.Full); repo.ACMMLevel != want {
			t.Fatalf("repo %d level %d differs from EffectiveACMMLevelForRepo %d", i, repo.ACMMLevel, want)
		}
	}
	raw, err := json.Marshal(repos[1])
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["acmmLevel"] != float64(3) {
		t.Fatalf("frontend repo payload acmmLevel = %v, want 3", wire["acmmLevel"])
	}
}

func TestParkedRowsCarryOrdinaryRowFacts11566(t *testing.T) {
	status := yourTurnStatus11566(t, 5, nil, ladderRepos11566())
	repo := status["repos"].([]any)[0].(map[string]any)
	var parked map[string]any
	for _, raw := range repo["nonActionableIssues"].([]any) {
		row := raw.(map[string]any)
		if row["number"] == float64(1) {
			parked = row
		}
	}
	if parked == nil {
		t.Fatal("parked row missing from the payload")
	}
	if parked["band"] != "waiting" || parked["chip"] != "decision" || parked["stale"] != true {
		t.Fatalf("parked row band/chip/stale = %v/%v/%v, want waiting/decision/true", parked["band"], parked["chip"], parked["stale"])
	}
	if _, ok := parked["signals"].([]any); !ok {
		t.Fatalf("parked row has no signals array: %v", parked)
	}
	linked, _ := parked["linked_prs"].([]any)
	if len(linked) != 1 || linked[0].(map[string]any)["number"] != float64(4) || linked[0].(map[string]any)["closing"] != true {
		t.Fatalf("parked row linked_prs = %v, want the open PR #4 whose closing reference names it", parked["linked_prs"])
	}
	if own, _ := parked["ownership"].(map[string]any); own["your_turn"] != true {
		t.Fatalf("parked row ownership = %v", parked["ownership"])
	}
	if status["admissionLabels"].([]any)[0] != "triage-ok" {
		t.Fatalf("admissionLabels = %v", status["admissionLabels"])
	}
}

// Browser side: the row rendered by the page ----------------------------------

func TestYourTurnMarkerAndRowsRendered11566(t *testing.T) {
	status := yourTurnStatus11566(t, 5, nil, ladderRepos11566())
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {}
window._lastStatus = `+yourTurnJSON(t, status)+`;
window._hiveSentinelLabel = 'sentinel-alert';
// An L5 card shows only its your-turn rows until expanded; the band titles below describe the expanded card.
localStorage.setItem('hive-repos-yourturn-expanded:h', JSON.stringify(['o/r']));
renderRepos(window._lastStatus.repos);
const cards = els['repos'].innerHTML;
const strip = els['repos-inbox'].innerHTML;
const legend = els['repos-legend'].innerHTML;

// One ownership signal: the waiting bands are Parked and Gated, and nothing on
// the Projects section says whose turn it is except the marker.
assert.match(cards, /repo-issue-band-title"[^>]*>Parked · 1</);
assert.match(cards, /repo-pr-band-title"[^>]*>Gated · 3</);
const banned = /needs human|need human review|needs you/i;
for (const [name, text] of [['cards', cards], ['strip', strip], ['legend', legend], ['counter', els['repos-needs-human'].textContent]]) {
  // The marker (and the inbox's Options button) carry the server's verb and reason; they are the ownership signal.
  const scanned = text.replace(/<button type="button" class="yt-menu-btn[^>]*>/g, '').replace(/<span class="yt-reason[^"]*">[^<]*<\/span>/g, '');
  const hit = scanned.match(banned);
  assert.ok(!hit, name + ' says ' + (hit && scanned.slice(Math.max(0, hit.index - 120), hit.index + 80)));
}
assert.deepEqual(window._lastStatus.overview_bands.issues.map(b => b.key), ['ready', 'in-progress', 'agent-filed', 'waiting', 'done']);
assert.deepEqual(window._lastStatus.overview_bands.prs.map(b => b.key), ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft']);

// The marker: a menu button before the state chip, on exactly the rows the
// server marked, and never a checkbox.
const markerRe = /<button type="button" class="yt-menu-btn yt-marker"[^>]*>/g;
const markerTags = cards.match(markerRe) || [];
assert.equal(markerTags.length, 3, 'parked issue, level-held PR and person needs-human PR carry a marker');
for (const tag of markerTags) {
  assert.match(tag, /aria-haspopup="menu"/);
  assert.match(tag, /aria-expanded="false"/);
  assert.doesNotMatch(tag, /role="checkbox"|type="checkbox"|aria-checked/);
  assert.match(tag, /aria-label="Your turn: [A-Z]+ — [^"]+\. Opens a menu of options\./);
  assert.match(tag, /data-yt-tip="[A-Z]+ — [^"]+"/);
}
const segments = cards.split(/(?=<span class="repo-(?:issue|pr)-pill-wrap)/);
const rowOf = n => { const row = segments.find(s => s.includes('<span class="pill-num">#' + n + '</span>')); assert.ok(row, 'row #' + n); return row; };
for (const n of [1, 2, 6]) {
  const row = rowOf(n);
  const glyph = row.indexOf('☐'), chip = row.indexOf('repo-pill-status');
  assert.ok(glyph > 0 && glyph < chip, '#' + n + ' marker precedes its chip');
  assert.match(row, /<span class="repo-pill-lead"><button type="button" class="yt-menu-btn yt-marker"/);
}
for (const n of [3, 4, 20]) assert.doesNotMatch(rowOf(n), /yt-menu-btn/, '#' + n + ' is not your turn');
// A row that is not your turn keeps its old markup: chip first, no lead wrapper.
assert.doesNotMatch(rowOf(4), /repo-pill-lead/);
// The glyph appears only on those three card rows and in the legend entry.
const outsideTooltips = cards.replace(/(?:title|aria-label)="[^"]*"/g, '');
assert.equal((outsideTooltips.match(/☐/g) || []).length, 3, 'the glyph is drawn only on the three your-turn rows');
assert.equal((strip.match(/☐/g) || []).length, 0, 'inbox rows open the menu from an Options button, not the glyph');
assert.equal((legend.match(/☐/g) || []).length, 1, 'one legend entry explains the marker');

// Sort: inside the Gated band both your-turn PRs precede the older hive one
// that merely carries the label; the order of the bands is unchanged.
const gated = cards.slice(cards.indexOf('repo-pr-band-waiting'));
const order = [...gated.matchAll(/<span class="pill-num">#(\d+)<\/span>/g)].map(m => Number(m[1]));
assert.deepEqual(order.slice(0, 3), [2, 6, 3]);
assert.ok(cards.indexOf('repo-issue-band-ready') < cards.indexOf('repo-issue-band-waiting'));

// The needs-human badge names the label and says when the marker appears.
assert.match(cards, /title="Labelled needs-human: automated fix attempts exhausted\. ☐ appears when it is your turn\./);
// The section counter is the inbox: the parked issue, the level-held PR and the person's needs-human PR.
assert.equal(els['repos-needs-human'].textContent, '· 3 your turn');
assert.equal(els['repos-needs-human'].style.display, 'inline');
assert.match(strip, />Your turn · 3</);
assert.match(legend, /waiting \/ gated/);
`, "renderRepos", "renderRepoLegend")
	runYourTurnNode(t, script)
}

func TestParkedRowsAreOrdinaryRowsOnTheCard11566(t *testing.T) {
	status := yourTurnStatus11566(t, 5, nil, ladderRepos11566())
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true, "renderRepoLegend": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {} function renderRepoLegend() {}
window._lastStatus = `+yourTurnJSON(t, status)+`;
function parkedRow(html) { const row = html.split(/(?=<span class="repo-(?:issue|pr)-pill-wrap)/).find(s => s.includes('<span class="pill-num">#1</span>')); assert.ok(row, 'parked row rendered'); return row; }
renderRepos(window._lastStatus.repos);
let row = parkedRow(els['repos'].innerHTML);
assert.match(row, /data-action="planIssue"/, 'plan affordance');
assert.match(row, /data-action="designIssue" data-arg-types="t"/, 'design affordance');
assert.match(row, /aria-label="closes via PR #4"/, 'linked-PR affordance (#12144 glyph)');
assert.match(row, /repo-issue-pill repo-pill-main waiting stale/, 'stale dimming');
assert.match(row, /repo-pill-status repo-issue-pill waiting stale/);
assert.match(row, />decision</, 'the chip word is unchanged');

// An Overview band filter on the waiting band keeps the parked row listed.
_overviewProjectFilterKey = 'blocked-needs-human';
renderRepos(window._lastStatus.repos);
row = parkedRow(els['repos'].innerHTML);
assert.match(row, /data-action="designIssue"/);
// A filter on bands the row is not in hides it, like every other row.
_overviewProjectFilterKey = 'held';
renderRepos(window._lastStatus.repos);
assert.doesNotMatch(els['repos'].innerHTML, /#1<\/span>/);
`, "renderRepos")
	runYourTurnNode(t, script)
}

func TestL5CardCollapseAndExpand11566(t *testing.T) {
	repos := append(ladderRepos11566(), func() FrontendRepo {
		r := ladderRepos11566()[0]
		r.Name, r.Full = "low", "o/low"
		for i := range r.OpenPrs {
			pr := r.OpenPrs[i].(FrontendPR)
			pr.PullRequest.Repo = "o/low"
			r.OpenPrs[i] = pr
		}
		for i := range r.HeldPrs {
			pr := r.HeldPrs[i].(FrontendPR)
			pr.PullRequest.Repo = "o/low"
			r.HeldPrs[i] = pr
		}
		r.ActionableIssues = []any{github.Issue{Repo: "o/low", Number: 20, Title: "plain ready issue", CreatedAt: ago11566(48 * time.Hour), UpdatedAt: ago11566(2 * time.Hour)}}
		r.NonActionableIssues = []any{github.RepoWorkIssue{Repo: "o/low", Number: 1, Title: "parked decision", Labels: []string{"needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: ago11566(30 * 24 * time.Hour), UpdatedAt: ago11566(20 * 24 * time.Hour)}}
		return r
	}())
	status := yourTurnStatus11566(t, 5, map[string]int{"o/low": 3}, repos)
	if got := status["repos"].([]any)[1].(map[string]any)["acmmLevel"]; got != float64(3) {
		t.Fatalf("overridden repo acmmLevel = %v, want 3", got)
	}
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true, "renderRepoLegend": true, "repaintReposForWidth": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {} function renderRepoLegend() {} function repaintReposForWidth() {}
window._lastStatus = `+yourTurnJSON(t, status)+`;
function card(html, repo) { const parts = html.split('<div class="repo-card'); const c = parts.find(p => p.includes('title="' + repo + '"')); assert.ok(c, repo); return c; }
renderRepos(window._lastStatus.repos);
let html = els['repos'].innerHTML;
const l5 = card(html, 'o/r'), l3 = card(html, 'o/low');

// L5: the your-turn rows stay expanded, the rest fold into one summary line.
for (const n of [1, 2, 6]) assert.match(l5, new RegExp('<span class="pill-num">#' + n + '</span>'), 'your-turn #' + n + ' stays');
for (const n of [3, 4, 20]) assert.doesNotMatch(l5, new RegExp('<span class="pill-num">#' + n + '</span>'), 'row #' + n + ' is folded');
assert.equal((l5.match(/class="yt-collapse-btn"/g) || []).length, 1, 'exactly one summary line');
assert.match(l5, /class="yt-collapse-btn"[^>]*aria-expanded="false"/);
assert.match(l5, /3 more — Issues: Unclaimed 1 · PRs: Gated 1 · Open 1/);
// A repository overridden to L3 on the L5 hive renders every band as before.
for (const n of [1, 2, 3, 4, 6, 20]) assert.match(l3, new RegExp('<span class="pill-num">#' + n + '</span>'), 'L3 shows #' + n);
assert.doesNotMatch(l3, /yt-collapse/);

// Expanding reveals the remaining rows (and keeps a way back).
toggleYourTurnCollapse('o/r');
html = els['repos'].innerHTML;
assert.ok(JSON.parse(_store['hive-repos-yourturn-expanded:h']).includes('o/r'));
renderRepos(window._lastStatus.repos);
const open = card(els['repos'].innerHTML, 'o/r');
for (const n of [1, 2, 3, 4, 6, 20]) assert.match(open, new RegExp('<span class="pill-num">#' + n + '</span>'));
assert.match(open, /class="yt-collapse-btn"[^>]*aria-expanded="true"/);
`, "renderRepos", "toggleYourTurnCollapse")
	runYourTurnNode(t, script)
}

func TestYourTurnInboxStrip11566(t *testing.T) {
	low := FrontendRepo{Name: "low", Full: "o/low",
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/low", Number: 31, Title: "low person pr", Author: "carol", Labels: []string{"needs-human"}, CreatedAt: ago11566(10 * time.Hour), UpdatedAt: ago11566(10 * time.Hour)}}},
		ActionableIssues: []any{
			github.Issue{Repo: "o/low", Number: 32, Title: "verified done", Labels: []string{"hive/already-done"}, CreatedAt: ago11566(9 * time.Hour), UpdatedAt: ago11566(9 * time.Hour)},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/low", Number: 33, Title: "stranger report", Labels: []string{"needs-triage"}, Bucket: repoWorkBucketReporterTriage, Reason: "Awaiting maintainer triage label", CreatedAt: ago11566(8 * time.Hour), UpdatedAt: ago11566(8 * time.Hour)},
		},
	}
	repos := append(ladderRepos11566(), low)
	status := yourTurnStatus11566(t, 5, map[string]int{"o/low": 4}, repos)
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true, "renderRepoLegend": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {} function renderRepoLegend() {}
window._lastStatus = `+yourTurnJSON(t, status)+`;
const numbersIn = html => [...html.matchAll(/<div class="yt-inbox-row">.*?<span class="pill-num">#(\d+)<\/span>/gs)].map(m => Number(m[1]));
const verbsIn = html => [...html.matchAll(/<div class="yt-inbox-group-title">([A-Z]+) · /g)].map(m => m[1]);
renderRepos(window._lastStatus.repos);
let strip = els['repos-inbox'].innerHTML;

// Every your-turn item of every repository shown, no other, grouped by verb in
// the spec's order, in the server's inbox_rank order within the groups.
const ranked = [];
window._lastStatus.repos.forEach(r => ['actionableIssues', 'heldIssues', 'nonActionableIssues', 'openPrs', 'heldPrs'].forEach(k => (r[k] || []).forEach(row => { if (row.ownership && row.ownership.your_turn) ranked.push([row.ownership.inbox_rank, row.number]); })));
ranked.sort((a, b) => a[0] - b[0]);
assert.deepEqual(numbersIn(strip), ranked.map(r => r[1]));
assert.deepEqual(ranked.map(r => r[0]), ranked.map((_, i) => i + 1));
assert.ok(numbersIn(strip).length >= 6);
for (const n of [3, 4, 20]) assert.ok(!numbersIn(strip).includes(n), '#' + n + ' is not your turn');
const verbs = verbsIn(strip);
assert.deepEqual(verbs, YOUR_TURN_VERB_ORDER.filter(v => verbs.includes(v)));
assert.ok(verbs.includes('CLOSE') && verbs.indexOf('CLOSE') === verbs.length - 1);

// Row content: repository, number, title, verb, reason, the options button;
// the held agent PR also shows the reviewer's most recent verdict.
const row = strip.match(/<div class="yt-inbox-row">(?:(?!<div class="yt-inbox-row">).)*?<span class="pill-num">#2<\/span>.*?<\/div>/s)[0];
assert.match(row, /yt-inbox-repo">o\/r</);
assert.match(row, /level held agent pr/);
assert.match(row, /yt-verb">REVIEW</);
assert.match(row, /yt-reason[^"]*">Held by the ACMM level gate/);
assert.match(row, /yt-verdict" href="https:\/\/github.com\/o\/r\/pull\/2#pullrequestreview-9"[^>]*>Reviewer: approved/);
assert.match(row, /<button type="button" class="yt-menu-btn yt-inbox-btn"[^>]*aria-haspopup="menu"[^>]*>Options<\/button>/);
assert.doesNotMatch(strip, /☐/);
assert.doesNotMatch(strip.match(/<div class="yt-inbox-row">(?:(?!<div class="yt-inbox-row">).)*?#6<\/span>.*?<\/div>/s)[0], /yt-verdict/, 'only a held agent PR shows a verdict');

// Header and counter agree: CLOSE items are counted apart.
const total = ranked.length;
const closeCount = (strip.match(/data-yt-verb="CLOSE"/g) ? 1 : 0);
assert.equal(closeCount, 1);
assert.match(strip, new RegExp('Your turn · ' + (total - 1) + ' · 1 to close'));
assert.equal(els['repos-needs-human'].textContent, '· ' + (total - 1) + ' your turn · 1 to close');

// Follows the repository selection...
localStorage.setItem('hive-repos-pill-filter:h', JSON.stringify({ mode: 'any', kinds: [], repo: 'o/low' }));
renderRepos(window._lastStatus.repos);
strip = els['repos-inbox'].innerHTML;
assert.deepEqual(numbersIn(strip).sort((a, b) => a - b), [31, 32, 33]);
assert.doesNotMatch(strip, /o\/r</);
assert.equal(els['repos-needs-human'].textContent, '· 2 your turn · 1 to close');
// ...and the pill-kind filter that narrows the cards narrows the strip.
localStorage.setItem('hive-repos-pill-filter:h', JSON.stringify({ mode: 'any', kinds: ['pr:held'], repo: '' }));
renderRepos(window._lastStatus.repos);
strip = els['repos-inbox'].innerHTML;
assert.deepEqual(numbersIn(strip), [2]);
assert.equal(els['repos-needs-human'].textContent, '· 1 your turn');
// The your-turn legend entry is a filter kind of its own.
localStorage.setItem('hive-repos-pill-filter:h', JSON.stringify({ mode: 'any', kinds: ['your-turn'], repo: '' }));
renderRepos(window._lastStatus.repos);
assert.equal(numbersIn(els['repos-inbox'].innerHTML).length, total);
`, "renderRepos")
	runYourTurnNode(t, script)
}

// With the Overview Your turn filter active, the strip and the counter keep the
// bandless your-turn rows (an ADMIT reporter-triage row, a reporter-confirmation
// CLOSE row) that the tile and the card bucket count, so N and M add up to the
// tile's number.
func TestYourTurnInboxKeepsBandlessRowsUnderOverviewFilter11566(t *testing.T) {
	repos := []FrontendRepo{{
		Name: "f", Full: "o/f", Issues: 3,
		ActionableIssues: []any{
			github.Issue{Repo: "o/f", Number: 1, Title: "plain ready issue", CreatedAt: ago11566(2 * time.Hour), UpdatedAt: ago11566(2 * time.Hour)},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/f", Number: 3, Title: "stranger report", Bucket: repoWorkBucketReporterTriage, Reason: "Awaiting maintainer triage label", CreatedAt: ago11566(8 * time.Hour), UpdatedAt: ago11566(8 * time.Hour)},
			github.RepoWorkIssue{Repo: "o/f", Number: 5, Title: "confirm fix", Labels: []string{"needs-reporter-confirmation"}, Bucket: repoWorkBucketFiltered, Reason: "needs-reporter-confirmation", CreatedAt: ago11566(90 * 24 * time.Hour), UpdatedAt: ago11566(60 * 24 * time.Hour)},
		},
	}}
	env := ownershipEnv11565(5)
	env.repoLevel = func(string) int { return 0 }
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	s := newTestServer()
	resp := s.statusWithOverviewBands(&StatusPayload{HiveID: "h", ACMMLevel: 5, AdmissionLabels: []string{"triage-ok"}, Repos: repos}, ownershipNow11565)
	tile := overviewTermCount(resp.ActionableNow.Equation, "your_turn")
	if tile != 2 {
		t.Fatalf("Your turn tile = %d, want the ADMIT row and the CLOSE row", tile)
	}
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true, "renderRepoLegend": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {} function renderRepoLegend() {}
window._lastStatus = `+yourTurnJSON(t, resp)+`;
const numbersIn = html => [...html.matchAll(/<div class="yt-inbox-row">.*?<span class="pill-num">#(\d+)<\/span>/gs)].map(m => Number(m[1]));
const rows = window._lastStatus.repos[0].nonActionableIssues;
assert.deepEqual(rows.map(r => [r.number, r.overview_term, r.ownership.verb]), [[3, 'your_turn', 'ADMIT'], [5, 'your_turn', 'CLOSE']]);
_overviewProjectFilterKey = 'your-turn';
renderRepos(window._lastStatus.repos);
const strip = els['repos-inbox'].innerHTML;
assert.deepEqual(numbersIn(strip).sort((a, b) => a - b), [3, 5]);
// The counter N/M equals the tile.
assert.equal(els['repos-needs-human'].textContent, '· 1 your turn · 1 to close');
assert.equal(1 + 1, `+yourTurnJSON(t, tile)+`);
`, "renderRepos")
	runYourTurnNode(t, script)
}

func TestYourTurnCountsCounterAndEmptyState11566(t *testing.T) {
	script := yourTurnPageScript(t, nil, `
const item = verb => ({ repo: 'o/r', kind: 'issue', row: { number: 1, title: 't', ownership: { your_turn: true, verb, reason: 'why', inbox_rank: 1 } } });
const mk = (n, m) => Array.from({ length: n }, () => item('REVIEW')).concat(Array.from({ length: m }, () => item('CLOSE')));
// 3 non-CLOSE items and 12 CLOSE items.
let counts = yourTurnCounts(mk(3, 12));
assert.deepEqual(counts, { n: 3, m: 12 });
assert.equal(yourTurnInboxHeaderText(counts), 'Your turn · 3 · 12 to close');
assert.equal(yourTurnCounterText(counts), '· 3 your turn · 12 to close');
// No CLOSE items: no suffix.
counts = yourTurnCounts(mk(3, 0));
assert.equal(yourTurnInboxHeaderText(counts), 'Your turn · 3');
assert.equal(yourTurnCounterText(counts), '· 3 your turn');
// Only CLOSE items: the header and the counter keep the zero, and the CLOSE group is shown.
counts = yourTurnCounts(mk(0, 12));
assert.equal(yourTurnInboxHeaderText(counts), 'Your turn · 0 · 12 to close');
assert.equal(yourTurnCounterText(counts), '· 0 your turn · 12 to close');
assert.match(yourTurnInboxHTML(mk(0, 12), [4]), /data-yt-verb="CLOSE"/);
assert.doesNotMatch(yourTurnInboxHTML(mk(0, 12), [4]), /yt-inbox-empty/);
// The counter is hidden only when both are zero.
assert.equal(yourTurnCounterText(yourTurnCounts([])), '');
assert.equal(yourTurnInboxHeaderText(yourTurnCounts([])), 'Your turn · 0');

// Empty state by the levels of the repositories shown.
const empty = levels => yourTurnInboxHTML([], levels).match(/yt-inbox-empty">([^<]*)</)[1];
assert.match(empty([2]), /advisory.*nothing is routed to you/);
assert.match(empty([1, 2]), /advisory/);
assert.match(empty([6]), /No exceptions/);
assert.match(empty([5, 6]), /No exceptions/);
assert.equal(empty([4]), 'Nothing is waiting on you.');
assert.equal(empty([3, 5, 6]), 'Nothing is waiting on you.');
assert.equal(empty([2, 5]), 'Nothing is waiting on you.');
assert.equal(empty([]), 'Nothing is waiting on you.');
`, "yourTurnCounts", "yourTurnInboxHeaderText", "yourTurnCounterText", "yourTurnInboxHTML", "yourTurnEmptyText")
	runYourTurnNode(t, script)
}

func TestYourTurnRowsSortFirstWithinBand11566(t *testing.T) {
	script := yourTurnPageScript(t, nil, `
window._lastStatus = { overview_bands: { issues: [{ key: 'ready', label: 'Unclaimed', short: 'unclaimed', rule: 'r' }, { key: 'waiting', label: 'Parked', short: 'parked', rule: 'r' }], prs: [{ key: 'waiting', label: 'Gated', rule: 'r' }, { key: 'open', label: 'Open', rule: 'r' }] } };
const own = { your_turn: true, verb: 'REVIEW', reason: 'why' };
// Newest your-turn first would be wrong: the older row would win on recency
// alone, so the your-turn rows are the NEWER ones.
const issues = [
  { number: 1, band: 'ready', updated_at: '2026-01-01T00:00:00Z', ownership: { your_turn: false } },
  { number: 2, band: 'ready', updated_at: '2026-01-02T00:00:00Z', labels: ['needs-human'], ownership: { your_turn: false } },
  { number: 3, band: 'ready', updated_at: '2026-01-09T00:00:00Z', ownership: own },
  { number: 4, band: 'waiting', updated_at: '2026-01-05T00:00:00Z', ownership: { your_turn: false } },
  { number: 5, band: 'waiting', updated_at: '2026-01-08T00:00:00Z', ownership: own },
];
let groups = groupedRepoIssues(issues);
assert.deepEqual(groups.map(g => g.band), ['ready', 'waiting'], 'band order is by band, not by who is waiting');
assert.deepEqual(groups[0].issues.map(e => e.issue.number), [3, 1, 2], 'a label is not a turn: needs-human on #2 does not promote it');
assert.deepEqual(groups[1].issues.map(e => e.issue.number), [5, 4]);
const prs = [
  { number: 10, band: 'open', updated_at: '2026-01-01T00:00:00Z', ownership: { your_turn: false } },
  { number: 11, band: 'open', updated_at: '2026-01-07T00:00:00Z', ownership: own },
  { number: 12, band: 'waiting', updated_at: '2026-01-02T00:00:00Z', ownership: { your_turn: false } },
  { number: 13, band: 'waiting', updated_at: '2026-01-03T00:00:00Z', labels: ['needs-human'], ownership: { your_turn: false } },
  { number: 14, band: 'waiting', updated_at: '2026-01-06T00:00:00Z', ownership: own },
];
const pg = groupedRepoPRs(prs.filter(p => p.band === 'open'), prs.filter(p => p.band === 'waiting'));
assert.deepEqual(pg.map(g => g.band), ['waiting', 'open']);
assert.deepEqual(pg[0].prs.map(e => e.pr.number), [14, 12, 13]);
assert.deepEqual(pg[1].prs.map(e => e.pr.number), [11, 10]);
`, "groupedRepoIssues", "groupedRepoPRs")
	runYourTurnNode(t, script)
}

// A card's non-actionable groups (Filtered, Waiting on reporter, ...) carry the
// marker too, so a your-turn row sorts ahead of unmarked rows there as well.
func TestYourTurnRowsSortFirstInNonActionableGroups11566(t *testing.T) {
	script := yourTurnPageScript(t, nil, `
const own = { your_turn: true, verb: 'INSPECT', reason: 'why' };
const issues = [
  { number: 1, bucket: 'filtered', updated_at: '2026-01-01T00:00:00Z', ownership: { your_turn: false } },
  { number: 2, bucket: 'filtered', updated_at: '2026-01-02T00:00:00Z', ownership: { your_turn: false } },
  { number: 3, bucket: 'filtered', updated_at: '2026-01-09T00:00:00Z', ownership: own },
  { number: 4, bucket: 'reporter_triage', updated_at: '2026-01-05T00:00:00Z', ownership: { your_turn: false } },
  { number: 5, bucket: 'reporter_triage', updated_at: '2026-01-08T00:00:00Z', ownership: own },
];
const groups = groupedRepoNonActionableIssues(issues);
assert.deepEqual(groups.map(g => g.label), ['Filtered', 'Needs triage']);
assert.deepEqual(groups[0].issues.map(i => i.number), [3, 1, 2], 'the newer your-turn row leads the Filtered group');
assert.deepEqual(groups[1].issues.map(i => i.number), [5, 4]);
`, "groupedRepoNonActionableIssues", "repoNonActionableIssueBucketSpec")
	runYourTurnNode(t, script)
}

func TestYourTurnMenuPerVerb11566(t *testing.T) {
	script := yourTurnPageScript(t, nil, `
const base = { kind: 'issue', url: 'https://github.com/o/r/issues/1', isOwner: true, planningAllowed: true, hasPlan: false, canToggleHold: true, held: false, sentinelLabel: 'sentinel-alert', admissionLabels: ['triage-ok'] };
const model = (own, extra) => yourTurnMenuModel(Object.assign({}, base, { own }, extra || {}));
const kinds = m => m.entries.map(e => e.type + ':' + (e.label || e.text || e.id));
const own = (verb, code, action) => ({ your_turn: true, verb, reason_code: code, reason: 'because', action });
const open = 'link:Open on GitHub';

// Every menu says what and why at its top (the opened menu repeats them) and always offers Open on GitHub.
let m = model(own('DECIDE', 'parked_needs_decision'));
assert.equal(m.verb, 'DECIDE'); assert.equal(m.reason, 'because');
assert.match(yourTurnMenuHTML(m, { repo: 'o/r', number: 1, url: base.url }), /^<div class="yt-menu-head" id="yt-menu-head" role="presentation"><span class="yt-verb">DECIDE<\/span> <span class="yt-reason">because<\/span><\/div>/);
assert.deepEqual(kinds(m), [open, 'copy:/hive approve', 'copy:/hive decision <text>']);
assert.deepEqual(kinds(model(own('STEER', 'parked_needs_direction'))), [open, 'copy:/hive approve', 'copy:/hive decision <text>']);
assert.deepEqual(kinds(model(own('REVIEW', 'parked_needs_human'))), [open, 'copy:/hive approve', 'copy:/hive decision <text>']);
const release = Object.assign(own('DECIDE', 'parked_needs_decision', 'release'), { release_tag: 'v0.12.0', release_target: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' });
assert.deepEqual(kinds(model(release)), ['action:Release v0.12.0', open, 'copy:/hive approve', 'copy:/hive decision <text>']);
assert.deepEqual(kinds(model(release, { canToggleHold: false })), [open, 'copy:/hive approve', 'copy:/hive decision <text>'], 'the repository write gate hides the release action');
// SPEC: the design action for an owner at L5+, with the release note.
assert.deepEqual(kinds(model(own('SPEC', 'parked_needs_spec'))), ['action:Design before planning', open, 'note:Adding the spec or removing the label releases it.']);
assert.deepEqual(kinds(model(own('SPEC', 'parked_needs_spec'), { isOwner: false })), [open, 'note:Adding the spec or removing the label releases it.'], 'role gating');
assert.deepEqual(kinds(model(own('SPEC', 'parked_needs_spec'), { planningAllowed: false })), [open, 'note:Adding the spec or removing the label releases it.'], 'below L5 there is no design action');
assert.deepEqual(kinds(model(own('SPEC', 'parked_needs_spec'), { hasPlan: true })), [open, 'note:Adding the spec or removing the label releases it.'], 'a planned issue has no design button either');
assert.match(kinds(model(own('TRIAGE', 'agent_filed_unacknowledged')))[1], /approved-direction label, assigning a person, or closing/);
assert.deepEqual(kinds(model(own('TRIAGE', 'agent_filed_unacknowledged'))).length, 2);
assert.deepEqual(kinds(model(own('ADMIT', 'untrusted_reporter'))), [open, 'note:Add the label triage-ok to admit it.']);
assert.match(kinds(model(own('INSPECT', 'sentinel_flagged')))[1], /Removing the sentinel-alert label after review releases it/);
assert.deepEqual(kinds(model(own('CLOSE', 'reporter_confirmation_stale'))), [open, 'copy:/fixed']);
assert.deepEqual(kinds(model(own('CLOSE', 'verified_done'))), [open]);
// REVIEW on PRs and on other issues: Open on GitHub only, unless the level gate is the only hold source.
const pr = { kind: 'pr', url: 'https://github.com/o/r/pull/2' };
assert.deepEqual(kinds(model(own('REVIEW', 'person_pr_needs_human'), pr)), [open]);
assert.deepEqual(kinds(model(own('REVIEW', 'stale_safety_net'))), [open], 'an issue the safety net surfaced');
assert.deepEqual(kinds(model(own('REVIEW', 'policy_hold', undefined), Object.assign({ held: true }, pr))), [open]);
const gated = own('REVIEW', 'level_gate_hold', 'release_hold');
assert.deepEqual(kinds(model(gated, Object.assign({ held: true }, pr))), ['action:Release hold', open]);
assert.deepEqual(kinds(model(gated, Object.assign({ held: true, canToggleHold: false }, pr))), [open], 'a user the hold toggle is not offered to does not see it');
assert.deepEqual(kinds(model(gated, Object.assign({ held: false }, pr))), [open]);
`, "yourTurnMenuModel", "yourTurnMenuHTML")
	runYourTurnNode(t, script)
}

func TestYourTurnMenuContextUsesExistingRoleGates11566(t *testing.T) {
	status := yourTurnStatus11566(t, 5, nil, ladderRepos11566())
	script := yourTurnPageScript(t, nil, `
window._lastStatus = `+yourTurnJSON(t, status)+`;
const names = ctx => yourTurnMenuModel(ctx).entries.map(e => e.label || e.text);
// Owner: release hold on the level-held agent PR, the design action on a needs-spec issue at L5.
window._hiveRole = 'owner';
let ctx = yourTurnMenuContext('o/r', 'pr', 2);
assert.equal(ctx.held, true);
assert.deepEqual(names(ctx), ['Release hold', 'Open on GitHub']);
assert.equal(ctx.url, 'https://github.com/o/r/pull/2');
// A user below owner without hold permission on the repository: only Open on GitHub.
window._hiveRole = 'read-write';
assert.deepEqual(names(yourTurnMenuContext('o/r', 'pr', 2)), ['Open on GitHub']);
repoHoldPermissionCache['o/r'] = true;
assert.deepEqual(names(yourTurnMenuContext('o/r', 'pr', 2)), ['Release hold', 'Open on GitHub'], 'the existing hold-permission answer decides');
// A row that is not your turn has no menu.
assert.equal(yourTurnMenuContext('o/r', 'pr', 3), null);
assert.equal(yourTurnMenuContext('o/r', 'pr', 999), null);
// The parked needs-decision issue: approve and decision commands.
window._hiveRole = 'owner';
assert.deepEqual(names(yourTurnMenuContext('o/r', 'issue', 1)), ['Open on GitHub', '/hive approve', '/hive decision <text>']);
`, "yourTurnMenuContext", "yourTurnMenuModel")
	runYourTurnNode(t, script)
}

func TestYourTurnReleaseOwnerStepAction12635(t *testing.T) {
	step := &github.IssueOwnerStep{Kind: "release", Tag: "v0.12.0", Target: strings.Repeat("a", 40)}
	statusFor := func(enabled bool) map[string]any {
		repos := []FrontendRepo{{
			Name: "r", Full: "o/r",
			NonActionableIssues: []any{github.RepoWorkIssue{
				Repo: "o/r", Number: 7, Title: "release", Labels: []string{"needs-decision"},
				Bucket: repoWorkBucketFiltered, Reason: "needs-decision", OwnerStep: step,
			}},
		}}
		env := ownershipEnv11565(5)
		env.releaseOnApprove = func(string) bool { return enabled }
		decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
		got := repos[0].Ownership[ownershipKey(ownershipKindIssue, 7)]
		if enabled {
			if got.Action != OwnershipActionRelease || got.ReleaseTag != step.Tag || got.ReleaseTarget != step.Target {
				t.Fatalf("enabled release ownership = %+v", got)
			}
		} else if got.Action != "" || got.ReleaseTag != "" || got.ReleaseTarget != "" {
			t.Fatalf("default-off release ownership changed: %+v", got)
		}
		raw, err := json.Marshal((&Server{}).statusWithOverviewBands(&StatusPayload{HiveID: "h", ACMMLevel: 5, Repos: repos}, ownershipNow11565))
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		return wire
	}
	disabled := statusFor(false)
	enabled := statusFor(true)
	script := yourTurnPageScript(t, map[string]bool{"releaseIssueOwnerStep": true}, `
const calls = [];
function releaseIssueOwnerStep(...args) { calls.push(args); }
window._hiveRole = 'owner';
window._lastStatus = `+yourTurnJSON(t, disabled)+`;
assert.ok(!yourTurnMenuModel(yourTurnMenuContext('o/r', 'issue', 7)).entries.some(e => e.id === 'release'), 'default-off row has no release action');
window._lastStatus = `+yourTurnJSON(t, enabled)+`;
const ctx = yourTurnMenuContext('o/r', 'issue', 7);
assert.equal(ctx.own.release_tag, 'v0.12.0');
assert.ok(!yourTurnMenuModel(Object.assign({}, ctx, { labelDriven: true })).entries.some(e => e.id === 'release'), 'label-driven repo never offers the release action next to the refusal note');
const model = yourTurnMenuModel(ctx);
assert.equal(model.entries[0].label, 'Release v0.12.0');
const menu = mkEl('release-menu');
menu.connected = true;
menu.innerHTML = yourTurnMenuHTML(model, ctx);
_yourTurnMenu = { el: menu, key: yourTurnKey('o/r', 'issue', 7), variant: 'marker', opener: null, ctx, rect: { top: 0, left: 0 } };
const action = menu.items.find(i => i.dataset.ytAct === 'release');
assert.ok(action, 'release menu item rendered');
yourTurnMenuClick({ target: action });
assert.deepEqual(calls, [['o/r', 7, 'v0.12.0']]);
`, "yourTurnMenuContext", "yourTurnMenuModel", "yourTurnMenuHTML", "yourTurnMenuClick")
	runYourTurnNode(t, script)
}

func TestYourTurnMenuIsAMenuButtonWithKeyboardSupport11566(t *testing.T) {
	status := yourTurnStatus11566(t, 5, nil, ladderRepos11566())
	script := yourTurnPageScript(t, map[string]bool{"toggleRepoItemHold": true, "releaseIssueOwnerStep": true, "designIssue": true, "writeClipboardText": true, "showToast": true},
		`
const calls = [];
function toggleRepoItemHold(...a) { calls.push(['hold', ...a]); }
function releaseIssueOwnerStep(...a) { calls.push(['release', ...a]); }
function designIssue(el) { calls.push(['design', el.dataset.repo, el.dataset.number]); }
function writeClipboardText(text) { calls.push(['copy', text]); return Promise.resolve(true); }
function showToast(m, k) { calls.push(['toast', k, m]); }
window._lastStatus = `+yourTurnJSON(t, status)+`;
window._hiveRole = 'owner';

// Hover and focus show the verb and the reason.
installYourTurnListeners(document);
const m = marker('o/r', 'pr', 2, 'marker', 'REVIEW — Held by the ACMM level gate — review it, then release the hold');
m.classes = ['yt-menu-btn'];
const fire = (type, target) => listeners.document[type].forEach(fn => fn({ target }));
fire('mouseover', m);
let tip = document.getElementById('yt-tip');
assert.ok(tip && tip.hidden === false);
assert.match(tip.textContent, /REVIEW/); assert.match(tip.textContent, /Held by the ACMM level gate/);
assert.equal(m.getAttribute('aria-describedby'), 'yt-tip');
fire('mouseout', m);
assert.equal(tip.hidden, true);
fire('focusin', m);
assert.equal(tip.hidden, false, 'keyboard focus shows it too');
fire('focusout', m);
assert.equal(tip.hidden, true);
assert.equal(m.getAttribute('aria-describedby'), null);

// Enter or Space (a button click) opens the menu; nothing is performed by opening it.
m.focus();
openYourTurnMenu(m);
assert.equal(calls.length, 0, 'opening performs nothing');
assert.ok(_yourTurnMenu && _yourTurnMenu.el.connected, 'menu opened');
assert.equal(m.getAttribute('aria-expanded'), 'true');
const menu = _yourTurnMenu.el;
assert.equal(menu.getAttribute('role'), 'menu');
assert.match(menu.innerHTML, /^<div class="yt-menu-head"[^>]*><span class="yt-verb">REVIEW<\/span> <span class="yt-reason">Held by the ACMM level gate/);
const items = menu.items;
assert.deepEqual(items.map(i => i.dataset.ytAct || i.tag), ['release_hold', 'a']);
assert.equal(activeEl, items[0], 'focus moves into the menu');

// Arrow keys move between entries and wrap; Home and End jump.
const key = (k) => { const e = { key: k, prevented: false, preventDefault() { this.prevented = true; } }; menu.listeners.keydown.forEach(fn => fn(e)); return e; };
key('ArrowDown'); assert.equal(activeEl, items[1]);
key('ArrowDown'); assert.equal(activeEl, items[0], 'wraps');
key('ArrowUp'); assert.equal(activeEl, items[1]);
key('Home'); assert.equal(activeEl, items[0]);
key('End'); assert.equal(activeEl, items[1]);
// Escape closes the menu and returns focus to the marker.
const escEvent = key('Escape');
assert.equal(escEvent.prevented, true);
assert.equal(_yourTurnMenu, null);
assert.equal(menu.connected, false);
assert.equal(activeEl, m, 'focus returns to the marker');
assert.equal(m.getAttribute('aria-expanded'), 'false');
assert.equal(calls.length, 0, 'closing performs nothing');
// Tab closes it and hands focus back to the marker so Tab continues from there.
openYourTurnMenu(m);
const tab = key('Tab');
assert.equal(tab.prevented, false);
assert.equal(_yourTurnMenu, null);
assert.equal(activeEl, m);
// Clicking the marker again closes an open menu.
openYourTurnMenu(m); openYourTurnMenu(m);
assert.equal(_yourTurnMenu, null);

// Choosing Release hold performs the existing hold toggle, once.
openYourTurnMenu(m);
_yourTurnMenu.el.listeners.click[0]({ target: _yourTurnMenu.el.items[0] });
assert.deepEqual(calls, [['hold', 'o/r', 2, 'pr', false, null]]);
assert.equal(_yourTurnMenu, null);
assert.equal(activeEl, m);

// A refresh that took the action away while the menu was open: choosing it does nothing.
calls.length = 0;
openYourTurnMenu(m);
const staleMenu = _yourTurnMenu.el;
const heldRow = window._lastStatus.repos[0].heldPrs.find(p => p.number === 2);
const savedAction = heldRow.ownership.action;
heldRow.ownership.action = '';
staleMenu.listeners.click[0]({ target: staleMenu.items[0] });
assert.deepEqual(calls.map(c => c[0]), ['toast'], 'only the notice, no hold toggle');
assert.equal(_yourTurnMenu, null);
heldRow.ownership.action = savedAction;
// The copy control copies the exact reply command; the design action calls the existing designIssue.
calls.length = 0;
const d = marker('o/r', 'issue', 1, 'marker', 'DECIDE — Parked until a maintainer decides');
openYourTurnMenu(d);
const copyBtn = _yourTurnMenu.el.items.find(i => i.dataset.ytCopy === '/hive decision <text>');
assert.ok(copyBtn);
_yourTurnMenu.el.listeners.click[0]({ target: copyBtn });
assert.deepEqual(calls, [['copy', '/hive decision <text>']]);
`, "openYourTurnMenu", "installYourTurnListeners", "closeYourTurnMenu")
	runYourTurnNode(t, script)
}

func TestYourTurnMenuDesignActionHandsIssueToArchitect11566(t *testing.T) {
	repos := []FrontendRepo{{
		Name: "r", Full: "o/r",
		NonActionableIssues: []any{github.RepoWorkIssue{Repo: "o/r", Number: 9, Title: "needs a spec", Labels: []string{"needs-spec"}, Bucket: repoWorkBucketFiltered, Reason: "needs-spec", URL: "https://github.com/o/r/issues/9", CreatedAt: ago11566(time.Hour), UpdatedAt: ago11566(time.Hour)}},
	}}
	status := yourTurnStatus11566(t, 5, nil, repos)
	script := yourTurnPageScript(t, map[string]bool{"toggleRepoItemHold": true, "designIssue": true, "writeClipboardText": true, "showToast": true},
		`
const calls = [];
function toggleRepoItemHold() { calls.push('hold'); }
function designIssue(el) { calls.push(['design', el.dataset.repo, el.dataset.number, el.dataset.url, el.dataset.title]); }
function writeClipboardText() { return Promise.resolve(true); }
function showToast() {}
window._lastStatus = `+yourTurnJSON(t, status)+`;
window._hiveRole = 'owner';
const m = marker('o/r', 'issue', 9, 'marker', 'SPEC — Parked until it has a spec');
openYourTurnMenu(m);
const items = _yourTurnMenu.el.items;
assert.deepEqual(items.map(i => i.dataset.ytAct || i.tag), ['design', 'a']);
assert.match(_yourTurnMenu.el.innerHTML, /Design before planning/);
_yourTurnMenu.el.listeners.click[0]({ target: items[0] });
assert.deepEqual(calls, [['design', 'o/r', '9', 'https://github.com/o/r/issues/9', 'needs a spec']]);
// Not an owner: the action is not offered.
window._hiveRole = 'read-write';
openYourTurnMenu(m);
assert.deepEqual(_yourTurnMenu.el.items.map(i => i.dataset.ytAct || i.tag), ['a']);
// Below L5 the hive does not plan, so the design action is not offered.
window._hiveRole = 'owner';
closeYourTurnMenu(false);
window._lastStatus.acmmLevel = 4;
openYourTurnMenu(m);
assert.deepEqual(_yourTurnMenu.el.items.map(i => i.dataset.ytAct || i.tag), ['a']);
`, "openYourTurnMenu", "closeYourTurnMenu")
	runYourTurnNode(t, script)
}

func TestYourTurnGlyphIsWrittenOnceAndOnlyAsTheMarker11566(t *testing.T) {
	html := indexHTML(t)
	if n := strings.Count(html, "☐"); n != 1 {
		t.Fatalf("the ☐ glyph is written %d times in index.html, want once (the YOUR_TURN_GLYPH constant)", n)
	}
	if !strings.Contains(html, "const YOUR_TURN_GLYPH = '☐';") {
		t.Fatal("YOUR_TURN_GLYPH is not the one place the glyph is written")
	}
	for _, other := range []string{"design-system.html", "components.css", "tokens.css"} {
		raw, err := staticFS.ReadFile("static/" + other)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "☐") {
			t.Fatalf("%s uses the your-turn glyph", other)
		}
	}
	// The marker is a button that opens a menu: no checkbox semantics anywhere in its builder.
	builder := jsFunc(t, html, "yourTurnButtonHTML")
	for _, forbidden := range []string{`role="checkbox"`, `type="checkbox"`, "aria-checked"} {
		if strings.Contains(builder, forbidden) {
			t.Fatalf("marker builder contains %q", forbidden)
		}
	}
	for _, want := range []string{`aria-haspopup="menu"`, `type="button"`, `data-action="openYourTurnMenu"`} {
		if !strings.Contains(builder, want) {
			t.Fatalf("marker builder lacks %q", want)
		}
	}
	// No animation: the marker's CSS carries no keyframes or transitions.
	if i := strings.Index(html, ".yt-marker, .yt-legend-marker {"); i < 0 || strings.Contains(html[i:i+400], "animation") {
		t.Fatal("the marker styles are missing or animate")
	}
	if strings.Contains(html, "@keyframes yt-") {
		t.Fatal("a your-turn animation was added")
	}
}

// The acceptance scenario of "One ownership signal": an L5 hive with a parked
// issue, a level-held agent PR and a hive needs-human PR within the
// adjudicator wait rule.
func TestOneOwnershipSignalOnAnL5Hive11566(t *testing.T) {
	// Band rules are the tooltips on band titles: they describe the band and
	// never order the operator to act, which is the marker's job.
	imperative := regexp.MustCompile(`(?i)(^|[—;:.]|\bor\b)\s*(add|assign|verify|confirm|close|read|rebase|fix|review|merge|pick up|approve|label|resolve|check)\b`)
	cfg := config.DashboardIssueBandsConfig{}
	for _, key := range []string{"ready", "in-progress", "agent-filed", "waiting", "done"} {
		if rule := issueBandSpec(key, cfg).Rule; imperative.MatchString(rule) {
			t.Errorf("issue band %q rule instructs the operator: %q", key, rule)
		}
	}
	for _, key := range []string{"eligible", "blocked", "in-review", "draft", "waiting", "open"} {
		if rule := prBandSpec(key, cfg).Rule; imperative.MatchString(rule) {
			t.Errorf("PR band %q rule instructs the operator: %q", key, rule)
		}
	}
	repos := ladderRepos11566()
	repos[0].OpenPrs = repos[0].OpenPrs[:1] // keep the hive needs-human PR #3 only
	repos[0].ActionableIssues = nil
	status := yourTurnStatus11566(t, 5, nil, repos)
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {}
window._lastStatus = `+yourTurnJSON(t, status)+`;
// The default L5 card keeps the your-turn rows and counts the rest by band: the Gated PR is in the summary.
renderRepos(window._lastStatus.repos);
assert.match(els['repos'].innerHTML, /1 more — PRs: Gated 1/);
assert.equal(els['repos-needs-human'].textContent, '· 2 your turn');
// Expanded, the Gated band lists both PRs.
localStorage.setItem('hive-repos-yourturn-expanded:h', JSON.stringify(['o/r']));
renderRepos(window._lastStatus.repos);
const cards = els['repos'].innerHTML, strip = els['repos-inbox'].innerHTML, legend = els['repos-legend'].innerHTML;
const banned = /needs human|need human review|needs you/i;
for (const text of [cards, strip, legend, els['repos-needs-human'].textContent]) { const hit = text.match(banned); assert.ok(!hit, 'says ' + (hit && text.slice(Math.max(0, hit.index - 120), hit.index + 80))); }
assert.match(cards, /repo-issue-band-title"[^>]*>Parked · 1</);
assert.match(cards, /repo-pr-band-title"[^>]*>Gated · 2</);
const segments = cards.split(/(?=<span class="repo-(?:issue|pr)-pill-wrap)/);
const rowOf = n => segments.find(s => s.includes('<span class="pill-num">#' + n + '</span>'));
assert.match(rowOf(2), /yt-marker/, 'the level-held PR is marked');
assert.doesNotMatch(rowOf(3), /yt-menu-btn/, 'the hive needs-human PR inside the wait is not');
assert.ok(cards.indexOf('repo-pr-band-waiting') > 0 && rowOf(2) && rowOf(3));
assert.equal(els['repos-needs-human'].textContent, '· 2 your turn');
assert.match(strip, />Your turn · 2</);
// Still the label: its badge names the label and where the marker comes from.
assert.match(rowOf(3), /Labelled needs-human: automated fix attempts exhausted\./);
`, "renderRepos", "renderRepoLegend")
	runYourTurnNode(t, script)
}
