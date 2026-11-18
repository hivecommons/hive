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
		`hiveExportViewer({ title: 'Customer export: '`,
		`person { id name me } } } } } } }'`,
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
		"customersInitial", "customersEvidenceLabel", "customersWhyHtml", "customersPickerHtml", "customersSuggestionGroups",
		"customersSuggestionGroupHtml", "customersAttachHtml", "customersSuggestionOrder", "customersNeighbour",
		"customersFocusSuggestion", "customersAfterSuggestion", "customersAccept", "customersDismiss", "customersShowMore",
		"customersPickerOpen", "customersProfilePickerOpen", "customersPickerClose", "customersAttach", "customersHintAnswer",
		"customersAvatarsOn", "customersGithubLogin", "customersAvatarHtml", "customersMorePill", "customersSourcePills",
		"rowChevronHtml", "customersOriginPill", "customersTagPill", "customersTagPills", "customersIsOpenTarget", "customersOrgPill", "customersProfileValue", "customersDetailsHtml",
		"customersFlagEmoji", "customersCountryName", "customersCountryPill", "customersAffiliationHtml",
		"graphcanvasRound", "graphcanvasLabel", "graphcanvasLayout", "graphcanvasSvg",
	} {
		fns = append(fns, jsFunc(t, html, name))
	}
	consts := ""
	for _, name := range []string{"CUSTOMERS_KIND_LABELS", "CUSTOMERS_ADDABLE_KINDS", "CUSTOMERS_LEVEL_LABELS", "CUSTOMERS_LEVEL_PILL", "CUSTOMERS_ORIGIN_LABELS", "CUSTOMERS_REF_KIND_LABELS",
		"CUSTOMERS_REF_GH", "CUSTOMERS_REF_WORDS", "CUSTOMERS_GROUP_SHOWN", "CUSTOMERS_SUGGESTED_GROUPS", "CUSTOMERS_OTHER_GROUP", "CUSTOMERS_KIND_ICONS",
		"CUSTOMERS_ORIGIN_SHORT", "CUSTOMERS_PROFILE_FIELDS", "CUSTOMERS_SOURCE_PILLS_SHOWN", "CUSTOMERS_TAG_PILLS_SHOWN", "CUSTOMERS_GITHUB_LOGIN", "CUSTOMERS_AVATAR_PX", "CUSTOMERS_AVATAR_FETCH_PX",
		"GRAPHCANVAS_W", "GRAPHCANVAS_H", "GRAPHCANVAS_RING_ARTEFACT", "GRAPHCANVAS_RING_PEOPLE", "GRAPHCANVAS_MAX_ARTEFACTS", "GRAPHCANVAS_LABEL_MAX"} {
		consts += jsConstBlock(t, html, name) + "\n"
	}
	script := consts + `
const CUSTOMERS_EVIDENCE_SHOWN = 5;
let _customersData = null;
let _customersOpen = null;
let _customersUi = { open: {}, expanded: {}, picker: '', profilePicker: false };
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
  profile: { organization: { value: 'acme', source: 'auto', rule: 'github_profile' }, location: { value: 'Berlin', source: 'auto', rule: 'github_profile' }, country: { value: 'DE', source: 'auto', rule: 'location' } },
  affiliation_suggestion: { value: 'Acme', rule: 'github_company', evidence: 'from GitHub company field "acme"' },
  satisfaction: { level: 'at_risk', source: 'manual', by: 'olivia', suggestion: { level: 'neutral', evidence: ['gh:pr:o/r#2'] } },
  merged_from: [{ person: { id: 'p_old', name: 'Old <Ada>' }, by: 'olivia', at: '2026-10-01T00:00:00Z' }],
  counts: { authored: 3 }, hints: [{ id: 'h_1', other_kind: 'email', other_value: 'ada@<x>.org', reason: 'the address and the login start the same' }, { id: 'h_2', other_kind: 'customer', other_value: 'Ada B' }]
};
const base = { enabled: true, avatars: true, me: Object.assign({}, person, { id: 'p_me', me: true, name: 'Me' }), people: [person, { id: 'p_two', name: 'Two' }], suggestions: [{ id: 's_1', kind: 'github', value: 'stranger', counts: { authored: 2 }, evidence: ['gh:issue:o/r#9'] }], default_tags: ['maintainer', 'contributor', 'end-user', 'stakeholder'], levels: ['unknown', 'at_risk', 'neutral', 'satisfied'] };

// Owner: everything.
_customersData = Object.assign({}, base, { full: true, owner: true });
let list = customersListHtml(_customersData);
let prof = customersProfileHtml(person);
must(list.includes('data-action="customersAccept"') && list.includes('data-action="customersDismiss"') && list.includes('Add customer'), 'owner list lacks add, accept or dismiss');
must(list.includes('data-arg0="me"') && list.includes('data-arg0="p_abc"'), 'list rows do not open me and the customer');
must(!list.includes('<b>Ada</b>') && list.includes('&lt;b&gt;Ada&lt;/b&gt;'), 'a name reached the page unescaped');
must(prof.includes('data-action="customersForget"'), 'owner profile lacks Forget');
must(prof.includes('<table class="customers-ident-table"') && prof.includes('<th scope="col">Identity</th>') && prof.includes('<th scope="row" class="customers-ident-value">') && prof.includes('data-action="customersIdentityRemove"') && prof.includes('<th scope="col">Source</th>'), 'identities are not a table with headers, a row header and row actions');
must(customersFlagEmoji('DE') === '\u{1F1E9}\u{1F1EA}' && customersFlagEmoji('de') === '' && customersFlagEmoji('<b>') === '' && customersFlagEmoji(null) === '', 'flag emoji');
must(list.includes('class="kc-pill customers-pill customers-country" role="img" aria-label="Country: ') && list.includes(' DE</span>'), 'the row lacks the country pill with its text code and accessible name');
must(customersCountryPill({ profile: { country: { value: '<b>' } } }) === '' && customersCountryPill({}) === '', 'a malformed or missing country must render nothing');
must(prof.includes('data-customers-field="country-line"') && prof.includes('data-customers-field="profile-country"') && prof.includes('(from location)'), 'Details lack the country line, its field or its provenance');
must(prof.includes('Likely: <strong>Acme</strong>') && prof.includes('from GitHub company field &quot;acme&quot;') && prof.includes('data-action="customersAffiliationAccept" data-arg0="Acme"') && prof.includes('data-action="customersAffiliationDismiss" data-arg0="Acme"'), 'the likely affiliation lacks its evidence, Accept or Dismiss');
must(customersAffiliationHtml({}) === '' && customersAffiliationHtml({ affiliation_suggestion: { value: '' } }) === '', 'no suggestion must render nothing');
must(prof.includes('data-action="customersSatisfactionSet"') && prof.includes('data-action="customersSatisfactionConfirm"') && prof.includes('private note'), 'owner profile lacks satisfaction or notes');
must(prof.includes('data-action="customersIdentityConfirm"'), 'owner cannot confirm a claimed identity');
must(prof.includes('data-action="customersHintAnswer" data-arg0="h_1" data-arg1="yes"') && prof.includes('data-arg0="h_1" data-arg1="no"') && prof.includes('ada@&lt;x&gt;.org'), 'a suggested-identity hint lacks Yes, No or an escaped identity');
must(!prof.includes('data-arg0="h_2" data-arg1="yes"') && prof.includes('data-arg0="h_2" data-arg1="no"'), 'a customer-to-customer hint offers Yes');
must(prof.includes('data-action="customersGoalConfirm" data-arg0="g_2"') && prof.includes('Issue #1') && !prof.includes('gh:issue:o/r#1'), 'a suggested goal lacks Confirm or its readable evidence');
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
` + customersLayoutNoiseScript + `
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

// customersLayoutNoiseScript is the part of TestCustomersViewGates that pins
// the Customers layout and noise rules (hivecommons/hive#18742): no raw
// references on a suggested row, readable evidence links, at most five, a
// group per source with "Show N more", long keys truncated rather than
// wrapped, no "0 goals", the "me" marker as a badge, and the Suggested group
// staying open (with its expansion and the focus moving on) after an accept,
// a dismiss, an attach or a "Same person?" answer.
const customersLayoutNoiseScript = `
// Raw references never print; links are readable and bounded.
const longKey = 'b43dc28a888d316aa1fe4a47bf3038cd1bca7bbb2b5cdbf5e7ecb9cc10672a95@group.calendar.google.com';
const sug = { id: 's_1', kind: 'email', value: longKey, counts: { added: 1, organised: 1 }, last_seen: '2026-11-17T10:00:00Z',
  evidence: ['gh:issue:hivecommons/hive#18726', 'gh:pr:hivecommons/hive#18738', 'event:93d1acd2360e10c0', 'inbox:email:m1', 'meeting:m2', 'gh:pr:hivecommons/hive#1'] };
let row = customersSuggestionRowHtml(sug);
must(!row.includes('gh:issue') && !row.includes('gh:pr') && !row.includes('event:93d1') && !row.includes('customers-evidence'), 'a raw reference is printed on a suggested row: ' + row);
must(row.includes('Why is this suggested?') && row.includes('<details class="customers-why"'), 'the evidence is not behind a disclosure');
must(row.includes('href="https://github.com/hivecommons/hive/issues/18726"') && row.includes('href="https://github.com/hivecommons/hive/pull/18738"') && row.includes('>Issue #18726</a>') && row.includes('>PR #18738</a>'), 'issue and PR links: ' + row);
must(row.includes('calendar invite') && row.includes('<span>mail</span>'), 'an event or mail is not named in words');
must((row.match(/<a href=/g) || []).length + (row.match(/<span>(calendar invite|mail|meeting|inbox file)<\/span>/g) || []).length <= CUSTOMERS_EVIDENCE_SHOWN, 'more than five references shown');
must(!row.includes('#1</a>'), 'the sixth reference was shown');
must(row.includes('rel="noopener noreferrer"') && row.includes('target="_blank"'), 'links must not leak the opener');
must(row.includes('class="customers-name" title="' + longKey + '"'), 'a long key lacks its full-value tooltip');
must(row.includes('data-action="customersAccept"') && row.includes('data-action="customersPickerOpen"') && row.includes('Add to existing') && row.includes('data-action="customersDismiss"'), 'a suggested row lacks Accept, Add to existing or Dismiss');
must(customersEvidenceLabel('gh:pr:bad repo#1').includes('activity') && !customersEvidenceLabel('gh:pr:o/r#1"><script>').includes('<script>'), 'a malformed reference must not become a link');
must(!customersEvidenceLabel('gh:pr:o/r#1').includes('javascript:'), 'link');

// No "0 goals" on a row, "me" is a badge, the name is its own element.
_customersData = Object.assign({}, base, { full: true, owner: true });
const noGoals = customersRowHtml({ id: 'p_z', name: 'Zed', origin: 'hive_user', tags: [{ name: 'maintainer', state: 'on', source: 'auto' }], goals: [], updated_at: '2026-11-17T10:00:00Z' }, false);
must(!/0 goals/.test(noGoals) && !noGoals.includes('goals'), '"0 goals" is printed: ' + noGoals);
must(customersRowHtml({ id: 'p_z', name: 'Zed', goals: [{ id: 'g', text: 't', state: 'confirmed' }] }, false).includes('1 goal'), 'a confirmed goal is not counted');
const meRow = customersRowHtml({ id: 'p_me', name: 'clubanderson', origin: 'hive_user' }, true);
must(meRow.includes('<span class="badge customers-me">me</span>') && !meRow.includes('clubanderson me'), 'me is not a badge: ' + meRow);
must(meRow.includes('class="customers-avatar"') && meRow.includes('>C</span>') && !meRow.includes('customers-row-actions') && !meRow.includes('>My profile<') && !meRow.includes('>Open<'), 'a row lacks its avatar, or still has an Open button');
must(customersGoalsHtml({ goals: [] }).includes('No goal yet.'), 'the profile does not say "No goal yet"');

// Avatars: a confirmed GitHub login gets the GitHub picture, lazily, with no
// referrer, at a fixed size, over the initial; nothing keyed by an email is ever
// requested; the hive can switch them off.
const ghPerson = { id: 'p_gh', name: 'Grace <Hopper>', origin: 'accepted', identities: [{ kind: 'email', value: 'grace@example.com' }, { kind: 'github', value: 'ghopper' }], tags: [] };
let ghRow = customersRowHtml(ghPerson, false);
must(ghRow.includes('<img class="customers-avatar-img" src="https://github.com/ghopper.png?size=' + CUSTOMERS_AVATAR_FETCH_PX + '"'), 'no GitHub avatar: ' + ghRow);
must(ghRow.includes('loading="lazy"') && ghRow.includes('referrerpolicy="no-referrer"') && ghRow.includes('width="' + CUSTOMERS_AVATAR_PX + '" height="' + CUSTOMERS_AVATAR_PX + '"') && ghRow.includes('alt="Grace &lt;Hopper&gt;"') && ghRow.includes('data-error-action="gh19"'), 'avatar attributes: ' + ghRow);
must(ghRow.includes('<span class="customers-initial" aria-hidden="true">G</span>'), 'the initial must stay under the picture');
const hosts = (ghRow.match(/(?:src|href)="https?:\/\/[^"\/]+/g) || []);
must(hosts.every(h => h.endsWith('://github.com')), 'a row asks a host other than github.com: ' + hosts);
const emailOnly = customersRowHtml({ id: 'p_e', name: 'Eve', identities: [{ kind: 'email', value: 'eve@example.com' }, { kind: 'slack', value: 'U1' }, { kind: 'github', value: 'not a login', }, { kind: 'github', value: 'mallory', claimed: true }] }, false);
must(!emailOnly.includes('<img') && !/gravatar|libravatar|md5|sha256/i.test(emailOnly), 'an email-only or unconfirmed person must not get a picture: ' + emailOnly);
must(customersGithubLogin({ identities: [{ kind: 'github', value: 'a/../b' }] }) === '' && customersGithubLogin({ identities: [{ kind: 'github', value: 'x'.repeat(40) }] }) === '', 'a malformed login reached a URL');
_customersData = Object.assign({}, base, { full: true, owner: true, avatars: false });
must(!customersRowHtml(ghPerson, false).includes('<img') && customersRowHtml(ghPerson, false).includes('>G</span>'), 'avatars off must show the initial and request nothing');
_customersData = Object.assign({}, base, { full: true, owner: true });
delete _customersData.avatars;
must(!customersRowHtml(ghPerson, false).includes('<img'), 'avatars must be off unless the hive says on');
_customersData = Object.assign({}, base, { full: true, owner: true });
must(customersSuggestionRowHtml({ id: 's_g', kind: 'github', value: 'octocat', evidence: [] }).includes('src="https://github.com/octocat.png?size='), 'a suggested GitHub person has no picture');
must(!customersSuggestionRowHtml({ id: 's_m', kind: 'email', value: 'a@example.com', evidence: [] }).includes('<img'), 'a suggested email got a picture');
must(customersSuggestionRowHtml({ id: 's_s', kind: 'slack', value: 'U1', evidence: [] }).includes('💬'), 'a slack suggestion lost its icon');

// Pills: tags, identities, origin; "+N" beyond the limit; manual marked.
const pillPerson = { id: 'p_pl', name: 'Pill', origin: 'hive_user', updated_at: '2026-11-17T10:00:00Z',
  identities: [{ kind: 'hive_user', value: 'pill' }, { kind: 'github', value: 'pill' }, { kind: 'email', value: 'p@example.com' }, { kind: 'slack', value: 'U9' }, { kind: 'name', value: 'Pill' }],
  tags: [{ name: 'maintainer', state: 'on', source: 'auto', rule: 'role:owner' }, { name: 'vip', state: 'on', source: 'manual', by: 'olivia' }, { name: 'end-user', state: 'off', source: 'auto' }, { name: 'contributor', state: 'on', source: 'auto' }, { name: 'stakeholder', state: 'on', source: 'auto' }],
  satisfaction: { level: 'satisfied' } };
const pillRow = customersRowHtml(pillPerson, false);
must(pillRow.includes('<span class="kc-pill customers-pill customers-tag" data-tag="maintainer" data-source="auto" title="maintainer - Automatic: role:owner">maintainer</span>'), 'a default tag is not a toned pill: ' + pillRow);
must(!/class="badge customers-chip"/.test(pillRow) && !pillRow.includes('customers-row-meta'), 'tags are still plain badges or the meta line is back');
must(pillRow.includes('customers-tag" data-source="manual" title="vip - Set by olivia"') && !pillRow.includes('data-tag="vip"') && pillRow.includes('customers-tag-dot'), 'a free-form manual tag must be neutral and marked');
must(!pillRow.includes('>end-user<'), 'an off tag is shown');
must((pillRow.match(/customers-tag"/g) || []).length === CUSTOMERS_TAG_PILLS_SHOWN && pillRow.includes('customers-more" title="stakeholder">+1</span>'), 'tag overflow: ' + pillRow);
must(pillRow.includes('customers-source" data-kind="github"') && pillRow.includes('customers-source" data-kind="email"') && pillRow.includes('customers-more" title="Slack: U9">+1</span>') && !pillRow.includes('>U9<') && !pillRow.includes('data-kind="hive_user"') && !pillRow.includes('data-kind="name"'), 'identity pills: ' + pillRow);
must(pillRow.includes('customers-origin" title="hive user, updated ') && pillRow.includes('>hive user</span>') && pillRow.includes('data-status="ok"') && pillRow.includes('customers-updated'), 'origin, status or date: ' + pillRow);
must(pillRow.indexOf('customers-source') < pillRow.indexOf('customers-origin') && pillRow.indexOf('customers-origin') < pillRow.indexOf('customers-tag') && pillRow.indexOf('customers-tag') < pillRow.indexOf('data-status="ok"') && pillRow.indexOf('data-status="ok"') < pillRow.indexOf('customers-updated'), 'order');
must(!/class="customers-row-title"/.test(pillRow) && (pillRow.match(/<div/g) || []).length === 3, 'a list row is more than its wrapper, the card and its main line: ' + pillRow);
// Inline profile: the chevron is a real button left of the avatar, the row
// carries the toggle, and the profile opens under the row while it is open.
must(/<button type="button" class="row-chevron"[^>]*aria-expanded="false"/.test(pillRow) && pillRow.indexOf('row-chevron') < pillRow.indexOf('class="customers-avatar"') && !pillRow.includes('customers-profile"') && pillRow.includes('data-action="customersToggle" data-arg0="p_pl"'), 'a closed row lacks its chevron button: ' + pillRow);
_customersOpen = pillPerson;
const openRow = customersRowHtml(pillPerson, false);
must(openRow.includes('aria-expanded="true"') && openRow.includes('aria-controls="customers-profile-p_pl"') && openRow.includes('<div class="customers-profile" id="customers-profile-p_pl"') && openRow.indexOf('customers-card') < openRow.indexOf('customers-profile'), 'an open row does not show its profile under it');
must(!customersRowHtml(person, false).includes('customers-profile"') && !customersRowHtml({ id: 'p_me', name: 'Me' }, true).includes('customers-profile"'), 'another row opened too');
must(!customersProfileHtml(pillPerson).includes('customersClose') && customersProfileHtml(pillPerson).includes('data-action="customersRename"'), 'the profile still has a back button or lost Rename');
// The toggle chip in a profile is the row's pill: the same classes, not a second style.
const chip = customersTagChipHtml('maintainer', pillPerson.tags[0]);
must(chip.includes('class="kc-pill customers-pill customers-tag customers-tag-toggle" data-tag="maintainer"') && chip.includes('aria-pressed="true"') && chip.includes('<span class="customers-tag-auto">auto</span>') && !chip.includes('customers-chip'), 'the tag toggle is not the row pill: ' + chip);
must(customersTagChipHtml('end-user', pillPerson.tags[2]).includes('data-state="off"') && customersTagChipHtml('contributor', undefined).includes('aria-pressed="false"'), 'an off tag lost its state');
_customersOpen = null;

// Groups by source, first five, "Show N more".
const manySugg = [];
for (let i = 0; i < 7; i++) manySugg.push({ id: 'g_' + i, kind: 'github', value: 'user' + i, counts: { mentioned_in: 1 }, evidence: [] });
manySugg.push({ id: 'e_0', kind: 'email', value: 'a@example.com', evidence: [] }, { id: 'e_1', kind: 'email', value: 'b@example.com', evidence: [] }, { id: 'x_0', kind: 'slack', value: 'U1', evidence: [] });
_customersData = Object.assign({}, base, { full: true, owner: true, suggestions: manySugg });
_customersUi = { open: {}, expanded: {}, picker: '', profilePicker: false };
let grouped = customersListHtml(_customersData);
must(grouped.indexOf('GitHub people') > 0 && grouped.indexOf('GitHub people') < grouped.indexOf('Email addresses') && grouped.indexOf('Email addresses') < grouped.indexOf('Other people'), 'groups are missing or out of order');
must((grouped.match(/data-customer-suggestion="g_/g) || []).length === 5 && grouped.includes('Show 2 more') && !grouped.includes('Show fewer'), 'a group must show five and offer Show 2 more');
must(!grouped.includes('<details class="customers-block customers-suggested" data-ui-key="suggested" open'), 'the Suggested group must start closed');
_customersUi.expanded.github = true;
grouped = customersListHtml(_customersData);
must((grouped.match(/data-customer-suggestion="g_/g) || []).length === 7 && grouped.includes('Show fewer'), 'an expanded group shows everything');
_customersUi.open.suggested = true;
must(customersListHtml(_customersData).includes('data-ui-key="suggested" open'), 'an open Suggested group is rendered closed');
must(customersSuggestionOrder().join() === 'g_0,g_1,g_2,g_3,g_4,g_5,g_6,e_0,e_1,x_0', 'display order: ' + customersSuggestionOrder().join());
must(customersNeighbour('g_6') === 'e_0' && customersNeighbour('x_0') === 'e_1' && customersNeighbour('nope') === '', 'neighbour');

// Picker: Me is first and the filter keys are lower-cased names.
_customersUi.picker = 'g_0';
const pickedHtml = customersListHtml(_customersData);
must(pickedHtml.includes('data-customers-picker="g_0"') && pickedHtml.includes('data-action="customersAttach" data-arg0="g_0" data-arg1="me"') && pickedHtml.includes('data-arg1="p_abc" data-name="&lt;b&gt;ada&lt;/b&gt;"'), 'picker: ' + pickedHtml.slice(pickedHtml.indexOf('customers-picker')));
must(pickedHtml.indexOf('data-arg1="me"') < pickedHtml.indexOf('data-arg1="p_abc"'), 'Me is not first in the picker');
_customersUi.picker = '';

// Profile side: a full viewer can pull a suggestion in; nobody else.
_customersUi.profilePicker = false;
must(customersAttachHtml(person).includes('data-action="customersProfilePickerOpen"') && customersAttachHtml(person).includes('Add a suggested person here'), 'the profile lacks Add a suggested person here');
_customersUi.profilePicker = true;
must(customersAttachHtml(person).includes('data-action="customersAttach" data-arg0="g_0" data-arg1="p_abc"') && customersAttachHtml(Object.assign({}, person, { me: true })).includes('data-arg1="me"'), 'profile picker');
_customersUi.profilePicker = false;
_customersData = { enabled: true, full: false, owner: false, me: mine, people: [], suggestions: manySugg, default_tags: base.default_tags, levels: base.levels };
must(customersAttachHtml(mine) === '', 'a read-only profile offers to attach suggestions');
_customersData = Object.assign({}, base, { full: true, owner: true, suggestions: [] });
must(customersAttachHtml(person).includes('Nobody is suggested right now.') && !customersAttachHtml(person).includes('customersProfilePickerOpen'), 'an empty list still offers the picker');

// Accept, Dismiss, Attach and a hint answer keep the Suggested group open
// and its expansion, and move the focus to the next suggestion.
const calls = [];
let focused = '';
let rendered = '';
let current = [];
const document = { getElementById: (id) => id === 'customers-panel' ? { querySelector: (q) => ({ focus: () => { focused = q; } }) } : null };
function showToast() {}
function hiveConfirm() { return Promise.resolve(true); }
function customersProfileError(m) { throw new Error(m); }
function customersTimelineLoad() {}
function customersRequest(method, path, body) { calls.push(method + ' ' + path + (body ? ' ' + JSON.stringify(body) : '')); return Promise.resolve({ name: 'N', hints: [] }); }
function renderCustomers() { customersShow(); }
function customersShow() { rendered = _customersOpen ? 'profile' : customersListHtml(_customersData); }
function fetchCustomers() { _customersData = Object.assign({}, _customersData, { suggestions: current.slice() }); customersShow(); return Promise.resolve(); }
const all = manySugg.slice();
(async () => {
  _customersOpen = null;
  _customersUi = { open: { suggested: true }, expanded: { github: true }, picker: '', profilePicker: false };
  _customersData = Object.assign({}, base, { full: true, owner: true, suggestions: all.slice() });
  current = all.filter(s => s.id !== 'g_2');
  await customersAccept('g_2');
  must(calls[0] === 'POST /api/customers/suggestions/g_2/accept', 'accept call: ' + calls[0]);
  must(rendered.includes('data-ui-key="suggested" open'), 'the Suggested group closed after an accept');
  must(!rendered.includes('data-customer-suggestion="g_2"') && rendered.includes('data-customer-suggestion="g_3"'), 'the accepted row is still listed, or its neighbour is not');
  must((rendered.match(/data-customer-suggestion="g_/g) || []).length === 6 && rendered.includes('Show fewer'), 'the expansion was lost after an accept');
  must(focused === '[data-customer-suggestion="g_3"] [data-action="customersAccept"]', 'the focus did not move to the next suggestion: ' + focused);

  current = current.filter(s => s.id !== 'g_6');
  await customersDismiss('g_6');
  must(calls[1] === 'POST /api/customers/suggestions/g_6/dismiss' && rendered.includes('data-ui-key="suggested" open'), 'the group closed after a dismiss');
  must(focused === '[data-customer-suggestion="e_0"] [data-action="customersAccept"]', 'dismiss focus: ' + focused);

  _customersUi.picker = 'e_0';
  current = current.filter(s => s.id !== 'e_0');
  await customersAttach('e_0', 'me');
  must(calls[2] === 'POST /api/customers/suggestions/e_0/accept {"into":"me"}', 'attach call: ' + calls[2]);
  must(rendered.includes('data-ui-key="suggested" open') && _customersUi.picker === '' && !rendered.includes('data-customer-suggestion="e_0"'), 'attach: the group closed, the picker stayed, or the row stayed');
  must(!rendered.includes('customers-picker'), 'the picker stayed open after an attach');

  // The same suggestion list answered from a profile: the group's state is
  // untouched and the focus goes to the next question.
  _customersOpen = { id: 'p_abc', name: 'Ada', hints: [{ id: 'h_1' }, { id: 'h_2' }] };
  _customersUi.open.suggested = true;
  await customersHintAnswer('h_1', 'yes');
  must(_customersUi.open.suggested === true && _customersUi.expanded.github === true, 'answering a hint reset the Suggested state');
  must(focused === '[data-hint="h_2"] button', 'hint focus: ' + focused);
  _customersOpen = null;
  customersShowMore('github');
  must(_customersUi.expanded.github === false, 'Show fewer did not fold the group');
})().catch(e => { console.error(e.stack || e.message); process.exit(1); });
`

// The avatar request goes to github.com from the browser, which the page's
// img-src must allow, and to nothing less narrow than https. This pins that
// the policy admits it without ever admitting plain http or a wildcard host.
func TestCustomersAvatarsCSPImgSrc(t *testing.T) {
	got := cspDirective(servedCSP(t), "img-src")
	if got != "img-src 'self' data: https:" {
		t.Fatalf("img-src = %q; GitHub avatars (https://github.com/<login>.png, which redirects to avatars.githubusercontent.com) need https:, and nothing broader (no http:, no *) is acceptable", got)
	}
}

// The Features tab renders the avatars switch wired to the key the API reads.
func TestFeaturesTab_CustomersAvatarsToggleMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`data-section="features" data-key="customersAvatarsEnabled"`,
		`yamlHint('customers.avatars.enabled')`,
		`f.customersAvatarsEffective !== f.customersAvatars`,
		`HIVE_CUSTOMERS_AVATARS_ENABLED overrides this setting`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

// customers.avatars.enabled: default on, owner-only, persisted, reported with
// the effective value, left alone by an unrelated PUT and overridden by env.
func TestGovernorFeatures_CustomersAvatarsToggleRoundTrip(t *testing.T) {
	t.Setenv(config.EnvCustomersAvatarsEnabled, "")
	s := covApiServer(t)
	type payload struct {
		Features struct {
			Avatars          bool `json:"customersAvatars"`
			AvatarsEffective bool `json:"customersAvatarsEffective"`
		} `json:"features"`
	}
	read := func() payload {
		var p payload
		if err := json.Unmarshal(doOwnerGet(s, "/api/config/governor").Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := read(); !got.Features.Avatars || !got.Features.AvatarsEffective {
		t.Fatalf("default payload = %+v, want on", got.Features)
	}
	if rec := putFeatures(s, map[string]any{"customersAvatarsEnabled": false}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden || !s.deps.Config.Customers.Avatars.IsEnabled() {
		t.Fatalf("non-owner PUT = %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersAvatarsEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT off: %d - %s", rec.Code, rec.Body.String())
	}
	if got := read(); got.Features.Avatars || got.Features.AvatarsEffective {
		t.Fatalf("after disable = %+v", got.Features)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK || s.deps.Config.Customers.Avatars.IsEnabled() {
		t.Fatalf("an unrelated PUT changed it: %d", rec.Code)
	}
	t.Setenv(config.EnvCustomersAvatarsEnabled, "true")
	if got := read(); got.Features.Avatars || !got.Features.AvatarsEffective {
		t.Fatalf("env override = %+v", got.Features)
	}
}

// TestCustomersProfileDetailsWiring pins the real name, organization and
// location fields: the Details block with its auto or by-hand marker, the
// organization pill on the row, and the profile edit op.
func TestCustomersProfileDetailsWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function customersDetailsHtml(p)`,
		`function customersOrgPill(p)`,
		`data-action="customersProfileSave"`,
		`data-action="customersProfileReset"`,
		`function customersCountryPill(p)`,
		`customersOrgPill(p) + customersCountryPill(p) + customersTagPills(p)`,
		`{ op: 'affiliation_dismiss', value: value }`,
		`{ op: 'profile', field: 'organization', value: value }`,
		`{ op: 'profile', field: f.key, value: next }`,
		`op: 'profile', field: key, state: 'auto'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Customers UI lacks %q", want)
		}
	}
	if !strings.Contains(html, `customersDetailsHtml(p) + customersGoalsHtml(p)`) {
		t.Error("the Details block is not part of the profile")
	}
}
