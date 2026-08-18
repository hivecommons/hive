package hub

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- saveAlertAcks error branches ----

// A nil server or nil alert state must be a no-op, not a panic: saveAlertAcks
// runs from a background loop that can outlive a partially-built server.
func TestSaveAlertAcksToleratesNilState(t *testing.T) {
	var nilServer *HubServer
	nilServer.saveAlertAcks() // must not panic

	s := &HubServer{}
	s.saveAlertAcks() // alerts == nil, must not panic
}

// When the acks file cannot be written the failure is logged and swallowed —
// losing an acknowledgement must never take the hub down.
func TestSaveAlertAcksSurvivesUnwritablePath(t *testing.T) {
	s := newTestAlertServer()

	old := alertAcksPath
	// A path whose parent is a FILE, so both MkdirAll and WriteFile fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	alertAcksPath = filepath.Join(blocker, "acks.json")
	t.Cleanup(func() { alertAcksPath = old })

	s.saveAlertAcks() // must not panic and must not leave a temp file behind
	if _, err := os.Stat(alertAcksPath); err == nil {
		t.Fatal("no acks file should exist at an unwritable path")
	}
}

// A successful save must be atomic: the temp file must not survive.
func TestSaveAlertAcksIsAtomic(t *testing.T) {
	s := newTestAlertServer()

	old := alertAcksPath
	dir := t.TempDir()
	alertAcksPath = filepath.Join(dir, "nested", "acks.json")
	t.Cleanup(func() { alertAcksPath = old })

	s.saveAlertAcks()

	if _, err := os.Stat(alertAcksPath); err != nil {
		t.Fatalf("acks file was not written: %v", err)
	}
	if _, err := os.Stat(alertAcksPath + ".tmp"); err == nil {
		t.Fatal("the temp file must be renamed away, not left behind")
	}
}

// ---- journeyBannerFor guards ----

// A nil server or a server with no journey state must return nil rather than
// panicking — the dashboard calls this on every poll.
func TestJourneyBannerForToleratesNilState(t *testing.T) {
	var nilServer *HubServer
	if b := nilServer.journeyBannerFor("hosted-x", time.Now()); b != nil {
		t.Fatal("a nil server must yield no banner")
	}

	s := &HubServer{}
	if b := s.journeyBannerFor("hosted-x", time.Now()); b != nil {
		t.Fatal("a server with no journey state must yield no banner")
	}
}

// An unknown hive has no registry entry and therefore no journey banner.
func TestJourneyBannerForUnknownHive(t *testing.T) {
	s := bulkTestHub(t)
	if b := s.journeyBannerFor("no-such-hive", time.Now()); b != nil {
		t.Fatalf("an unknown hive must yield no banner, got %+v", b)
	}
}

// ---- recentlyStarted ----

// A pod that started moments ago is "recently started", which suppresses a
// crash-loop alert while it is still coming up.
func TestRecentlyStartedWindow(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		startedAt string
		want      bool
	}{
		{"just started", now.Add(-time.Second).Format(time.RFC3339), true},
		{"well past the ceiling", now.Add(-24 * time.Hour).Format(time.RFC3339), false},
		{"unparseable", "not-a-timestamp", false},
		{"empty", "", false},
		// A clock skew that puts the start in the FUTURE must not be treated as
		// "recently started" — a negative age is nonsense, not a fresh pod.
		{"future start (clock skew)", now.Add(time.Hour).Format(time.RFC3339), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recentlyStarted(tc.startedAt, now); got != tc.want {
				t.Fatalf("recentlyStarted(%q) = %v, want %v", tc.startedAt, got, tc.want)
			}
		})
	}
}

// Surrounding whitespace from a kubectl field must not defeat parsing.
func TestRecentlyStartedTrimsWhitespace(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	stamp := "  " + now.Add(-time.Second).Format(time.RFC3339) + "\n"
	if !recentlyStarted(stamp, now) {
		t.Fatal("a padded timestamp must still parse")
	}
}

// ---- existingVanityHost ----

// No cluster config means no host to discover.
func TestExistingVanityHostWithoutCluster(t *testing.T) {
	s := bulkTestHub(t)
	if got := s.existingVanityHost("hosted-x", nil); got != "" {
		t.Fatalf("want empty host, got %q", got)
	}
}

// For nginx the vanity host is the ingress rule that is NOT the placeholder —
// returning the placeholder would point the owner at the wrong URL.
func TestExistingVanityHostSkipsPlaceholderRule(t *testing.T) {
	dir := t.TempDir()
	// jsonpath returns both rule hosts space-separated.
	script := "#!/bin/sh\necho 'hosted-x.example.com vanity.acme.dev'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := bulkTestHub(t)
	cluster := &ClusterConfig{ID: "c1", Domain: "example.com"}
	got := s.existingVanityHost("hosted-x", cluster)
	if got != "vanity.acme.dev" {
		t.Fatalf("want the non-placeholder host, got %q", got)
	}
}

// When only the placeholder rule exists there is no vanity host yet.
func TestExistingVanityHostReturnsEmptyWhenOnlyPlaceholder(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'hosted-x.example.com'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := bulkTestHub(t)
	cluster := &ClusterConfig{ID: "c1", Domain: "example.com"}
	if got := s.existingVanityHost("hosted-x", cluster); got != "" {
		t.Fatalf("only the placeholder exists, want empty, got %q", got)
	}
}

// A kubectl failure must yield "" rather than a partial or garbage host.
func TestExistingVanityHostOnKubectlFailure(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'error' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := bulkTestHub(t)
	cluster := &ClusterConfig{ID: "c1", Domain: "example.com"}
	if got := s.existingVanityHost("hosted-x", cluster); got != "" {
		t.Fatalf("a kubectl failure must yield an empty host, got %q", got)
	}
}

// OpenShift clusters read the Route's host instead of an Ingress rule.
func TestExistingVanityHostOpenShiftRoute(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'vanity.apps.openshift.example'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := bulkTestHub(t)
	cluster := &ClusterConfig{ID: "c1", Domain: "example.com", IngressType: ingressTypeOpenShiftRoute}
	got := s.existingVanityHost("hosted-x", cluster)
	if got != "vanity.apps.openshift.example" {
		t.Fatalf("want the Route host, got %q", got)
	}
}

// ---- saveHubBanners ----

// A successful save is atomic and creates missing parent directories.
func TestSaveHubBannersWritesAtomically(t *testing.T) {
	s := bulkTestHub(t)
	s.hubBannersMu.Lock()
	s.hubBanners = map[string]*HubBannerEntry{
		"hosted-x": {ID: "hosted-x", Message: "hello", Color: "blue", SentAt: "2026-07-31T00:00:00Z"},
	}
	s.hubBannersMu.Unlock()

	old := hubBannersPath
	hubBannersPath = filepath.Join(t.TempDir(), "nested", "banners.json")
	t.Cleanup(func() { hubBannersPath = old })

	s.saveHubBanners()

	data, err := os.ReadFile(hubBannersPath)
	if err != nil {
		t.Fatalf("banners were not persisted: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Fatalf("banner content missing: %s", data)
	}
	if _, err := os.Stat(hubBannersPath + ".tmp"); err == nil {
		t.Fatal("the temp file must be renamed away, not left behind")
	}
}

// An unwritable destination must be logged and swallowed: failing to persist a
// banner must never take the hub down.
func TestSaveHubBannersSurvivesUnwritablePath(t *testing.T) {
	s := bulkTestHub(t)

	old := hubBannersPath
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	hubBannersPath = filepath.Join(blocker, "banners.json")
	t.Cleanup(func() { hubBannersPath = old })

	s.saveHubBanners() // must not panic
	if _, err := os.Stat(hubBannersPath); err == nil {
		t.Fatal("no banners file should exist at an unwritable path")
	}
}

// ---- clusterOutage ----

// A cluster-wide outage suppresses per-hive alerts; below the minimum fleet
// size or the failure ratio, individual hives are still worth alerting on.
func TestClusterOutageThresholds(t *testing.T) {
	cases := []struct {
		name           string
		failed, probed int
		want           bool
	}{
		{"too few hives to judge", 1, 1, false},
		{"all of a large fleet failed", 10, 10, true},
		{"a single hive in a large fleet", 1, 10, false},
		{"nothing probed", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterOutage(tc.failed, tc.probed); got != tc.want {
				t.Fatalf("clusterOutage(%d,%d) = %v, want %v",
					tc.failed, tc.probed, got, tc.want)
			}
		})
	}
}

// The minimum-hives guard must come first: a 1/1 failure is 100% but is far too
// small a sample to call a cluster outage, and suppressing on it would hide a
// genuine single-hive failure.
func TestClusterOutageRequiresMinimumSample(t *testing.T) {
	if clusterOutage(urlClusterOutageMinHives-1, urlClusterOutageMinHives-1) {
		t.Fatal("a fleet below the minimum sample must never be called an outage")
	}
	if !clusterOutage(urlClusterOutageMinHives, urlClusterOutageMinHives) {
		t.Fatal("a fully-failed fleet at the minimum sample size is an outage")
	}
}

// ---- CORS preflight and trusted origins ----

// The bulk endpoint must answer a CORS preflight without doing any work, and
// must echo the origin ONLY for trusted hosts — reflecting an arbitrary origin
// with credentials allowed would let any site drive the fleet API.
func TestBulkHandlerPreflightOnlyTrustsKnownOrigins(t *testing.T) {
	s := bulkTestHub(t)

	trusted := []string{
		"https://hive.kubestellar.io",
		"http://localhost:5174",
		"http://127.0.0.1:8080",
	}
	for _, origin := range trusted {
		t.Run("trusted "+origin, func(t *testing.T) {
			req := httptest.NewRequest("OPTIONS", "/api/saas/hives/bulk", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			s.handleBulkHiveAction(w, req)

			if w.Code != http.StatusNoContent {
				t.Fatalf("preflight code = %d, want 204", w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Fatalf("trusted origin must be echoed, got %q", got)
			}
			if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Fatalf("credentials header = %q", got)
			}
		})
	}

	untrusted := []string{
		"https://evil.example.com",
		"https://hive.kubestellar.io.evil.com",
		"https://notlocalhost",
		"::not a url::",
		// Audit F4: sibling tenants are untrusted for credentialed CORS. The
		// browser hands them the parent-domain session cookie, so reflecting
		// Allow-Origin + Allow-Credentials back to one gives a hostile hive
		// operator a scripted cross-tenant read of the hub API.
		"https://console.hive.kubestellar.io",
		"https://attacker-hive.hive.kubestellar.io",
	}
	for _, origin := range untrusted {
		t.Run("untrusted "+origin, func(t *testing.T) {
			req := httptest.NewRequest("OPTIONS", "/api/saas/hives/bulk", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			s.handleBulkHiveAction(w, req)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("untrusted origin %q must NOT be echoed, got %q", origin, got)
			}
		})
	}
}

// ---- handleLogin redirect safety ----

// The `redirect` parameter must never send a user to an attacker-controlled
// host after login. Anything that is not a same-site path is rewritten to
// /dashboard unless it is an explicitly trusted origin.
func TestHandleLoginRejectsLookalikeRedirectHosts(t *testing.T) {
	s := bulkTestHub(t)

	hostile := []string{
		"https://evil.example.com/steal",
		"//evil.example.com",
		"//hive.kubestellar.io.evil.com",
		"https://hive.kubestellar.io.evil.com/x",
	}
	for _, rd := range hostile {
		t.Run(rd, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/login?redirect="+url.QueryEscape(rd), nil)
			w := httptest.NewRecorder()
			s.handleLogin(w, req)

			loc := w.Header().Get("Location")
			// The hostile target must not survive into the OAuth state.
			if strings.Contains(loc, "evil.example.com") {
				t.Fatalf("open redirect: hostile host reached Location %q", loc)
			}
			if strings.Contains(loc, "evil.com") {
				t.Fatalf("open redirect: lookalike host reached Location %q", loc)
			}
		})
	}
}

// A same-site path is preserved, so a deep link still lands where the user
// expected after signing in.
func TestHandleLoginKeepsDeepLinkPath(t *testing.T) {
	s := bulkTestHub(t)
	req := httptest.NewRequest("GET", "/login?redirect=%2Fdashboard%2Fhives", nil)
	w := httptest.NewRecorder()
	s.handleLogin(w, req)

	loc := w.Header().Get("Location")
	if !strings.Contains(loc, url.QueryEscape("/dashboard/hives")) &&
		!strings.Contains(loc, "%2Fdashboard%2Fhives") {
		t.Fatalf("same-site redirect was not preserved in %q", loc)
	}
}

// `rd` is accepted as an alias for `redirect`, and must get the same treatment.
func TestHandleLoginRdAliasGuardsLookalikeHost(t *testing.T) {
	s := bulkTestHub(t)
	req := httptest.NewRequest("GET", "/login?rd="+url.QueryEscape("https://evil.example.com"), nil)
	w := httptest.NewRecorder()
	s.handleLogin(w, req)

	if loc := w.Header().Get("Location"); strings.Contains(loc, "evil.example.com") {
		t.Fatalf("the rd alias bypassed the redirect guard: %q", loc)
	}
}

// ---- handleOGCard ----

// The Open Graph card is served with an image content type and a cache header,
// so link previews render and do not re-fetch on every unfurl.
func TestHandleOGCardServesImageWithCache(t *testing.T) {
	s := bulkTestHub(t)
	req := httptest.NewRequest("GET", "/og-card.png", nil)
	w := httptest.NewRecorder()
	s.handleOGCard(w, req)

	if w.Code == http.StatusNotFound {
		t.Skip("og card asset not embedded in this build")
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q, want image/png", ct)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Fatalf("cache control = %q, want a max-age", cc)
	}
	if w.Body.Len() == 0 {
		t.Fatal("og card body is empty")
	}
}
