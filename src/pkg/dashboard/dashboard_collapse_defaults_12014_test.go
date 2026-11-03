package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Cards a brand-new browser must see expanded: they answer "is the hive alive
// and merging?" (#12014). Everything else in the layout template is detail.
var dashboardCriticalSections12014 = []string{
	"overview-section", "governor", "repos-section", "activity-monitor-section", "review-queue-section", "approvals-section", "agents-section", "agent-activity-section",
}

// jsConstBlock returns the `const NAME = ...;` statement for an Object.freeze([...]) list.
func jsConstBlock(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "const "+name+" = ")
	if start < 0 {
		t.Fatalf("index.html does not define const %s", name)
	}
	end := strings.Index(html[start:], ";\n")
	if end < 0 {
		t.Fatalf("const %s has no terminating semicolon", name)
	}
	return html[start : start+end+1]
}

func dashboardCollapseDefaultsPreamble12014(t *testing.T, html string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(dashboardLayoutPreamble9062())
	for _, name := range []string{
		"DASHBOARD_DEFAULT_COLLAPSED_SECTIONS", "DASHBOARD_COLLAPSE_DEFAULTS_KEY", "DASHBOARD_COLLAPSE_DEFAULTS_VERSION",
		"DASHBOARD_COLLAPSE_DEFAULTS_LEGACY", "DASHBOARD_PRIOR_STATE_KEY_PREFIX", "DASHBOARD_PRIOR_STATE_IGNORED_KEYS",
	} {
		b.WriteString(jsConstBlock(t, html, name))
		b.WriteByte('\n')
	}
	b.WriteString(`
const localStorage = {data:{}, setItem(k,v){this.data[k]=String(v)}, getItem(k){return Object.prototype.hasOwnProperty.call(this.data,k)?this.data[k]:null}, removeItem(k){delete this.data[k]}, key(i){return Object.keys(this.data)[i] || null}, get length(){return Object.keys(this.data).length}, clear(){this.data={}}};
function dashboardUpdateLayoutMenuState(){}
`)
	for _, name := range []string{
		"dashboardHasPriorState", "dashboardSeedCollapseDefaults", "dashboardCollapseDefaultsActive", "dashboardDefaultCollapsed",
		"isSectionCollapsed", "writeSectionCollapsedKey", "setSectionCollapsed", "dashboardLayoutAllIds", "dashboardLayoutDefaultCollapsed",
	} {
		b.WriteString(jsFunc(t, html, name))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestDashboardCollapseDefaultsClassification(t *testing.T) {
	html := indexHTML(t)
	block := jsConstBlock(t, html, "DASHBOARD_DEFAULT_COLLAPSED_SECTIONS")
	for _, id := range dashboardCriticalSections12014 {
		if strings.Contains(block, "'"+id+"'") {
			t.Errorf("critical section %q must not be collapsed by default", id)
		}
	}
	for _, id := range dashboardLayoutIDs9062 {
		critical := false
		for _, c := range dashboardCriticalSections12014 {
			if c == id {
				critical = true
			}
		}
		if !critical && !strings.Contains(block, "'"+id+"'") {
			t.Errorf("non-critical layout section %q is missing from DASHBOARD_DEFAULT_COLLAPSED_SECTIONS", id)
		}
	}
	// The marker must be seeded before the review-section migration writes its own key,
	// otherwise every browser would look like a prior visitor.
	seed := strings.Index(html, "    dashboardSeedCollapseDefaults();\n")
	migrate := strings.Index(html, "    dashboardMigrateReviewSectionStorage();\n")
	if seed < 0 || migrate < 0 || seed > migrate {
		t.Fatalf("dashboardSeedCollapseDefaults() must run before dashboardMigrateReviewSectionStorage() (seed=%d migrate=%d)", seed, migrate)
	}
	for _, want := range []string{
		`id="oc-gh-menu-layout-expand-all" data-action="expandAllDashboardSections"`,
		`id="oc-gh-menu-layout-collapse-all" data-action="collapseAllDashboardSections"`,
		`function expandAllDashboardSections()`,
		`function collapseAllDashboardSections()`,
		`function setAllDashboardSectionsCollapsed(collapsed)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing expand/collapse-all wiring %q", want)
		}
	}
}

func TestDashboardCollapseDefaultsNewBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — collapse default resolution was NOT executed by this run")
	}
	html := indexHTML(t)
	script := dashboardCollapseDefaultsPreamble12014(t, html) + `
dashboardSeedCollapseDefaults();
if (localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY) !== DASHBOARD_COLLAPSE_DEFAULTS_VERSION) throw new Error('fresh browser marker: '+localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY));
for (const id of ['pr-throughput-section','token-panel','cost-panel','audit-section','faq-section']) if (!isSectionCollapsed(id)) throw new Error('new browser should collapse '+id);
for (const id of ['overview-section','governor','repos-section','review-queue-section','approvals-section','agents-section','agent-activity-section']) if (isSectionCollapsed(id)) throw new Error('new browser must keep '+id+' expanded');
// Explicit expand of a default-collapsed card persists as '0' and wins over the default.
setSectionCollapsed('cost-panel', false);
if (localStorage.getItem(SECTION_LS_PREFIX+'cost-panel') !== '0') throw new Error('explicit expand not persisted');
if (isSectionCollapsed('cost-panel')) throw new Error('explicit expand lost to default');
// Explicit collapse of a default-expanded card persists as '1'.
setSectionCollapsed('agents-section', true);
if (localStorage.getItem(SECTION_LS_PREFIX+'agents-section') !== '1' || !isSectionCollapsed('agents-section')) throw new Error('explicit collapse not persisted');
// Expanding a default-expanded card leaves no key behind (unchanged behaviour).
setSectionCollapsed('agents-section', false);
if (localStorage.getItem(SECTION_LS_PREFIX+'agents-section') !== null) throw new Error('default-expanded card should not keep a key');
// Reset (removing the per-section keys) returns to the defaults, marker survives.
Object.keys(localStorage.data).filter(k => k.indexOf(SECTION_LS_PREFIX) === 0).forEach(k => localStorage.removeItem(k));
if (!isSectionCollapsed('cost-panel')) throw new Error('reset should restore collapsed default');
if (!dashboardCollapseDefaultsActive()) throw new Error('marker lost on reset');
// Seeding again is a no-op.
dashboardSeedCollapseDefaults();
if (localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY) !== DASHBOARD_COLLAPSE_DEFAULTS_VERSION) throw new Error('marker rewritten');
// The default layout snapshot lists exactly the default-collapsed template sections.
const def = Object.keys(dashboardLayoutDefaultCollapsed()).sort().join(',');
const want = DASHBOARD_LAYOUT_TEMPLATE.main.filter(id => DASHBOARD_DEFAULT_COLLAPSED_SECTIONS.includes(id)).sort().join(',');
if (def !== want) throw new Error('default snapshot collapsed set '+def+' != '+want);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node collapse defaults (new browser) failed: %v\n%s", err, out)
	}
}

func TestDashboardCollapseDefaultsLegacyBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — collapse default resolution was NOT executed by this run")
	}
	html := indexHTML(t)
	script := dashboardCollapseDefaultsPreamble12014(t, html) + `
// A pasted auth token alone does not make a browser "legacy".
localStorage.setItem('hive-token', 'abc');
if (dashboardHasPriorState()) throw new Error('hive-token alone must not count as prior state');
localStorage.clear();
// Any other persisted dashboard key does.
localStorage.setItem('hive.dashboard.reviewSectionMigrated', '1');
dashboardSeedCollapseDefaults();
if (localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY) !== DASHBOARD_COLLAPSE_DEFAULTS_LEGACY) throw new Error('existing browser marker: '+localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY));
for (const id of DASHBOARD_LAYOUT_TEMPLATE.main) if (isSectionCollapsed(id)) throw new Error('legacy browser must keep '+id+' expanded');
if (Object.keys(dashboardLayoutDefaultCollapsed()).length !== 0) throw new Error('legacy default snapshot must be all-expanded');
// Legacy browsers keep the historical persistence shape: expanded cards have no key.
setSectionCollapsed('cost-panel', true);
setSectionCollapsed('cost-panel', false);
if (localStorage.getItem(SECTION_LS_PREFIX+'cost-panel') !== null) throw new Error('legacy expand should remove the key');
// A '0' written by a newer browser is still honoured.
localStorage.setItem(SECTION_LS_PREFIX+'agents-section', '0');
if (isSectionCollapsed('agents-section')) throw new Error("'0' must read as expanded");
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node collapse defaults (legacy browser) failed: %v\n%s", err, out)
	}
}
