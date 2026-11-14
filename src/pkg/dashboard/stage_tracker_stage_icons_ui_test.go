package dashboard

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// The SPA's icon set and the server's allowlist are one list
// (hivecommons/hive#18033): an id the server accepts must draw, and the
// SPA must not draw an id the server would refuse.
func TestStageIconSetMatchesAllowlist(t *testing.T) {
	html := indexHTML(t)
	block := jsConstBlock(t, html, "STAGE_ICON_SET")
	keyRe := regexp.MustCompile(`(?m)^\s*'?([a-z][a-z-]*)'?: '`)
	var js []string
	for _, m := range keyRe.FindAllStringSubmatch(block, -1) {
		js = append(js, m[1])
	}
	want := append([]string{}, workflowmodel.ColumnIconIDs...)
	sort.Strings(js)
	sort.Strings(want)
	if strings.Join(js, ",") != strings.Join(want, ",") {
		t.Errorf("STAGE_ICON_SET ids %v != workflowmodel.ColumnIconIDs %v", js, want)
	}
	if !strings.Contains(html, `const STAGE_ICON_DEFAULT = '`+workflowmodel.ColumnIconDefault+`';`) {
		t.Errorf("STAGE_ICON_DEFAULT is not %q", workflowmodel.ColumnIconDefault)
	}
	for _, want := range []string{
		`js/tracker-stage-icons.js`,
		`function stageIconColumnHtml(col, cls, label)`,
		`function wfEdColumnIconField(c, base, dis)`,
		`function wfEdSetColumnIcon(path, value)`,
		`.ham-seg.ham-wf-seg.done .ham-stage-ico { opacity: 0.6; }`,
		`.ham-emoji-ico {`,
	} {
		if !strings.Contains(html, want) && !strings.Contains(manifestText(t), want) {
			t.Errorf("index.html missing stage-icon wiring %q", want)
		}
	}
}

func manifestText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static-src/manifest.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Under node: names normalise and resolve (unknown → the neutral dot), an
// override must be an allowlisted id or one emoji (markup and words are
// refused and never rendered), and the model strip draws the icon alone
// in done / todo chevrons and ahead of the word in the active one.
func TestStageIconsNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
const STAGE_TRACKER_TERMINAL = 110;
const STAGE_TRACKER_WORKFLOW_UNCOVERED_WORD = 'Not covered';
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
` + jsConstBlock(t, html, "STAGE_ICON_SET") + "\n" + jsConstBlock(t, html, "STAGE_ICON_BY_NAME") + "\nconst STAGE_ICON_DEFAULT = 'dot';\n" +
		jsFunc(t, html, "stageIconNormalizeName") + jsFunc(t, html, "stageIconIsId") + jsFunc(t, html, "stageIconIsEmoji") +
		jsFunc(t, html, "stageIconIsValid") + jsFunc(t, html, "stageIconIdForName") + jsFunc(t, html, "stageIconFor") +
		jsFunc(t, html, "stageIconHtml") + jsFunc(t, html, "stageIconColumnHtml") +
		jsFunc(t, html, "stageTrackerWorkflowColumnIconHtml") + jsFunc(t, html, "stageTrackerWorkflowColumnIndex") +
		jsFunc(t, html, "stageTrackerWorkflowStripItems") + jsFunc(t, html, "stageTrackerWorkflowHeaderHtml") + `
// normalisation and the canonical map
assert.strictEqual(stageIconNormalizeName('  Reviewer   Check '), 'reviewer check');
assert.strictEqual(stageIconNormalizeName('CI / Review'), 'ci review');
assert.strictEqual(stageIconNormalizeName('Deployed · staging'), 'deployed staging');
const expect = { 'Open': 'inbox', 'Actionable': 'bolt', 'Claimed': 'hand', 'PR in progress': 'pull-request', 'CI / Review': 'flask', 'Review': 'eye', 'Merge queue': 'merge', 'Merged': 'check',
  'Requested': 'inbox', 'Accepted': 'checkbox', 'Drafting': 'pencil', 'Reviewer check': 'eye', 'Stakeholder review': 'people', 'Approval': 'thumbs-up', 'Done': 'check',
  'Intake': 'inbox', 'Triage': 'tag', 'Analysis': 'search', 'Writing': 'pencil', 'Publish': 'rocket', 'No such stage': 'dot' };
for (const k of Object.keys(expect)) assert.strictEqual(stageIconIdForName(k), expect[k], k);
for (const id of Object.values(STAGE_ICON_BY_NAME)) assert.ok(stageIconIsId(id), 'map names unknown id ' + id);
// the allowlist
for (const ok of ['', 'bolt', ' merge ', '✅', '🚀', '👍🏽', '👩‍💻', '🇺🇸']) assert.ok(stageIconIsValid(ok), 'valid ' + ok);
for (const bad of ['Bolt', 'sparkle', '<svg>', '<img src=x onerror=alert(1)>', 'a', '✅✅✅', 'ok ✅', 'javascript:']) assert.ok(!stageIconIsValid(bad), 'invalid ' + bad);
assert.strictEqual(stageIconFor({ label: 'Drafting', icon: '<b>x</b>' }), 'pencil', 'invalid override falls back to the label');
assert.strictEqual(stageIconFor({ label: 'Drafting', icon: 'rocket' }), 'rocket');
assert.strictEqual(stageIconFor({ label: 'Drafting', icon: '🚀' }), '🚀');
assert.strictEqual(stageIconHtml('<svg onload=alert(1)>', 'c'), '', 'markup never renders');
assert.strictEqual(stageIconHtml('sparkle', 'c'), '');
const svg = stageIconHtml('bolt', 'ham-ico ham-stage-ico');
assert.ok(svg.startsWith('<svg class="ham-ico ham-stage-ico"') && svg.includes('stroke="currentColor"') && svg.includes('aria-hidden="true"'));
assert.ok(stageIconHtml('bolt', 'c', 'Actionable').includes('role="img" aria-label="Actionable"'));
const emo = stageIconHtml('🚀', 'ham-ico');
assert.ok(emo.includes('ham-emoji-ico') && emo.includes('🚀') && !emo.includes('<svg'));
// the strip: icon alone when inactive, icon + word when active
const wf = { name: 't', version: '1', columns: [
  { id: 'a', label: 'Requested', states: [], numbers: [10] },
  { id: 'b', label: 'Drafting', states: [], numbers: [20], icon: '🚀' },
  { id: 'c', label: 'Review', states: [], numbers: [30], icon: '<script>' },
  { id: 'd', label: 'Done', terminal: true, states: [], numbers: [] },
]};
const lis = stageTrackerWorkflowStripItems({ stage: 20 }, wf, []).split('</li>').filter(Boolean);
assert.strictEqual(lis.length, 4);
assert.ok(lis[0].includes(' done') && lis[0].includes('<svg') && lis[0].includes('ham-stage-ico') && !lis[0].includes('ham-seg-t'), 'done chevron = icon alone: ' + lis[0]);
assert.ok(lis[1].includes(' active') && lis[1].includes('ham-emoji-ico') && lis[1].includes('<span class="ham-seg-t">Drafting</span>'), 'active = icon + word: ' + lis[1]);
assert.ok(lis[2].includes(' todo') && lis[2].includes(STAGE_ICON_SET.eye) && !lis[2].includes('<script>'), 'invalid override falls back and never renders: ' + lis[2]);
assert.ok(lis[3].includes(' todo') && lis[3].includes('cap') && lis[3].includes(STAGE_ICON_SET.check));
assert.ok(lis[0].includes('title="Requested"'), 'label stays the hover');
// header keeps the label and gains the icon
const head = stageTrackerWorkflowHeaderHtml(wf);
assert.ok(head.includes('ham-col-ico') && head.includes('Requested') && head.includes('Done'));
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
