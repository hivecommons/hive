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
		{
			// The onboarding form sends "public" for an explicit github.com choice.
			// On a GHE cluster it must STAY public — not be stored as the literal
			// host "public", and not fall through to the cluster GHE backfill.
			name:        "public request on a GHE cluster stays public (never backfilled)",
			requestHost: githubHostPublic,
			adminHost:   "",
			wantHost:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &SaaSHive{ID: "hosted-approve", ClusterID: defaultClusterID}
			// Mirrors handleApproveProvision's resolution order — including the
			// request-side "public" sentinel the onboarding form now sends for an
			// explicit github.com choice, which must pin public just like an admin
			// override rather than be stored as the literal host "public".
			if tc.adminHost == githubHostPublic {
				h.GitHubHost = ""
				h.GitHubBaseURL = githubHostPublic
			} else if tc.adminHost != "" {
				h.GitHubHost = tc.adminHost
			} else if tc.requestHost != "" {
				if tc.requestHost == githubHostPublic {
					h.GitHubHost = ""
					h.GitHubBaseURL = githubHostPublic
				} else {
					h.GitHubHost = tc.requestHost
				}
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

// TestNoPerBeatClusterHostFill pins the removal of the per-beat
// blank-host-from-cluster fill. FORGE IS PER-HIVE: a claimed hive's empty
// github_host means public github.com (the field's documented meaning), and a
// GHE cluster hosts github.com projects beside its GHE ones — so nothing on the
// heartbeat path may materialise the CLUSTER's forge into a hive's record. The
// only remaining github_host writers are claim-time recording (assign/approve,
// covered above), a HEALTHY spoke's own positive report
// (reconcileGitHubHostFromSpoke), and the mis-flip restore
// (repairMisflippedForgeFromRequest).
func TestNoPerBeatClusterHostFill(t *testing.T) {
	useTempHiveDir(t)
	s := newGHETestHub()

	// A claimed hive with a blank host on the GHE cluster — the shape the old
	// per-beat fill rewrote to the cluster forge, flipping github.com projects.
	h := &SaaSHive{ID: "hosted-blank", Owner: "a", Org: "ibm", Status: statusAssigned,
		Repos: []string{"alchemy-logging"}, PrimaryRepo: "alchemy-logging",
		ACMMLevel: 2, ClusterID: defaultClusterID}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// A beat from a healthy public spoke must leave the record blank (=public).
	payload := &HeartbeatPayload{HiveID: h.ID, ClusterID: defaultClusterID}
	s.reconcileGitHubHostFromSpoke(payload)
	s.repairMisflippedForgeFromRequest(payload)
	if got := loadSaaSHive(h.ID); got == nil || got.GitHubHost != "" {
		t.Errorf("GitHubHost = %q, want blank — the cluster's forge must never be written into a claimed hive's record on a heartbeat", got.GitHubHost)
	}

	// And the per-hive resolver reads that blank as PUBLIC, not as the cluster.
	cluster := s.clusters[defaultClusterID]
	if id := ResolveHiveIdentity(loadSaaSHive(h.ID), &cluster); !sameGitHubHost(id.Forge, publicForgeHost) {
		t.Errorf("resolved forge = %q, want public — empty github_host on a claimed hive means github.com", id.Forge)
	}
}

// TestRepairedHostReachesSpokeViaHeartbeat is the end-to-end trace for a
// recorded GHE host: once a hive's OWN github_host names the GHE forge (set at
// claim time, by a healthy spoke's report, or by an operator), the heartbeat
// must push the GHE API URL to a spoke still reporting the public one.
//
// Before the needGHEAPIPush gate this failed: ClaimDelivered == true and a
// matching vanity URL made every push condition false, so the reconcile
// returned nil forever and the recorded host never left the hub.
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

	// With no recorded host the hub has nothing to say — and, per-hive doctrine,
	// nothing on the beat path fills one in from the cluster.
	if pc := projectConfigForHiveID(h.ID, h.Org, h.Repos, h.PrimaryRepo, h.ACMMLevel, "", publicAPIURL); pc != nil {
		t.Fatalf("expected no push before the host is recorded, got %+v", pc)
	}

	// A HEALTHY spoke positively reporting the GHE forge records the host — the
	// per-hive evidence path that replaced the cluster-default fill.
	if !s.reconcileGitHubHostFromSpoke(&HeartbeatPayload{
		HiveID: h.ID, ClusterID: defaultClusterID, GitHubHost: gheTestHost, GitHubAPIURL: gheTestAPIURL,
	}) {
		t.Fatal("a healthy spoke's positive GHE report did not record the host")
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
