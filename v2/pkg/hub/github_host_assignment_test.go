package hub

import (
	"io"
	"log/slog"
	"testing"
)

// gheTestHost is the GitHub Enterprise instance used throughout these tests —
// the same one the live vllm-d cluster is configured with.
const gheTestHost = "github.ibm.com"

// gheTestBaseURL / gheTestAPIURL are that host's cluster-level settings.
const (
	gheTestBaseURL = "https://" + gheTestHost
	gheTestAPIURL  = "https://" + gheTestHost + "/api/v3"
)

// newGHETestHub returns a hub whose default cluster is a GHE cluster, plus a
// discard logger so the repair's audit logging does not spam test output.
func newGHETestHub() *HubServer {
	return &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {
				ID:            defaultClusterID,
				Name:          "ghe-cluster",
				GitHubBaseURL: gheTestBaseURL,
				GitHubAPIURL:  gheTestAPIURL,
			},
			"public-cluster": {ID: "public-cluster", Name: "public"},
		},
	}
}

// useTempHiveDir points the on-disk SaaS hive store at a temp dir for the
// duration of a test.
func useTempHiveDir(t *testing.T) {
	t.Helper()
	old := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = old })
}

// applyAssignHostChoice mirrors the host-resolution the assign/approve handlers
// perform: an explicit "public" becomes a blank host plus the sentinel base
// URL, any other explicit host is recorded verbatim, and only a still-blank
// host inherits the cluster default. Keeping it in one place lets the table
// below exercise the exact precedence both handlers implement.
func applyAssignHostChoice(s *HubServer, h *SaaSHive, requested string) {
	if requested == githubHostPublic {
		h.GitHubHost = ""
		h.GitHubBaseURL = githubHostPublic
	} else if requested != "" {
		h.GitHubHost = requested
	}
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
	}
}

// TestAssignmentGitHubHostPrecedence covers the three assignment cases the GHE
// gap turns on: an explicit host is kept, a blank host on a GHE cluster is
// backfilled, and an explicit "public" override on a GHE cluster stays public.
func TestAssignmentGitHubHostPrecedence(t *testing.T) {
	s := newGHETestHub()

	tests := []struct {
		name          string
		clusterID     string
		requestedHost string
		wantHost      string
		wantAPIURL    string
	}{
		{
			name:          "explicit host is recorded verbatim",
			clusterID:     defaultClusterID,
			requestedHost: "github.other.com",
			wantHost:      "github.other.com",
			wantAPIURL:    "https://github.other.com/api/v3",
		},
		{
			name:          "blank host on a GHE cluster is backfilled",
			clusterID:     defaultClusterID,
			requestedHost: "",
			wantHost:      gheTestHost,
			wantAPIURL:    gheTestAPIURL,
		},
		{
			name:          "explicit public override on a GHE cluster stays public",
			clusterID:     defaultClusterID,
			requestedHost: githubHostPublic,
			wantHost:      "",
			wantAPIURL:    "",
		},
		{
			name:          "blank host on a public cluster records nothing",
			clusterID:     "public-cluster",
			requestedHost: "",
			wantHost:      "",
			wantAPIURL:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &SaaSHive{ID: "hosted-x", ClusterID: tc.clusterID}
			applyAssignHostChoice(s, h, tc.requestedHost)
			if h.GitHubHost != tc.wantHost {
				t.Errorf("GitHubHost = %q, want %q", h.GitHubHost, tc.wantHost)
			}
			// The host only matters because of what it makes the heartbeat push.
			if got := gheAPIURLForHost(h.GitHubHost); got != tc.wantAPIURL {
				t.Errorf("pushed github_api_url = %q, want %q", got, tc.wantAPIURL)
			}
		})
	}
}

// TestApprovePrefersRequestGitHubHost: approving a provision request must honour
// the host the REQUESTER supplied (parsed from the org URL they pasted) rather
// than dropping it, and an admin's explicit choice in the approve modal must
// win over the request.
func TestApprovePrefersRequestGitHubHost(t *testing.T) {
	s := newGHETestHub()

	tests := []struct {
		name        string
		requestHost string
		adminHost   string
		wantHost    string
	}{
		{
			name:        "request host is used when the admin picks the default",
			requestHost: "github.requested.com",
			adminHost:   "",
			wantHost:    "github.requested.com",
		},
		{
			name:        "admin override wins over the request",
			requestHost: "github.requested.com",
			adminHost:   "github.admin.com",
			wantHost:    "github.admin.com",
		},
		{
			name:        "admin public override beats a GHE request and the GHE cluster",
			requestHost: "github.requested.com",
			adminHost:   githubHostPublic,
			wantHost:    "",
		},
		{
			name:        "no request host and no admin choice falls back to the cluster",
			requestHost: "",
			adminHost:   "",
			wantHost:    gheTestHost,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &SaaSHive{ID: "hosted-approve", ClusterID: defaultClusterID}
			// Mirrors handleApproveProvision's resolution order.
			if tc.adminHost == githubHostPublic {
				h.GitHubHost = ""
				h.GitHubBaseURL = githubHostPublic
			} else if tc.adminHost != "" {
				h.GitHubHost = tc.adminHost
			} else if tc.requestHost != "" {
				h.GitHubHost = tc.requestHost
			}
			if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
				h.GitHubHost = host
			}
			if h.GitHubHost != tc.wantHost {
				t.Errorf("GitHubHost = %q, want %q", h.GitHubHost, tc.wantHost)
			}
		})
	}
}

// TestRepairGitHubHostsFromClusters covers the retroactive repair: it fixes an
// already-assigned hive with a blank host on a GHE cluster, refuses to touch an
// explicit host, an explicit public override, a public cluster, or an unclaimed
// placeholder — and is idempotent on a second run.
func TestRepairGitHubHostsFromClusters(t *testing.T) {
	useTempHiveDir(t)
	s := newGHETestHub()

	seed := []*SaaSHive{
		// The vllm-d case: assigned, blank host, GHE cluster.
		{ID: "hosted-blank-ghe", Owner: "taylormgeorge91", Org: "katamari",
			Repos: []string{"ibm-aiops-orchestrator"}, PrimaryRepo: "ibm-aiops-orchestrator",
			ACMMLevel: 2, ClusterID: defaultClusterID},
		// Explicitly set to a DIFFERENT host — must never be clobbered.
		{ID: "hosted-explicit", Owner: "a", Org: "o", GitHubHost: "github.explicit.com",
			Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 1, ClusterID: defaultClusterID},
		// Explicit public override on a GHE cluster — must stay public.
		{ID: "hosted-public-override", Owner: "a", Org: "o", GitHubBaseURL: githubHostPublic,
			Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 1, ClusterID: defaultClusterID},
		// Public cluster — nothing to record.
		{ID: "hosted-public-cluster", Owner: "a", Org: "o",
			Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 1, ClusterID: "public-cluster"},
		// Unclaimed placeholder — assign owns it, repair must skip it.
		{ID: "hosted-available-x", Owner: hubAdminUsername, Status: statusAvailable,
			ClusterID: defaultClusterID},
	}
	for _, h := range seed {
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
	}

	const wantRepaired = 1 // only hosted-blank-ghe qualifies
	if got := s.repairGitHubHostsFromClusters(); got != wantRepaired {
		t.Errorf("repairGitHubHostsFromClusters() = %d, want %d", got, wantRepaired)
	}

	want := map[string]string{
		"hosted-blank-ghe":       gheTestHost,
		"hosted-explicit":        "github.explicit.com",
		"hosted-public-override": "",
		"hosted-public-cluster":  "",
		"hosted-available-x":     "",
	}
	for id, wantHost := range want {
		h := loadSaaSHive(id)
		if h == nil {
			t.Fatalf("hive %s disappeared", id)
		}
		if h.GitHubHost != wantHost {
			t.Errorf("%s: GitHubHost = %q, want %q", id, h.GitHubHost, wantHost)
		}
	}

	// Idempotent: a second sweep over an already-repaired fleet changes nothing.
	if got := s.repairGitHubHostsFromClusters(); got != 0 {
		t.Errorf("second repair sweep changed %d hives, want 0", got)
	}
	if h := loadSaaSHive("hosted-blank-ghe"); h == nil || h.GitHubHost != gheTestHost {
		t.Errorf("repaired host did not survive a second sweep: %+v", h)
	}
}

// TestRepairedHostReachesSpokeViaHeartbeat is the end-to-end trace that makes
// the repair worth anything: filling GitHubHost on an ALREADY-DELIVERED claim
// must actually cause projectConfigForHiveID to push the GHE API URL.
//
// Before the needGHEAPIPush gate this failed: ClaimDelivered == true and a
// matching vanity URL made every push condition false, so the reconcile
// returned nil forever and the repaired host never left the hub.
func TestRepairedHostReachesSpokeViaHeartbeat(t *testing.T) {
	useTempHiveDir(t)
	s := newGHETestHub()

	h := &SaaSHive{
		ID: "hosted-delivered", Owner: "taylormgeorge91", Org: "katamari",
		Repos: []string{"ibm-aiops-orchestrator"}, PrimaryRepo: "ibm-aiops-orchestrator",
		ACMMLevel: 2, ClusterID: defaultClusterID,
		// The claim already landed — this is the state every pre-existing
		// misconfigured hive is in.
		ClaimDelivered: true,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// publicAPIURL is what a misconfigured GHE spoke reports today.
	const publicAPIURL = "https://api.github.com"

	// Before the repair the hub has nothing to say.
	if pc := projectConfigForHiveID(h.ID, h.Org, h.Repos, h.PrimaryRepo, h.ACMMLevel, "", publicAPIURL); pc != nil {
		t.Fatalf("expected no push before repair, got %+v", pc)
	}

	// This is the entry point the heartbeat handler calls, per hive, per beat.
	if !s.repairGitHubHostForHive(h.ID) {
		t.Fatal("per-hive repair reported no change for a blank host on a GHE cluster")
	}
	// Idempotent on the next beat — the heartbeat calls this every time.
	if s.repairGitHubHostForHive(h.ID) {
		t.Error("per-hive repair changed an already-repaired hive on a second beat")
	}

	// After the repair the very next heartbeat must carry the GHE API URL.
	pc := projectConfigForHiveID(h.ID, h.Org, h.Repos, h.PrimaryRepo, h.ACMMLevel, "", publicAPIURL)
	if pc == nil {
		t.Fatal("repaired host produced no heartbeat push — the spoke would never learn its GHE API URL")
	}
	if pc.GitHubAPIURL != gheTestAPIURL {
		t.Errorf("pushed github_api_url = %q, want %q", pc.GitHubAPIURL, gheTestAPIURL)
	}
	// The push must not disturb the project the spoke already runs.
	if pc.Org != h.Org || pc.PrimaryRepo != h.PrimaryRepo {
		t.Errorf("push altered the delivered project: org=%q primary=%q", pc.Org, pc.PrimaryRepo)
	}

	// Once the spoke reports the new API URL, the hub goes quiet again — no
	// per-beat churn.
	if pc2 := projectConfigForHiveID(h.ID, h.Org, h.Repos, h.PrimaryRepo, h.ACMMLevel, "", gheTestAPIURL); pc2 != nil {
		t.Errorf("expected silence once the spoke reports the GHE API URL, got %+v", pc2)
	}

	// A spoke too old to report its API URL sends "". That is UNKNOWN, not a
	// mismatch — pushing on it would re-send every beat with no read-back to
	// ever stop it.
	if pc3 := projectConfigForHiveID(h.ID, h.Org, h.Repos, h.PrimaryRepo, h.ACMMLevel, "", ""); pc3 != nil {
		t.Errorf("expected no push for a spoke that does not report its API URL, got %+v", pc3)
	}
}
