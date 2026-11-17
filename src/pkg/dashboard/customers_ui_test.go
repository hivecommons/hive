package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestCustomersSectionWiring pins the Customers section
// (hivecommons/hive#18742): the sidebar entry in the Collaborate group after
// Meetings, hidden until the API says the feature is on; the section's card
// config and help; the API calls; and the controls a profile offers.
func TestCustomersSectionWiring(t *testing.T) {
	html := indexHTML(t)
	meetNav := strings.Index(html, `data-section="meetings-section" data-action="ocNavigate"`)
	custNav := strings.Index(html, `data-section="customers-section" data-action="ocNavigate" data-arg0="customers-section" hidden>`)
	sessNav := strings.Index(html, `data-section="sessions-section" data-action="ocNavigate"`)
	if meetNav < 0 || custNav < meetNav || sessNav < custNav {
		t.Fatalf("Customers must sit after Meetings in the Collaborate group, hidden by default: meetings=%d customers=%d sessions=%d", meetNav, custNav, sessNav)
	}
	group := strings.Index(html, `id="collaborate-nav-group"`)
	if group < 0 || group > custNav || strings.Contains(html[group:custNav], `</nav>`) {
		t.Fatal("the Customers link is not inside the Collaborate group")
	}
	for _, want := range []string{
		`id="customers-section" data-dashboard-section="customers-section" hidden>`,
		`id="customers-panel"`,
		`🪪 Customers</h2>`,
		`'customers-section': { title: '🪪 Customers', summary: 'your profile' }`,
		`'customers-section': { heading: 'Customers', help:`,
		`if(id==='customers-section')return !customersFeatureEnabled();`,
		`window._customersEnabled = data.enabled === true;`,
		`fetch('/api/customers')`,
		`dashboardSetInterval('customers', fetchCustomers, CUSTOMERS_POLL_MS)`,
		`setSectionSummary('customers-section'`,
		`refreshSectionCardShell('customers-section')`,
		// A viewer's own profile is always written as "me": the one path
		// a read-only user may post to.
		`const id = _customersOpen.me ? 'me' : _customersOpen.id;`,
		`'/api/customers/' + encodeURIComponent(id) + '/edit'`,
		`customersRequest('DELETE', '/api/customers/' + encodeURIComponent(p.id))`,
		`'/api/customers/suggestions/' + encodeURIComponent(sid) + '/accept'`,
		`'/api/customers/suggestions/' + encodeURIComponent(sid) + '/dismiss'`,
		`data-action="customersGoalConfirm"`,
		`data-action="customersGoalReject"`,
		`data-action="customersSatisfactionConfirm"`,
		`data-action="customersSatisfactionReject"`,
		`data-action="customersIdentityConfirm"`,
		`data-action="customersForget"`,
		`customersRequest('POST', '/api/graph', { query: CUSTOMERS_GRAPH_QUERY, operationName: 'Graph'`,
		`'/api/customers/' + encodeURIComponent(id) + '/' + kind`,
		`'/api/customers/' + encodeURIComponent(id) + '/split'`,
		`'/export'`,
		`person { id name me } } } } } }'`,
		`'/api/customers/hints/' + encodeURIComponent(hid) + '/answer'`,
		`customersRequest('POST', '/api/graph', { query: CUSTOMERS_TIMELINE_QUERY, operationName: 'Timeline', variables: variables })`,
		`person(id: $id) { edges(first: $first, after: $after) { nodes { predicate at source artefact { ref kind } } next } }`,
		`Reset to automatic`,
		`There is no score: only a level, and what led to it.`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Customers wiring missing %q", want)
		}
	}
	// hive_user is what "me" resolves on; the page must not offer it.
	kinds := jsConstBlock(t, html, "CUSTOMERS_ADDABLE_KINDS")
	if strings.Contains(kinds, "hive_user") {
		t.Error("the page offers hive_user as an identity to add")
	}
}

// TestCustomersViewGates runs the page's own render helpers: what the
// owner sees that a read-write user does not, and what a read-only user
// never sees.
func TestCustomersViewGates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the Customers render helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	var fns []string
	for _, name := range []string{
		"customersIsFull", "customersIsOwner", "customersActiveTags", "customersGoalsSplit", "customersSummaryText",
		"customersLevelPill", "customersEvidenceHtml", "customersKindOptions", "customersRowHtml", "customersSuggestionRowHtml",
		"customersAddFormHtml", "customersListHtml", "customersIdentityRowHtml", "customersIdentitiesHtml", "customersTagChipHtml",
		"customersTagsHtml", "customersGoalRowHtml", "customersGoalsHtml", "customersSatisfactionHtml", "customersNotesHtml",
		"customersActivityHtml", "customersForgetHtml", "customersProfileHtml", "customersFormatTime",
		"customersTimelineGroups", "customersTimelineRowHtml", "customersTimelineHtml", "customersHintRowHtml",
		"customersMergeHtml", "customersGraphBodyHtml", "customersGraphHtml",
		"graphcanvasRound", "graphcanvasLabel", "graphcanvasLayout", "graphcanvasSvg",
	} {
		fns = append(fns, jsFunc(t, html, name))
	}
	consts := ""
	for _, name := range []string{"CUSTOMERS_KIND_LABELS", "CUSTOMERS_ADDABLE_KINDS", "CUSTOMERS_LEVEL_LABELS", "CUSTOMERS_LEVEL_PILL", "CUSTOMERS_ORIGIN_LABELS", "CUSTOMERS_REF_KIND_LABELS",
		"GRAPHCANVAS_W", "GRAPHCANVAS_H", "GRAPHCANVAS_RING_ARTEFACT", "GRAPHCANVAS_RING_PEOPLE", "GRAPHCANVAS_MAX_ARTEFACTS", "GRAPHCANVAS_LABEL_MAX"} {
		consts += jsConstBlock(t, html, name) + "\n"
	}
	script := consts + `
const CUSTOMERS_EVIDENCE_SHOWN = 5;
let _customersData = null;
let _customersTimeline = { id: '', nodes: [], next: '', error: '', loading: false };
let _customersGraph = { id: '', on: false, nodes: null, error: '', loading: false };
function escapeHtml(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
` + strings.Join(fns, "\n") + `
function must(cond, msg) { if (!cond) throw new Error(msg); }
const person = {
  id: 'p_abc', name: '<b>Ada</b>', origin: 'manual', me: false, notes: 'private note',
  identities: [{ kind: 'hive_user', value: 'ada' }, { kind: 'email', value: 'ada@example.com', claimed: true }, { kind: 'github', value: 'ada', manual: true, by: 'olivia' }],
  tags: [{ name: 'maintainer', state: 'on', source: 'auto', rule: 'role:owner' }, { name: 'vip', state: 'on', source: 'manual', by: 'olivia' }, { name: 'end-user', state: 'off', source: 'auto' }],
  goals: [{ id: 'g_1', text: 'Ship exports', state: 'confirmed', source: 'manual', by: 'ada' }, { id: 'g_2', text: 'Wants SSO', state: 'suggested', source: 'auto', evidence: ['gh:issue:o/r#1'] }],
  satisfaction: { level: 'at_risk', source: 'manual', by: 'olivia', suggestion: { level: 'neutral', evidence: ['gh:pr:o/r#2'] } },
  merged_from: [{ person: { id: 'p_old', name: 'Old <Ada>' }, by: 'olivia', at: '2026-10-01T00:00:00Z' }],
  counts: { authored: 3 }, hints: [{ id: 'h_1', other_kind: 'email', other_value: 'ada@<x>.org', reason: 'the address and the login start the same' }, { id: 'h_2', other_kind: 'customer', other_value: 'Ada B' }]
};
const base = { enabled: true, me: Object.assign({}, person, { id: 'p_me', me: true, name: 'Me' }), people: [person, { id: 'p_two', name: 'Two' }], suggestions: [{ id: 's_1', kind: 'github', value: 'stranger', counts: { authored: 2 }, evidence: ['gh:issue:o/r#9'] }], default_tags: ['maintainer', 'contributor', 'end-user', 'stakeholder'], levels: ['unknown', 'at_risk', 'neutral', 'satisfied'] };

// Owner: everything.
_customersData = Object.assign({}, base, { full: true, owner: true });
let list = customersListHtml(_customersData);
let prof = customersProfileHtml(person);
must(list.includes('data-action="customersAccept"') && list.includes('data-action="customersDismiss"') && list.includes('Add customer'), 'owner list lacks add, accept or dismiss');
must(list.includes('data-arg0="me"') && list.includes('data-arg0="p_abc"'), 'list rows do not open me and the customer');
must(!list.includes('<b>Ada</b>') && list.includes('&lt;b&gt;Ada&lt;/b&gt;'), 'a name reached the page unescaped');
must(prof.includes('data-action="customersForget"'), 'owner profile lacks Forget');
must(prof.includes('data-action="customersSatisfactionSet"') && prof.includes('data-action="customersSatisfactionConfirm"') && prof.includes('private note'), 'owner profile lacks satisfaction or notes');
must(prof.includes('data-action="customersIdentityConfirm"'), 'owner cannot confirm a claimed identity');
must(prof.includes('data-action="customersHintAnswer" data-arg0="h_1" data-arg1="yes"') && prof.includes('data-arg0="h_1" data-arg1="no"') && prof.includes('ada@&lt;x&gt;.org'), 'a suggested-identity hint lacks Yes, No or an escaped identity');
must(!prof.includes('data-arg0="h_2" data-arg1="yes"') && prof.includes('data-arg0="h_2" data-arg1="no"'), 'a customer-to-customer hint offers Yes');
must(prof.includes('data-action="customersGoalConfirm" data-arg0="g_2"') && prof.includes('gh:issue:o/r#1'), 'a suggested goal lacks Confirm or its evidence');
must(!prof.includes('data-action="customersGoalConfirm" data-arg0="g_1"'), 'a confirmed goal offers Confirm');
must(prof.includes('data-arg0="vip" data-arg1="auto"') && !prof.includes('data-arg0="maintainer" data-arg1="auto"'), 'Reset to automatic must show on manual tags only');
must(!prof.includes('data-arg0="hive_user:ada"'), 'the hive user identity offers Remove');
must(customersSummaryText(_customersData) === '3 customers · 1 suggested', 'summary: ' + customersSummaryText(_customersData));
must(prof.includes('data-action="customersMerge"') && prof.includes('<option value="p_two">Two</option>') && !prof.includes('<option value="p_abc">'), 'the merge picker lists other customers only');
must(prof.includes('data-action="customersUnmerge" data-arg0="p_old"') && prof.includes('Old &lt;Ada&gt;') && !prof.includes('<Ada>'), 'a merged profile lacks Undo or is not escaped');
must(prof.includes('data-action="customersExport"') && prof.includes('data-action="customersGraphToggle"') && prof.includes('Show graph'), 'owner profile lacks Export or the graph toggle');
must(prof.includes('data-action="customersSplit" data-arg0="github:ada"') && !prof.includes('data-arg0="hive_user:ada" data-action="customersSplit"') && !prof.includes('customersSplit" data-arg0="hive_user:ada"') && !prof.includes('customersSplit" data-arg0="email:ada@example.com"'), 'Split off must be offered for a confirmed identity only, never the hive user or a claim');

// The timeline is read from the graph and grouped by what it points at. It
// shows references, escaped, and offers the next page only when there is one.
must(prof.includes('id="customers-timeline"') && prof.includes('No activity recorded yet.'), 'an empty timeline is not rendered');
_customersTimeline = { id: 'p_abc', next: 'cursor', error: '', loading: false, nodes: [
  { predicate: 'reported', at: '2026-11-02T10:00:00Z', artefact: { ref: 'gh:issue:o/r#1', kind: 'gh' } },
  { predicate: 'spoke_in', at: '2026-11-01T10:00:00Z', artefact: { ref: 'meeting:<m1>', kind: 'meeting' } },
  { predicate: 'authored', at: '2026-10-30T10:00:00Z', artefact: { ref: 'gh:pr:o/r#2', kind: 'gh' } } ] };
const groups = customersTimelineGroups(_customersTimeline.nodes);
must(groups.length === 2 && groups[0].label === 'GitHub' && groups[0].nodes.length === 2 && groups[1].label === 'Meetings', 'timeline groups: ' + JSON.stringify(groups));
let timeline = customersTimelineHtml();
must(timeline.includes('GitHub (2)') && timeline.includes('gh:pr:o/r#2') && timeline.includes('spoke in') && timeline.includes('meeting:&lt;m1&gt;') && !timeline.includes('<m1>'), 'timeline: ' + timeline);
must(timeline.includes('data-action="customersTimelineMore"'), 'a timeline with a next page lacks Show more');
_customersTimeline.next = '';
must(!customersTimelineHtml().includes('customersTimelineMore'), 'the last page still offers Show more');
_customersTimeline = { id: 'p_abc', nodes: [], next: '', error: 'Could not load activity: boom', loading: false };
must(customersTimelineHtml().includes('Could not load activity: boom'), 'a timeline error is not shown');
_customersTimeline = { id: '', nodes: [], next: '', error: '', loading: false };

// Read-write: no Forget and no Export, but merge and split.
_customersData = Object.assign({}, base, { full: true, owner: false });
prof = customersProfileHtml(person);
must(!prof.includes('customersForget') && !prof.includes('customersExport'), 'read-write profile offers Forget or Export');
must(prof.includes('data-action="customersMerge"') && prof.includes('data-action="customersSplit"'), 'read-write profile lacks merge or split');
must(prof.includes('data-action="customersSatisfactionSet"'), 'read-write profile lacks satisfaction');

// Read-only: own profile only; no suggestions, add, satisfaction, notes, activity, forget or confirm.
const mine = { id: 'p_me', name: 'Reed', me: true, origin: 'hive_user', identities: [{ kind: 'hive_user', value: 'reed' }, { kind: 'email', value: 'r@example.com', claimed: true }], tags: [], goals: [{ id: 'g_9', text: 'Wants an API', state: 'suggested', source: 'auto' }], satisfaction: {} };
_customersData = { enabled: true, full: false, owner: false, me: mine, people: [], suggestions: [], default_tags: base.default_tags, levels: base.levels };
list = customersListHtml(_customersData);
prof = customersProfileHtml(mine);
for (const banned of ['customersAccept', 'customersDismiss', 'customersAdd', 'Suggested (']) must(!list.includes(banned), 'read-only list shows ' + banned);
for (const banned of ['customersForget', 'customersSatisfaction', 'customersNotesSave', 'customersIdentityConfirm', 'Activity', 'customersMerge', 'customersUnmerge', 'customersSplit', 'customersExport']) must(!prof.includes(banned), 'read-only profile shows ' + banned);
must(prof.includes('data-action="customersGraphToggle"'), 'a read-only user may open the graph of their own profile');
must(prof.includes('data-action="customersGoalConfirm" data-arg0="g_9"') && prof.includes('data-action="customersGoalAdd"') && prof.includes('data-action="customersTagSet"') && prof.includes('data-action="customersIdentityAdd"'), 'read-only profile lacks its own goal, tag or identity controls');
must(prof.includes('unconfirmed'), 'a claimed identity is not marked unconfirmed');
must(customersSummaryText(_customersData) === 'your profile', 'read-only summary: ' + customersSummaryText(_customersData));
must(customersSummaryText({ enabled: false }) === 'off', 'off summary');

// Graph view: layout and drawing.
const gEdges = [
  { predicate: 'reported', artefact: { ref: 'gh:issue:o/r#1', kind: 'gh', people: [{ predicate: 'reported', person: { id: 'p_me', name: 'Me', me: true } }, { predicate: 'assigned', person: { id: 'p_x', name: 'Xavier <b>' } }] } },
  { predicate: 'commented_on', artefact: { ref: 'gh:issue:o/r#1', kind: 'gh', people: [] } },
  { predicate: 'attended', artefact: { ref: 'meeting:m1', kind: 'meeting', people: [{ predicate: 'attended', person: { id: 'p_x', name: 'Xavier <b>' } }, { predicate: 'spoke_in', person: { id: 'p_y', name: 'Yuri' } }] } },
  { predicate: 'sent', artefact: null }, null, {}
];
const lay = graphcanvasLayout({ id: 'p_me', name: 'Me' }, gEdges);
must(lay.nodes.filter(n => n.kind === 'person').length === 1 && lay.nodes.filter(n => n.kind === 'artefact').length === 2, 'artefacts must be unique and the person drawn once: ' + JSON.stringify(lay.nodes.map(n => n.id)));
must(lay.nodes.filter(n => n.kind === 'other').map(n => n.id).sort().join() === 'p_x,p_y', 'the person themselves must not appear as another person, and others are unique');
must(lay.nodes.every(n => Number.isFinite(n.x) && Number.isFinite(n.y) && n.x >= 0 && n.x <= lay.width && n.y >= 0 && n.y <= lay.height), 'a node is off the canvas or NaN');
must(lay.links.some(l => l.from === 'p_me' && l.to === 'gh:issue:o/r#1' && l.label === 'reported, commented_on'), 'predicates on one artefact are joined');
let svg = graphcanvasSvg(lay);
must(svg.startsWith('<svg class="gv"') && svg.includes('data-action="customersOpen" data-arg0="p_x"') && !svg.includes('data-arg0="p_me"') && !svg.includes('<b>') && svg.includes('Xavier &lt;b&gt;'), 'svg: ' + svg.slice(0, 300));
must(!svg.includes('style=') && !svg.includes('NaN') && (svg.match(/<circle/g) || []).length === lay.nodes.length, 'svg has inline styles, NaN, or a missing node');
let many = [];
for (let i = 0; i < 60; i++) many.push({ predicate: 'authored', artefact: { ref: 'gh:pr:o/r#' + i, kind: 'gh', people: [] } });
must(graphcanvasLayout({ id: 'p', name: 'P' }, many).nodes.length === GRAPHCANVAS_MAX_ARTEFACTS + 1, 'artefacts are not bounded');
must(graphcanvasLayout({ id: 'p', name: 'P' }, []).nodes.length === 1 && graphcanvasLayout({ id: 'p', name: 'P' }, null).links.length === 0, 'an empty graph draws only the person');
must(graphcanvasLabel('x'.repeat(100)).length === GRAPHCANVAS_LABEL_MAX, 'labels are not bounded');
_customersGraph = { id: 'p_abc', on: true, nodes: gEdges, error: '', loading: false };
must(customersGraphBodyHtml(person).includes('<svg class="gv"'), 'the graph body does not draw');
_customersGraph = { id: 'p_abc', on: true, nodes: [], error: '', loading: false };
must(customersGraphBodyHtml(person).includes('Nothing to draw yet.'), 'an empty graph says so');
_customersGraph = { id: 'p_abc', on: true, nodes: null, error: 'Could not load the graph: boom', loading: false };
must(customersGraphBodyHtml(person).includes('Could not load the graph: boom'), 'a graph error is not shown');
_customersGraph = { id: '', on: false, nodes: null, error: '', loading: false };
must(customersLevelPill('unknown') === '' && customersLevelPill('satisfied').includes('data-status="ok"'), 'level pill');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node customers render helpers failed: %v\n%s", err, out)
	}
}

type customersFeatures struct {
	Features struct {
		CustomersEnabled   bool `json:"customersEnabled"`
		CustomersEffective bool `json:"customersEffective"`
	} `json:"features"`
}

func getCustomersFeatures(t *testing.T, s *Server) customersFeatures {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d - %s", rec.Code, rec.Body.String())
	}
	var payload customersFeatures
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	return payload
}

// TestGovernorFeatures_CustomersToggleRoundTrip covers the one switch,
// customers.enabled: default on, owner-only, persisted, reported back, left
// alone by a PUT that does not mention it, and overridden by the env var.
func TestGovernorFeatures_CustomersToggleRoundTrip(t *testing.T) {
	t.Setenv(config.EnvCustomersEnabled, "")
	s := covApiServer(t)
	if got := getCustomersFeatures(t, s); !got.Features.CustomersEnabled || !got.Features.CustomersEffective {
		t.Fatalf("default payload = %+v, want on", got.Features)
	}
	if rec := putFeatures(s, map[string]any{"customersEnabled": false}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT customersEnabled = %d, want 403", rec.Code)
	}
	if !s.deps.Config.CustomersEnabled() {
		t.Fatal("a refused PUT changed the config")
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT customersEnabled: %d - %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Customers.IsEnabled() || s.deps.Config.CustomersEnabled() {
		t.Fatal("customers.enabled false was not persisted into config")
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d", rec.Code)
	}
	if s.deps.Config.Customers.IsEnabled() {
		t.Fatal("a PUT that did not mention customersEnabled turned it back on")
	}
	t.Setenv(config.EnvCustomersEnabled, "true")
	if got := getCustomersFeatures(t, s); got.Features.CustomersEnabled || !got.Features.CustomersEffective {
		t.Fatalf("payload under env override = %+v, want saved off, effective on", got.Features)
	}
	t.Setenv(config.EnvCustomersEnabled, "")
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT on: %d", rec.Code)
	}
	if !s.deps.Config.Customers.IsEnabled() {
		t.Fatal("explicit true did not turn customers back on")
	}
}

// The Features tab renders the switch wired to the key the API reads.
func TestFeaturesTab_CustomersToggleMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`data-section="features" data-key="customersEnabled"`,
		`yamlHint('customers.enabled')`,
		`f.customersEffective !== f.customersEnabled`,
		`HIVE_CUSTOMERS_ENABLED overrides this setting`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
