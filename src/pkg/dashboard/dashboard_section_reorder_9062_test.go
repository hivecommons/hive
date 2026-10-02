package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

var dashboardLayoutIDs9062 = []string{
	"overview-section", "governor", "advisory-section", "token-panel", "cost-panel",
	"repos-section", "beads-section", "acmm-eval-section", "approvals-section", "platform-section",
	"audit-section", "review-queue-section", "nous-section", "inception-section", "knowledge-section", "contributors-section",
	"debug-section", "logs-section", "agents-section", "faq-section",
}

func dashboardLayoutPreamble9062() string {
	return `var DASHBOARD_LAYOUT_KEY='hive.dashboard.layout';
var DASHBOARD_LAYOUT_VERSION=1;
var DASHBOARD_LAYOUT_REGIONS=['main'];
var DASHBOARD_LAYOUT_TEMPLATE={main:['overview-section','governor','advisory-section','token-panel','cost-panel','repos-section','beads-section','acmm-eval-section','approvals-section','platform-section','audit-section','nous-section','inception-section','knowledge-section','contributors-section','debug-section','logs-section','agents-section','faq-section']};
var DASHBOARD_LAYOUT_GRIP_SELECTOR='[data-dashboard-grip]';
var DASHBOARD_LAYOUT_CARD_SELECTOR='[data-dashboard-section]';
var DASHBOARD_LAYOUT_ANCHOR_ID='dashboard-notices';
var DASHBOARD_LAYOUT_PRESETS_KEY='hive.layout.presets';
var DASHBOARD_LAYOUT_APPLIED_KEY='hive.layout.applied';
var DASHBOARD_LAYOUT_SNAPSHOT_VERSION=1;
var DASHBOARD_LAYOUT_PRESET_LIMIT=5;
var DASHBOARD_LAYOUT_DEFAULT_NAME='My layout';
const SECTION_LS_PREFIX='hive-section-collapsed-';
const AGENT_COMPACT_LS_PREFIX='hive-agent-compact-';
const ALL_AGENTS_COMPACT_KEY='hive-all-agents-compact';
var dashboardDragState=null;
var dashboardKeyboardSnapshot=null;
var window={_lastStatus:{features:{strategy_lab:true}}};
function dashboardFeatureEnabled(name){try{return !!(window._lastStatus&&window._lastStatus.features&&window._lastStatus.features[name]===true);}catch(e){return false;}}
function strategyLabEnabled(){return dashboardFeatureEnabled('strategy_lab');}
function dashboardFeatureSectionHidden(id){return id==='nous-section'&&!strategyLabEnabled();}
`
}

func TestDashboardSectionReorderStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, id := range dashboardLayoutIDs9062 {
		if id == "governor" {
			continue // kept as the historical exact opening tag; initDashboardSectionLayout marks it at runtime.
		}
		if !strings.Contains(html, `data-dashboard-section="`+id+`"`) {
			t.Fatalf("dashboard section %q is not marked reorderable", id)
		}
	}
	for _, want := range []string{
		`hive.dashboard.layout`,
		`DASHBOARD_LAYOUT_TEMPLATE={main:['overview-section','governor','advisory-section','token-panel','cost-panel','repos-section','beads-section','acmm-eval-section','approvals-section','platform-section','audit-section','review-queue-section','nous-section','inception-section','knowledge-section','contributors-section','debug-section','logs-section','agents-section','faq-section']}`,
		`id="oc-gh-menu-layout-save"`,
		`id="oc-gh-menu-layout-reset"`,
		`id="oc-gh-menu-layout-export"`,
		`id="dashboard-layout-live" aria-live="polite"`,
		`class="dashboard-grip"`,
		`data-dashboard-grip`,
		`setPointerCapture`,
		`pointerdown`,
		`pointermove`,
		`pointerup`,
		`pointercancel`,
		`ArrowUp`,
		`ArrowDown`,
		`Escape`,
		`function dashboardLayoutNormalize`,
		`function dashboardApplyLayout`,
		`function resetDashboardLayout`,
		`function dashboardSortSidebar`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing dashboard reorder wiring %q", want)
		}
	}
	if strings.Contains(html, `runs-section`) {
		t.Fatal("v5 dashboard layout unexpectedly includes runs-section; keep the template pinned to the actual v5 top-level ids")
	}
}

func TestDashboardSectionReorderTopLevelGrips(t *testing.T) {
	html := indexHTML(t)
	for _, id := range dashboardLayoutIDs9062 {
		idx := strings.Index(html, `data-dashboard-section="`+id+`"`)
		if id == "governor" {
			idx = strings.Index(html, `<div class="governor" id="governor">`)
		}
		if idx < 0 {
			t.Fatalf("missing reorderable section %q", id)
		}
		window := html[idx:]
		if len(window) > 1200 {
			window = window[:1200]
		}
		if !strings.Contains(window, `data-dashboard-grip`) {
			dynamicGrip := map[string]string{
				"governor":    `aria-label="Move Governor section"`,
				"token-panel": `aria-label="Move Token Usage section"`,
				"cost-panel":  `aria-label="Move Cost section"`,
			}[id]
			if dynamicGrip == "" || !strings.Contains(html, dynamicGrip) {
				t.Fatalf("section %q has no grip in its header/render template", id)
			}
		}
	}
}

func TestDashboardLayoutNormalizeFutureUnknownAndMissing(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — dashboard layout normalization was NOT executed by this run")
	}
	html := indexHTML(t)
	script := dashboardLayoutPreamble9062() + jsFunc(t, html, "dashboardLayoutAllIds") + "\n" + jsFunc(t, html, "dashboardLayoutNormalize") + `
const got = dashboardLayoutNormalize({v:99, main:['faq-section','unknown','governor','faq-section']});
const want = ['faq-section','overview-section','governor','advisory-section','token-panel','cost-panel','repos-section','beads-section','acmm-eval-section','approvals-section','platform-section','audit-section','nous-section','inception-section','knowledge-section','contributors-section','debug-section','logs-section','agents-section'];
if (got.v !== 1) throw new Error('version not normalized: '+got.v);
if (JSON.stringify(got.main) !== JSON.stringify(want)) throw new Error('normalized order '+JSON.stringify(got.main));
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dashboard normalize failed: %v\n%s", err, out)
	}
}

func TestDashboardLayoutNormalizeDropsStrategyLabWhenFeatureHidden(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — dashboard layout normalization was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Replace(dashboardLayoutPreamble9062(), "strategy_lab:true", "strategy_lab:false", 1) + jsFunc(t, html, "dashboardLayoutAllIds") + "\n" + jsFunc(t, html, "dashboardLayoutNormalize") + `
const got = dashboardLayoutNormalize({v:1, main:['nous-section','overview-section','unknown']}).main;
if (got.includes('nous-section')) throw new Error('hidden Strategy Lab survived normalization: '+JSON.stringify(got));
if (got[0] !== 'overview-section') throw new Error('unexpected first section: '+JSON.stringify(got));
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dashboard hidden-feature normalize failed: %v\n%s", err, out)
	}
}

func TestDashboardApplyResetAndSidebarSort(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — dashboard layout apply/sidebar behavior was NOT executed by this run")
	}
	html := indexHTML(t)
	funcs := []string{"dashboardEnsureSections", "dashboardRegionEl", "dashboardCardEl", "dashboardLayoutAnnounce", "dashboardLayoutAllIds", "dashboardLayoutNormalize", "dashboardLayoutCurrent", "dashboardLayoutEquals", "dashboardLayoutExtraKeys", "dashboardLayoutSnapshot", "dashboardNormalizeSnapshot", "dashboardSnapshotEquals", "dashboardLayoutDefaultSnapshot", "dashboardReadLayoutPresets", "dashboardWriteLayoutPresets", "dashboardReadAppliedLayout", "dashboardMarkAppliedLayout", "dashboardUpdateLayoutMenuState", "dashboardUpdateResetButton", "dashboardLayoutWrite", "dashboardIsSectionNode", "dashboardTopAnchor", "dashboardPinNoticeAnchor", "dashboardFirstSection", "dashboardInsertSection", "dashboardApplyLayout", "dashboardSortSidebar", "resetDashboardLayout"}
	var b strings.Builder
	b.WriteString(dashboardLayoutPreamble9062())
	b.WriteString(`
class El {
  constructor(id, attr, val){ this.id=id; this.children=[]; this.parentElement=null; this.attrs={}; this.style={}; this.hidden=false; this.textContent=''; this.innerHTML=''; this.classList={toggle(){},add(){},remove(){},contains(){return false}}; if(attr)this.attrs[attr]=val; }
  getAttribute(n){ return this.attrs[n] || null; }
  setAttribute(n,v){ this.attrs[n]=v; }
  appendChild(ch){ if(ch.parentElement) ch.parentElement.children=ch.parentElement.children.filter(x=>x!==ch); ch.parentElement=this; this.children.push(ch); return ch; }
  insertBefore(ch,before){ if(ch.parentElement) ch.parentElement.children=ch.parentElement.children.filter(x=>x!==ch); ch.parentElement=this; const i=this.children.indexOf(before); if(i<0)this.children.push(ch); else this.children.splice(i,0,ch); return ch; }
  querySelector(sel){ if(sel === ':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR) return this.children.find(c=>c.attrs['data-dashboard-section']); return null; }
  querySelectorAll(sel){ if(sel === ':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR) return this.children.filter(c=>c.attrs['data-dashboard-section']); if(sel === '.oc-nav-item') return this.children.filter(c=>c.attrs['data-section'] || c.attrs['class']==='oc-nav-item'); return []; }
  closest(sel){ return sel === '.oc-nav-group' ? this.parentElement : null; }
}
const localStorage = {data:{}, setItem(k,v){this.data[k]=String(v)}, getItem(k){return this.data[k] ?? null}, removeItem(k){delete this.data[k]}, key(i){return Object.keys(this.data)[i] || null}, get length(){return Object.keys(this.data).length}};
function isSectionCollapsed(sectionId){ return localStorage.getItem(SECTION_LS_PREFIX + sectionId) === '1'; }
function applySectionCollapse(){}
function hiveConfirm(){ return Promise.resolve(true); }
function showToast(){ return {appendChild(){}}; }
function esc(s){ return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
const root = new El('hive-dashboard-root');
const sectionMap = {};
DASHBOARD_LAYOUT_TEMPLATE.main.forEach(id => { const el = new El(id, 'data-dashboard-section', id); sectionMap[id]=el; root.appendChild(el); });
const navGroup = new El('dashboard-section-nav-group');
const oldGroup = new El('old-group');
const helpGroup = new El('help-group');
const navItems = ['faq-section','governor','overview-section','repos-section'].map((id, i) => { const el = new El('nav-'+id, 'data-section', id); (i < 2 ? navGroup : oldGroup).appendChild(el); return el; });
const help = new El('help-link'); help.attrs['class']='oc-nav-item'; helpGroup.appendChild(help);
const groups = [navGroup, oldGroup, helpGroup];
const layoutStatus = new El('oc-gh-menu-layout-status');
const layoutWrap = new El('oc-gh-avatar-wrap');
const layoutList = new El('oc-gh-menu-layout-list');
const document = {
  getElementById(id){ return id === 'hive-dashboard-root' ? root : (id === 'dashboard-section-nav-group' ? navGroup : (id === 'oc-gh-menu-layout-status' ? layoutStatus : (id === 'oc-gh-avatar-wrap' ? layoutWrap : (id === 'oc-gh-menu-layout-list' ? layoutList : null)))); },
  querySelector(sel){ const m = sel.match(/^\[data-dashboard-section="(.+)"\]$/); return m ? sectionMap[m[1]] : null; },
  querySelectorAll(sel){ if (sel === '.oc-nav-item[data-section]') return navItems; if (sel === '.oc-nav-group') return groups; return []; }
};
`)
	for _, name := range funcs {
		b.WriteString(jsFunc(t, html, name))
		b.WriteByte('\n')
	}
	b.WriteString(`
(async function(){
dashboardApplyLayout(dashboardLayoutNormalize({v:2, main:['faq-section','governor']}));
let first = root.querySelectorAll(':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR).slice(0,4).map(e => e.getAttribute('data-dashboard-section')).join(',');
if (first !== 'faq-section,overview-section,governor,advisory-section') throw new Error('page order '+first);
let nav = navGroup.children.map(e => e.getAttribute('data-section')).join(',');
if (nav !== 'faq-section,overview-section,governor,repos-section') throw new Error('sidebar order '+nav);
if (oldGroup.hidden !== true) throw new Error('empty old nav group not hidden');
if (helpGroup.hidden === true) throw new Error('non-section help group hidden');
dashboardLayoutWrite();
if (JSON.parse(localStorage.data[DASHBOARD_LAYOUT_KEY]).main[0] !== 'faq-section') throw new Error('layout not written');
if (layoutStatus.textContent !== 'Layout: custom (unsaved)') throw new Error('layout status '+layoutStatus.textContent);
await resetDashboardLayout();
first = root.querySelectorAll(':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR).slice(0,3).map(e => e.getAttribute('data-dashboard-section')).join(',');
if (first !== 'overview-section,governor,advisory-section') throw new Error('reset order '+first);
if (localStorage.data[DASHBOARD_LAYOUT_KEY] !== undefined) throw new Error('reset did not remove storage');
if (layoutStatus.textContent !== 'Layout: default') throw new Error('reset status '+layoutStatus.textContent);
})();
`)
	cmd := exec.Command(node, "-e", b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dashboard apply/sidebar failed: %v\n%s\nscript funcs=%s", err, out, regexp.QuoteMeta(strings.Join(funcs, ",")))
	}
}

// #9426: a layout saved before newer sections existed must not push those
// sections below the trailing FAQ — they slot in next to their template
// neighbours and FAQ stays last unless the operator moved it.
func TestDashboardLayoutNormalizeKeepsFAQLastForStaleSavedLayout(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — dashboard layout normalization was NOT executed by this run")
	}
	html := indexHTML(t)
	script := dashboardLayoutPreamble9062() + jsFunc(t, html, "dashboardLayoutAllIds") + "\n" + jsFunc(t, html, "dashboardLayoutNormalize") + `
const stale = {v:1, main:['overview-section','governor','advisory-section','token-panel','cost-panel','repos-section','beads-section','acmm-eval-section','approvals-section','platform-section','audit-section','nous-section','inception-section','knowledge-section','contributors-section','debug-section','faq-section']};
const got = dashboardLayoutNormalize(stale).main;
if (got[got.length-1] !== 'faq-section') throw new Error('FAQ not last: '+JSON.stringify(got));
const tail = got.slice(-4).join(',');
if (tail !== 'debug-section,logs-section,agents-section,faq-section') throw new Error('new sections not slotted before FAQ: '+tail);
if (new Set(got).size !== got.length) throw new Error('duplicate ids: '+JSON.stringify(got));
const moved = dashboardLayoutNormalize({v:1, main:['faq-section','cost-panel','overview-section']}).main;
if (moved[0] !== 'faq-section') throw new Error('operator-moved FAQ not respected: '+JSON.stringify(moved));
if (moved.length !== 19) throw new Error('missing sections not restored: '+moved.length);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dashboard normalize failed: %v\n%s", err, out)
	}
}
