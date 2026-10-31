package dashboard

import (
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/webassets/trackerrow"
)

// TestTrackerRowSharedAssetCompat pins the spoke tracker to the shared row
// asset the hub embeds (hivecommons/hive#16062): each shared file is
// byte-identical to its static-src part, listed in manifest.txt, and carried
// verbatim by the committed static/index.html. Any drift between what the
// spoke serves and what the hub would embed fails here.
func TestTrackerRowSharedAssetCompat(t *testing.T) {
	html := indexHTML(t)
	manifest, err := os.ReadFile("static-src/manifest.txt")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(string(manifest), "\n") {
		listed[strings.TrimSpace(line)] = true
	}
	for _, name := range trackerrow.Files {
		shared, err := fs.ReadFile(trackerrow.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		part, err := os.ReadFile("static-src/js/" + name)
		if err != nil {
			t.Fatalf("shared asset %s has no spoke part: %v", name, err)
		}
		if string(shared) != string(part) {
			t.Errorf("pkg/webassets/trackerrow/%s differs from static-src/js/%s; edit both together", name, name)
		}
		if !listed["js/"+name] {
			t.Errorf("manifest.txt does not list js/%s", name)
		}
		if !strings.Contains(html, string(shared)) {
			t.Errorf("static/index.html does not carry trackerrow/%s verbatim", name)
		}
	}
}

// TestTrackerRowSharedAssetRendersSpokeFixtures renders the same fixtures
// through the functions the spoke page defines and through the shared asset
// files, and pins both to the spoke's existing columns and title markup.
func TestTrackerRowSharedAssetRendersSpokeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the shared row asset was NOT executed by this run")
	}
	html := indexHTML(t)
	var shared strings.Builder
	for _, name := range trackerrow.Files {
		b, err := fs.ReadFile(trackerrow.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		shared.Write(b)
		shared.WriteString("\n")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsFunc(t, html, "esc"),
		"const spoke = (function () {",
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSplitTitleTag"),
		jsFunc(t, html, "stageTrackerTitleHtml"),
		"return { anchor: stageTrackerAnchor, split: stageTrackerSplitTitleTag, title: stageTrackerTitleHtml }; })();",
		"const shared = (function () {",
		shared.String(),
		"return { anchor: stageTrackerAnchor, split: stageTrackerSplitTitleTag, title: stageTrackerTitleHtml }; })();",
		`
const stages = [{ number: 70, branch_from: 50 }, { number: 75, branch_from: 60 }];
const anchors = [
  [[0, 'issue'], 0], [[10, 'issue'], 10], [[15, 'issue'], 10], [[17, 'issue'], 10],
  [[20, 'issue'], 20], [[30, 'pr'], 30], [[40, 'issue'], 30], [[45, 'issue'], 10],
  [[50, 'pr'], 50], [[60, 'pr'], 50], [[70, 'pr'], 50], [[75, 'pr'], 50],
  [[80, 'pr'], 80], [[85, 'issue'], 10], [[95, 'pr'], 80], [[100, 'pr'], 80],
  [[110, 'pr'], 110], [[120, 'pr'], 110], [[999, 'pr'], 110],
  [[50, 'pr', { merge_gate: { state: 'merging' } }], 80],
  [[60, 'pr', { merge_gate: { state: 'merge-failed', merge_failed_kind: 'checks-pending' } }], 50],
  [[70, 'pr', { merge_gate: { state: 'merge-failed', merge_failed_kind: 'conflict' } }], 80],
  [[50, 'pr', { merge_train: { state: 'front' } }], 80],
  [[20, 'pr', { merge_train: { state: 'front' } }], 20],
];
for (const [[stage, kind, item], want] of anchors) {
  const got = shared.anchor(stage, stages, kind, item);
  assert.strictEqual(got, spoke.anchor(stage, stages, kind, item), 'spoke and shared anchor disagree at ' + stage + ' ' + kind);
  assert.strictEqual(got, want, 'stage ' + stage + ' ' + kind + ' ' + JSON.stringify(item || {}));
}
const titles = [
  ['[scanner] Fix the <b>thing</b>', '', '<span class="ham-badge ham-title-tag" title="Filed by [scanner]">scanner</span> <span class="ham-title" title="[scanner] Fix the &lt;b&gt;thing&lt;/b&gt;">Fix the &lt;b&gt;thing&lt;/b&gt;</span>'],
  ['[ACMM L0] Score "repo"', 'ham-sub', '<span class="ham-title ham-sub" title="[ACMM L0] Score &quot;repo&quot;">[ACMM L0] Score &quot;repo&quot;</span>'],
  ['[reviewer]', '', '<span class="ham-title" title="[reviewer]">[reviewer]</span>'],
  [null, '', '<span class="ham-title" title=""></span>'],
];
for (const [title, cls, want] of titles) {
  assert.deepStrictEqual(shared.split(title), spoke.split(title));
  const got = shared.title(title, cls);
  assert.strictEqual(got, spoke.title(title, cls), 'spoke and shared title disagree for ' + title);
  assert.strictEqual(got, want);
}
`,
	}, "\n")
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("shared row asset fixtures: %v\n%s", err, out)
	}
}
