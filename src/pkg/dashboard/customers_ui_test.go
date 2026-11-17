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
	} {
		fns = append(fns, jsFunc(t, html, name))
	}
	consts := ""
	for _, name := range []string{"CUSTOMERS_KIND_LABELS", "CUSTOMERS_ADDABLE_KINDS", "CUSTOMERS_LEVEL_LABELS", "CUSTOMERS_LEVEL_PILL", "CUSTOMERS_ORIGIN_LABELS"} {
		consts += jsConstBlock(t, html, name) + "\n"
	}
	script := consts + `
const CUSTOMERS_EVIDENCE_SHOWN = 5;
let _customersData = null;
function escapeHtml(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
` + strings.Join(fns, "\n") + `
function must(cond, msg) { if (!cond) throw new Error(msg); }
const person = {
  id: 'p_abc', name: '<b>Ada</b>', origin: 'manual', me: false, notes: 'private note',
  identities: [{ kind: 'hive_user', value: 'ada' }, { kind: 'email', value: 'ada@example.com', claimed: true }, { kind: 'github', value: 'ada', manual: true, by: 'olivia' }],
  tags: [{ name: 'maintainer', state: 'on', source: 'auto', rule: 'role:owner' }, { name: 'vip', state: 'on', source: 'manual', by: 'olivia' }, { name: 'end-user', state: 'off', source: 'auto' }],
  goals: [{ id: 'g_1', text: 'Ship exports', state: 'confirmed', source: 'manual', by: 'ada' }, { id: 'g_2', text: 'Wants SSO', state: 'suggested', source: 'auto', evidence: ['gh:issue:o/r#1'] }],
  satisfaction: { level: 'at_risk', source: 'manual', by: 'olivia', suggestion: { level: 'neutral', evidence: ['gh:pr:o/r#2'] } },
  counts: { authored: 3 }, hints: [{ id: 'h_1' }]
};
const base = { enabled: true, me: Object.assign({}, person, { id: 'p_me', me: true, name: 'Me' }), people: [person], suggestions: [{ id: 's_1', kind: 'github', value: 'stranger', counts: { authored: 2 }, evidence: ['gh:issue:o/r#9'] }], default_tags: ['maintainer', 'contributor', 'end-user', 'stakeholder'], levels: ['unknown', 'at_risk', 'neutral', 'satisfied'] };

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
must(prof.includes('data-action="customersGoalConfirm" data-arg0="g_2"') && prof.includes('gh:issue:o/r#1'), 'a suggested goal lacks Confirm or its evidence');
must(!prof.includes('data-action="customersGoalConfirm" data-arg0="g_1"'), 'a confirmed goal offers Confirm');
must(prof.includes('data-arg0="vip" data-arg1="auto"') && !prof.includes('data-arg0="maintainer" data-arg1="auto"'), 'Reset to automatic must show on manual tags only');
must(!prof.includes('data-arg0="hive_user:ada"'), 'the hive user identity offers Remove');
must(customersSummaryText(_customersData) === '2 customers · 1 suggested', 'summary: ' + customersSummaryText(_customersData));

// Read-write: no Forget.
_customersData = Object.assign({}, base, { full: true, owner: false });
prof = customersProfileHtml(person);
must(!prof.includes('customersForget'), 'read-write profile offers Forget');
must(prof.includes('data-action="customersSatisfactionSet"'), 'read-write profile lacks satisfaction');

// Read-only: own profile only; no suggestions, add, satisfaction, notes, activity, forget or confirm.
const mine = { id: 'p_me', name: 'Reed', me: true, origin: 'hive_user', identities: [{ kind: 'hive_user', value: 'reed' }, { kind: 'email', value: 'r@example.com', claimed: true }], tags: [], goals: [{ id: 'g_9', text: 'Wants an API', state: 'suggested', source: 'auto' }], satisfaction: {} };
_customersData = { enabled: true, full: false, owner: false, me: mine, people: [], suggestions: [], default_tags: base.default_tags, levels: base.levels };
list = customersListHtml(_customersData);
prof = customersProfileHtml(mine);
for (const banned of ['customersAccept', 'customersDismiss', 'customersAdd', 'Suggested (']) must(!list.includes(banned), 'read-only list shows ' + banned);
for (const banned of ['customersForget', 'customersSatisfaction', 'customersNotesSave', 'customersIdentityConfirm', 'Activity']) must(!prof.includes(banned), 'read-only profile shows ' + banned);
must(prof.includes('data-action="customersGoalConfirm" data-arg0="g_9"') && prof.includes('data-action="customersGoalAdd"') && prof.includes('data-action="customersTagSet"') && prof.includes('data-action="customersIdentityAdd"'), 'read-only profile lacks its own goal, tag or identity controls');
must(prof.includes('unconfirmed'), 'a claimed identity is not marked unconfirmed');
must(customersSummaryText(_customersData) === 'your profile', 'read-only summary: ' + customersSummaryText(_customersData));
must(customersSummaryText({ enabled: false }) === 'off', 'off summary');
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
