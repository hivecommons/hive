package hub

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// vanityRepairTestDomain is the cluster domain the repair derives vanity hosts
// from in these tests, mirroring the live hive-oke cluster.
const vanityRepairTestDomain = "hive.kubestellar.io"

// newVanityRepairTestHub builds a hub with one nginx cluster that has a domain
// (so a vanity host can be derived) and a servability seam that succeeds,
// standing in for a reachable cluster whose ingress the hub can patch.
func newVanityRepairTestHub() *HubServer {
	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {
				ID:          defaultClusterID,
				Name:        "oke",
				Domain:      vanityRepairTestDomain,
				IngressType: "nginx",
			},
		},
	}
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error { return nil }
	return s
}

// A hive claimed BEFORE the vanity feature existed (real org, placeholder id,
// empty vanity_url) must get a vanity URL — and its id must NOT change, since
// the id is a k8s namespace suffix, an ingress host, a socket path and the SSO
// token's "h" claim.
func TestVanityRepairBackfillsClaimedHiveWithoutChangingID(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	const id = "hosted-available-oke-07-placeholder-wlj4"
	h := &SaaSHive{
		ID: id, Status: "running", Owner: "MattSweetIBM", Org: "MattSweetIBM",
		Repos: []string{"s1netops"}, PrimaryRepo: "s1netops", ACMMLevel: 3,
		ClusterID: defaultClusterID, ClaimDelivered: true,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// Before the repair every read path falls back to the placeholder host.
	if got := claimedVanityURL(loadSaaSHive(id)); got != "" {
		t.Fatalf("precondition: expected no vanity URL, got %q", got)
	}

	if !s.repairVanityURLForHive(id) {
		t.Fatal("repair reported no change for a pre-vanity claimed hive")
	}

	got := loadSaaSHive(id)
	if got.ID != id {
		t.Errorf("repair changed the hive id: %q, want %q (ids are namespaces/SSO claims)", got.ID, id)
	}
	if got.VanityURL == "" {
		t.Fatal("repair left the vanity URL empty")
	}
	// Derived from the claimed PROJECT, on the cluster domain — not the placeholder.
	if !strings.HasPrefix(got.VanityURL, "https://hosted-mattsweetibm-s1netops-") {
		t.Errorf("vanity URL %q is not derived from org/primary repo", got.VanityURL)
	}
	if !strings.HasSuffix(got.VanityURL, "."+vanityRepairTestDomain) {
		t.Errorf("vanity URL %q is not on the cluster domain", got.VanityURL)
	}
	if strings.Contains(got.VanityURL, "placeholder") {
		t.Errorf("vanity URL %q still carries the placeholder host", got.VanityURL)
	}
	// The whole point: the read paths now surface the vanity URL.
	if v := claimedVanityURL(got); v != got.VanityURL {
		t.Errorf("claimedVanityURL = %q, want %q", v, got.VanityURL)
	}
}

// The repair must be idempotent: the heartbeat calls it on EVERY beat, and
// generateHiveID mints a fresh random suffix per call, so a second run must not
// change anything (otherwise the vanity host would churn every heartbeat).
func TestVanityRepairIsIdempotent(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	const id = "hosted-available-oke-01-placeholder-bb95"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Org: "TradingAsBuddies",
		Repos: []string{"app"}, PrimaryRepo: "app", ACMMLevel: 2,
		ClusterID: defaultClusterID,
	}); err != nil {
		t.Fatal(err)
	}

	if !s.repairVanityURLForHive(id) {
		t.Fatal("first repair reported no change")
	}
	first := loadSaaSHive(id).VanityURL

	if s.repairVanityURLForHive(id) {
		t.Error("second repair changed an already-repaired hive")
	}
	if second := loadSaaSHive(id).VanityURL; second != first {
		t.Errorf("vanity URL churned across beats: %q then %q", first, second)
	}

	// The same holds for the fleet sweep.
	if n := s.repairVanityURLsForClaimedHives(); n != 0 {
		t.Errorf("sweep over an already-repaired fleet changed %d hives, want 0", n)
	}
}

// An UNCLAIMED placeholder must be left completely alone: assign owns it and
// mints its vanity URL at claim time.
func TestVanityRepairSkipsUnclaimedPlaceholder(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	const id = "hosted-available-oke-09"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: statusAvailable, Owner: hubAdminUsername, ClusterID: defaultClusterID,
	}); err != nil {
		t.Fatal(err)
	}

	if s.repairVanityURLForHive(id) {
		t.Error("repair touched an unclaimed placeholder")
	}
	if v := loadSaaSHive(id).VanityURL; v != "" {
		t.Errorf("unclaimed placeholder gained a vanity URL %q", v)
	}
}

// A hive whose cluster the hub cannot reach (the firewalled vllm-d case: the hub
// cannot kubectl to create a route) must keep its working placeholder host
// rather than adopt an unserved vanity host that would 503 every hub link.
func TestVanityRepairKeepsPlaceholderWhenHostNotServable(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error {
		return fmt.Errorf("cluster unreachable")
	}

	const id = "hosted-available-vllmd-10"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Org: "katamari",
		Repos: []string{"ibm-aiops-orchestrator"}, PrimaryRepo: "ibm-aiops-orchestrator",
		ACMMLevel: 2, ClusterID: defaultClusterID,
	}); err != nil {
		t.Fatal(err)
	}

	if s.repairVanityURLForHive(id) {
		t.Error("repair adopted a vanity host it could not make servable")
	}
	if v := loadSaaSHive(id).VanityURL; v != "" {
		t.Errorf("unservable vanity host was adopted: %q — hub links would 503", v)
	}
	// claimedVanityURL therefore still yields nothing, so the placeholder stands.
	if v := claimedVanityURL(loadSaaSHive(id)); v != "" {
		t.Errorf("claimedVanityURL = %q, want \"\" so the working placeholder is used", v)
	}
}

// A hive with an incomplete claim (no org, or no primary repo) has nothing to
// derive a friendly host from, and a hive on a cluster with no domain likewise.
func TestVanityRepairSkipsUnderivableHives(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()
	s.clusters["no-domain"] = ClusterConfig{ID: "no-domain", Name: "nodomain"}

	seed := []*SaaSHive{
		{ID: "hosted-no-org", Status: "running", Repos: []string{"r"}, PrimaryRepo: "r", ClusterID: defaultClusterID},
		{ID: "hosted-no-repo", Status: "running", Org: "o", ClusterID: defaultClusterID},
		{ID: "hosted-no-domain", Status: "running", Org: "o", Repos: []string{"r"}, PrimaryRepo: "r", ClusterID: "no-domain"},
	}
	for _, h := range seed {
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
	}

	for _, h := range seed {
		if s.repairVanityURLForHive(h.ID) {
			t.Errorf("%s: repair changed a hive with nothing to derive a host from", h.ID)
		}
		if v := loadSaaSHive(h.ID).VanityURL; v != "" {
			t.Errorf("%s: gained a vanity URL %q", h.ID, v)
		}
	}
}

// An existing vanity URL is never re-minted or clobbered.
func TestVanityRepairNeverClobbersExistingVanityURL(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	const id = "hosted-has-vanity"
	const existing = "https://hosted-acme-app-abcd.hive.kubestellar.io"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Org: "acme", Repos: []string{"app"}, PrimaryRepo: "app",
		ClusterID: defaultClusterID, VanityURL: existing,
	}); err != nil {
		t.Fatal(err)
	}

	if s.repairVanityURLForHive(id) {
		t.Error("repair changed a hive that already had a vanity URL")
	}
	if v := loadSaaSHive(id).VanityURL; v != existing {
		t.Errorf("vanity URL = %q, want the untouched %q", v, existing)
	}
}

// End-to-end: once repaired, the very next heartbeat must PUSH the vanity URL to
// the spoke, so the spoke adopts it and the registry's dashboardUrl follows.
// Without this the repair would only fix the hub's display and leave the spoke
// reporting the placeholder forever.
func TestRepairedVanityURLReachesSpokeViaHeartbeat(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	const id = "hosted-available-oke-03-placeholder-y99x"
	const placeholderURL = "https://hosted-available-oke-03-placeholder-y99x.hive.kubestellar.io"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Org: "IBM", Repos: []string{"s1netops"},
		PrimaryRepo: "s1netops", ACMMLevel: 3, ClusterID: defaultClusterID,
		ClaimDelivered: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Before the repair the hub has no vanity URL to push.
	if pc := projectConfigForHiveID(id, "IBM", []string{"s1netops"}, "s1netops", 3, placeholderURL, ""); pc != nil && pc.DashboardURL != "" {
		t.Fatalf("expected no vanity push before repair, got %+v", pc)
	}

	if !s.repairVanityURLForHive(id) {
		t.Fatal("repair reported no change")
	}
	want := loadSaaSHive(id).VanityURL

	pc := projectConfigForHiveID(id, "IBM", []string{"s1netops"}, "s1netops", 3, placeholderURL, "")
	if pc == nil {
		t.Fatal("repaired hive produced no heartbeat push — the spoke would keep the placeholder")
	}
	if pc.DashboardURL != want {
		t.Errorf("pushed dashboard URL = %q, want %q", pc.DashboardURL, want)
	}

	// Once the spoke reports the vanity URL back, the reconcile goes quiet.
	if pc2 := projectConfigForHiveID(id, "IBM", []string{"s1netops"}, "s1netops", 3, want, ""); pc2 != nil && pc2.DashboardURL != "" {
		t.Errorf("still pushing after the spoke adopted the vanity URL: %+v", pc2)
	}
}

// openShiftVanityTestDomain mirrors the live vllm-d apps wildcard domain.
const openShiftVanityTestDomain = "apps.fmaas-vllm-d.example.com"

// newOpenShiftVanityTestHub builds a hub with one OpenShift-Route cluster whose
// spokes the hub cannot reach (no kubectl under test), standing in for the
// firewalled heartbeat-only vllm-d pool.
func newOpenShiftVanityTestHub() *HubServer {
	return &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {
				ID:          defaultClusterID,
				Name:        "vllm-d",
				Domain:      openShiftVanityTestDomain,
				IngressType: ingressTypeOpenShiftRoute,
			},
		},
	}
}

// On an OpenShift cluster whose Routes the hub cannot read, addVanityHostToIngress
// must report FAILURE. It previously `continue`d past every unreachable source
// Route and still returned nil, so the retroactive repair believed the host was
// servable, adopted it, and replaced a working placeholder link with a 503 — the
// live vllm-d regression across 11 claimed hives.
func TestAddVanityHostToIngressFailsWhenNoRouteCanBeMirrored(t *testing.T) {
	s := newOpenShiftVanityTestHub()
	cluster := s.clusters[defaultClusterID]

	err := s.addVanityHostToIngress("hosted-available-vllmd-01", "hosted-x-y-ab12."+openShiftVanityTestDomain, &cluster)
	if err == nil {
		t.Fatal("addVanityHostToIngress reported success with no route mirrored — " +
			"the caller would adopt an unservable vanity host and 503 every hub link")
	}
	if !strings.Contains(err.Error(), "no route found") {
		t.Errorf("error %q does not name the missing-route cause", err)
	}
}

// The guard must actually hold end to end: on an unreachable OpenShift cluster the
// hive keeps its EMPTY VanityURL, so every read path falls back to the working
// placeholder host rather than an adopted-but-unservable vanity host.
func TestVanityRepairKeepsPlaceholderOnUnreachableOpenShiftCluster(t *testing.T) {
	useTempHiveDir(t)
	s := newOpenShiftVanityTestHub()

	const id = "hosted-available-vllmd-01"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Owner: "someone", Org: "ibm-aiops",
		Repos: []string{"katamari"}, PrimaryRepo: "katamari", ACMMLevel: 3,
		ClusterID: defaultClusterID, ClaimDelivered: true,
	}); err != nil {
		t.Fatal(err)
	}

	if s.repairVanityURLForHive(id) {
		t.Error("repair adopted a vanity URL on a cluster where it could not create a route")
	}
	if got := loadSaaSHive(id).VanityURL; got != "" {
		t.Errorf("VanityURL = %q, want empty so the working placeholder host stands", got)
	}
	if got := claimedVanityURL(loadSaaSHive(id)); got != "" {
		t.Errorf("claimedVanityURL = %q, want empty (placeholder fallback)", got)
	}
}

// A repair that CANNOT succeed must not churn a new random host per heartbeat.
// generateHiveID mints a fresh 4-char suffix per call, so an unguarded retry
// produced a different host every beat (observed live: three hosts in four
// minutes). Repeated failing repairs must leave the record untouched.
func TestVanityRepairDoesNotChurnHostWhenItCannotSucceed(t *testing.T) {
	useTempHiveDir(t)
	s := newOpenShiftVanityTestHub()

	const id = "hosted-available-vllmd-12"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Owner: "someone", Org: "ibm-aiops",
		Repos: []string{"katamari"}, PrimaryRepo: "katamari", ACMMLevel: 3,
		ClusterID: defaultClusterID, ClaimDelivered: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate several heartbeats against a cluster that stays unreachable.
	for beat := range 5 {
		if s.repairVanityURLForHive(id) {
			t.Fatalf("beat %d: repair reported a change it could not make servable", beat)
		}
		if got := loadSaaSHive(id).VanityURL; got != "" {
			t.Fatalf("beat %d: adopted %q despite an unservable host", beat, got)
		}
	}
}

// The nginx path must be unaffected by the OpenShift accounting fix: a reachable
// nginx cluster still adopts its vanity host exactly once and keeps it stable.
func TestVanityRepairNginxBehaviorUnchanged(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	const id = "hosted-available-oke-42-placeholder-zz01"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Status: "running", Owner: "someone", Org: "kubestellar",
		Repos: []string{"console"}, PrimaryRepo: "console", ACMMLevel: 3,
		ClusterID: defaultClusterID, ClaimDelivered: true,
	}); err != nil {
		t.Fatal(err)
	}

	if !s.repairVanityURLForHive(id) {
		t.Fatal("nginx repair reported no change — existing behavior regressed")
	}
	first := loadSaaSHive(id).VanityURL
	if first == "" {
		t.Fatal("nginx repair left the vanity URL empty")
	}
	if s.repairVanityURLForHive(id) {
		t.Error("nginx repair was not idempotent")
	}
	if second := loadSaaSHive(id).VanityURL; second != first {
		t.Errorf("nginx vanity host churned: %q then %q", first, second)
	}
}
