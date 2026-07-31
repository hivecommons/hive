package hub

import "testing"

// A claimed hive whose meta carries a validated vanity_url must have that URL
// pushed to the spoke as DashboardURL — even when its project record is stale/
// incomplete (the common placeholder-URL-persists case: vanity_url exists in
// meta but was never delivered because the reconcile bailed on an incomplete
// claim). The push must be URL-only (no stale org/repos/ACMM).
func TestVanityURLPushedForClaimedHiveWithStaleProject(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	vanity := "https://hosted-zacburns-vllm-abcd.apps.fmaas-vllm-d.fmaas.res.ibm.com"
	h := &SaaSHive{
		ID:     "hosted-available-vllmd-10",
		Status: "running", // claimed (not "available")
		// Deliberately incomplete/stale project (no ACMM, no primary) — mirrors a
		// real pre-claim record that still carries a populated vanity_url.
		VanityURL: vanity,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// Spoke still reports the raw placeholder host.
	pc := projectConfigForHiveID(h.ID, "", nil, "", 0,
		"https://hosted-available-vllmd-10.apps.fmaas-vllm-d.fmaas.res.ibm.com", "")
	if pc == nil {
		t.Fatal("expected a vanity URL push for a claimed hive with a meta vanity_url, got nil")
	}
	if pc.DashboardURL != vanity {
		t.Errorf("expected DashboardURL %q, got %q", vanity, pc.DashboardURL)
	}
	// A URL-only push must NOT carry the stale project (that would blank/downgrade
	// the spoke's working config).
	if pc.Org != "" || len(pc.Repos) != 0 || pc.PrimaryRepo != "" || pc.ACMMLevel != 0 {
		t.Errorf("URL-only push must not carry project fields, got %+v", pc)
	}

	// Once the spoke reports the vanity host back, the reconcile goes quiet.
	if pc2 := projectConfigForHiveID(h.ID, "", nil, "", 0, vanity, ""); pc2 != nil {
		t.Errorf("expected nil once the spoke reports the vanity URL, got %+v", pc2)
	}
}

// A hive on a heartbeat-only cluster adopts the vanity URL the hub shows/links
// for it (My Hives, SSO /open) purely from its meta vanity_url, without the hub
// ever creating an ingress/route — claimedVanityURL is the single source.
func TestVanityURLAdoptedWithoutHubCreatedIngress(t *testing.T) {
	vanity := "https://hosted-zacburns-vllm-abcd.apps.fmaas-vllm-d.fmaas.res.ibm.com"
	h := &SaaSHive{ID: "hosted-vllmd-hb", Status: "running", VanityURL: vanity}
	if got := claimedVanityURL(h); got != vanity {
		t.Errorf("claimed heartbeat-only hive should surface its meta vanity_url %q, got %q", vanity, got)
	}
}

// An UNASSIGNED placeholder (status "available") keeps its placeholder URL: the
// hub must neither push a vanity URL nor override its shown DashboardURL.
func TestUnassignedPlaceholderKeepsPlaceholderURL(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	// Even if a stray vanity_url were present, an available placeholder must be
	// left alone.
	h := &SaaSHive{
		ID:        "hosted-available-slot-01",
		Status:    statusAvailable,
		VanityURL: "https://should-not-be-used.example.com",
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	if got := claimedVanityURL(h); got != "" {
		t.Errorf("available placeholder must not surface a vanity URL, got %q", got)
	}
	placeholder := "https://hosted-available-slot-01.apps.fmaas-vllm-d.fmaas.res.ibm.com"
	if pc := projectConfigForHiveID(h.ID, "", nil, "", 0, placeholder, ""); pc != nil {
		t.Errorf("available placeholder must not be pushed a vanity URL, got %+v", pc)
	}
}

// The don't-503-on-unserved-host safety: a claimed hive with NO vanity URL in
// meta must never have one invented — the hub keeps the placeholder rather than
// pushing/linking a host that has no backend.
func TestNoVanityPushWhenUnserved(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	h := &SaaSHive{
		ID:          "hosted-claimed-no-vanity",
		Status:      "running",
		Org:         "acme",
		Repos:       []string{"acme/widget"},
		PrimaryRepo: "acme/widget",
		ACMMLevel:   2,
		// VanityURL deliberately empty: no validated host exists.
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// claimedVanityURL yields nothing to link/show, so the placeholder stands.
	if got := claimedVanityURL(h); got != "" {
		t.Errorf("no meta vanity_url means nothing to surface, got %q", got)
	}
	// The reconcile, once the claim is delivered and matched, must be quiet — it
	// must NOT invent a vanity URL to push.
	h.ClaimDelivered = true
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	pc := projectConfigForHiveID(h.ID, "acme", []string{"acme/widget"}, "acme/widget", 2,
		"https://hosted-claimed-no-vanity.hive.kubestellar.io", "")
	if pc != nil {
		t.Errorf("must not invent/push a vanity URL for a hive with none, got %+v", pc)
	}
}
