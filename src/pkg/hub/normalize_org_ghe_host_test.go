package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNormalizeOrgRef_GHEHostOnly is the regression for the claim-path bug that
// produced two broken CLAIMED hives on the vllm-d (github.ibm.com) cluster:
// pasting ONLY the GHE forge host into the "GitHub Organization" field captured
// the HOSTNAME as the org (org="github.ibm.com"), dropping the real org and
// minting a vanity URL from <host>-<repo>. A bare forge host with no path must
// resolve to (host, "") so the caller rejects it, never (,"github.ibm.com").
func TestNormalizeOrgRef_GHEHostOnly(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantHost string
		wantOrg  string
	}{
		// The bug: a lone GHE/public host must NOT become the org.
		{"ghe host bare", "github.ibm.com", "github.ibm.com", ""},
		{"ghe host https", "https://github.ibm.com", "github.ibm.com", ""},
		{"ghe host trailing slash", "https://github.ibm.com/", "github.ibm.com", ""},
		{"public host bare", "github.com", "github.com", ""},
		{"public host https", "https://github.com", "github.com", ""},
		{"another ghe host", "github.cisco.com", "github.cisco.com", ""},

		// Still-correct behaviour: a host WITH an org path splits normally.
		{"ghe host with org", "https://github.ibm.com/z-aiops-unite", "github.ibm.com", "z-aiops-unite"},
		{"ghe host with org no scheme", "github.ibm.com/z-aiops-unite", "github.ibm.com", "z-aiops-unite"},

		// A bare org name (even one containing a dot) is untouched — the fix keys
		// on the "github." forge prefix, so a real org is never reclassified.
		{"bare org", "z-aiops-unite", "", "z-aiops-unite"},
		{"bare org with dot", "my.org", "", "my.org"},
		{"empty", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, org := normalizeOrgRef(tc.in)
			if host != tc.wantHost || org != tc.wantOrg {
				t.Fatalf("normalizeOrgRef(%q) = (%q, %q); want (%q, %q)",
					tc.in, host, org, tc.wantHost, tc.wantOrg)
			}
		})
	}
}

func TestNormalizeProjectRef_GHEHostDoesNotBecomeOrg(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantHost  string
		wantOrg   string
		wantRepos []string
	}{
		{"public org repo", "hivecommons/hive", "", "hivecommons", []string{"hive"}},
		{"ghe host org repo", "github.ibm.com/lee-cooper/toolkit", "github.ibm.com", "lee-cooper", []string{"toolkit"}},
		{"ghe host org", "github.ibm.com/lee-cooper", "github.ibm.com", "lee-cooper", nil},
		{"ghe trailing slash", "https://github.ibm.com/lee-cooper/toolkit/", "github.ibm.com", "lee-cooper", []string{"toolkit"}},
		{"public trailing slash", "hivecommons/hive/", "", "hivecommons", []string{"hive"}},
		{"dotted org is not host", "my.org/repo", "", "my.org", []string{"repo"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, org, repos := normalizeProjectRef(tc.in)
			if host != tc.wantHost || org != tc.wantOrg {
				t.Fatalf("normalizeProjectRef(%q) = (%q, %q, %v); want host/org (%q, %q)",
					tc.in, host, org, repos, tc.wantHost, tc.wantOrg)
			}
			if len(repos) != len(tc.wantRepos) {
				t.Fatalf("normalizeProjectRef(%q) repos = %v; want %v", tc.in, repos, tc.wantRepos)
			}
			for i := range repos {
				if repos[i] != tc.wantRepos[i] {
					t.Fatalf("normalizeProjectRef(%q) repos = %v; want %v", tc.in, repos, tc.wantRepos)
				}
			}
		})
	}
}

// TestAssignHive_RejectsBareGHEHostAsOrg is the end-to-end regression for the
// exact path that produced hosted-available-vllmd-02 / -09: an admin (or the
// claim UI) submits a bare GHE forge host in the org field. The assign handler
// must 400 — never store the hostname as the org. Before the fix, isValidName
// accepted "github.ibm.com" (dots are legal in names), so the host was silently
// stored as the org and the hive was claimed against host-as-org.
func TestAssignHive_RejectsBareGHEHostAsOrg(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newHandlerHub2()

	for _, orgField := range []string{"github.ibm.com", "https://github.ibm.com", "https://github.ibm.com/"} {
		saveSaaSHive(&SaaSHive{ID: "ph-hostorg", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
		body := `{"owner":"bob","org":"` + orgField + `","repos":"forthehive","primary_repo":"forthehive"}`
		rec := httptest.NewRecorder()
		req := setPathValue(reqWithUser(http.MethodPost, "/assign", body, hubAdminUsername), "id", "ph-hostorg")
		s.handleAssignHive(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("assign with org=%q status = %d, want 400 (body=%s)", orgField, rec.Code, rec.Body.String())
		}
		// And it must NOT have persisted the host as the org.
		if got := loadSaaSHive("ph-hostorg"); got != nil && got.Org == "github.ibm.com" {
			t.Fatalf("assign with org=%q stored the host as the org: %q", orgField, got.Org)
		}
	}

	// A proper GHE org URL still assigns cleanly and records the real org + host.
	saveSaaSHive(&SaaSHive{ID: "ph-realorg", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
	body := `{"owner":"bob","org":"https://github.ibm.com/z-aiops-unite","repos":"forthehive","primary_repo":"forthehive"}`
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/assign", body, hubAdminUsername), "id", "ph-realorg")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign with real GHE org URL status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got := loadSaaSHive("ph-realorg")
	if got == nil || got.Org != "z-aiops-unite" {
		t.Fatalf("real GHE org not parsed from URL: %+v", got)
	}
	if got.GitHubHost != "github.ibm.com" {
		t.Errorf("GHE host not recorded from org URL: GitHubHost = %q, want github.ibm.com", got.GitHubHost)
	}
}
