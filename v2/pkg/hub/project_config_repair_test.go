package hub
import "testing"
// A wedged spoke (invalid repo, claim already delivered) must still get a
// corrected project pushed — otherwise it crash-loops forever.
func TestProjectConfigRepairsWedgedSpoke(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	h := &SaaSHive{
		ID: "hosted-wedged", Owner: "someone", Org: "enricom-ibm",
		Repos: []string{"enricom-ibm/jackrabbit"}, PrimaryRepo: "enricom-ibm/jackrabbit",
		ACMMLevel: 2, ClaimDelivered: true,
	}
	if err := saveSaaSHive(h); err != nil { t.Fatal(err) }

	// Spoke reports the malformed value it is stuck on.
	bad := "github.ibm.com/enricom-ibm/jackrabbit"
	pc := projectConfigForHiveID("hosted-wedged", "enricom-ibm", []string{bad}, bad, 2, "")
	if pc == nil {
		t.Fatal("expected a repair push for a spoke reporting an invalid repo, got nil")
	}
	if !isValidRepoRef(pc.PrimaryRepo) {
		t.Errorf("pushed primary_repo %q is still invalid", pc.PrimaryRepo)
	}
	for _, r := range pc.Repos {
		if !isValidRepoRef(r) { t.Errorf("pushed repo %q is still invalid", r) }
	}

	// A healthy delivered claim must stay quiet (no churn every beat).
	if pc2 := projectConfigForHiveID("hosted-wedged", "enricom-ibm",
		[]string{"enricom-ibm/jackrabbit"}, "enricom-ibm/jackrabbit", 2, ""); pc2 != nil {
		t.Errorf("expected nil for a healthy delivered claim, got %+v", pc2)
	}
}
