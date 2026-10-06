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
	for _, name := range []string{
		"escapeHtml", "versionDeliveryLabel", "versionTrackingLabel", "versionTrackingTooltip", "upgradeTargetLabel", "versionCompareURL", "versionStatusText", "versionLastUpgradeText",
		"versionNowMs", "versionReadUpgradeProgress", "versionWriteUpgradeProgress", "versionClearUpgradeProgress", "versionMarkUpgradeComplete", "versionReconcileUpgradeProgress", "versionElapsedText", "versionScheduleUpgradePoll",
		"versionShortSHA", "versionSameCommit", "versionDashHTML", "versionPolicy", "versionManagedSuffix", "versionTrackingSummary", "versionCadenceLabel", "versionStatusSummary",
		"versionUpgradeProgressStatus", "versionUpgradeHiveHTML", "versionBeeProgressHTML", "versionButtonHTML", "renderVersionUpgradeAction", "renderVersionDetails", "versionManualUpgradeActive",
		"versionNavbarUpgradeHTML", "ensureVersionMenuPortal", "renderVersionMenu", "renderVersionChip", "renderNavbarUpgradeIndicator", "renderVersionSurfaces", "fetchGitVersion",
	} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(versionProvenanceAssertions)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("version renderer failed: %v\n%s", err, out)
	}
}

func TestVersionPopoverEscapesSidebarClipping(t *testing.T) {
	html := indexHTML(t)
	mustContain := []string{
		`function ensureVersionMenuPortal()`,
		`document.body.appendChild(menu)`,
		`const menu = ensureVersionMenuPortal();`,
		`.oc-version-menu {`,
		`position: fixed`,
		`--oc-version-menu-min-width: min(420px, calc(100vw - 24px))`,
		`document.querySelector('.oc-sidebar')`,
		`placeBesideSidebar`,
		`menu.contains(e.target)`,
	}
	for _, snippet := range mustContain {
		if !strings.Contains(html, snippet) {
			t.Fatalf("version popover portal/clipping guard missing %q", snippet)
		}
	}
	sidebarStart := strings.Index(html, `.oc-sidebar {`)
	if sidebarStart < 0 {
		t.Fatalf("sidebar CSS rule missing")
	}
	sidebarEnd := strings.Index(html[sidebarStart:], `}`)
	if sidebarEnd < 0 {
		t.Fatalf("sidebar CSS rule is malformed")
	}
	sidebarRule := html[sidebarStart : sidebarStart+sidebarEnd]
	if !strings.Contains(sidebarRule, `overflow-y: auto`) {
		t.Fatalf("test fixture no longer covers sidebar overflow clipping risk: %s", sidebarRule)
	}
	if strings.Contains(html, `#oc-git-version #spoke-upgrade-btn`) {
		t.Fatalf("upgrade action styling must not depend on the portaled menu staying under #oc-git-version")
	}
}

const versionProvenanceAssertions = `
const assert = require('node:assert/strict');
const elements = {
  'git-version': {},
  'oc-git-version': {},
  'oc-version-chip': { setAttribute() {}, focus() {} },
  'oc-version-menu': { hidden: true },
  'oc-navbar-upgrade': { hidden: true, setAttribute() {} }
};
const document = { getElementById: id => elements[id] || null };
const window = {};
let payload, calls = 0, _upgradeInProgress = false, _upgradeTargetHash = null;
const VERSION_UPGRADE_STORAGE_KEY = 'hive.version.upgradeProgress';
const VERSION_UPGRADE_DONE_MS = 30000;
const VERSION_UPGRADE_LONG_MS = 15 * 60 * 1000;
const VERSION_UPGRADE_POLL_MS = 5000;
let _versionUpgradePollTimer = null;
const localStorage = { data: {}, getItem(k) { return this.data[k] ?? null; }, setItem(k, v) { this.data[k] = String(v); }, removeItem(k) { delete this.data[k]; } };
function setTimeout() { return 1; }
function clearTimeout() {}
function relativeAge() { return '2h ago'; }
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
  elements['oc-navbar-upgrade'].innerHTML = '';
  elements['oc-navbar-upgrade'].hidden = true;
  const before = calls;
  await fetchGitVersion();
  assert.equal(calls, before + 1);
  assert.equal(elements['git-version'].innerHTML, '', 'legacy heading version strip should stay empty');
  const chip = elements['oc-version-chip'].innerHTML;
  const menu = elements['oc-version-menu'].innerHTML;
  const navbar = elements['oc-navbar-upgrade'].innerHTML;
  assert.ok(menu.includes('/commit/a1b2c3d0123456789'));
  assert.ok(menu.includes('a1b2c3d0123456789'));
  if (!chip.includes('oc-version-navbar-upgrade')) assert.ok(chip.includes('a1b2c3d'));
  return { chip, menu, navbar, navbarHidden: elements['oc-navbar-upgrade'].hidden, legacy: window._lastVersionHTML || '' };
}
(async () => {
  for (const channel of ['stable', 'candidate', 'edge']) {
    const ref = 'ghcr.io/hivecommons/hive:' + channel;
    const { chip, menu } = await render({ channel, tracking: 'floating', imageRef: ref });
    assert.ok(chip.includes('>' + channel + ' (v5)</span>'));
    assert.ok(menu.includes('Channel</span><strong title="' + channel + ' (v5)">' + channel + ' (v5)</strong>'));
    assert.ok(menu.includes('Tracking: floating (mutable tag)'));
    assert.ok(menu.includes('Channel: ' + channel));
    assert.ok(menu.includes('Built from: v5'));
    assert.ok(menu.includes('Image: ' + ref));
  }
  let out = await render({ tracking: 'floating', imageRef: 'hive:v5-latest' });
  assert.ok(out.chip.includes('>v5</span>'));
  assert.ok(!out.menu.includes('Channel:'));
  assert.ok(out.menu.includes('Tracking: floating (mutable tag)'));
  out = await render({ tracking: 'pinned', imageRef: 'hive:a1b2c3d' });
  assert.ok(out.menu.includes('>pinned a1b2c3d</strong>'));
  assert.ok(out.menu.includes('Tracking: pinned (immutable tag or digest)'));
  assert.ok(out.menu.includes('Built from: v5'));
  out = await render({ channel: 'stable', tracking: 'pinned', imageRef: 'hive:stable@sha256:' + 'a'.repeat(64) });
  assert.ok(out.chip.includes('>stable (v5)</span>'));
  assert.ok(out.menu.includes('>pinned a1b2c3d</strong>'));
  for (const tracking of [undefined, 'unknown', 'unexpected']) {
    out = await render({ tracking });
    assert.ok(out.menu.includes('Tracking: unknown (authoritative image reference unavailable)'));
    assert.ok(!out.menu.includes('Image:'));
  }
  out = await render({ channel: 'edge', branch: 'unknown', tracking: 'floating' });
  assert.ok(out.chip.includes('>edge</span>'));
  out = await render({ branch: 'unknown' });
  assert.ok(out.menu.includes('Tracking: unknown (authoritative image reference unavailable)'));
  out = await render({ channel: '<img src=x onerror="boom">', branch: 'v5"', imageRef: '<script>alert(1)</script>', tracking: 'pinned' });
  assert.ok(!out.chip.includes('<img'));
  assert.ok(!out.menu.includes('<script>'));
  assert.ok(out.menu.includes('&lt;script&gt;'));
  assert.ok(out.menu.includes('&quot;'));
  out = await render({ behind: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', commitsBehind: 2, tracking: 'pinned', deployment: { runtime: 'docker-compose', upgradeSupported: true } });
  assert.ok(out.chip.includes('oc-version-update-dot'));
  assert.ok(out.menu.includes('upgrade available → b2c3d4e'));
  assert.ok(out.menu.includes('Compare with target'));
  assert.ok(out.menu.includes('Upgrade to b2c3d4e'));
  assert.ok(out.menu.includes('data-action="gh27"'));
  out = await render({ upgradeMarker: { failed: true, target: 'b2c3d4e', attempts: 3, maxAttempts: 3 }, autoUpdate: { state: 'failed' }, tracking: 'floating' });
  assert.ok(out.menu.includes('upgrade failed'));
  assert.ok(out.legacy.includes('auto-update failed'));
  out = await render({ behind: true, autoUpgrade: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', tracking: 'floating', deployment: { runtime: 'kubernetes', upgradeSupported: true } });
  assert.ok(out.legacy.includes('Queued for auto-upgrade'));
  // #6904: the passive badge no longer hides the owner's manual escape hatch.
  assert.ok(out.menu.includes('spoke-upgrade-btn'));
  assert.ok(out.menu.includes('Upgrade to b2c3d4e'));
  assert.ok(out.menu.includes('data-action="gh27"'));
  localStorage.setItem(VERSION_UPGRADE_STORAGE_KEY, JSON.stringify({ target: 'b2c3d4e', targetShort: 'b2c3d4e', startedFrom: 'a1b2c3d0123456789', startedAt: versionNowMs() - 120000 }));
  out = await render({ latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', tracking: 'floating', deployment: { runtime: 'kubernetes', upgradeSupported: true } });
  assert.equal(out.navbarHidden, false);
  assert.ok(out.navbar.includes('oc-version-navbar-upgrade'));
  assert.ok(out.navbar.includes('Upgrading'));
  assert.ok(out.navbar.includes('b2c3d4e'));
  versionClearUpgradeProgress();
  out = await render({ behind: true, latestHash: 'b2c3d4e', latestShort: 'b2c3d4e', tracking: 'floating', deployment: { runtime: 'unknown', upgradeSupported: false, reason: 'deployment runtime is not explicitly configured' } });
  assert.ok(out.menu.includes('deployment runtime is not explicitly configured (unknown)'));
  assert.ok(out.menu.includes('disabled aria-disabled="true"'));
  assert.ok(!out.menu.includes('data-action="gh27"'));
  out = await render({ latestHash: 'a1b2c3d0123456789', tracking: 'floating' });
  assert.ok(out.menu.includes('Up to date'));
  assert.ok(!out.chip.includes('oc-version-update-dot'));
})().catch(err => { console.error(err); process.exitCode = 1; });
`
