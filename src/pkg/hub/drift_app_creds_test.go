package hub

import (
	"strings"
	"testing"
	"time"
)

// TestComputeDrift_OperatorSideAppCredsAreNotBlamedOnTheOwner asserts drift
// reports name the operator when the App key is the problem, and keep the
// existing owner-facing text otherwise.
func TestComputeDrift_OperatorSideAppCredsAreNotBlamedOnTheOwner(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		permIssue string
		wantKind  string
		// wantDetailHas / wantDetailLacks assert the copy is accurate.
		wantDetailHas   []string
		wantDetailLacks []string
	}{
		{
			name:            "missing key blames the operator",
			state:           appStateKeyMissingToken,
			permIssue:       "credentials not provisioned",
			wantKind:        DriftKindAppCredsOperator,
			wantDetailHas:   []string{"only an operator can fix", "not been delivered"},
			wantDetailLacks: []string{"is not installed", "permissions are insufficient"},
		},
		{
			name:            "invalid key blames the operator",
			state:           appStateKeyInvalidToken,
			permIssue:       "jwt rejected",
			wantKind:        DriftKindAppCredsOperator,
			wantDetailHas:   []string{"only an operator can fix", "does not match the App"},
			wantDetailLacks: []string{"is not installed"},
		},
		{
			name:          "a genuine permission issue keeps the existing text",
			state:         "insufficient-permissions",
			permIssue:     "Issues: read",
			wantKind:      DriftKindAppPermIssue,
			wantDetailHas: []string{"permissions are insufficient"},
		},
		{
			name:          "a genuinely uninstalled App still says so",
			state:         "not-installed",
			permIssue:     "",
			wantKind:      DriftKindAppMissing,
			wantDetailHas: []string{"not installed"},
		},
		{
			name:          "an old spoke reporting no state keeps the old behaviour",
			state:         "",
			permIssue:     "",
			wantKind:      DriftKindAppMissing,
			wantDetailHas: []string{"not installed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := MyHiveEntry{Role: "owner"}
			h.ID = "hive-1"
			h.Name = "acme/widget"
			h.GitHubAppRequired = true
			h.GitHubAppPermIssue = tt.permIssue
			h.GitHubAppState = tt.state
			// Claimed (non-placeholder) so the App signals are evaluated.
			h.Owner = "someone"
			h.ACMMLevel = 3
			h.AgentCount = 1

			rep := computeDrift(h, fleetNorm{}, nil, time.Now())

			var found *DriftSignal
			for i := range rep.Signals {
				if rep.Signals[i].Kind == tt.wantKind {
					found = &rep.Signals[i]
					break
				}
			}
			if found == nil {
				var kinds []string
				for _, s := range rep.Signals {
					kinds = append(kinds, s.Kind)
				}
				t.Fatalf("no %q signal; got kinds %v", tt.wantKind, kinds)
			}
			for _, want := range tt.wantDetailHas {
				if !strings.Contains(found.Reason, want) {
					t.Errorf("detail = %q, want it to contain %q", found.Reason, want)
				}
			}
			for _, lack := range tt.wantDetailLacks {
				if strings.Contains(found.Reason, lack) {
					t.Errorf("detail = %q, must NOT contain %q", found.Reason, lack)
				}
			}
			// An operator-side failure must never ALSO be filed as an
			// owner-facing adoption problem.
			if tt.wantKind == DriftKindAppCredsOperator {
				for _, s := range rep.Signals {
					if s.Kind == DriftKindAppMissing || s.Kind == DriftKindAppPermIssue {
						t.Errorf("operator-side creds also produced an owner-facing %q signal: %q", s.Kind, s.Reason)
					}
				}
			}
		})
	}
}
