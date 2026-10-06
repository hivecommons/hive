package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSidebarVersionChipHasExplicitAffordance(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="oc-version-chip"`,
		`role="button"`,
		`aria-expanded="false"`,
		`title="Version & upgrade details"`,
		`class="oc-version-chip-chevron"`,
		`.oc-version-chip[aria-expanded="false"] .oc-version-chip-chevron`,
		`.oc-version-bee-chip`,
		`.oc-version-menu:empty { display: none; }`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("version chip missing %q", want)
		}
	}
}

func TestSidebarVersionMenuKeepsReadableValueColumn(t *testing.T) {
	html := indexHTML(t)
	menuRule := cssRule(t, html, ".oc-version-menu")
	for _, want := range []string{
		"--oc-version-menu-width: min(640px, calc(100vw - 24px))",
		"width: var(--oc-version-menu-width)",
		"max-width: var(--oc-version-menu-width)",
	} {
		if !strings.Contains(menuRule, want) {
			t.Fatalf("version menu width rule missing %q in %s", want, menuRule)
		}
	}

	rowRule := cssRule(t, html, ".oc-version-row")
	if !strings.Contains(rowRule, "grid-template-columns: max-content minmax(0, 1fr)") {
		t.Fatalf("version row value column must be allowed to grow: %s", rowRule)
	}
	valueRule := cssRule(t, html, ".oc-version-row strong")
	for _, forbidden := range []string{"text-overflow: ellipsis", "white-space: nowrap", "overflow: hidden"} {
		if strings.Contains(valueRule, forbidden) {
			t.Fatalf("version values should not truncate with %q in %s", forbidden, valueRule)
		}
	}
	if !strings.Contains(html, "function positionVersionMenu()") {
		t.Fatal("version menu should be repositioned to stay inside the viewport when widened")
	}
}

func TestSidebarVersionChipShowsUpgradeProgressAnimation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version chip JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
const assert = require('node:assert/strict');
function escapeHtml(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
const chip = {
  innerHTML: '',
  title: '',
  attrs: {},
  classes: new Set(['oc-version-chip']),
  classList: { toggle(name, on) { on ? chip.classes.add(name) : chip.classes.delete(name); } },
  setAttribute(k, v) { this.attrs[k] = v; }
};
const document = { getElementById(id) { return id === 'oc-version-chip' ? chip : null; } };
` + jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionNowMs") + "\n" +
		jsFunc(t, html, "versionElapsedText") + "\n" +
		jsFunc(t, html, "versionUpgradeProgressStatus") + "\n" +
		jsFunc(t, html, "versionUpgradeHiveHTML") + "\n" +
		jsFunc(t, html, "versionNavbarUpgradeHTML") + "\n" +
		jsFunc(t, html, "renderVersionChip") + `
renderVersionChip({hash:'1111111abcdef', short:'1111111', branch:'v5'}, {deliveryLabel:'v5', upgradeProgress:{target:'94386f3abcdef', targetShort:'94386f3', startedAt:1000}});
assert.ok(chip.innerHTML.includes('oc-version-navbar-upgrade'), chip.innerHTML);
assert.ok(chip.innerHTML.includes('oc-version-hive'), chip.innerHTML);
assert.ok(chip.innerHTML.includes('oc-version-bee'), chip.innerHTML);
assert.ok(chip.innerHTML.includes('Upgrading…'), chip.innerHTML);
assert.ok(chip.innerHTML.includes('→ 94386f3'), chip.innerHTML);
assert.ok(chip.classes.has('is-upgrading'), 'missing is-upgrading class');
assert.equal(chip.attrs['aria-label'], 'Upgrade in progress to 94386f3 — Version & upgrade details');
renderVersionChip({hash:'94386f3abcdef', short:'94386f3', branch:'v5'}, {deliveryLabel:'v5', upgradeProgress:null});
assert.ok(!chip.innerHTML.includes('oc-version-navbar-upgrade'), chip.innerHTML);
assert.ok(chip.innerHTML.includes('oc-version-sha'), chip.innerHTML);
assert.ok(chip.innerHTML.includes('94386f3'), chip.innerHTML);
assert.ok(!chip.classes.has('is-upgrading'), 'stale is-upgrading class');
assert.equal(chip.attrs['aria-label'], 'Version & upgrade details');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar version chip upgrade progress failed: %v\n%s", err, out)
	}
}

func TestSidebarVersionDetailsFitAndFallbackRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version details JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
function escapeHtml(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
function relativeAge(){ return '2h ago'; }
const window = {};
` + jsFunc(t, html, "versionDeliveryLabel") + "\n" +
		jsFunc(t, html, "versionTrackingLabel") + "\n" +
		jsFunc(t, html, "versionTrackingTooltip") + "\n" +
		jsFunc(t, html, "versionCompareURL") + "\n" +
		jsFunc(t, html, "versionStatusText") + "\n" +
		jsFunc(t, html, "versionLastUpgradeText") + "\n" +
		jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionSameCommit") + "\n" +
		jsFunc(t, html, "versionDashHTML") + "\n" +
		jsFunc(t, html, "versionPolicy") + "\n" +
		jsFunc(t, html, "versionManagedSuffix") + "\n" +
		jsFunc(t, html, "versionTrackingSummary") + "\n" +
		jsFunc(t, html, "versionCadenceLabel") + "\n" +
		jsFunc(t, html, "versionStatusSummary") + "\n" +
		jsFunc(t, html, "versionButtonHTML") + "\n" +
		jsFunc(t, html, "renderVersionUpgradeAction") + "\n" +
		jsFunc(t, html, "renderVersionDetails") + `
const out = renderVersionDetails({hash:'07d99d8426ec7b5ae364e25abcdef0123456789', short:'07d99d8', branch:'v5', channel:'candidate', tracking:'floating', latestHash:'07d99d8426ec7b5ae364e25abcdef0123456789', target:{sha:'07d99d8426ec7b5ae364e25abcdef0123456789', short:'07d99d8', managedBy:'hub'}, autoUpdate:{state:'up_to_date', managedBy:'hub', enabled:true, detail:'ok'}, releaseStatus:{attempt:{state:'succeeded', completedAt:'2026-10-02T10:00:00Z'}}}, {deliveryLabel:'candidate (v5)', offeredUpgradeHash:'07d99d8426ec7b5ae364e25abcdef0123456789', offeredUpgradeShort:'07d99d8'});
if (!out.includes('>07d99d8<')) throw new Error('short sha missing: '+out);
if (!out.includes('title="07d99d8426ec7b5ae364e25abcdef0123456789"')) throw new Error('full sha title missing: '+out);
for (const row of ['Tracking','Status','Cadence']) if (!out.includes('>'+row+'</span>')) throw new Error('missing row '+row+': '+out);
if (!out.includes('>Next update</span>') || !out.includes('>unknown</strong>')) throw new Error('next update unknown row missing: '+out);
const eta = renderVersionDetails({hash:'abcdef1234567890', short:'abcdef1', autoUpdate:{state:'behind', enabled:true, nextUpdateAt:'2026-10-03T13:00:00Z'}}, {deliveryLabel:'stable'});
if (!eta.includes('2026-10-03T13:00:00Z') || eta.includes('>unknown</strong>')) throw new Error('next update time missing: '+eta);
const off = renderVersionDetails({hash:'abcdef1234567890', short:'abcdef1', autoUpdate:{state:'disabled', enabled:false}}, {deliveryLabel:'stable'});
if (off.includes('Next update')) throw new Error('next update shown while disabled: '+off);
const fallback = renderVersionDetails({hash:'abcdef1234567890', short:'abcdef1', branch:'v5', channel:'candidate', tracking:'', autoUpdate:{state:'unknown'}}, {deliveryLabel:''});
if (!fallback.includes('candidate (v5)')) throw new Error('tracking fallback missing: '+fallback);
const empty = renderVersionDetails({hash:'abcdef1234567890', short:'abcdef1', tracking:'unknown', autoUpdate:{state:'unknown'}}, {deliveryLabel:''});
if (!empty.includes('>—</strong>')) throw new Error('fallback dash missing: '+empty);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar version details failed: %v\n%s", err, out)
	}
}

func TestSidebarVersionUpgradeActionRules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version upgrade JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
function escapeHtml(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
var _upgradeInProgress = false;
` + jsFunc(t, html, "versionPolicy") + "\n" +
		jsFunc(t, html, "versionCadenceLabel") + "\n" +
		jsFunc(t, html, "versionSameCommit") + "\n" +
		jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionNowMs") + "\n" +
		jsFunc(t, html, "versionElapsedText") + "\n" +
		jsFunc(t, html, "versionUpgradeProgressStatus") + "\n" +
		jsFunc(t, html, "versionUpgradeHiveHTML") + "\n" +
		jsFunc(t, html, "versionBeeProgressHTML") + "\n" +
		jsFunc(t, html, "versionButtonHTML") + "\n" +
		jsFunc(t, html, "versionManualUpgradeActive") + "\n" +
		jsFunc(t, html, "renderVersionUpgradeAction") + `
const fixtures = [
  {
    name:'hub-managed',
    v:{hash:'aaa1111', short:'aaa1111', target:{sha:'bbb2222', short:'bbb2222', managedBy:'hub'}, autoUpdate:{enabled:true, managedBy:'hub'}, upgradePolicy:{schedule:'daily', schedule_hour:13, schedule_timezone:'America/New_York'}, deployment:{upgradeSupported:true, runtime:'kubernetes'}},
    ctx:{offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222', offeredUpgradeLabel:'Hub target'},
    want:'Upgrade to bbb2222', disabled:false, action:true
  },
  {
    name:'manual',
    v:{hash:'aaa1111', short:'aaa1111', target:{sha:'bbb2222', short:'bbb2222'}, autoUpdate:{enabled:false, state:'disabled'}, deployment:{upgradeSupported:true, runtime:'kubernetes'}},
    ctx:{offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222', offeredUpgradeLabel:'target'},
    want:'Upgrade to bbb2222', disabled:false, action:true
  },
  {
    name:'current',
    v:{hash:'bbb2222', short:'bbb2222', target:{sha:'bbb2222', short:'bbb2222'}, autoUpdate:{state:'up_to_date'}, deployment:{upgradeSupported:true}},
    ctx:{offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222'},
    want:'Up to date ✓', disabled:true, action:false
  },
  {
    name:'paused',
    v:{hash:'aaa1111', short:'aaa1111', target:{sha:'bbb2222', short:'bbb2222', paused:true}, autoUpdate:{state:'paused'}, deployment:{upgradeSupported:true}},
    ctx:{offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222'},
    want:'Upgrade paused', disabled:true, action:false
  },
  {
    name:'checking',
    v:{hash:'aaa1111', short:'aaa1111', target:{resolved:false}, autoUpdate:{state:'unknown'}, deployment:{upgradeSupported:true}},
    ctx:{offeredUpgradeHash:'', offeredUpgradeShort:''},
    want:'Checking…', disabled:true, action:false
  },
  {
    name:'progress',
    v:{hash:'aaa1111', short:'aaa1111', target:{sha:'bbb2222'}, deployment:{upgradeSupported:true}},
    ctx:{offeredUpgradeHash:'bbb2222', offeredUpgradeShort:'bbb2222', upgradeProgress:{target:'bbb2222abcdef', targetShort:'bbb2222', startedAt:1000}},
    want:'Upgrading…', disabled:true, action:false
  }
];
for (const f of fixtures) {
  const out = renderVersionUpgradeAction(f.v, f.ctx);
  if (!out.includes('id="spoke-upgrade-btn"')) throw new Error(f.name+' missing button: '+out);
  if (!out.includes(f.want)) throw new Error(f.name+' missing label '+f.want+': '+out);
  if (f.disabled && !out.includes('disabled aria-disabled="true"')) throw new Error(f.name+' should be disabled: '+out);
  if (!f.disabled && out.includes('disabled aria-disabled="true"')) throw new Error(f.name+' should be enabled: '+out);
  if (f.action && !out.includes('data-action="gh27"')) throw new Error(f.name+' missing action: '+out);
  if (!f.action && out.includes('data-action="gh27"')) throw new Error(f.name+' unexpectedly actionable: '+out);
  if (f.action && !out.includes('oc-version-upgrade-available')) throw new Error(f.name+' missing available styling: '+out);
}
const liveStale = {hash:'e3036bc63882580d803e4d195cd72ed9033e3c0d', short:'e3036bc', target:{source:'hub', resolved:true, sha:'eb4cb90', short:'eb4cb90', managedBy:'hub'}, upgradePolicy:{target_sha:'eb4cb90'}, manualUpgrade:{state:'started', target:'990d0b2'}, autoUpdate:{state:'behind', managedBy:'hub'}, deployment:{upgradeSupported:true, runtime:'kubernetes'}};
if (versionManualUpgradeActive(liveStale, liveStale.target.sha)) throw new Error('stale manual upgrade should not remain active');
let liveOut = renderVersionUpgradeAction(liveStale, {offeredUpgradeHash:liveStale.target.sha, offeredUpgradeShort:liveStale.target.short, offeredUpgradeLabel:'Hub target'});
if (!liveOut.includes('Upgrade to eb4cb90') || !liveOut.includes('data-action="gh27"') || liveOut.includes('Upgrading…')) throw new Error('live stale manual state should show enabled upgrade: '+liveOut);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar version upgrade rules failed: %v\n%s", err, out)
	}
}

func TestSidebarVersionUpgradeProgressStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`@media (prefers-reduced-motion: reduce)`,
		`@keyframes ocBeeOrbit`,
		`VERSION_UPGRADE_STORAGE_KEY`,
		`function versionRecordUpgradeStart`,
		`function versionReconcileUpgradeProgress`,
		`function versionMarkUpgradeComplete`,
		`Upgrading…`,
		`oc-version-upgrade-progress`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing upgrade progress wiring %q", want)
		}
	}
}

func TestSidebarVersionUpgradeClickPersistsAndDisables(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version upgrade click JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
const VERSION_UPGRADE_STORAGE_KEY = 'hive.version.upgradeProgress';
const VERSION_UPGRADE_POLL_MS = 5000;
let _versionUpgradePollTimer = null;
var _upgradeInProgress = false;
var _upgradeTargetHash = null;
const localStorage = {data:{}, getItem(k){return this.data[k] || null}, setItem(k,v){this.data[k]=String(v)}, removeItem(k){delete this.data[k]}};
const btn = {disabled:false, textContent:'', style:{}, attrs:{}, setAttribute(k,v){this.attrs[k]=v}};
const document = { getElementById(id){ return id === 'spoke-upgrade-btn' ? btn : null; } };
const window = {_lastVersionData:{hash:'aaa1111abcdef'}};
function showToast(){}
function fetch(){ return new Promise(function(){}); }
function setTimeout(){ return 1; }
function clearTimeout(){}
` + jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionNowMs") + "\n" +
		jsFunc(t, html, "versionWriteUpgradeProgress") + "\n" +
		jsFunc(t, html, "versionRecordUpgradeStart") + "\n" +
		jsFunc(t, html, "versionScheduleUpgradePoll") + "\n" +
		jsFunc(t, html, "selfUpgrade") + `
selfUpgrade('bbb2222abcdef');
if (!btn.disabled || btn.textContent !== 'Upgrading…' || btn.attrs['aria-disabled'] !== 'true') throw new Error('button not disabled as upgrading: '+JSON.stringify(btn));
const stored = JSON.parse(localStorage.data[VERSION_UPGRADE_STORAGE_KEY] || '{}');
if (stored.target !== 'bbb2222abcdef' || stored.targetShort !== 'bbb2222' || stored.startedFrom !== 'aaa1111abcdef') throw new Error('progress not persisted: '+JSON.stringify(stored));
if (!_upgradeInProgress || _upgradeTargetHash !== 'bbb2222abcdef') throw new Error('globals not marked in progress');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar upgrade click failed: %v\n%s", err, out)
	}
}

func TestSidebarVersionProgressClearsAfterMatchedGrace(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar version progress persistence JS was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
const VERSION_UPGRADE_STORAGE_KEY = 'hive.version.upgradeProgress';
const VERSION_UPGRADE_DONE_MS = 30000;
const VERSION_UPGRADE_LONG_MS = 15 * 60 * 1000;
var _upgradeInProgress = true;
var _upgradeTargetHash = 'bbb2222abcdef';
const localStorage = {data:{}, getItem(k){return this.data[k] || null}, setItem(k,v){this.data[k]=String(v)}, removeItem(k){delete this.data[k]}};
` + jsFunc(t, html, "versionShortSHA") + "\n" +
		jsFunc(t, html, "versionSameCommit") + "\n" +
		jsFunc(t, html, "versionPolicy") + "\n" +
		jsFunc(t, html, "versionNowMs") + "\n" +
		jsFunc(t, html, "versionReadUpgradeProgress") + "\n" +
		jsFunc(t, html, "versionWriteUpgradeProgress") + "\n" +
		jsFunc(t, html, "versionClearUpgradeProgress") + "\n" +
		jsFunc(t, html, "versionMarkUpgradeComplete") + "\n" +
		jsFunc(t, html, "versionPolicy") + "\n" +
		jsFunc(t, html, "versionReconcileUpgradeProgress") + `
versionWriteUpgradeProgress({target:'bbb2222abcdef', targetShort:'bbb2222', startedAt:1000});
let st = versionReconcileUpgradeProgress({hash:'bbb2222abcdef'}, 2000);
if (!st || !st.completedAt) throw new Error('match was not marked complete: '+JSON.stringify(st));
st = versionReadUpgradeProgress(st.completedAt + VERSION_UPGRADE_DONE_MS + 1);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null) throw new Error('completed progress was not cleared after grace');
versionWriteUpgradeProgress({target:'ccc3333abcdef', targetShort:'ccc3333', startedAt:1000});
st = versionReconcileUpgradeProgress({hash:'aaa1111'}, 2000);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null || _upgradeInProgress) throw new Error('unknown target progress was not cleared');
versionWriteUpgradeProgress({target:'eee5555abcdef', targetShort:'eee5555', startedAt:1000});
st = versionReconcileUpgradeProgress({hash:'aaa1111', latestHash:'eee5555abcdef', target:{source:'hub', resolved:false}}, 2000);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null || _upgradeInProgress) throw new Error('unresolved hub target progress was not cleared');
versionWriteUpgradeProgress({target:'8bd5272abcdef', targetShort:'8bd5272', startedFrom:'old1111abcdef', startedAt:1000});
st = versionReconcileUpgradeProgress({hash:'8bd5272abcdef', target:{source:'hub', resolved:true, sha:'new9999abcdef'}}, 2000);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null || _upgradeInProgress) throw new Error('completed old target did not clear when a new target is available');
versionWriteUpgradeProgress({target:'fff6666abcdef', targetShort:'fff6666', startedFrom:'old1111abcdef', startedAt:1000});
st = versionReconcileUpgradeProgress({hash:'8bd5272abcdef', target:{source:'hub', resolved:true, sha:'new9999abcdef'}}, 2000);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null || _upgradeInProgress) throw new Error('running sha changed since click did not clear progress');
versionWriteUpgradeProgress({target:'old2222abcdef', targetShort:'old2222', startedAt:1000});
st = versionReconcileUpgradeProgress({hash:'8bd5272abcdef', target:{source:'hub', resolved:true, sha:'new9999abcdef'}}, 2000);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null || _upgradeInProgress) throw new Error('legacy progress without click source did not clear when running differs from target');
versionWriteUpgradeProgress({target:'ddd4444abcdef', targetShort:'ddd4444', startedAt:1000});
st = versionReadUpgradeProgress(1000 + VERSION_UPGRADE_LONG_MS + 1);
if (st !== null || localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY) !== null) throw new Error('expired progress was not cleared');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node sidebar progress persistence failed: %v\n%s", err, out)
	}
}
