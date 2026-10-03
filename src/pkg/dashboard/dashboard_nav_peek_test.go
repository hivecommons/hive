package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboardNavPeekStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function navPeekExpand(sectionId)",
		"function navPeekRestore(nextSectionId, opts)",
		"function ocAfterNavLayoutSettles(sectionIds, callback)",
		"data-nav-peek",
		"navPeekRestore(section, { instant: true });",
		"navPeekExpand(section);",
		"persist: false",
		"scrollIntoView({ block: 'start'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard nav peek wiring missing %q", want)
		}
	}
}

func TestDashboardNavPeekHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — dashboard nav peek helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
const SECTION_LS_PREFIX = 'hive-section-collapsed-';
let layoutUpdates = 0;
function dashboardUpdateLayoutMenuState(){ layoutUpdates++; }
function normalizeSectionCardChrome(){}
function applySectionBodyCollapse(body, collapsed){ if (body) { body.collapsed = collapsed; body.classList.toggle('collapsed', collapsed); } }
function replaySparklinesIn(){}
const localStorage = { data:{}, setItem(k,v){this.data[k]=String(v)}, getItem(k){return Object.prototype.hasOwnProperty.call(this.data,k)?this.data[k]:null}, removeItem(k){delete this.data[k]} };
class ClassList {
  constructor(){ this.values = new Set(); }
  add(v){ this.values.add(v); }
  remove(v){ this.values.delete(v); }
  contains(v){ return this.values.has(v); }
  toggle(v,on){ if (on === undefined) on = !this.values.has(v); on ? this.values.add(v) : this.values.delete(v); return on; }
}
class El {
  constructor(id){ this.id=id; this.attrs={}; this.classList=new ClassList(); }
  getAttribute(k){ return Object.prototype.hasOwnProperty.call(this.attrs,k) ? this.attrs[k] : null; }
  setAttribute(k,v){ this.attrs[k]=String(v); }
  removeAttribute(k){ delete this.attrs[k]; }
}
const sections = {};
function makeSection(id){ sections[id]={ section:new El(id), body:new El(id+'-body'), chevron:new El(id+'-chevron'), card:new El(id+'-card'), header:new El(id+'-header') }; return sections[id]; }
['repos-section','contributors-section','overview-section'].forEach(makeSection);
const document = {
  getElementById(id){ return sections[id] && sections[id].section || null; },
  querySelector(sel){
    if (sel === '[data-nav-peek="1"]') {
      const found = Object.values(sections).find(s => s.section.getAttribute('data-nav-peek') === '1');
      return found ? found.section : null;
    }
    const m = sel.match(/^#([^ ]+) /);
    if (!m) return null;
    const s = sections[m[1]];
    if (!s) return null;
    if (sel.includes('.dash-card-body') || sel.includes('.section-body')) return s.body;
    if (sel.includes('.section-chevron')) return s.chevron;
    if (sel.includes('.dash-card-header') || sel.includes('.section-header-toggle')) return s.header;
    if (sel.includes('.dash-card')) return s.card;
    return null;
  }
};
function assert(cond, msg){ if (!cond) throw new Error(msg); }
` + jsFunc(t, html, "isSectionCollapsed") + "\n" + jsFunc(t, html, "setSectionCollapsed") + "\n" + jsFunc(t, html, "toggleSection") + "\nvar _dashboardNavPeekSectionId = null;\n" + jsFunc(t, html, "applySectionCollapse") + "\n" + jsFunc(t, html, "navPeekSectionId") + "\n" + jsFunc(t, html, "navPeekExpand") + "\n" + jsFunc(t, html, "navPeekRestore") + `
localStorage.setItem(SECTION_LS_PREFIX + 'repos-section', '1');
assert(navPeekExpand('repos-section') === true, 'collapsed section did not peek-expand');
assert(localStorage.getItem(SECTION_LS_PREFIX + 'repos-section') === '1', 'peek expansion persisted expanded state');
assert(sections['repos-section'].section.getAttribute('data-nav-peek') === '1', 'peek flag missing');
assert(sections['repos-section'].body.collapsed === false, 'peeked body is still collapsed');
assert(sections['repos-section'].header.getAttribute('aria-expanded') === 'true', 'peeked header aria not expanded');
applySectionCollapse('repos-section');
assert(sections['repos-section'].body.collapsed === false, 'refresh collapsed a peek-expanded section');
assert(localStorage.getItem(SECTION_LS_PREFIX + 'repos-section') === '1', 'refresh changed persisted collapse state');
localStorage.setItem(SECTION_LS_PREFIX + 'contributors-section', '1');
assert(navPeekRestore('contributors-section') === true, 'different nav did not restore previous peek');
assert(sections['repos-section'].section.getAttribute('data-nav-peek') === null, 'restore left peek flag');
assert(sections['repos-section'].body.collapsed === true, 'restore did not collapse previous section');
assert(localStorage.getItem(SECTION_LS_PREFIX + 'repos-section') === '1', 'restore changed persisted collapse state');
assert(navPeekExpand('contributors-section') === true, 'new collapsed target did not peek');
assert(sections['contributors-section'].body.collapsed === false, 'new target not expanded');
assert(navPeekRestore('contributors-section') === false, 'same-section restore should be a no-op');
assert(sections['contributors-section'].body.collapsed === false, 'same-section nav collapsed target');
navPeekRestore('repos-section');
assert(sections['contributors-section'].body.collapsed === true, 'setup restore did not collapse contributors');
assert(navPeekExpand('repos-section') === true, 'manual setup peek failed');
toggleSection('repos-section');
assert(sections['repos-section'].section.getAttribute('data-nav-peek') === null, 'manual toggle did not clear peek flag');
assert(localStorage.getItem(SECTION_LS_PREFIX + 'repos-section') === null, 'manual toggle did not persist expanded state');
assert(sections['repos-section'].body.collapsed === false, 'manual toggle collapsed instead of committing peek expansion');
assert(navPeekRestore('contributors-section') === false, 'manual toggle should cancel later restore');
assert(sections['repos-section'].body.collapsed === false, 'cancelled restore still collapsed manual section');
assert(navPeekExpand('overview-section') === false, 'expanded section should not be peeked');
assert(sections['overview-section'].section.getAttribute('data-nav-peek') === null, 'expanded section got peek flag');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dashboard nav peek failed: %v\n%s", err, out)
	}
}

func TestDashboardNavPeekNavigationWaitsForSettledLayout(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — dashboard nav peek navigation helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
const SECTION_LS_PREFIX = 'hive-section-collapsed-';
const SECTION_MAX_HEIGHT_PROPERTY = 'max-height';
const NAV_JUMP_SETTLE_MS = 350;
const NAV_JUMP_RESIZE_QUIET_MS = 60;
const calls = [];
let timers = [];
let rafs = [];
function setTimeout(fn, ms){ timers.push(fn); return fn; }
function clearTimeout(fn){ timers = timers.filter(t => t !== fn); }
function requestAnimationFrame(fn){ rafs.push(fn); }
function runTimers(){ const q = timers.slice(); timers = []; q.forEach(fn => fn()); }
function runRafs(){ const q = rafs.slice(); rafs = []; q.forEach(fn => fn()); }
function dashboardUpdateLayoutMenuState(){}
function normalizeSectionCardChrome(){}
function applySectionBodyCollapse(body, collapsed, animate){ calls.push((collapsed ? 'collapse:' : 'expand:') + body.sectionId + ':' + String(animate)); body.collapsed = collapsed; }
function replaySparklinesIn(){}
function triggerOverviewAnimations(){}
const localStorage = { data:{}, setItem(k,v){this.data[k]=String(v)}, getItem(k){return Object.prototype.hasOwnProperty.call(this.data,k)?this.data[k]:null}, removeItem(k){delete this.data[k]} };
class ClassList {
  constructor(){ this.values = new Set(); }
  add(v){ this.values.add(v); }
  remove(v){ this.values.delete(v); }
  contains(v){ return this.values.has(v); }
  toggle(v,on){ if (on === undefined) on = !this.values.has(v); on ? this.values.add(v) : this.values.delete(v); return on; }
}
class El {
  constructor(id, sectionId){ this.id=id; this.sectionId=sectionId || id; this.attrs={}; this.classList=new ClassList(); this.innerHTML=''; this.listeners={}; }
  getAttribute(k){ return Object.prototype.hasOwnProperty.call(this.attrs,k) ? this.attrs[k] : null; }
  setAttribute(k,v){ this.attrs[k]=String(v); }
  removeAttribute(k){ delete this.attrs[k]; }
  addEventListener(type, fn){ this.listeners[type]=fn; }
  removeEventListener(type, fn){ if (this.listeners[type] === fn) delete this.listeners[type]; }
  scrollIntoView(){ calls.push('scroll:' + this.id); }
}
const sections = {};
function makeSection(id){ sections[id]={ section:new El(id), body:new El(id+'-body', id), chevron:new El(id+'-chevron', id), card:new El(id+'-card', id), header:new El(id+'-header', id) }; return sections[id]; }
['repos-section','contributors-section','overview-section'].forEach(makeSection);
const detail = new El('oc-agent-detail');
const navItems = [];
const document = {
  getElementById(id){ return id === 'oc-agent-detail' ? detail : (sections[id] && sections[id].section || null); },
  querySelector(sel){
    if (sel === '[data-nav-peek="1"]') {
      const found = Object.values(sections).find(s => s.section.getAttribute('data-nav-peek') === '1');
      return found ? found.section : null;
    }
    if (sel.indexOf('.oc-nav-item') === 0) return null;
    const m = sel.match(/^#([^ ]+) /);
    if (!m) return null;
    const s = sections[m[1]];
    if (!s) return null;
    if (sel.includes('.dash-card-body') || sel.includes('.section-body')) return s.body;
    if (sel.includes('.section-chevron')) return s.chevron;
    if (sel.includes('.dash-card-header') || sel.includes('.section-header-toggle')) return s.header;
    if (sel.includes('.dash-card')) return s.card;
    return null;
  },
  querySelectorAll(sel){ return sel === '.oc-nav-item' ? navItems : []; }
};
const history = { replaceState(){} };
const window = { hiveURLWithHash: h => h };
function dashboardFeatureSectionHidden(){ return false; }
let _ocSelectedAgent = 'agent';
function ocStopPanePoll(){}
function ocUpdateFocusedState(){}
function assert(cond, msg){ if (!cond) throw new Error(msg + ' :: ' + JSON.stringify(calls)); }
` + jsFunc(t, html, "isSectionCollapsed") + "\n" + jsFunc(t, html, "setSectionCollapsed") + "\n" + jsFunc(t, html, "toggleSection") + "\nvar _dashboardNavPeekSectionId = null;\n" + jsFunc(t, html, "navPeekSectionId") + "\n" + jsFunc(t, html, "navPeekExpand") + "\n" + jsFunc(t, html, "navPeekRestore") + "\n" + jsFunc(t, html, "ocSectionBody") + "\n" + jsFunc(t, html, "ocAfterNavLayoutSettles") + "\n" + jsFunc(t, html, "ocScrollSectionIntoView") + "\n" + jsFunc(t, html, "ocNavigate") + `
localStorage.setItem(SECTION_LS_PREFIX + 'repos-section', '1');
localStorage.setItem(SECTION_LS_PREFIX + 'contributors-section', '1');
assert(navPeekExpand('repos-section') === true, 'setup peek failed');
calls.length = 0;
ocNavigate('contributors-section');
assert(calls.join('|') === 'collapse:repos-section:false|expand:contributors-section:true', 'restore/expand order wrong or scroll happened early');
runTimers();
assert(calls.indexOf('scroll:contributors-section') < 0, 'scroll ran before settle paint');
runRafs(); runRafs();
assert(calls.join('|') === 'collapse:repos-section:false|expand:contributors-section:true|scroll:contributors-section', 'scroll did not wait for settle');

calls.length = 0; timers = []; rafs = [];
ocNavigate('contributors-section');
runRafs(); runRafs();
assert(calls.join('|') === 'scroll:contributors-section', 'same peek target toggled instead of only scrolling');

calls.length = 0; timers = []; rafs = [];
sections['contributors-section'].section.removeAttribute('data-nav-peek');
_dashboardNavPeekSectionId = null;
localStorage.removeItem(SECTION_LS_PREFIX + 'overview-section');
ocNavigate('overview-section');
runRafs(); runRafs();
assert(calls.join('|') === 'scroll:overview-section', 'expanded section used peek bookkeeping');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dashboard nav peek navigation failed: %v\n%s", err, out)
	}
}
