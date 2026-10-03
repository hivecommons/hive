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
	// fetchGitVersion reconciles persisted upgrade progress before it renders
	// (versionManualUpgradeActive & co.); without those helpers the try block
	// throws a ReferenceError, the catch swallows it, and the legacy strip is
	// never cleared — a silent pass turned into a silent fail.
	for _, name := range []string{"escapeHtml", "versionDeliveryLabel", "versionTrackingLabel", "versionTrackingTooltip", "upgradeTargetLabel", "versionCompareURL", "versionStatusText", "versionLastUpgradeText", "versionShortSHA", "versionSameCommit", "versionDashHTML", "versionPolicy", "versionManagedSuffix", "versionTrackingSummary", "versionCadenceLabel", "versionStatusSummary", "versionNowMs", "versionReadUpgradeProgress", "versionWriteUpgradeProgress", "versionClearUpgradeProgress", "versionMarkUpgradeComplete", "versionReconcileUpgradeProgress", "versionManualUpgradeActive", "versionElapsedText", "versionUpgradeProgressStatus", "versionBeeProgressHTML", "versionButtonHTML", "renderVersionUpgradeAction", "renderVersionDetails", "renderVersionMenu", "renderVersionChip", "fetchGitVersion"} {
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
const elements = {
  'git-version': {},
  'oc-git-version': {},
  'oc-version-chip': { setAttribute() {}, focus() {} },
  'oc-version-menu': { hidden: true }
};
const document = { getElementById: id => elements[id] || null };
const window = {};
const VERSION_UPGRADE_STORAGE_KEY = 'hive.version.upgradeProgress';
const VERSION_UPGRADE_DONE_MS = 30000;
const VERSION_UPGRADE_LONG_MS = 15 * 60 * 1000;
const localStorage = {data:{}, getItem(k){return this.data[k] || null}, setItem(k,v){this.data[k]=String(v)}, removeItem(k){delete this.data[k]}};
let payload, calls = 0, _upgradeInProgress = false, _upgradeTargetHash = null;
function showToast() {}
function renderReleaseStatus() {}
async function fetch(url) {
  assert.equal(url, '/api/version', 'browser must only read the version API');
  calls++;
  return { json: async () => payload };
}
async function render(overrides = {}) {
  payload = { hash: 'a1b2c3d0123456789', short: 'a1b2c3d', branch: 'v5', ...overrides };
  elements['git-version'].innerHTML = 'stale';
  elements['oc-version-chip'].innerHTML = '';
  elements['oc-version-menu'].innerHTML = '';
  const before = calls;
  await fetchGitVersion();
  assert.equal(calls, before + 1);
  assert.equal(elements['git-version'].innerHTML, '', 'legacy heading version strip should stay empty');
  const chip = elements['oc-version-chip'].innerHTML;
  const menu = elements['oc-version-menu'].innerHTML;
  assert.ok(menu.includes('/commit/a1b2c3d0123456789'));
  assert.ok(menu.includes('a1b2c3d0123456789'));
  assert.ok(chip.includes('a1b2c3d'));
  return { chip, menu, legacy: window._lastVersionHTML || '' };
}
(async () => {
  for (const channel of ['stable', 'candidate', 'edge']) {
    const ref = 'ghcr.io/hivecommons/hive:' + channel;
    const { chip, menu } = await render({ channel, tracking: 'floating', imageRef: ref });
    assert.ok(chip.includes('>' + channel + ' (v5)</span>'));
    assert.ok(menu.includes('Channel</span><strong title="' + channel + ' (v5)">' + channel + ' (v5)</strong>'));
    assert.ok(menu.includes('>floating</strong>'));
    assert.ok(menu.includes('Channel: ' + channel));
    assert.ok(menu.includes('Built from: v5'));
    assert.ok(menu.includes('Image: ' + ref));
  }
  let out = await render({ tracking: 'floating', imageRef: 'hive:v5-latest' });
  assert.ok(out.chip.includes('>v5</span>'));
  assert.ok(!out.menu.includes('Channel:'));
  assert.ok(out.menu.includes('>floating</strong>'));
  out = await render({ tracking: 'pinned', imageRef: 'hive:a1b2c3d' });
  assert.ok(out.menu.includes('>pinned</strong>'));
  assert.ok(out.menu.includes('Built from: v5'));
  out = await render({ channel: 'stable', tracking: 'pinned', imageRef: 'hive:stable@sha256:' + 'a'.repeat(64) });
  assert.ok(out.chip.includes('>stable (v5)</span>'));
  assert.ok(out.menu.includes('>pinned</strong>'));
  for (const tracking of [undefined, 'unknown', 'unexpected']) {
    out = await render({ tracking });
    assert.ok(out.menu.includes('>tracking unknown</strong>'));
    assert.ok(!out.menu.includes('Image:'));
  }
  out = await render({ channel: 'edge', branch: 'unknown', tracking: 'floating' });
  assert.ok(out.chip.includes('>edge</span>'));
  out = await render({ branch: 'unknown' });
  assert.ok(out.menu.includes('>tracking unknown</strong>'));
  out = await render({ channel: '<img src=x onerror="boom">', branch: 'v5"', imageRef: '<script>alert(1)</script>', tracking: 'pinned' });
  assert.ok(!out.chip.includes('<img'));
  assert.ok(!out.menu.includes('<script>'));
  assert.ok(out.menu.includes('&lt;script&gt;'));
  assert.ok(out.menu.includes('&quot;'));
  out = await render({ behind: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', commitsBehind: 2, tracking: 'pinned', deployment: { runtime: 'docker-compose', upgradeSupported: true } });
  assert.ok(out.chip.includes('oc-version-update-dot'));
  assert.ok(out.menu.includes('2 behind'));
  assert.ok(out.menu.includes('Compare with target'));
  assert.ok(out.menu.includes('↑ Upgrade'));
  out = await render({ upgradeMarker: { failed: true, target: 'b2c3d4e', attempts: 3, maxAttempts: 3 }, tracking: 'floating' });
  assert.ok(out.menu.includes('Auto-update failed'));
  assert.ok(out.legacy.includes('auto-update failed'));
  out = await render({ behind: true, autoUpgrade: true, tracking: 'floating', deployment: { runtime: 'kubernetes', upgradeSupported: true } });
  assert.ok(out.legacy.includes('Queued for auto-upgrade'));
  // #6904: the passive badge no longer hides the owner's manual escape hatch.
  assert.ok(out.menu.includes('spoke-upgrade-btn'));
  assert.ok(out.menu.includes('Upgrade now'));
  out = await render({ behind: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', tracking: 'floating', deployment: { runtime: 'unknown', upgradeSupported: false, reason: 'deployment runtime is not explicitly configured' } });
  assert.ok(out.menu.includes('manual update required'));
  // The button stays on screen but disabled, carrying the reason as its title.
  assert.ok(out.menu.includes('id="spoke-upgrade-btn" class="hv-btn btn-primary btn-sm" type="button" disabled aria-disabled="true" title="deployment runtime is not explicitly configured (unknown)"'));
  out = await render({ latestHash: 'a1b2c3d0123456789', tracking: 'floating' });
  assert.ok(out.menu.includes('Up to date'));
  assert.ok(!out.chip.includes('oc-version-update-dot'));
})().catch(err => { console.error(err); process.exitCode = 1; });
`
