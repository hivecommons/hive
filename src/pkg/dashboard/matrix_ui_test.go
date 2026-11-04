package dashboard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMatrixPresentationRetired(t *testing.T) {
	html := indexHTML(t)
	for _, obsolete := range []string{"HIVE_MATRIX", "hiveMatrixStart", "hiveMatrixAttach", "matrix-mode", "matrix-canvas", "mx.effect", "hive.matrix.", `id="oc-matrix-tab"`} {
		if strings.Contains(html, obsolete) {
			t.Errorf("shipped page retains retired Matrix presentation %q", obsolete)
		}
	}
	for _, name := range []string{"js/matrix.js", "css/matrix.css"} {
		if _, err := os.Stat("static-src/" + name); !os.IsNotExist(err) {
			t.Errorf("retired asset %s still exists (err=%v)", name, err)
		}
	}
}

// Exercise the status-controlled visibility of the actual toolbar element.
// The reader browser scenarios follow its shipped href and boot the reader.
func TestCodeActivityToolbarStatus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command(node, "-e", codeActivityToolbarTest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Code activity toolbar: %v\n%s", err, out)
	}
}

const codeActivityToolbarTest = `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const html = fs.readFileSync('static/index.html', 'utf8');
const entry = html.match(/<a [^>]*id="oc-reader-tab"[^>]*>[\s\S]*?<\/a>/)[0];
assert(entry.includes(' hidden')); assert(entry.includes('>Code activity</span>'));
const tab = {hidden: true};
const document = {getElementById(id) {assert.equal(id, 'oc-reader-tab'); return tab;}};
// Use the shipped status handler, including its boolean guard.
const handler = html.match(/if \(typeof data.matrixEnabled === 'boolean'\) \{[^}]*\}/)[0];
for (const [value, expected] of [[undefined, true], [false, true], [true, false], [undefined, false], [false, true]]) {
  vm.runInNewContext(handler, {document, data: {matrixEnabled: value}});
  assert.equal(tab.hidden, expected);
}
`
