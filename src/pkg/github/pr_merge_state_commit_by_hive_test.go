package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An email-only hive committer identity (no login resolved) counts as the
// hive's own only on a commit GitHub verified; a login resolved to the App
// bot counts whatever the verification. GitHub's verification binds the
// signing key to the committer address only, so an email-only author match
// never counts, verified or not (hivecommons/hive#15182); an author login
// resolved to the App bot counts only on a verified commit, since GitHub
// derives it from the free-text author email (hivecommons/hive#15421).
func TestCommitByHive(t *testing.T) {
	t.Setenv(signedTrailerDomainEnv, "")
	type identity struct{ login, email string }
	cases := []struct {
		name              string
		author, committer identity
		verification      map[string]any
		want              bool
	}{
		{"email only, verified", identity{"", "quality@hive.kubestellar.io"}, identity{"", "quality@hive.kubestellar.io"}, map[string]any{"verified": true}, true},
		{"email only, unverified", identity{"", "quality@hive.kubestellar.io"}, identity{"", "quality@hive.kubestellar.io"}, map[string]any{"verified": false}, false},
		{"email only, no verification", identity{"", "quality@hive.kubestellar.io"}, identity{"", "quality@hive.kubestellar.io"}, nil, false},
		{"bot noreply email only, unverified", identity{"", "1+hive-app[bot]@users.noreply.github.com"}, identity{"", "1+hive-app[bot]@users.noreply.github.com"}, map[string]any{"verified": false}, false},
		{"bot login, unverified", identity{reconcileBot, "anything@example.com"}, identity{reconcileBot, "anything@example.com"}, map[string]any{"verified": false}, true},
		{"bot login, no verification", identity{reconcileBot, "anything@example.com"}, identity{reconcileBot, "anything@example.com"}, nil, true},
		{"person login, verified", identity{"alice", "quality@hive.kubestellar.io"}, identity{"alice", "quality@hive.kubestellar.io"}, map[string]any{"verified": true}, false},
		// A signer forging --author into the pane domain: the signature vouches
		// for alice as committer, never for the author address.
		{"person committer, email-only pane author, verified", identity{"", "quality@hive.kubestellar.io"}, identity{"alice", "alice@example.com"}, map[string]any{"verified": true}, false},
		{"person committer, email-only bot noreply author, verified", identity{"", "1+hive-app[bot]@users.noreply.github.com"}, identity{"alice", "alice@example.com"}, map[string]any{"verified": true}, false},
		// update-branch under the App: author resolves to the bot, GitHub commits.
		{"bot login author, web-flow committer", identity{reconcileBot, "1+hive-app[bot]@users.noreply.github.com"}, identity{"web-flow", "noreply@github.com"}, map[string]any{"verified": true}, true},
		{"bot login author, web-flow committer, unverified", identity{reconcileBot, "1+hive-app[bot]@users.noreply.github.com"}, identity{"web-flow", "noreply@github.com"}, nil, false},
		// A pusher setting user.email to the App's noreply address gets the bot
		// login resolved as author on an unsigned commit (hivecommons/hive#15421).
		{"spoofed bot login author, person committer, unverified", identity{reconcileBot, "1+hive-app[bot]@users.noreply.github.com"}, identity{"alice", "alice@example.com"}, map[string]any{"verified": false}, false},
		{"spoofed bot login author, no committer login, no verification", identity{reconcileBot, "1+hive-app[bot]@users.noreply.github.com"}, identity{"", "alice@example.com"}, nil, false},
		// The hive's own unsigned push: the committer resolves to the App bot.
		{"bot login author, bot login committer, unverified", identity{reconcileBot, "1+hive-app[bot]@users.noreply.github.com"}, identity{reconcileBot, "1+hive-app[bot]@users.noreply.github.com"}, map[string]any{"verified": false}, true},
		// The fixer's rebase keeps the person as author; the hive commits.
		{"person author, email-only pane committer, verified", identity{"alice", "alice@example.com"}, identity{"", "fixer@hive.kubestellar.io"}, map[string]any{"verified": true}, true},
		{"person author, email-only pane committer, unverified", identity{"alice", "alice@example.com"}, identity{"", "fixer@hive.kubestellar.io"}, map[string]any{"verified": false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/widget/commits/abc123" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
				}
				commit := map[string]any{
					"author":    map[string]any{"name": "n", "email": tc.author.email},
					"committer": map[string]any{"name": "n", "email": tc.committer.email},
					"message":   "m",
				}
				if tc.verification != nil {
					commit["verification"] = tc.verification
				}
				body := map[string]any{"sha": "abc123", "commit": commit}
				if tc.author.login != "" {
					body["author"] = map[string]any{"login": tc.author.login}
				}
				if tc.committer.login != "" {
					body["committer"] = map[string]any{"login": tc.committer.login}
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
