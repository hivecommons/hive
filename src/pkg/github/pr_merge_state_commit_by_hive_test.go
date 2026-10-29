package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An email-only hive identity (no login resolved) counts as the hive's own
// only on a commit GitHub verified; a login resolved to the App bot counts
// whatever the verification (hivecommons/hive#15182).
func TestCommitByHive(t *testing.T) {
	t.Setenv(signedTrailerDomainEnv, "")
	cases := []struct {
		name         string
		login, email string
		verification map[string]any
		want         bool
	}{
		{"email only, verified", "", "quality@hive.kubestellar.io", map[string]any{"verified": true}, true},
		{"email only, unverified", "", "quality@hive.kubestellar.io", map[string]any{"verified": false}, false},
		{"email only, no verification", "", "quality@hive.kubestellar.io", nil, false},
		{"bot noreply email only, unverified", "", "1+hive-app[bot]@users.noreply.github.com", map[string]any{"verified": false}, false},
		{"bot login, unverified", reconcileBot, "anything@example.com", map[string]any{"verified": false}, true},
		{"bot login, no verification", reconcileBot, "anything@example.com", nil, true},
		{"person login, verified", "alice", "quality@hive.kubestellar.io", map[string]any{"verified": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/widget/commits/abc123" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
				}
				ident := map[string]any{"name": "n", "email": tc.email}
				commit := map[string]any{"author": ident, "committer": ident, "message": "m"}
				if tc.verification != nil {
					commit["verification"] = tc.verification
				}
				body := map[string]any{"sha": "abc123", "commit": commit}
				if tc.login != "" {
					body["author"] = map[string]any{"login": tc.login}
					body["committer"] = map[string]any{"login": tc.login}
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
			c.SetAppBotLogin(reconcileBot)
			got, err := c.CommitByHive(context.Background(), "acme/widget", "abc123")
			if err != nil {
				t.Fatalf("CommitByHive: %v", err)
			}
			if got != tc.want {
				t.Errorf("CommitByHive = %v, want %v", got, tc.want)
			}
		})
	}
}
