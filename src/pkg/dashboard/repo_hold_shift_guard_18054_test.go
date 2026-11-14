package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hivecommons/hive#18054: on the Projects card, Hold on cncf/techdocs #151,
// #153 and #221 "did not stick". GitHub's timeline showed why: each hold
// label came off again a moment after a Hold click on a DIFFERENT row (#151
// released in the same second #283 was held, #153 three seconds after #54,
// #221 two seconds after #139). A hold toggle moves its row into the held
// group and the next status render re-sorts the groups, so the operator
// working down the list hit another item's Release button. These tests pin
// the guard: a Release that lands while the rows are moving is not sent, a
// Hold still is, and one item never has two toggles in flight.

// repoHoldShiftGuardJS is the guard's declarations as shipped: everything
// from REPO_HOLD_SHIFT_GUARD_MS through repoHoldReleaseTooSoon.
func repoHoldShiftGuardJS(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, "const REPO_HOLD_SHIFT_GUARD_MS")
	if start < 0 {
		t.Fatal("index.html does not define REPO_HOLD_SHIFT_GUARD_MS")
	}
	last := jsFunc(t, html, "repoHoldReleaseTooSoon")
	end := strings.Index(html[start:], last)
	if end < 0 {
		t.Fatal("repoHoldReleaseTooSoon is not declared after REPO_HOLD_SHIFT_GUARD_MS")
	}
	return html[start : start+end+len(last)]
}

func runRepoHoldShiftGuardScript(t *testing.T, name, harness, assertions string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the hold shift guard was NOT executed by this run")
	}
	html := indexHTML(t)
	start := strings.Index(html, "const HOLD_LABEL_SPELLINGS = [")
	if start < 0 {
		t.Fatal("index.html does not define HOLD_LABEL_SPELLINGS")
	}
	spellings := html[start : start+strings.Index(html[start:], "\n")]
	script := "let clock = 1000000; Date.now = () => clock;\n" +
		"const window = { _lastStatus: { hiveId: 'h1', repos: [{ full: 'o/r', workBreakdown: { issues: { actionable: 2, hold: 0 } }, actionableIssues: [{ number: 151, labels: [] }, { number: 283, labels: [] }], heldIssues: [], openPrs: [], heldPrs: [] }] } };\n" +
		"let _statusSeqFloor = 0;\n" +
		"const posts = []; const toasts = [];\n" +
		"function showToast(m, k){ toasts.push(k + ':' + m); }\n" +
		"function render(){}\n" +
		// The real repaint path: renderRepos notes the layout on every render.
		"function repaintReposForWidth(){ noteRepoHoldLayout(window._lastStatus.repos, Date.now()); }\n" +
		harness + "\n" +
		spellings + "\n" +
		jsFunc(t, html, "canonicalHiveHoldLabel") + "\n" +
		jsFunc(t, html, "holdLabels") + "\n" +
		jsFunc(t, html, "repoParts") + "\n" +
		jsFunc(t, html, "updateRepoBreakdownForHold") + "\n" +
		jsFunc(t, html, "moveRepoItemHold") + "\n" +
		repoHoldShiftGuardJS(t, html) + "\n" +
		jsFunc(t, html, "toggleRepoItemHold") + "\n" +
		assertions
	path := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("%s failed:\n%s", name, strings.TrimSpace(string(out)))
	}
}

const repoHoldPostingFetch = `
function fetch(url, opts) {
  if (opts) posts.push({ url, body: JSON.parse(opts.body) });
  return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
}
`

func TestRepoHoldReleaseIgnoredWhileRowsMove(t *testing.T) {
	runRepoHoldShiftGuardScript(t, "hold-shift-guard", repoHoldPostingFetch, `
(async () => {
  const fails = [];
  const released = n => posts.some(p => p.url === '/api/repos/o/r/items/' + n + '/hold' && p.body.held === false);
  noteRepoHoldLayout(window._lastStatus.repos, clock); // first paint
  // Hold #151: its row moves into the held group.
  await toggleRepoItemHold('o/r', 151, 'issue', true, { dataset: {}, textContent: '⏸ Hold' });
  // 300 ms later the click aimed at the next Hold lands on #151's Release.
  clock += 300;
  await toggleRepoItemHold('o/r', 151, 'issue', false, { dataset: {}, textContent: '▶ Release' });
  if (released(151)) fails.push('a Release that landed while the rows moved was sent');
  if (!toasts.some(t => t.includes('o/r#151') && t.includes('Release again'))) fails.push('no toast named the unsent Release: ' + JSON.stringify(toasts));
  if (!window._lastStatus.repos[0].heldIssues.some(i => i.number === 151)) fails.push('#151 left the held group');
  // A Hold in the same window is still sent: clicking Hold down a list works.
  await toggleRepoItemHold('o/r', 283, 'issue', true, { dataset: {}, textContent: '⏸ Hold' });
  if (!posts.some(p => p.url === '/api/repos/o/r/items/283/hold' && p.body.held === true)) fails.push('a Hold was delayed by the guard');
  // Once the list has been still for the guard window, Release goes through.
  clock += REPO_HOLD_SHIFT_GUARD_MS;
  await toggleRepoItemHold('o/r', 151, 'issue', false, { dataset: {}, textContent: '▶ Release' });
  if (!released(151)) fails.push('a deliberate Release on a still list was blocked');
  if (fails.length) { console.log(fails.join('\n')); process.exit(1); }
  console.log('ok');
})().catch(e => { console.log(e && e.stack || e); process.exit(1); });
`)
}

func TestRepoHoldToggleOneRequestPerItemInFlight(t *testing.T) {
	runRepoHoldShiftGuardScript(t, "hold-in-flight", `
let finishFirst = null;
function fetch(url, opts) {
  if (opts) {
    posts.push({ url, body: JSON.parse(opts.body) });
    if (!finishFirst) return new Promise(r => { finishFirst = () => r({ ok: true, json: () => Promise.resolve({}) }); });
  }
  return Promise.resolve({ ok: true, json: () => Promise.resolve({}) });
}
`, `
(async () => {
  const fails = [];
  noteRepoHoldLayout(window._lastStatus.repos, clock);
  // The optimistic repaint replaces the clicked button, so its busy mark is
  // gone; a second click on the item's new Release must not race the Hold.
  const first = toggleRepoItemHold('o/r', 151, 'issue', true, { dataset: {}, textContent: '⏸ Hold' });
  clock += 10 * REPO_HOLD_SHIFT_GUARD_MS;
  await toggleRepoItemHold('o/r', 151, 'issue', false, { dataset: {}, textContent: '▶ Release' });
  if (posts.length !== 1) fails.push('second toggle sent while the first was in flight: ' + JSON.stringify(posts));
  finishFirst();
  await first;
  await toggleRepoItemHold('o/r', 151, 'issue', false, { dataset: {}, textContent: '▶ Release' });
  if (posts.length !== 2 || posts[1].body.held !== false) fails.push('Release after the Hold settled was not sent: ' + JSON.stringify(posts));
  if (fails.length) { console.log(fails.join('\n')); process.exit(1); }
  console.log('ok');
})().catch(e => { console.log(e && e.stack || e); process.exit(1); });
`)
}

func TestRepoHoldLayoutSignatureMarksMoves(t *testing.T) {
	runRepoHoldShiftGuardScript(t, "hold-layout-signature", repoHoldPostingFetch, `
const fails = [];
const repos = [{ full: 'o/r', actionableIssues: [{ number: 1 }, { number: 2 }], heldIssues: [{ number: 3 }, { number: 4 }], openPrs: [], heldPrs: [] }];
noteRepoHoldLayout(repos, 100);
if (repoHoldReleaseTooSoon(150)) fails.push('the first paint counted as a move');
noteRepoHoldLayout(repos, 200);
if (repoHoldReleaseTooSoon(250)) fails.push('an unchanged repaint counted as a move');
// A status render that only re-sorts the held group still moves the rows.
repos[0].heldIssues = [{ number: 4 }, { number: 3 }];
noteRepoHoldLayout(repos, 300);
if (!repoHoldReleaseTooSoon(300 + REPO_HOLD_SHIFT_GUARD_MS - 1)) fails.push('a changed held group was not a move');
if (repoHoldReleaseTooSoon(300 + REPO_HOLD_SHIFT_GUARD_MS)) fails.push('the guard outlived its window');
if (fails.length) { console.log(fails.join('\n')); process.exit(1); }
console.log('ok');
`)
}

// renderRepos is what notes each layout; without the call the guard never
// sees the status render that re-sorts the card.
func TestRenderReposNotesHoldLayout(t *testing.T) {
	if src := jsFunc(t, indexHTML(t), "renderRepos"); !strings.Contains(src, "noteRepoHoldLayout(repos, Date.now())") {
		t.Fatal("renderRepos does not call noteRepoHoldLayout; a status re-sort would not arm the Release guard")
	}
}
