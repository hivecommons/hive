package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestWorkSourceSettingsRendersWavefrontConfig(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: work-source settings render rule was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
const _configState = { data: { agents: [] }, dirty: {} };
` + jsFunc(t, html, "esc") + `
let settingHelpSeq = 0;
` + jsFunc(t, html, "dashboardDocsHref") + `
` + jsFunc(t, html, "settingHelpMark") + `
` + jsFunc(t, html, "wsWavefrontSource") + `
` + jsFunc(t, html, "wsWavefrontErrors") + `
` + jsFunc(t, html, "renderGovWorkSource") + `
const out = renderGovWorkSource({ type: 'github', wavefront: {
  enabled: true,
  path: '/data/wavefront/graph.json',
  repo: 'acme/crust',
  receipts_dir: '/data/wavefront/receipts'
}});
assert.match(out, /Wavefront \(Crustify\) migration graph/);
assert.match(out, /crustify-rs\/crustify/);
assert.match(out, /docs\.hivecommons\.dev\/docs\/hive\/integrations/);
assert.match(out, /URL fetches plain JSON only today; there is no auth field/);
assert.match(out, /data-action="wsWavefrontSetEnabled"/);
assert.match(out, /name="wf-source" value="path" checked/);
assert.match(out, /value="\/data\/wavefront\/graph\.json"/);
assert.match(out, /data-arg0="wavefront\.repo"/);
assert.match(out, /data-arg0="wavefront\.receipts_dir"/);
assert.match(renderGovWorkSource({ wavefront: { source: 'url' } }), /name="wf-source" value="url" checked/);
const bad = renderGovWorkSource({ wavefront: { enabled: true, path: '/graph.json', url: 'ftp://example/graph.json', repo: 'crust' } });
assert.match(bad, /Choose exactly one graph source/);
assert.match(bad, /URL must start with http:\/\/ or https:\/\//);
assert.match(bad, /Repo must use owner\/name/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node work-source Wavefront render failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
