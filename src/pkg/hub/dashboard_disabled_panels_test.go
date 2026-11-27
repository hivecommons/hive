package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
	"github.com/hivecommons/hive/pkg/config"
)

// TestAdminNPSReportsHubEnabledFlag pins #20197 on the server side: the admin
// NPS payload carries the hub's NPS switch so the dashboard can hide the card
// on a hub that never opted in. Env is set explicitly; pkg/hub tests must not
// depend on ambient HIVE_NPS_* values.
func TestAdminNPSReportsHubEnabledFlag(t *testing.T) {
	useTempNPSStore(t)
	t.Setenv(config.NPSRelayURLEnvVar, "")
	t.Setenv(config.NPSRelayPullSecretEnvVar, "")
	s := npsTestHub("h1")
	s.configPath = ""
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{env: "false", want: false},
		{env: "true", want: true},
	} {
		t.Setenv(config.NPSEnabledEnvVar, tc.env)
		rec := httptest.NewRecorder()
		s.handleAdminNPS(rec, httptest.NewRequest(http.MethodGet, npsAdminPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("HIVE_NPS_ENABLED=%s: code = %d", tc.env, rec.Code)
		}
		var got struct {
			Enabled *bool `json:"enabled"`
			Total   int   `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Enabled == nil || *got.Enabled != tc.want {
			t.Fatalf("HIVE_NPS_ENABLED=%s: enabled = %v, want %v (body=%s)", tc.env, got.Enabled, tc.want, rec.Body.String())
		}
	}
}

// TestDashboardHidesDisabledNPSCard pins the dashboard side of #20197: the
// admin poll no longer unhides the NPS section unconditionally, and
// loadAdminNPS shows it only when npsCardVisible agrees.
func TestDashboardHidesDisabledNPSCard(t *testing.T) {
	html := dashScript(t)
	users := npsHubJSFunc(t, html, "loadAdminUsers")
	if strings.Contains(users, "getElementById('nps-section').hidden = false") {
		t.Error("loadAdminUsers unhides the NPS section for every admin; visibility must follow the hub's NPS switch")
	}
	load := npsHubJSFunc(t, html, "loadAdminNPS")
	if !strings.Contains(load, "npsCardVisible(data)") {
		t.Error("loadAdminNPS must gate the NPS section on npsCardVisible")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; npsCardVisible behavior was NOT executed by this run")
	}
	script := npsHubJSFunc(t, html, "npsCardVisible") + `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
check('disabled hub with no responses hides the card', npsCardVisible({enabled: false, total: 0}) === false);
check('disabled hub with stored responses keeps the card', npsCardVisible({enabled: false, total: 3}) === true);
check('enabled hub with no responses keeps the card', npsCardVisible({enabled: true, total: 0}) === true);
check('older payload without the flag keeps the card', npsCardVisible({total: 0}) === true);
if (fails) process.exit(1);
`
	runDashboardJS(t, node, script)
}

// TestDashboardHidesOffStateFeaturePanels pins #20197 for the Scale Controls
// panels: Customers profile sharing and Ask AI help start hidden, are shown
// only while their feature is on, and every off feature is named once in the
// optional-features-off line.
func TestDashboardHidesOffStateFeaturePanels(t *testing.T) {
	html := dashScript(t)
	for _, id := range []string{"customers-share-setting", "ask-setting"} {
		tag := `id="` + id + `"`
		at := strings.Index(html, tag)
		if at < 0 {
			t.Fatalf("dashboard is missing %s", tag)
		}
		end := strings.Index(html[at:], ">")
		if !strings.Contains(html[at:at+end], " hidden") {
			t.Errorf("%s must render hidden until the feature is reported on", id)
		}
	}
	if !strings.Contains(npsHubJSFunc(t, html, "loadScaleSettings"), "applyOptionalFeaturePanels(eff)") {
		t.Error("loadScaleSettings must apply the optional feature panel visibility")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; applyOptionalFeaturePanels behavior was NOT executed by this run")
	}
	start := strings.Index(html, "var OPTIONAL_FEATURE_PANELS = [")
	end := strings.Index(html[start:], "];")
	if start < 0 || end < 0 {
		t.Fatal("OPTIONAL_FEATURE_PANELS not found")
	}
	script := html[start:start+end+2] + "\n" + npsHubJSFunc(t, html, "applyOptionalFeaturePanels") + `
const els = {
  'customers-share-setting': {hidden: true},
  'ask-setting': {hidden: true},
  'optional-features-off': {hidden: true, textContent: ''},
};
globalThis.document = { getElementById: id => els[id] || null };
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
applyOptionalFeaturePanels({customers_share_enabled: false, ask_enabled: false});
check('share panel hidden when off', els['customers-share-setting'].hidden === true);
check('ask panel hidden when off', els['ask-setting'].hidden === true);
check('off line shown', els['optional-features-off'].hidden === false);
check('off line names both env vars', els['optional-features-off'].textContent.includes('HIVE_HUB_CUSTOMERS_SHARE_ENABLED') && els['optional-features-off'].textContent.includes('HIVE_ASK_ENABLED'));
applyOptionalFeaturePanels({customers_share_enabled: true, ask_enabled: true});
check('share panel shown when on', els['customers-share-setting'].hidden === false);
check('ask panel shown when on', els['ask-setting'].hidden === false);
check('off line hidden when everything is on', els['optional-features-off'].hidden === true);
if (fails) process.exit(1);
`
	runDashboardJS(t, node, script)
}

// TestHubAdminsListReportsLoginProviders pins #20198: the hub admins payload
// names the enabled sign-in providers so the dashboard can label the field.
func TestHubAdminsListReportsLoginProviders(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "")
	s := hubAdminAPIServer()
	s.authProviders = auth.NewRegistry(&auth.Provider{Name: "custom", DisplayName: "SSO", IsOIDC: true})
	mkUser(t, hubAdminUsername)

	rec := serveHubAdminAPI(s, reqWithUser(http.MethodGet, "/api/hub/admins", "", hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		LoginProviders []string `json:"login_providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.LoginProviders) != 1 || got.LoginProviders[0] != "custom" {
		t.Fatalf("login_providers = %v, want [custom]", got.LoginProviders)
	}
}

// TestHubAdminGrantAcceptsCustomOIDCSubject confirms the save path accepts
// the custom:<subject> form a custom-OIDC hub stores (#20198).
func TestHubAdminGrantAcceptsCustomOIDCSubject(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withHubAdminsPath(t)
	t.Setenv(hubAdminsEnv, "")
	s := hubAdminAPIServer()
	mkUser(t, hubAdminUsername)

	rec := serveHubAdminAPI(s, reqWithUser(http.MethodPost, "/api/hub/admins", `{"login":"custom:00uAbC123"}`, hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Fatalf("grant custom subject status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !isHubAdmin("custom:00uAbC123") {
		t.Fatal("custom:<subject> grant did not make that identity a hub admin")
	}
	if isHubAdmin("custom:00uabc123") {
		t.Fatal("custom subjects are case-sensitive; a case-folded subject must not match")
	}
}

// TestDashboardHubAdminsFieldFollowsSignInProvider pins the dashboard side of
// #20198: the field is no longer hardwired to "GitHub login" on a hub that
// signs in with custom OIDC.
func TestDashboardHubAdminsFieldFollowsSignInProvider(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(npsHubJSFunc(t, html, "loadHubAdmins"), "applyHubAdminIdentityField(hubAdminIdentityField(data.login_providers))") {
		t.Fatal("loadHubAdmins must label the field from login_providers")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; hubAdminIdentityField behavior was NOT executed by this run")
	}
	script := npsHubJSFunc(t, html, "hubAdminIdentityField") + `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name, JSON.stringify(arguments)); } }
const gh = hubAdminIdentityField(['github']);
check('github hub asks for a GitHub login', gh.placeholder === 'GitHub login' && gh.label === 'GitHub login');
const legacy = hubAdminIdentityField(undefined);
check('unknown provider list keeps the GitHub wording', legacy.placeholder === 'GitHub login');
const oidc = hubAdminIdentityField(['custom']);
check('custom OIDC hub does not say GitHub', !/GitHub/.test(oidc.placeholder + oidc.hint + oidc.label));
check('custom OIDC hub shows custom:<subject>', oidc.placeholder.includes('custom:<subject>') && oidc.hint.includes('custom:<subject>'));
const mixed = hubAdminIdentityField(['github', 'custom']);
check('mixed hub names both forms', mixed.placeholder.includes('GitHub login') && mixed.placeholder.includes('custom:<subject>'));
if (fails) process.exit(1);
`
	runDashboardJS(t, node, script)
}

func runDashboardJS(t *testing.T, node, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dashboard_check.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("dashboard JS check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
