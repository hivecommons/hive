package hub

import (
	"strings"
	"testing"
)

// vllmDWithBothForges is the vllm-d cluster entry as it must look for the
// public hives on it to be repairable: GHE remains the DEFAULT (it is what the
// cluster is used for), and the public App is additionally named so a hive can
// ELECT github.com. Per the platform owner: "vllm-d is used for GHE, but gh can
// be elected."
//
// The flat github_* fields are left exactly as they are in the live file, so
// this doubles as a check that the legacy shape still parses and still supplies
// the GHE identity.
func vllmDWithBothForges() *ClusterConfig {
	return &ClusterConfig{
		ID:            "vllm-d",
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
		GitHubAppID:   5686,
		GitHubAppSlug: "kubestellar-hive-ghe",
		Forges: map[string]ClusterForgeIdentity{
			"github.com": {AppID: 3568013, AppSlug: "kubestellar-hive"},
		},
	}
}

// TestPublicElectionOnGHEDefaultClusterResolves is the live repair this change
// exists to unblock.
//
// TODAY this 409s: POST /api/saas/hives/hosted-available-vllmd-05/forge
// {"host":"github.com"} returns "the target forge identity is incomplete for
// cluster \"vllm-d\"", because identity was resolved from cluster config that
// has ONE slot and it holds the GHE App. The refusal was correct — the hub had
// no public App to name — but it left seven hives unrepairable.
func TestPublicElectionOnGHEDefaultClusterResolves(t *testing.T) {
	s := &HubServer{}
	cluster := vllmDWithBothForges()

	public, err := parseForgeTarget("github.com")
	if err != nil {
		t.Fatal(err)
	}
	got, missing := s.forgeIdentityForTarget(cluster, public)
	if len(missing) != 0 {
		t.Fatalf("a public election on the GHE-default vllm-d cluster must RESOLVE, got missing=%v", missing)
	}
	if got.AppID != 3568013 {
		t.Errorf("app id = %d, want the PUBLIC app 3568013 (not the GHE 5686)", got.AppID)
	}
	if got.AppSlug != "kubestellar-hive" {
		t.Errorf("app slug = %q, want kubestellar-hive — %q is what authored as ghe[bot] on public repos",
			got.AppSlug, "kubestellar-hive-ghe")
	}
}

// TestMixedForgeClusterResolvesBothWays is the required mixed-forge test: on ONE
// cluster entry, a github.com hive and a github.ibm.com hive must each resolve
// to their own forge's identity, with no leakage between them. Leakage in this
// exact direction is the incident.
func TestMixedForgeClusterElectionResolvesBothWays(t *testing.T) {
	s := &HubServer{}
	cluster := vllmDWithBothForges()

	for _, tc := range []struct {
		host      string
		wantID    int64
		wantSlug  string
		otherID   int64
		otherSlug string
	}{
		{"github.com", 3568013, "kubestellar-hive", 5686, "kubestellar-hive-ghe"},
		{"github.ibm.com", 5686, "kubestellar-hive-ghe", 3568013, "kubestellar-hive"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			target, err := parseForgeTarget(tc.host)
			if err != nil {
				t.Fatal(err)
			}
			got, missing := s.forgeIdentityForTarget(cluster, target)
			if len(missing) != 0 {
				t.Fatalf("%s must resolve on a dual-forge cluster, got missing=%v", tc.host, missing)
			}
			if got.AppID != tc.wantID || got.AppSlug != tc.wantSlug {
				t.Errorf("identity = %+v, want app %d / slug %q", got, tc.wantID, tc.wantSlug)
			}
			if got.AppID == tc.otherID || got.AppSlug == tc.otherSlug {
				t.Errorf("LEAKAGE: %s resolved to the other forge's identity %+v", tc.host, got)
			}
		})
	}
}

// TestLegacySingleIdentityClusterUnchanged proves the schema is backward
// compatible: an entry with no `forges` map behaves exactly as it does today,
// including still REFUSING a forge it has no App for. Nothing in clusters.json
// has to change for the fleet to keep working.
func TestLegacySingleIdentityClusterUnchanged(t *testing.T) {
	s := &HubServer{}
	// hive-oke, live shape: public, no URLs (implicit), one App.
	hiveOKE := &ClusterConfig{ID: "hive-oke", GitHubAppID: 3568013, GitHubAppSlug: "kubestellar-hive"}
	// vllm-d, live shape BEFORE this change: GHE only.
	vllmD := &ClusterConfig{
		ID: "vllm-d", GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL: "https://github.ibm.com/api/v3",
		GitHubAppID:  5686, GitHubAppSlug: "kubestellar-hive-ghe",
	}
	public, _ := parseForgeTarget("github.com")
	ghe, _ := parseForgeTarget("github.ibm.com")

	if got, missing := s.forgeIdentityForTarget(hiveOKE, public); len(missing) != 0 || got.AppID != 3568013 {
		t.Errorf("legacy public cluster: got %+v missing %v", got, missing)
	}
	if got, missing := s.forgeIdentityForTarget(vllmD, ghe); len(missing) != 0 || got.AppID != 5686 {
		t.Errorf("legacy GHE cluster: got %+v missing %v", got, missing)
	}
	// THE PRESERVED REFUSAL. A legacy GHE-only cluster still has no public App,
	// so a public election still refuses and still writes nothing. This is the
	// 409 that must survive — it is what stopped the incident getting worse.
	got, missing := s.forgeIdentityForTarget(vllmD, public)
	if len(missing) == 0 {
		t.Fatalf("a cluster naming no public App must still REFUSE a public election, got %+v", got)
	}
	if got.AppID != 0 || got.AppSlug != "" {
		t.Errorf("a refused identity must be entirely empty (all-or-nothing), got %+v", got)
	}
	// Conversely, hive-oke has no GHE App.
	if _, missing := s.forgeIdentityForTarget(hiveOKE, ghe); len(missing) == 0 {
		t.Error("a public-only cluster must refuse a GHE election")
	}
}

// TestIncompleteForgeEntryStillRefuses checks the atomicity guarantee holds on
// the NEW schema too: a half-filled `forges` entry is refused, not accepted with
// a slug that falls back to the public default and 404s the install link.
func TestIncompleteForgeEntryStillRefuses(t *testing.T) {
	s := &HubServer{}
	public, _ := parseForgeTarget("github.com")

	for _, tc := range []struct {
		name     string
		identity ClusterForgeIdentity
		wantName string
	}{
		{"app_id missing", ClusterForgeIdentity{AppSlug: "kubestellar-hive"}, "app_id"},
		{"app_slug missing", ClusterForgeIdentity{AppID: 3568013}, "app_slug"},
		{"both missing", ClusterForgeIdentity{}, "app_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &ClusterConfig{
				ID: "vllm-d", GitHubBaseURL: "https://github.ibm.com",
				GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe",
				Forges: map[string]ClusterForgeIdentity{"github.com": tc.identity},
			}
			got, missing := s.forgeIdentityForTarget(cluster, public)
			if len(missing) == 0 {
				t.Fatalf("an incomplete forge entry must be refused, got %+v", got)
			}
			if got.AppID != 0 || got.AppSlug != "" {
				t.Errorf("refused identity must be empty, got %+v", got)
			}
			if !strings.Contains(strings.Join(missing, " "), tc.wantName) {
				t.Errorf("the refusal must NAME %s so an operator can fix it, got %v", tc.wantName, missing)
			}
		})
	}
}

// TestClusterDefaultForge pins default resolution, including that an entry with
// no forge information at all defaults to public — today's behaviour for
// hive-oke, which carries no URLs.
func TestClusterDefaultForge(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *ClusterConfig
		want string
	}{
		{"nil cluster", nil, "github.com"},
		{"no forge info (hive-oke shape)", &ClusterConfig{ID: "hive-oke"}, "github.com"},
		{"GHE base url (vllm-d shape)",
			&ClusterConfig{GitHubBaseURL: "https://github.ibm.com"}, "github.ibm.com"},
		{"GHE api url only",
			&ClusterConfig{GitHubAPIURL: "https://github.ibm.com/api/v3"}, "github.ibm.com"},
		{"explicit default_forge wins",
			&ClusterConfig{GitHubBaseURL: "https://github.ibm.com", DefaultForge: "github.com"}, "github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterDefaultForge(tc.c); got != tc.want {
				t.Errorf("clusterDefaultForge = %q, want %q", got, tc.want)
			}
		})
	}
}
