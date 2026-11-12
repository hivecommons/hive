package hub

import "testing"

// A placeholder assigned with an explicit forge must show THAT forge in the
// Location pill while the claim is pending, not the cluster default the spoke
// still heartbeats from its pre-claim config (vllm-d: github.ibm.com). Live
// example: "ash" assigned to thinkscientist with github.com selected read
// github.ibm.com for the whole "claim pending" window.
func TestClaimPendingGitHubHostOverridesSpokeReport(t *testing.T) {
	cases := []struct {
		name      string
		entry     MyHiveEntry
		sh        *SaaSHive
		wantHost  string
		wantApply bool
	}{
		{
			name:      "pending claim with explicit github.com beats GHE heartbeat",
			entry:     MyHiveEntry{RegistryEntry: RegistryEntry{GitHubHost: "github.ibm.com"}, AssignedUnclaimed: true},
			sh:        &SaaSHive{Status: statusAssigned, GitHubHost: "github.com"},
			wantHost:  "github.com",
			wantApply: true,
		},
		{
			name:      "pending claim with explicit GHE beats public heartbeat",
			entry:     MyHiveEntry{RegistryEntry: RegistryEntry{GitHubHost: "github.com"}, AssignedUnclaimed: true},
			sh:        &SaaSHive{Status: statusAssigned, GitHubHost: "https://github.ibm.com/"},
			wantHost:  "github.ibm.com",
			wantApply: true,
		},
		{
			name:      "delivered claim leaves the spoke report authoritative",
			entry:     MyHiveEntry{RegistryEntry: RegistryEntry{GitHubHost: "github.ibm.com"}, AssignedUnclaimed: false},
			sh:        &SaaSHive{Status: statusAssigned, ClaimDelivered: true, GitHubHost: "github.com"},
			wantApply: false,
		},
		{
			name:      "pending claim without a recorded host does not override",
			entry:     MyHiveEntry{RegistryEntry: RegistryEntry{GitHubHost: "github.ibm.com"}, AssignedUnclaimed: true},
			sh:        &SaaSHive{Status: statusAssigned},
			wantApply: false,
		},
		{
			name:      "no hive record does not override",
			entry:     MyHiveEntry{AssignedUnclaimed: true},
			sh:        nil,
			wantApply: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := claimPendingGitHubHost(&tc.entry, tc.sh)
			if ok != tc.wantApply {
				t.Fatalf("apply = %v, want %v", ok, tc.wantApply)
			}
			if ok && got != tc.wantHost {
				t.Errorf("host = %q, want %q", got, tc.wantHost)
			}
		})
	}
}
