package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWritebackKindTableKeepsEveryOverrideField runs the Settings → Features
// write-back kind table (renderWritebackContainmentSection) and its save
// handler (markDirtyWritebackKinds) in node.
//
// PUT /api/config/governor/features replaces the whole writeback.kinds map,
// and the handler rebuilds that map from every row of the table. It used to
// read only the min level and opt-in label cells, so any dashboard edit of
// the table silently dropped unlabeled_min_level and own_only from every
// kind (and an own-only kind's min_level, whose cell is disabled), undoing
// what hive.yaml or the admin MCP writeback.kind_policy operation had set.
//
// The fixture's rows are parsed out of the rendered markup, so the handler
// reads exactly the values the operator sees.
func TestWritebackKindTableKeepsEveryOverrideField(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the write-back kind table save was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"function settingHelpMark() { return ''; }",
		"function yamlHint() { return ''; }",
		"function loadWritebackSuppressed() {}",
		"let dirty = {};",
		"function markDirty(section, key, value) { dirty[section] = dirty[section] || {}; dirty[section][key] = value; }",
		"let rows = [];",
		"const document = { querySelectorAll: sel => sel === '#wb-kind-table tr[data-wb-kind]' ? rows : [] };",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "renderWritebackContainmentSection"),
		jsFunc(t, html, "markDirtyWritebackKinds"),
	}, "\n") + writebackKindTableAssertions

	path := filepath.Join(t.TempDir(), "wbkinds.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("write-back kind table check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const writebackKindTableAssertions = `
const assert = require('node:assert/strict');
const decode = s => s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&#96;/g, '` + "`" + `').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');

// A browser-shaped row per <tr data-wb-kind>: dataset from its attributes,
// and each control's value as the browser would report it (a select with no
// selected option reports its first option, the "default" one, value "").
function parseRows(markup) {
  const out = [];
  const trRe = /<tr data-wb-kind="([^"]*)"([^>]*)>([\s\S]*?)<\/tr>/g;
  let m;
  while ((m = trRe.exec(markup))) {
    const dataset = { wbKind: decode(m[1]) };
    const ov = /data-wb-override="([^"]*)"/.exec(m[2]);
    if (ov) dataset.wbOverride = decode(ov[1]);
    const controls = {};
    // Quote-aware: data-setting-key values carry "<key>", whose ">" would
    // end a naive [^>]* match before value= and disabled.
    const input = /<input(?:"[^"]*"|[^">])*class="wb-kind-level"(?:"[^"]*"|[^">])*>/.exec(m[3]);
    if (input) {
      const v = / value="([^"]*)"/.exec(input[0]);
      controls['.wb-kind-level'] = { value: v ? decode(v[1]) : '', disabled: / disabled[ >]/.test(input[0]) };
    }
    const selRe = /<select class="(wb-kind-[a-z]+)"[^>]*>([\s\S]*?)<\/select>/g;
    let s;
    while ((s = selRe.exec(m[3]))) {
      const picked = /<option value="([^"]*)" selected>/.exec(s[2]);
      controls['.' + s[1]] = { value: picked ? picked[1] : '', disabled: false };
    }
    out.push({ dataset, controls, querySelector: q => controls[q] || null });
  }
  return out;
}
const byKind = kind => rows.find(r => r.dataset.wbKind === kind);
const save = () => { dirty = {}; markDirtyWritebackKinds(); return dirty.features.writebackKinds; };

const f = {
  // Effective policies, as GET /api/config/governor/features returns them.
  writebackKindMatrix: [
    { kind: 'base_refresh', policy: { min_level: 6, require_label: true, unlabeled_min_level: 6 } },
    { kind: 'stage_label', policy: { min_level: 5, require_label: false } },
    { kind: 'sentinel', policy: { min_level: 5, require_label: true, unlabeled_min_level: 6 } },
    { kind: 'hold_gate_notice', policy: { min_level: 3, require_label: false, own_only: true } },
    { kind: 'welcome_comment', policy: { min_level: 5, require_label: true, unlabeled_min_level: 6 } },
  ],
  // The operator's overrides (writeback.kinds).
  writebackKinds: {
    base_refresh: { unlabeled_min_level: 6 },
    stage_label: { require_label: false },
    sentinel: { own_only: false },
    hold_gate_notice: { min_level: 3, own_only: true },
  },
};
const markup = renderWritebackContainmentSection(f, {});
rows = parseRows(markup);
assert.equal(rows.length, 5);

// The table shows and edits all four fields.
for (const want of ['<th>Without label', '<th>Hive-authored only',
  'data-setting-key="writeback.kinds.<key>.unlabeled_min_level"',
  'data-setting-key="writeback.kinds.<key>.own_only"']) {
  assert(markup.includes(want), 'table markup lacks ' + want);
}
assert.equal(byKind('base_refresh').controls['.wb-kind-unlabeled'].value, '6', 'base_refresh shows its unlabeled_min_level override');
assert.equal(byKind('sentinel').controls['.wb-kind-own'].value, 'false', 'sentinel shows its own_only: false override');
assert.equal(byKind('hold_gate_notice').controls['.wb-kind-own'].value, 'true');
assert.equal(byKind('welcome_comment').controls['.wb-kind-unlabeled'].value, '', 'no override renders the default option');

// Saving the table untouched sends every override back as it was. On the
// old handler base_refresh, sentinel and hold_gate_notice all came back null.
assert.deepStrictEqual(save(), {
  base_refresh: { unlabeled_min_level: 6 },
  stage_label: { require_label: false },
  sentinel: { own_only: false },
  hold_gate_notice: { min_level: 3, own_only: true },
  welcome_comment: null,
});

// Editing one kind keeps the untouched ones, including base_refresh whose
// override is only unlabeled_min_level.
byKind('stage_label').controls['.wb-kind-level'].value = '4';
assert.deepStrictEqual(save(), {
  base_refresh: { unlabeled_min_level: 6 },
  stage_label: { require_label: false, min_level: 4 },
  sentinel: { own_only: false },
  hold_gate_notice: { min_level: 3, own_only: true },
  welcome_comment: null,
});

// The new cells write their fields, and setting one back to default drops
// just that field (a kind left with none sends null, as before).
byKind('base_refresh').controls['.wb-kind-unlabeled'].value = '5';
byKind('welcome_comment').controls['.wb-kind-own'].value = 'true';
byKind('sentinel').controls['.wb-kind-own'].value = '';
assert.deepStrictEqual(save(), {
  base_refresh: { unlabeled_min_level: 5 },
  stage_label: { require_label: false, min_level: 4 },
  sentinel: null,
  hold_gate_notice: { min_level: 3, own_only: true },
  welcome_comment: { own_only: true },
});
`
