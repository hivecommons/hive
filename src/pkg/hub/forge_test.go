package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseForgeTarget locks the validation contract: a forge is validated into
// a known kind, never accepted as an arbitrary string.
func TestParseForgeTarget(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		kind    ForgeKind
		host    string
		apiURL  string
	}{
		{name: "public bare", in: "github.com", kind: ForgeGitHub, host: "github.com", apiURL: ""},
		{name: "public via URL", in: "https://github.com/", kind: ForgeGitHub, host: "github.com"},
		{name: "public via api host", in: "api.github.com", kind: ForgeGitHub, host: "github.com"},
		{name: "uppercase normalizes", in: "GitHub.COM", kind: ForgeGitHub, host: "github.com"},
		{
			name: "ghe bare", in: "github.ibm.com",
			kind: ForgeGitHubEnterprise, host: "github.ibm.com",
			apiURL: "https://github.ibm.com/api/v3",
		},
		{
			// The exact paste an operator is most likely to make — an org URL.
			// It must normalize to the same target as the bare host, not be
			// recorded verbatim and produce a spoke that talks to nothing.
			name: "ghe pasted org URL", in: "https://github.ibm.com/enricom-ibm",
			kind: ForgeGitHubEnterprise, host: "github.ibm.com",
			apiURL: "https://github.ibm.com/api/v3",
		},
		{
			name: "future ghe host", in: "github.cisco.com",
			kind: ForgeGitHubEnterprise, host: "github.cisco.com",
			apiURL: "https://github.cisco.com/api/v3",
		},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "whitespace rejected", in: "   ", wantErr: true},
		{name: "no dot rejected", in: "notahost", wantErr: true},
		{name: "space in host rejected", in: "git hub.com", wantErr: true},
		{name: "double dot rejected", in: "github..com", wantErr: true},
		{name: "trailing dot rejected", in: "github.com.", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseForgeTarget(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseForgeTarget(%q) = %+v, want an error — arbitrary strings must not be accepted", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseForgeTarget(%q) unexpected error: %v", tc.in, err)
			}
			if got.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.Host != tc.host {
				t.Errorf("host = %q, want %q", got.Host, tc.host)
			}
			if got.APIURL != tc.apiURL {
				t.Errorf("api url = %q, want %q", got.APIURL, tc.apiURL)
			}
		})
	}
}

// TestForgeSwitchSurvivesHeartbeat is the whole point of the feature, and it is
// a REGRESSION TEST for a failure verified on the live fleet.
//
// github_host was edited to "github.ibm.com" in a hive's meta.json on the hub
// and the hub restarted. It did not stick: the spoke re-reported github.com on
// its next beat and the hub adopted it. A hub-side edit is provably
// insufficient, because the spoke is the one that reports the host and nothing
// the hub writes to meta changes what the spoke runs.
//
// The switch must therefore PUSH, and keep pushing until the spoke confirms.
func TestForgeSwitchSurvivesHeartbeat(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}

	// A hive on github.com that an operator has just switched to github.ibm.com:
	// the target is recorded and delivery is armed.
	saveSaaSHive(&SaaSHive{
		ID: "vllmd03", Status: "running", Org: "enricom-ibm",
		Repos: []string{"jackrabbit"}, PrimaryRepo: "jackrabbit", ACMMLevel: 2,
		GitHubHost: "github.ibm.com", RequestedGitHubHost: "github.ibm.com",
		ForgeDelivered: false,
	})

	// Beat 1: the spoke still reports the OLD host and the OLD api url. This is
	// exactly the state that silently reverted the manual edit. The hub must
	// push the enterprise API URL.
	got := projectConfigForHiveID("vllmd03", "enricom-ibm", []string{"jackrabbit"}, "jackrabbit", 2, "", "https://api.github.com")
	if got == nil {
		t.Fatal("no push for an undelivered forge switch — this is the exact production silence that reverted the manual edit")
	}
	if got.GitHubAPIURL != "https://github.ibm.com/api/v3" {
		t.Errorf("pushed GitHubAPIURL = %q, want the enterprise API", got.GitHubAPIURL)
	}

	// ...and the hub must NOT declare victory on the spoke's stale report.
	// Latching here would end the delivery with no evidence it worked.
	if s.adoptSpokeForge("vllmd03", "github.com") {
		t.Error("adopted delivery from a spoke still reporting the OLD host")
	}
	if h := loadSaaSHive("vllmd03"); h == nil || h.ForgeDelivered {
		t.Fatalf("ForgeDelivered must stay false while the spoke reports the old host, got %+v", h)
	}

	// Beat 2: the spoke has adopted the API URL. HostLabel() falls back to
	// api_url, so it now reports github.ibm.com — which is what closes the
	// read-back and stops the push for good.
	if !s.adoptSpokeForge("vllmd03", "github.ibm.com") {
		t.Fatal("spoke reported the requested host but delivery did not complete")
	}
	h := loadSaaSHive("vllmd03")
	if h == nil || !h.ForgeDelivered || h.GitHubHost != "github.ibm.com" {
		t.Fatalf("after delivery meta = %+v, want ForgeDelivered=true host=github.ibm.com", h)
	}

	// IDEMPOTENCE: the push stops permanently. Continuing to push would make
	// the forge un-editable on the spoke forever — the #2061 ACMM failure mode.
	if got := projectConfigForHiveID("vllmd03", "enricom-ibm", []string{"jackrabbit"}, "jackrabbit", 2, "", "https://github.ibm.com/api/v3"); got != nil {
		t.Errorf("delivered forge must never push again, got %+v", got)
	}
	if s.adoptSpokeForge("vllmd03", "github.ibm.com") {
		t.Error("re-adopting a delivered forge must be a no-op")
	}
}

// TestForgeSwitchBackToPublicIsDeliverable covers the direction that the
// existing GHE push could never handle.
//
// Everywhere else an empty github_api_url means "leave the spoke alone", and
// gheAPIURLForHost("github.com") is "" by definition — so a naive
// implementation can move a hive TO enterprise but never back. The public
// target is therefore delivered as an EXPLICIT api.github.com.
func TestForgeSwitchBackToPublicIsDeliverable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	saveSaaSHive(&SaaSHive{
		ID: "back", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 2,
		GitHubHost: "github.com", RequestedGitHubHost: "github.com",
		ForgeDelivered: false,
	})

	got := projectConfigForHiveID("back", "o", []string{"r"}, "r", 2, "", "https://github.ibm.com/api/v3")
	if got == nil {
		t.Fatal("switching back to public github.com produced no push — the hive would stay on the enterprise API forever")
	}
	if got.GitHubAPIURL != defaultPublicAPIURL {
		t.Errorf("pushed GitHubAPIURL = %q, want an explicit %q (empty would be ignored by the spoke)", got.GitHubAPIURL, defaultPublicAPIURL)
	}

	if !s.adoptSpokeForge("back", "github.com") {
		t.Fatal("spoke reported github.com but delivery did not complete")
	}
	if got := projectConfigForHiveID("back", "o", []string{"r"}, "r", 2, "", defaultPublicAPIURL); got != nil {
		t.Errorf("delivered public forge must never push again, got %+v", got)
	}
}

// TestForgeDeliveryIgnoresSilentSpoke locks "unknown is not a match".
//
// A spoke too old to report github_host sends "". Latching delivery on that
// would end the push with zero evidence the switch landed, leaving the hive on
// the old forge while the hub believes it moved — a silent failure, and the
// worst outcome available.
func TestForgeDeliveryIgnoresSilentSpoke(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	saveSaaSHive(&SaaSHive{
		ID: "old", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 2,
		GitHubHost: "github.ibm.com", RequestedGitHubHost: "github.ibm.com",
	})
	if s.adoptSpokeForge("old", "") {
		t.Error("an empty host report is UNKNOWN, not a delivery confirmation")
	}
	if h := loadSaaSHive("old"); h == nil || h.ForgeDelivered {
		t.Fatalf("silent spoke must not complete delivery, got %+v", h)
	}
}

// TestForgePushIsInertWithoutASwitch guarantees the feature is invisible to the
// ~every hive nobody has switched. Both new fields default to their zero values
// on every existing record, so the push must be gated on a NON-EMPTY request.
func TestForgePushIsInertWithoutASwitch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	// A perfectly ordinary delivered hive, as every record on the PVC looks today.
	saveSaaSHive(&SaaSHive{
		ID: "plain", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 3, RequestedACMMLevel: 3,
		ClaimDelivered: true, ACMMDelivered: true,
	})
	if got := projectConfigForHiveID("plain", "o", []string{"r"}, "r", 3, "", "https://api.github.com"); got != nil {
		t.Errorf("a hive with no forge switch must push nothing, got %+v", got)
	}
	if s.adoptSpokeForge("plain", "github.com") {
		t.Error("a hive with no forge switch must not record a delivery")
	}
	// And an unparseable stored target is never pushed rather than pushing junk.
	saveSaaSHive(&SaaSHive{
		ID: "junk", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 3, RequestedGitHubHost: "not a host",
	})
	if got := pendingForgeAPIURL(loadSaaSHive("junk"), ""); got != "" {
		t.Errorf("unparseable stored forge target must push nothing, got %q", got)
	}
}

// TestForgeIdentityIsAtomic is the regression test for a hive that was broken
// by hand, and it is the most important test in this file.
//
// A hive was moved to github.ibm.com by editing github_host alone. The host
// changed; the identity did not. It kept the github.com app_id (3568013 where
// GHE needs its own), an EMPTY app_slug, and an installation_id belonging to
// the old forge. The owner clicked "Install GitHub App" and got a GitHub 404,
// because an empty app_slug falls back to config.DefaultGitHubAppSlug
// ("kubestellar-hive") — the github.com App's slug, which names nothing on GHE.
//
// A wrong install link is worse than no link: it tells the user the product is
// broken. So a forge switch resolves the identity as a SET and refuses when any
// part of it is unknown, rather than half-applying.
func TestForgeIdentityIsAtomic(t *testing.T) {
	s := &HubServer{logger: slog.Default()}

	// A github.com cluster (hive-oke shape): no GHE URLs, its own app + slug.
	const publicClusterApp int64 = 111
	publicCluster := &ClusterConfig{
		ID: "hive-oke", GitHubAppID: publicClusterApp, GitHubAppSlug: "kubestellar-hive",
	}
	// An enterprise cluster (vllm-d shape): GHE base/api URLs, its own app+slug.
	const gheClusterApp int64 = 222
	gheCluster := &ClusterConfig{
		ID: "vllm-d", GitHubAppID: gheClusterApp,
		GitHubAppSlug: "kubestellar-hive-ghe",
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
	}

	public, err := parseForgeTarget("github.com")
	if err != nil {
		t.Fatal(err)
	}
	ghe, err := parseForgeTarget("github.ibm.com")
	if err != nil {
		t.Fatal(err)
	}

	// Fully configured clusters resolve a COMPLETE identity — id AND slug.
	got, missing := s.forgeIdentityForTarget(publicCluster, public)
	if len(missing) != 0 || got.AppID != publicClusterApp || got.AppSlug != "kubestellar-hive" {
		t.Errorf("github.com cluster: identity = %+v missing = %v, want the configured app id and slug", got, missing)
	}
	got, missing = s.forgeIdentityForTarget(gheCluster, ghe)
	if len(missing) != 0 || got.AppID != gheClusterApp || got.AppSlug != "kubestellar-hive-ghe" {
		t.Errorf("GHE cluster: identity = %+v missing = %v, want the configured GHE app id and slug", got, missing)
	}

	// THE LIVE BUG: a GHE cluster whose github_app_slug was never populated.
	// This must be REFUSED, not silently accepted with an empty slug that falls
	// back to the github.com default and 404s on the install link.
	slugless := &ClusterConfig{
		ID: "vllm-d", GitHubAppID: gheClusterApp,
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
	}
	got, missing = s.forgeIdentityForTarget(slugless, ghe)
	if len(missing) == 0 {
		t.Fatalf("a cluster with no github_app_slug must be refused — an empty slug 404s the install link on GHE; got identity %+v", got)
	}
	if got.AppSlug != "" || got.AppID != 0 {
		t.Errorf("a refused identity must be entirely empty (all-or-nothing), got %+v", got)
	}
	if !strings.Contains(strings.Join(missing, " "), "github_app_slug") {
		t.Errorf("the refusal must NAME the missing field so an operator can fix clusters.json, got %v", missing)
	}

	// Cross-forge: the cluster's App is registered on the OTHER forge, so it
	// names nothing on the target. Refused rather than pushed.
	if _, missing = s.forgeIdentityForTarget(publicCluster, ghe); len(missing) == 0 {
		t.Error("a github.com cluster must not supply an identity for a GHE target")
	}
	if _, missing = s.forgeIdentityForTarget(gheCluster, public); len(missing) == 0 {
		t.Error("a GHE cluster must not supply an identity for a github.com target")
	}
	// A cluster with no configured App, and a nil cluster, are both refusals.
	if _, missing = s.forgeIdentityForTarget(&ClusterConfig{ID: "bare"}, public); len(missing) == 0 {
		t.Error("a cluster with no github_app_id must be refused")
	}
	if _, missing = s.forgeIdentityForTarget(nil, public); len(missing) == 0 {
		t.Error("a nil cluster must be refused")
	}
}

// forgeTestSecret signs the auth cookie in the handler tests below.
const forgeTestSecret = "forge-test-secret"

// postForgeSwitch drives the real handler end to end as the given user.
func postForgeSwitch(t *testing.T, s *HubServer, hiveID, username, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/forge", strings.NewReader(body))
	req.SetPathValue("id", hiveID)
	if username != "" {
		req.AddCookie(&http.Cookie{
			Name:  "hive_hub_user",
			Value: mintHubUserCookieValueV2(deriveDomainKey(forgeTestSecret, infoSessionEd25519Seed), username),
		})
	}
	rec := httptest.NewRecorder()
	s.handleSwitchForge(rec, req)
	return rec
}

// TestSwitchForgeRefusesPartialIdentity drives the HANDLER through the live
// failure: a GHE cluster whose github_app_slug was never populated.
//
// The switch must write NOTHING. A half-applied switch is what produced a hive
// on github.ibm.com still carrying the github.com app_id and an empty slug —
// and an "Install GitHub App" link that 404s.
func TestSwitchForgeRefusesPartialIdentity(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "enricom8"}); err != nil {
		t.Fatal(err)
	}
	saveSaaSHive(&SaaSHive{
		ID: "vllmd03", Owner: "enricom8", Status: "running", ClusterID: "vllm-d",
		Org: "enricom-ibm", Repos: []string{"jackrabbit"}, PrimaryRepo: "jackrabbit",
		ACMMLevel: 2, GitHubHost: "github.com",
	})
	s := &HubServer{
		logger:    slog.Default(),
		hubSecret: forgeTestSecret,
		clusters: map[string]ClusterConfig{
			// The live vllm-d shape at the time of the incident: an app id, GHE
			// URLs, and NO github_app_slug.
			"vllm-d": {
				ID: "vllm-d", GitHubAppID: 5686,
				GitHubBaseURL: "https://github.ibm.com",
				GitHubAPIURL:  "https://github.ibm.com/api/v3",
			},
		},
		pendingGitHubAppConfigs: map[string]*HeartbeatGitHubAppConfig{},
	}

	rec := postForgeSwitch(t, s, "vllmd03", "enricom8", `{"host":"github.ibm.com"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — an incomplete target identity must refuse, not half-apply (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "github_app_slug") {
		t.Errorf("the refusal must name the missing field, got %q", rec.Body.String())
	}

	// NOTHING may have been written: not the host, not the pin, not the
	// delivery arming. This is the atomicity guarantee.
	h := loadSaaSHive("vllmd03")
	if h == nil {
		t.Fatal("hive vanished")
	}
	if h.GitHubHost != "github.com" {
		t.Errorf("github_host = %q, want the ORIGINAL github.com — a refused switch must not move the host", h.GitHubHost)
	}
	if h.RequestedGitHubHost != "" || h.ForgeDelivered {
		t.Errorf("a refused switch must not arm delivery, got requested=%q delivered=%v", h.RequestedGitHubHost, h.ForgeDelivered)
	}
	if h.GitHubBaseURL != "" || h.GitHubAPIURL != "" {
		t.Errorf("a refused switch must not pin URLs, got base=%q api=%q", h.GitHubBaseURL, h.GitHubAPIURL)
	}
	if got := s.consumePendingGitHubAppConfig("vllmd03"); got != nil {
		t.Errorf("a refused switch must queue no App delivery, got %+v", got)
	}
}

// TestSwitchForgeSwapsTheWholeIdentity is the success path, and it asserts that
// every field in the identity set moves together — including the app_slug whose
// absence caused the live 404.
func TestSwitchForgeSwapsTheWholeIdentity(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "enricom8"}); err != nil {
		t.Fatal(err)
	}
	saveSaaSHive(&SaaSHive{
		ID: "vllmd03", Owner: "enricom8", Status: "running", ClusterID: "vllm-d",
		Org: "enricom-ibm", Repos: []string{"jackrabbit"}, PrimaryRepo: "jackrabbit",
		ACMMLevel: 2, GitHubHost: "github.com",
	})
	const gheAppID int64 = 5686
	s := &HubServer{
		logger:    slog.Default(),
		hubSecret: forgeTestSecret,
		clusters: map[string]ClusterConfig{
			"vllm-d": {
				ID: "vllm-d", GitHubAppID: gheAppID,
				GitHubAppSlug: "kubestellar-hive-ghe",
				GitHubBaseURL: "https://github.ibm.com",
				GitHubAPIURL:  "https://github.ibm.com/api/v3",
			},
		},
		pendingGitHubAppConfigs: map[string]*HeartbeatGitHubAppConfig{},
	}

	rec := postForgeSwitch(t, s, "vllmd03", "enricom8", `{"host":"github.ibm.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	h := loadSaaSHive("vllmd03")
	if h == nil {
		t.Fatal("hive vanished")
	}
	if h.GitHubHost != "github.ibm.com" {
		t.Errorf("github_host = %q, want github.ibm.com", h.GitHubHost)
	}
	if h.GitHubBaseURL != "https://github.ibm.com" {
		t.Errorf("base_url = %q, want the GHE web base — the field left empty by the manual edit", h.GitHubBaseURL)
	}
	if h.GitHubAPIURL != "https://github.ibm.com/api/v3" {
		t.Errorf("api_url = %q, want the GHE API", h.GitHubAPIURL)
	}
	if h.RequestedGitHubHost != "github.ibm.com" || h.ForgeDelivered {
		t.Errorf("delivery must be armed and undelivered, got requested=%q delivered=%v", h.RequestedGitHubHost, h.ForgeDelivered)
	}

	// The App identity queued for the spoke must carry the GHE app id AND the
	// GHE slug, and must NOT carry an installation id from the old forge.
	//
	// The queue is now the DURABLE PendingAppConfig on the hive record rather
	// than an in-memory one-shot map, so a hub restart between arming and the
	// next beat no longer discards the switch. The assertions below are
	// unchanged — only where the identity is read from.
	got := h.PendingAppConfig
	if got == nil {
		t.Fatal("no App identity queued for the spoke — the hive would keep the github.com App")
	}
	if got.AppID != gheAppID {
		t.Errorf("queued app_id = %d, want the cluster's GHE app %d", got.AppID, gheAppID)
	}
	if got.AppSlug != "kubestellar-hive-ghe" {
		t.Errorf("queued app_slug = %q, want the GHE slug — an empty slug is what 404s the install link", got.AppSlug)
	}
	if got.InstallationID != 0 {
		t.Errorf("installation_id = %d, want 0 — an ID from the previous forge must NEVER be carried over", got.InstallationID)
	}
	// PendingAppIdentity has no PrivateKey field at all: key material is owned
	// by the per-cluster reconcile, which gates every push on HasKey(). The
	// endpoint cannot push a key even by mistake, which is a stronger guarantee
	// than the assertion this replaces.
	if got.APIURL != "https://github.ibm.com/api/v3" {
		t.Errorf("queued api_url = %q, want the GHE API — an identity queued without it is the half-applied state that 404s", got.APIURL)
	}

	// The response must tell the operator the install is still outstanding,
	// rather than reading as a completed success.
	if !strings.Contains(rec.Body.String(), "installation_id was cleared") {
		t.Errorf("response must flag the cleared installation_id, got %q", rec.Body.String())
	}

	// IDEMPOTENT and re-runnable: switching again to the same forge is safe.
	rec2 := postForgeSwitch(t, s, "vllmd03", "enricom8", `{"host":"github.ibm.com"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-running the same switch must succeed, got %d (%q)", rec2.Code, rec2.Body.String())
	}
}

// TestSwitchForgeAuthorization locks the owner-or-admin rule.
func TestSwitchForgeAuthorization(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	for _, u := range []string{"enricom8", "someoneelse", hubAdminUsername} {
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: u}); err != nil {
			t.Fatal(err)
		}
	}
	saveSaaSHive(&SaaSHive{
		ID: "h", Owner: "enricom8", Status: "running", ClusterID: "c",
		Org: "o", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 2,
	})
	s := &HubServer{
		logger:    slog.Default(),
		hubSecret: forgeTestSecret,
		clusters: map[string]ClusterConfig{
			"c": {ID: "c", GitHubAppID: 111, GitHubAppSlug: "kubestellar-hive"},
		},
		pendingGitHubAppConfigs: map[string]*HeartbeatGitHubAppConfig{},
	}

	if rec := postForgeSwitch(t, s, "h", "someoneelse", `{"host":"github.com"}`); rec.Code != http.StatusForbidden {
		t.Errorf("a non-owner must be refused, got %d", rec.Code)
	}
	if rec := postForgeSwitch(t, s, "h", hubAdminUsername, `{"host":"github.com"}`); rec.Code != http.StatusOK {
		t.Errorf("an admin must be allowed, got %d (%q)", rec.Code, rec.Body.String())
	}
	if rec := postForgeSwitch(t, s, "h", "enricom8", `{"host":"github.com"}`); rec.Code != http.StatusOK {
		t.Errorf("the owner must be allowed, got %d (%q)", rec.Code, rec.Body.String())
	}
	// A bad host is rejected before anything is written.
	if rec := postForgeSwitch(t, s, "h", "enricom8", `{"host":"not a host"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an invalid forge host must 400, got %d", rec.Code)
	}
}

// TestParseForgeTargetWithKind covers the GitLab/Gitea explicit-kind path added
// in anticipation of pkg/forge routing: a non-GitHub kind yields a bare-host
// BaseURL and an EMPTY APIURL, while the GitHub kinds behave exactly as the
// host-only parse. The empty APIURL is asserted as the hub-side contract; note
// that nothing completes it today, since no running code path constructs a
// pkg/forge adapter (see the note at the top of forge.go).
func TestParseForgeTargetWithKind(t *testing.T) {
	cases := []struct {
		name        string
		kind, host  string
		wantKind    ForgeKind
		wantHost    string
		wantBaseURL string
		wantAPIURL  string
		wantErr     bool
	}{
		{"gitlab saas", "gitlab", "gitlab.com", ForgeGitLab, "gitlab.com", "https://gitlab.com", "", false},
		{"gitlab self-managed from URL", "gitlab", "https://gitlab.example.com/group", ForgeGitLab, "gitlab.example.com", "https://gitlab.example.com", "", false},
		{"gitea", "gitea", "codeberg.org", ForgeGitea, "codeberg.org", "https://codeberg.org", "", false},
		{"gitlab requires a host", "gitlab", "", "", "", "", "", true},
		{"gitlab rejects a non-host", "gitlab", "not a host", "", "", "", "", true},
		{"empty kind → public github", "", "github.com", ForgeGitHub, "github.com", "", "", false},
		{"github-enterprise host still works", "", "github.ibm.com", ForgeGitHubEnterprise, "github.ibm.com", "https://github.ibm.com", "https://github.ibm.com/api/v3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseForgeTargetWithKind(tc.kind, tc.host)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseForgeTargetWithKind(%q,%q) = %+v, want error", tc.kind, tc.host, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != tc.wantKind || got.Host != tc.wantHost || got.BaseURL != tc.wantBaseURL || got.APIURL != tc.wantAPIURL {
				t.Fatalf("got %+v, want kind=%s host=%s base=%s api=%s", got, tc.wantKind, tc.wantHost, tc.wantBaseURL, tc.wantAPIURL)
			}
		})
	}
}
