package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestGitHubAppStructuredFailure(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		detail     string
		wantClass  string
		wantStatus int
	}{
		{"not installed token 404", github.AppStateNotInstalled.String(), "creating installation token: POST https://github.ibm.com/api/v3/app/installations/456/access_tokens: 404", "installation-not-found", 404},
		{"wrong installation", github.AppStateWrongInstallation.String(), "", "wrong-installation", 0},
		{"key invalid", github.AppStateKeyInvalid.String(), "401", "key-invalid", 401},
		{"key missing", github.AppStateKeyMissing.String(), "", "key-missing", 0},
		{"insufficient permissions", github.AppStateInsufficientPerms.String(), "403", "insufficient-permissions", 403},
		{"repo not covered", github.AppStateRepoNotCovered.String(), "404", "repo-not-covered", 404},
		{"repo moved", github.AppStateRepoMoved.String(), "", "repo-moved", 0},
		{"write forbidden", github.AppStateWriteForbidden.String(), "403", "write-forbidden", 403},
		{"generic token error", "", "creating installation token failed: 500", "token-error", 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, gotStatus := githubAppStructuredFailure(tc.state, tc.detail)
			if gotClass != tc.wantClass || gotStatus != tc.wantStatus {
				t.Fatalf("githubAppStructuredFailure() = (%q, %d), want (%q, %d)",
					gotClass, gotStatus, tc.wantClass, tc.wantStatus)
			}
		})
	}
}
