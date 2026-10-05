package dashboard

import (
	"os/exec"
	"testing"
)

func TestStandaloneChannelUpgradeButtonUsesRef(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	html := indexHTML(t)
	script := `
const assert = require('node:assert/strict');
function escapeHtml(s) { return String(s || ''); }
` + jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionSameCommit") + "\n" +
		jsFunc(t, html, "versionButtonHTML") + "\n" +
		jsFunc(t, html, "upgradeTargetLabel") + "\n" +
		jsFunc(t, html, "renderVersionUpgradeAction") + `
const v = {hash:'aaaaaaa', target:{source:'channel', channel:'stable', ref:'ghcr.io/hivecommons/hive:stable', sha:'bbbbbbb', short:'bbbbbbb', resolved:true}, deployment:{runtime:'podman-quadlet', upgradeSupported:true}, autoUpdate:{state:'behind'}};
const ctx = {offeredUpgradeHash:'bbbbbbb', offeredUpgradeShort:'bbbbbbb', offeredUpgradeLabel:upgradeTargetLabel(v)};
const out = renderVersionUpgradeAction(v,ctx);
assert.ok(out.includes('data-arg0="ghcr.io/hivecommons/hive:stable"'), out);
assert.ok(out.includes('Tracked channel :stable'),out);
assert.ok(out.includes('bbbbbbb'),out);
v.target.resolved = false;
assert.ok(!renderVersionUpgradeAction(v,ctx).includes('data-action="gh27"'));
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("channel button: %v\n%s", err, out)
	}
}
