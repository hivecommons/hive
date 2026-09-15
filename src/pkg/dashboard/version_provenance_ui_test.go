package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Execute the shipped fetch/render path against API fixtures. This checks both
// version locations, escaping, and existing behind/upgrade affordances.
func TestVersionProvenanceRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: version rendering behavior was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	for _, name := range []string{"escapeHtml", "versionDeliveryLabel", "versionTrackingLabel", "versionTrackingTooltip", "fetchGitVersion"} {
		if name == "fetchGitVersion" {
			source.WriteString("async ")
		}
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(versionProvenanceAssertions)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("version renderer failed: %v\n%s", err, out)
	}
}

const versionProvenanceAssertions = `
const assert = require('node:assert/strict');
const elements = { 'git-version': {}, 'oc-git-version': {} };
const document = { getElementById: id => elements[id] };
let payload, calls = 0, _upgradeInProgress = false, _upgradeTargetHash = null;
async function fetch(url) {
  assert.equal(url, '/api/version', 'browser must only read the version API');
  calls++;
  return { json: async () => payload };
}
async function render(overrides = {}) {
  payload = { hash: 'a1b2c3d0123456789', short: 'a1b2c3d', branch: 'v5', ...overrides };
  elements['git-version'].innerHTML = '';
  elements['oc-git-version'].innerHTML = '';
  const before = calls;
  await fetchGitVersion();
  assert.equal(calls, before + 1);
  const html = elements['git-version'].innerHTML;
  assert.equal(html, elements['oc-git-version'].innerHTML);
  assert.ok(html.includes('/commit/a1b2c3d0123456789'));
  assert.ok(html.includes('>a1b2c3d</a>'));
  return html;
}
(async () => {
  for (const channel of ['stable', 'candidate', 'edge']) {
    const ref = 'ghcr.io/hivecommons/hive:' + channel;
    const html = await render({ channel, tracking: 'floating', imageRef: ref });
    assert.ok(html.includes('>' + channel + ' (v5)</span>'));
    assert.ok(html.includes('· floating</span>'));
    assert.ok(html.includes('Channel: ' + channel));
    assert.ok(html.includes('Built from: v5'));
    assert.ok(html.includes('Image: ' + ref));
  }
  let html = await render({ tracking: 'floating', imageRef: 'hive:v5-latest' });
  assert.ok(html.includes('>v5</span>'));
  assert.ok(!html.includes('Channel:'));
  assert.ok(html.includes('· floating</span>'));
  html = await render({ tracking: 'pinned', imageRef: 'hive:a1b2c3d' });
  assert.ok(html.includes('· pinned</span>'));
  assert.ok(html.includes('Built from branch v5'));
  html = await render({ channel: 'stable', tracking: 'pinned', imageRef: 'hive:stable@sha256:' + 'a'.repeat(64) });
  assert.ok(html.includes('>stable (v5)</span>'));
  assert.ok(html.includes('· pinned</span>'));
  for (const tracking of [undefined, 'unknown', 'unexpected']) {
    html = await render({ tracking });
    assert.ok(html.includes('· tracking unknown</span>'));
    assert.ok(!html.includes('Image:'));
  }
  html = await render({ channel: 'edge', branch: 'unknown', tracking: 'floating' });
  assert.ok(html.includes('>edge</span>'));
  html = await render({ branch: 'unknown' });
  assert.ok(html.includes('· tracking unknown</span>'));
  html = await render({ channel: '<img src=x onerror="boom">', branch: 'v5"', imageRef: '<script>alert(1)</script>', tracking: 'pinned' });
  assert.ok(!html.includes('<img'));
  assert.ok(!html.includes('<script>'));
  assert.ok(html.includes('&lt;script&gt;'));
  assert.ok(html.includes('&quot;'));
  html = await render({ behind: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', commitsBehind: 2, tracking: 'pinned', deployment: { runtime: 'docker-compose', upgradeSupported: true } });
  assert.ok(html.includes('2 behind</span>'));
  assert.ok(html.includes('Upgrade available'));
  html = await render({ behind: true, autoUpgrade: true, tracking: 'floating', deployment: { runtime: 'kubernetes', upgradeSupported: true } });
  assert.ok(html.includes('Queued for auto-upgrade'));
  // #6904: the passive badge no longer hides the owner's manual escape hatch.
  assert.ok(html.includes('spoke-upgrade-btn'));
  assert.ok(html.includes('Upgrade now'));
  html = await render({ behind: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', tracking: 'floating', deployment: { runtime: 'unknown', upgradeSupported: false, reason: 'deployment runtime is not explicitly configured' } });
  assert.ok(html.includes('manual update required'));
  assert.ok(!html.includes('spoke-upgrade-btn'));
  html = await render({ latestHash: 'a1b2c3d0123456789', tracking: 'floating' });
  assert.ok(html.includes('Up to date with remote'));
})().catch(err => { console.error(err); process.exitCode = 1; });
`
