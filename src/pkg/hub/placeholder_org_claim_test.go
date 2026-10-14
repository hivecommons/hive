package hub

import (
	"log/slog"
	"testing"
)

// TestAdoptSpokeProjectConfig_NeverAdoptsSlotPlaceholderOrg is the #11800
// regression: ten vllm-d hives settled on org=available-… with
// ClaimDelivered=true and ran for weeks without GitHub credentials. A recycled
// placeholder slot carrying a stale delivered latch adopted the spoke's old
// placeholder self-report, overwriting the approved request org.
func TestAdoptSpokeProjectConfig_NeverAdoptsSlotPlaceholderOrg(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}

	// A recycled slot assigned to the request org, still carrying the prior
	// tenancy's ClaimDelivered=true latch.
	const id = "hosted-available-vllmd-260731-nq4a"
	saveSaaSHive(&SaaSHive{
		ID: id, Status: statusAssigned, Owner: "ibmid:tony",
		Org: "Tony-Olajide", Repos: []string{"kie"}, PrimaryRepo: "kie",
		ACMMLevel: 3, RequestedACMMLevel: 3, ACMMDelivered: true,
		ClaimDelivered: true,
	})

	// The spoke heartbeats its old placeholder org once.
	s.adoptSpokeProjectConfig(id, "available-vllmd-260731-nq4a", []string{"kie"}, "kie", 3)
	h := loadSaaSHive(id)
	if h == nil || h.Org != "Tony-Olajide" {
		t.Fatalf("placeholder org adopted: meta = %+v, want org=Tony-Olajide", h)
	}
	if h.ClaimDelivered {
		t.Fatalf("ClaimDelivered still true after placeholder report; push stays disabled: %+v", h)
	}
	if h.AssignedAt == "" {
		t.Errorf("re-armed claim must restamp AssignedAt so the stuck sweep does not reclaim it at once")
	}

	// The hub keeps pushing the request org.
	got := projectConfigForHiveID(id, "available-vllmd-260731-nq4a", []string{"kie"}, "kie", 3, "", "")
	if got == nil || got.Org != "Tony-Olajide" {
		t.Fatalf("push = %+v, want org=Tony-Olajide pushed", got)
	}

	// Further placeholder beats still never adopt or deliver.
	s.adoptSpokeProjectConfig(id, "available-vllmd-260731-nq4a", []string{"kie"}, "kie", 3)
	if h := loadSaaSHive(id); h == nil || h.Org != "Tony-Olajide" || h.ClaimDelivered {
		t.Fatalf("second placeholder beat: meta = %+v, want org kept and ClaimDelivered=false", h)
	}

	// Once the spoke applies the request org, delivery completes normally.
	s.adoptSpokeProjectConfig(id, "Tony-Olajide", []string{"kie"}, "kie", 3)
	if h := loadSaaSHive(id); h == nil || !h.ClaimDelivered || h.Org != "Tony-Olajide" {
		t.Fatalf("matching report: meta = %+v, want ClaimDelivered=true org=Tony-Olajide", h)
	}
}

// TestAdoptSpokeProjectConfig_RestoresRequestOrgOverPlaceholderMeta covers the
// already-corrupted production record: meta itself holds the placeholder org
// with a delivered latch. The approved request for that slot wins and the push
// re-arms.
func TestAdoptSpokeProjectConfig_RestoresRequestOrgOverPlaceholderMeta(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}

	const id = "hosted-available-vllmd-15"
	saveSaaSHive(&SaaSHive{
		ID: id, Status: statusAssigned, Owner: "ibmid:hursley",
		Org: "available-vllmd-15", Repos: []string{"kie"}, PrimaryRepo: "kie",
		ACMMLevel: 3, RequestedACMMLevel: 3, ACMMDelivered: true,
		ClaimDelivered: true,
	})
	if err := saveProvisionRequest(&ProvisionRequest{
		Username: "ibmid:hursley", Org: "ptc-hursley", Repos: "kie", PrimaryRepo: "kie",
		Status: provisionStatusApproved, AssignedHive: id,
	}); err != nil {
		t.Fatal(err)
	}

	s.adoptSpokeProjectConfig(id, "available-vllmd-15", []string{"kie"}, "kie", 3)
	h := loadSaaSHive(id)
	if h == nil || h.Org != "ptc-hursley" || h.ClaimDelivered {
		t.Fatalf("meta = %+v, want org restored to ptc-hursley and ClaimDelivered=false", h)
	}
	if got := projectConfigForHiveID(id, "available-vllmd-15", []string{"kie"}, "kie", 3, "", ""); got == nil || got.Org != "ptc-hursley" {
		t.Fatalf("push = %+v, want org=ptc-hursley pushed", got)
	}
}

func TestIsSlotPlaceholderOrg(t *testing.T) {
	for org, want := range map[string]bool{
		"available-vllmd-14": true,
		"Available-oke-11":   true,
		"placeholder-r05x":   true,
		"flashsystems":       false,
		"":                   false,
		"my-available-thing": false,
	} {
		if got := isSlotPlaceholderOrg(org); got != want {
			t.Errorf("isSlotPlaceholderOrg(%q) = %v, want %v", org, got, want)
		}
	}
}
