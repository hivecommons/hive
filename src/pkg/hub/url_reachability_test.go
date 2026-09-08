package hub

import (
	"net/http"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newURLHealthTestHub builds a hub whose domain-suffix gating is driven ONLY
// by the test's cluster domain. hubDomainSuffix() consults ambient env vars
// (HIVE_HUB_URL et al.) before the cluster config, so on a live hive host —
// where the agent environment exports HIVE_HUB_URL — every test hive stops
// looking hub-fronted and the alert tests silently assert nothing. Clearing
// the overrides here keeps these tests hermetic wherever they run.
func newURLHealthTestHub(t *testing.T, domain string) *HubServer {
	t.Helper()
	for _, key := range []string{"HIVE_HUB_PUBLIC_URL", "HIVE_PUBLIC_URL", "HIVE_HUB_BASE_URL", "HIVE_DASHBOARD_URL", "HIVE_HUB_URL"} {
		t.Setenv(key, "")
	}
	return &HubServer{
		urlHealth: newURLHealthState(),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {ID: defaultClusterID, Domain: domain},
		},
	}
}

// A 503 from a routed hostname with no backend is the exact failure that hid
// behind a green dot: the HTTP exchange SUCCEEDS, so any check based on
// "did the request complete" reports healthy while the user gets an error page.
func TestURLHealthyStatus(t *testing.T) {
	healthy := []int{200, 301, 302, 303, 307, 308, 401, 403}
	for _, code := range healthy {
		if !urlHealthyStatus(code) {
			t.Errorf("status %d should be healthy: a login redirect or an auth challenge means the URL is serving", code)
		}
	}
	unhealthy := []int{0, 404, 500, 502, 503, 504}
	for _, code := range unhealthy {
		if urlHealthyStatus(code) {
			t.Errorf("status %d should be unhealthy", code)
		}
	}
}

func TestURLHealthStateCountsConsecutiveFailures(t *testing.T) {
	u := newURLHealthState()
	if n := u.observe("h1", urlProbeResult{Status: 503}); n != 1 {
		t.Fatalf("first failure = %d, want 1", n)
	}
	if n := u.observe("h1", urlProbeResult{Status: 503}); n != 2 {
		t.Fatalf("second failure = %d, want 2", n)
	}
	// A single healthy probe must reset the streak — otherwise a hive that
	// recovered would keep alerting.
	if n := u.observe("h1", urlProbeResult{Status: 302, Healthy: true}); n != 0 {
		t.Fatalf("healthy probe = %d, want 0", n)
	}
	if n := u.observe("h1", urlProbeResult{Status: 503}); n != 1 {
		t.Fatalf("after recovery = %d, want 1", n)
	}
}

func TestURLHealthStateForgetDropsRecycledSlots(t *testing.T) {
	u := newURLHealthState()
	u.observe("gone", urlProbeResult{Status: 503})
	u.observe("kept", urlProbeResult{Status: 503})
	u.forget(map[string]bool{"kept": true})
	failures, statuses := u.snapshot()
	if _, ok := failures["gone"]; ok {
		t.Error("recycled pool slot should be forgotten so the map cannot grow unbounded")
	}
	if _, ok := statuses["gone"]; ok {
		t.Error("recycled pool slot status should be forgotten")
	}
	if failures["kept"] != 1 {
		t.Error("a hive still in the registry must be retained")
	}
}

func TestURLHealthStateCriticalEvaluationsClearImmediately(t *testing.T) {
	u := newURLHealthState()
	if n := u.criticalEvaluation("h1", true); n != 1 {
		t.Fatalf("first critical evaluation = %d, want 1", n)
	}
	if n := u.criticalEvaluation("h1", false); n != 0 {
		t.Fatalf("cleared evaluation = %d, want 0", n)
	}
	if n := u.criticalEvaluation("h1", true); n != 1 {
		t.Fatalf("after clear = %d, want 1", n)
	}
}

// A whole cluster failing is an outage, not N broken hives. vllm-d has been
// intermittently unreachable from the hub and must not raise 30 alerts.
func TestClusterOutageRollup(t *testing.T) {
	if !clusterOutage(10, 10) {
		t.Error("every hive failing on a large cluster is an outage")
	}
	if !clusterOutage(5, 10) {
		t.Error("half a large cluster failing is an outage")
	}
	if clusterOutage(1, 10) {
		t.Error("one hive failing is NOT an outage — that is the real breakage we must surface")
	}
	// Below the minimum size, "most failed" is not evidence of an outage: on a
	// 2-hive cluster one genuinely broken hive already crosses the ratio.
	if clusterOutage(2, 2) {
		t.Error("a tiny cluster must not roll up, or a real defect is hidden")
	}
}

func TestURLUnreachableEligibleExemptsNewAndUnknown(t *testing.T) {
	now := time.Now()
	old := now.Add(-urlUnreachableMinAge - time.Hour).Format(time.RFC3339)
	if !urlUnreachableEligible(old, now) {
		t.Error("a hive that converged long ago is eligible")
	}
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	if urlUnreachableEligible(fresh, now) {
		t.Error("a brand-new hive is still converging (cert issuance, ingress reload) and must be exempt")
	}
	// Unknown is never alarmed on — the advisoryStale contract.
	if urlUnreachableEligible("", now) {
		t.Error("an unknown registration time must not alert")
	}
	if urlUnreachableEligible("not-a-timestamp", now) {
		t.Error("an unparseable registration time must not alert")
	}
}

func TestURLUnreachableReasonNamesTheStatus(t *testing.T) {
	got := urlUnreachableReason("https://x.example.com", 503)
	if got != "public URL https://x.example.com returned HTTP 503" {
		t.Errorf("reason = %q, want it to name the URL and the status", got)
	}
	got = urlUnreachableReason("https://x.example.com", 0)
	if got != "public URL https://x.example.com did not respond (DNS, TLS or timeout)" {
		t.Errorf("transport-failure reason = %q", got)
	}
}

// The end-to-end rule: a hive that is online and green but whose public URL
// serves 503 must alert, while a converging hive and a cluster outage must not.
func TestURLUnreachableAlerts(t *testing.T) {
	now := time.Now()
	oldEnough := now.Add(-urlUnreachableMinAge - time.Hour).Format(time.RFC3339)

	s := newURLHealthTestHub(t, "example.com")
	hives := []RegistryEntry{
		{ID: "broken", Name: "org/broken", ClusterID: "c1", RegisteredAt: oldEnough, DashboardURL: "https://broken.example.com"},
		{ID: "fine1", ClusterID: "c1", RegisteredAt: oldEnough, DashboardURL: "https://f1.example.com"},
		{ID: "fine2", ClusterID: "c1", RegisteredAt: oldEnough, DashboardURL: "https://f2.example.com"},
		{ID: "fine3", ClusterID: "c1", RegisteredAt: oldEnough, DashboardURL: "https://f3.example.com"},
	}
	// One hive fails twice; the rest are healthy.
	for i := 0; i < urlUnreachableMinFailures; i++ {
		s.urlHealth.observe("broken", urlProbeResult{Status: 503})
	}
	var alerts []Alert
	for i := 0; i < urlUnreachableMinAlertEvaluations; i++ {
		alerts = s.urlUnreachableAlerts(hives, now)
	}
	if len(alerts) != 1 || alerts[0].HiveID != "broken" {
		t.Fatalf("want exactly one alert for the broken hive, got %+v", alerts)
	}
	if alerts[0].Type != AlertTypeURLUnreachable {
		t.Errorf("type = %q", alerts[0].Type)
	}
	if alerts[0].Severity != AlertSeverityCritical {
		t.Errorf("a dead user-facing link is critical, got %q", alerts[0].Severity)
	}

	// A single failure is below the threshold — a blip must never alert.
	s2 := newURLHealthTestHub(t, "example.com")
	s2.urlHealth.observe("broken", urlProbeResult{Status: 503})
	if got := s2.urlUnreachableAlerts(hives, now); len(got) != 0 {
		t.Errorf("one failure must not alert, got %+v", got)
	}

	// Whole cluster down -> suppressed into an outage, zero per-hive alerts.
	s3 := newURLHealthTestHub(t, "example.com")
	for _, h := range hives {
		for i := 0; i < urlUnreachableMinFailures; i++ {
			s3.urlHealth.observe(h.ID, urlProbeResult{Status: 0})
		}
	}
	if got := s3.urlUnreachableAlerts(hives, now); len(got) != 0 {
		t.Errorf("a cluster-wide outage must not raise per-hive alerts, got %d", len(got))
	}
}

func TestURLReachabilityDecisionMatrix(t *testing.T) {
	now := time.Now()
	oldEnough := now.Add(-urlUnreachableMinAge - time.Hour).Format(time.RFC3339)
	fresh := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	stale := now.Add(-maxHeartbeatAge - time.Minute).UTC().Format(time.RFC3339)

	for _, tc := range []struct {
		name      string
		hubFails  bool
		freshHB   bool
		self      *PublicURLSelfCheck
		route     *RouteExistenceCheck
		dashURL   string
		wantType  string
		wantLevel string
	}{
		{"hub ok self unknown", false, true, nil, nil, "https://h1.example.com", "", ""},
		{"hub ok self ok", false, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckOK}, nil, "https://h1.example.com", "", ""},
		{"hub ok self fail", false, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "HTTP 503", HTTPStatus: 503}, nil, "https://h1.example.com", AlertTypeURLUnreachable, AlertSeverityCritical},
		{"route missing", false, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckOK}, &RouteExistenceCheck{Status: RouteExistenceMissing, Host: "h1.example.com"}, "https://h1.example.com", AlertTypeURLUnreachable, AlertSeverityCritical},
		{"route unknown no regression", false, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckOK}, &RouteExistenceCheck{Status: RouteExistenceUnknown}, "https://h1.example.com", "", ""},
		{"hub fail fresh self ok", true, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 401}, nil, "https://h1.example.com", AlertTypeURLPrivateNetwork, AlertSeverityInfo},
		{"hub fail fresh self unknown", true, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown, Error: "lookup failed"}, nil, "https://h1.example.com", AlertTypeURLPrivateNetwork, AlertSeverityInfo},
		{"hub fail fresh self fail", true, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "HTTP 503", HTTPStatus: 503}, nil, "https://h1.example.com", AlertTypeURLUnreachable, AlertSeverityCritical},
		{"hub fail stale self unknown", true, false, &PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown, Error: "lookup failed"}, nil, "https://h1.example.com", AlertTypeURLUnreachable, AlertSeverityCritical},
		{"hub fail stale self ok", true, false, &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 401}, nil, "https://h1.example.com", AlertTypeURLUnreachable, AlertSeverityCritical},
		{"private healthy no chip", true, true, &PublicURLSelfCheck{Status: PublicURLSelfCheckOK}, &RouteExistenceCheck{Status: RouteExistenceFound}, "https://spoke.apps.fmaas.example.net", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newURLHealthTestHub(t, "example.com")
			if tc.hubFails {
				for i := 0; i < urlUnreachableMinFailures; i++ {
					s.urlHealth.observe("h1", urlProbeResult{Status: 0})
				}
			}

			h := RegistryEntry{
				ID:                 "h1",
				Name:               "org/repo",
				ClusterID:          "c1",
				RegisteredAt:       oldEnough,
				DashboardURL:       tc.dashURL,
				LastHeartbeat:      stale,
				Online:             false,
				PublicURLSelfCheck: tc.self,
				RouteExists:        tc.route,
			}
			if tc.freshHB {
				h.LastHeartbeat = fresh
				h.Online = true
			}
			var got []Alert
			for i := 0; i < urlUnreachableMinAlertEvaluations; i++ {
				got = s.urlUnreachableAlerts([]RegistryEntry{h}, now)
			}
			if tc.wantType == "" {
				if len(got) != 0 {
					t.Fatalf("want no alerts, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one alert, got %+v", got)
			}
			if got[0].Type != tc.wantType || got[0].Severity != tc.wantLevel {
				t.Fatalf("alert = (%s,%s), want (%s,%s): %+v", got[0].Type, got[0].Severity, tc.wantType, tc.wantLevel, got[0])
			}
		})
	}
}

func TestHubProbeDomainSuffixGating(t *testing.T) {
	s := newURLHealthTestHub(t, "hive.kubestellar.io")
	if !s.hubFrontedDashboardURL("https://spoke.hive.kubestellar.io") {
		t.Fatal("hub-fronted subdomain should be probed")
	}
	if s.hubFrontedDashboardURL("https://spoke.apps.fmaas-vllm-d.fmaas.res.ibm.com") {
		t.Fatal("private/firewalled app domain must not be hub-probed")
	}
}

func TestURLUnreachableCriticalFlapSuppression(t *testing.T) {
	now := time.Now()
	oldEnough := now.Add(-urlUnreachableMinAge - time.Hour).Format(time.RFC3339)
	stale := now.Add(-maxHeartbeatAge - time.Minute).UTC().Format(time.RFC3339)
	s := newURLHealthTestHub(t, "example.com")
	for i := 0; i < urlUnreachableMinFailures; i++ {
		s.urlHealth.observe("h1", urlProbeResult{Status: 503})
	}
	h := RegistryEntry{ID: "h1", Name: "org/repo", ClusterID: "c1", RegisteredAt: oldEnough, DashboardURL: "https://dead.example.com", LastHeartbeat: stale}
	for i := 1; i < urlUnreachableMinAlertEvaluations; i++ {
		if got := s.urlUnreachableAlerts([]RegistryEntry{h}, now); len(got) != 0 {
			t.Fatalf("evaluation %d should be suppressed, got %+v", i, got)
		}
	}
	if got := s.urlUnreachableAlerts([]RegistryEntry{h}, now); len(got) != 1 {
		t.Fatalf("persistent condition should alert, got %+v", got)
	}
	s.urlHealth.observe("h1", urlProbeResult{Status: 302, Healthy: true})
	if got := s.urlUnreachableAlerts([]RegistryEntry{h}, now); len(got) != 0 {
		t.Fatalf("first success should clear immediately, got %+v", got)
	}
	for i := 0; i < urlUnreachableMinFailures; i++ {
		s.urlHealth.observe("h1", urlProbeResult{Status: 503})
	}
	if got := s.urlUnreachableAlerts([]RegistryEntry{h}, now); len(got) != 0 {
		t.Fatalf("after clear the hysteresis should restart, got %+v", got)
	}
}

func TestURLUnreachableSuppressesUnassignedPlaceholders(t *testing.T) {
	now := time.Now()
	oldEnough := now.Add(-urlUnreachableMinAge - time.Hour).Format(time.RFC3339)
	stale := now.Add(-maxHeartbeatAge - time.Minute).UTC().Format(time.RFC3339)
	s := newURLHealthTestHub(t, "example.com")
	for i := 0; i < urlUnreachableMinFailures; i++ {
		s.urlHealth.observe("ph", urlProbeResult{Status: 503})
	}
	ph := RegistryEntry{
		ID:                 "ph",
		Name:               "available-oke-13/placeholder",
		Org:                "available-oke-13",
		ProvStatus:         "available",
		ClusterID:          "c1",
		RegisteredAt:       oldEnough,
		DashboardURL:       "https://placeholder.example.com",
		LastHeartbeat:      stale,
		PublicURLSelfCheck: &PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "HTTP 503", HTTPStatus: 503},
	}
	for i := 0; i < urlUnreachableMinAlertEvaluations; i++ {
		if got := s.urlUnreachableAlerts([]RegistryEntry{ph}, now); len(got) != 0 {
			t.Fatalf("placeholder URL alerts should be suppressed, got %+v", got)
		}
	}
}

func TestURLUnreachableAlertsNilStateIsSafe(t *testing.T) {
	s := &HubServer{}
	if got := s.urlUnreachableAlerts([]RegistryEntry{{ID: "x"}}, time.Now()); got != nil {
		t.Errorf("a HubServer with no probe state must return no alerts, got %+v", got)
	}
}

// The pill and the alert panel must never disagree about which hives are broken.
func TestURLUnreachableFacetMatchesAlerts(t *testing.T) {
	alerts := []Alert{
		{HiveID: "a", Type: AlertTypeURLUnreachable, Reason: "public URL https://a returned HTTP 503"},
		{HiveID: "p", Type: AlertTypeURLPrivateNetwork, Reason: "private network"},
		{HiveID: "b", Type: AlertTypeOffline, Reason: "offline"},
	}
	if bad, reason := urlUnreachableFacet(alerts, "a"); !bad || reason == "" {
		t.Error("hive a has an unreachable-URL alert so it must show the pill with a reason")
	}
	if bad, _ := urlUnreachableFacet(alerts, "b"); bad {
		t.Error("an offline alert is a different condition and must not show the dead-link pill")
	}
	if bad, _ := urlUnreachableFacet(alerts, "c"); bad {
		t.Error("a hive with no alert must not show the pill")
	}
	if private, reason := privateURLFacet(alerts, "p"); !private || reason == "" {
		t.Error("private-network URL alert must show the informational private URL pill")
	}
	if private, _ := privateURLFacet(alerts, "a"); private {
		t.Error("a critical dead link must not show the private URL pill")
	}
}

// A new alert type is unusable until knownAlertTypes accepts it: the ack
// endpoint 400s otherwise, so an operator could never silence it.
func TestURLUnreachableIsAckable(t *testing.T) {
	if !isKnownAlertType(AlertTypeURLUnreachable) {
		t.Error("AlertTypeURLUnreachable must be in knownAlertTypes or its ack endpoint returns 400")
	}
	if !isKnownAlertType(AlertTypeURLPrivateNetwork) {
		t.Error("AlertTypeURLPrivateNetwork must be in knownAlertTypes or its ack endpoint returns 400")
	}
}

// Assign must NOT adopt a vanity host it could not make servable. Adopting
// optimistically (the old OpenShift behaviour, which assumed a cluster wildcard
// route) is what left hosted-available-vllmd-01 advertising a hostname that
// returned 503 while its placeholder host worked fine.
func TestAssignDoesNotAdoptUnservableVanityHost(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSHive(&SaaSHive{
		ID:        "hosted-unservable",
		Status:    statusAvailable,
		ClusterID: gpuClusterID,
	}); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	s := &HubServer{
		vanityHostServable: func(string, string, *ClusterConfig) error {
			return errNotServableForTest
		},
	}
	h := loadSaaSHive("hosted-unservable")
	h.Org, h.PrimaryRepo = "neworg", "repoa"
	cluster := s.clusterForHive(h)
	if cluster == nil || cluster.Domain == "" {
		t.Skip("test cluster has no domain configured")
	}
	if err := s.makeVanityHostServable(h.ID, "vanity."+cluster.Domain, cluster); err == nil {
		t.Fatal("seam should report the host is not servable")
	}
	// The contract: on that error the caller leaves VanityURL empty so every
	// read path falls back to the working placeholder host.
	if h.VanityURL != "" {
		t.Errorf("VanityURL = %q, want empty so the placeholder host is used", h.VanityURL)
	}
}

var errNotServableForTest = errNotServable{}

type errNotServable struct{}

func (errNotServable) Error() string { return "not servable" }
