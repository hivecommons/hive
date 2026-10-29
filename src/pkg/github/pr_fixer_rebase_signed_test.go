package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The signing step for the fixer's rebase of a contributor PR
// (hivecommons/hive#15182): an unverified, email-only pane-identity head the
// App force-pushed over the approved head is sealed as one GitHub-signed
// merge commit (approved head + the base it was rebased onto, the fixer's
// tree, no custom author or committer so GitHub signs it); anything else is
// left alone or refused without a write.
func TestSignFixerRebase(t *testing.T) {
	t.Setenv(signedTrailerDomainEnv, "")
	const (
		approved = "aaaa000000000000000000000000000000000001"
		rebased  = "bbbb000000000000000000000000000000000002"
		onto     = "cccc000000000000000000000000000000000003"
		tree     = "dddd000000000000000000000000000000000004"
		signed   = "eeee000000000000000000000000000000000005"
		other    = "ffff000000000000000000000000000000000006"
	)
	type push struct{ typename, login, before, after string }
	appPush := push{"Bot", "hive-app", approved, rebased}
	cases := []struct {
		name           string
		committerLogin string
		committerEmail string
		verified       bool
		headRepo       string
		pushes         []push
		refSHA         string
		createStatus   int
		want           string
		wantErr        bool
		wantMoved      bool
		wantGraphQL    bool
		wantWrites     bool
	}{
		{name: "fixer rebase, App force-push over the approved head", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{appPush}, want: signed, wantGraphQL: true, wantWrites: true},
		{name: "REST bot login spelling", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{{"Bot", reconcileBot, approved, rebased}}, want: signed, wantGraphQL: true, wantWrites: true},
		{name: "fork head the App can write", committerEmail: "fixer@hive.kubestellar.io", headRepo: "alice/r", pushes: []push{appPush}, want: signed, wantGraphQL: true, wantWrites: true},
		{name: "fork head the App cannot write", committerEmail: "fixer@hive.kubestellar.io", headRepo: "alice/r", pushes: []push{appPush}, createStatus: http.StatusForbidden, wantErr: true, wantGraphQL: true},
		{name: "committer resolves to a login", committerLogin: "alice", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{appPush}},
		{name: "already verified", committerEmail: "fixer@hive.kubestellar.io", verified: true, headRepo: "o/r", pushes: []push{appPush}},
		{name: "committer outside the pane domain", committerEmail: "alice@example.com", headRepo: "o/r", pushes: []push{appPush}},
		{name: "a person force-pushed the pane-identity head", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{{"User", "alice", approved, rebased}}, wantErr: true, wantGraphQL: true},
		{name: "App force-push over another head", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{{"Bot", "hive-app", other, rebased}}, wantErr: true, wantGraphQL: true},
		{name: "no force-push of this head", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{{"Bot", "hive-app", approved, other}}, wantErr: true, wantGraphQL: true},
		{name: "head moved before signing", committerEmail: "fixer@hive.kubestellar.io", headRepo: "o/r", pushes: []push{appPush}, refSHA: other, wantErr: true, wantMoved: true, wantGraphQL: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu        sync.Mutex
				graphQLs  int
				created   map[string]any
				updated   map[string]any
				writePath []string
			)
			headPrefix := "/repos/" + tc.headRepo + "/git/"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/commits/"+rebased:
					commit := map[string]any{
						"author":       map[string]any{"name": "alice", "email": "alice@example.com"},
						"committer":    map[string]any{"name": "fixer", "email": tc.committerEmail},
						"message":      "feat: alice's change",
						"tree":         map[string]any{"sha": tree},
						"verification": map[string]any{"verified": tc.verified},
					}
					body := map[string]any{"sha": rebased, "commit": commit, "author": map[string]any{"login": "alice"}}
					if tc.committerLogin != "" {
						body["committer"] = map[string]any{"login": tc.committerLogin}
					}
					_ = json.NewEncoder(w).Encode(body)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					graphQLs++
					nodes := make([]map[string]any, 0, len(tc.pushes))
					for _, p := range tc.pushes {
						nodes = append(nodes, map[string]any{"actor": map[string]any{"__typename": p.typename, "login": p.login}, "beforeCommit": map[string]any{"oid": p.before}, "afterCommit": map[string]any{"oid": p.after}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
						"headRefName": "feature", "baseRefName": "main",
						"headRepository": map[string]any{"nameWithOwner": tc.headRepo},
						"timelineItems":  map[string]any{"nodes": nodes},
					}}}})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/compare/main..."+rebased:
					_ = json.NewEncoder(w).Encode(map[string]any{"merge_base_commit": map[string]any{"sha": onto}})
				case r.Method == http.MethodGet && r.URL.Path == headPrefix+"ref/heads/feature":
					sha := rebased
					if tc.refSHA != "" {
						sha = tc.refSHA
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/feature", "object": map[string]any{"sha": sha}})
				case r.Method == http.MethodPost && r.URL.Path == headPrefix+"commits":
					writePath = append(writePath, r.URL.Path)
					if tc.createStatus != 0 {
						w.WriteHeader(tc.createStatus)
						_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
						return
					}
					_ = json.NewDecoder(r.Body).Decode(&created)
					_ = json.NewEncoder(w).Encode(map[string]any{"sha": signed})
				case r.Method == http.MethodPatch && r.URL.Path == headPrefix+"refs/heads/feature":
					writePath = append(writePath, r.URL.Path)
					_ = json.NewDecoder(r.Body).Decode(&updated)
					_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/feature", "object": map[string]any{"sha": signed}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := NewClientForTest(server.URL, "o", []string{"r"}, nil)
			c.SetAppBotLogin(reconcileBot)

			got, err := c.SignFixerRebase(context.Background(), "o/r", 8, approved, rebased)
			if (err != nil) != tc.wantErr {
				t.Fatalf("SignFixerRebase err = %v, wantErr %v", err, tc.wantErr)
			}
			if IsSignedHeadMoved(err) != tc.wantMoved {
				t.Fatalf("IsSignedHeadMoved(%v) = %v, want %v", err, !tc.wantMoved, tc.wantMoved)
			}
			if got != tc.want {
				t.Fatalf("SignFixerRebase = %q, want %q", got, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if (graphQLs > 0) != tc.wantGraphQL {
				t.Fatalf("graphql calls = %d, want any: %v", graphQLs, tc.wantGraphQL)
			}
			if !tc.wantWrites {
				if updated != nil {
					t.Fatalf("a refused head must not move the branch: %v", updated)
				}
				if tc.createStatus == 0 && len(writePath) != 0 {
					t.Fatalf("a refused head must not write: %v", writePath)
				}
				return
			}
			if created == nil || updated == nil {
				t.Fatalf("writes: created=%v updated=%v", created, updated)
			}
			if _, ok := created["author"]; ok {
				t.Fatalf("a custom author stops GitHub signing the commit: %v", created)
			}
			if _, ok := created["committer"]; ok {
				t.Fatalf("a custom committer stops GitHub signing the commit: %v", created)
			}
			parents, _ := created["parents"].([]any)
			if len(parents) != 2 || parents[0] != approved || parents[1] != onto {
				t.Fatalf("parents = %v, want [approved head, rebase base]", created["parents"])
			}
			if created["tree"] != tree {
				t.Fatalf("tree = %v, want the fixer's tree %s", created["tree"], tree)
			}
			msg, _ := created["message"].(string)
			if !strings.HasPrefix(msg, "Merge main into feature\n") || !strings.Contains(msg, BotSignedOffByTrailer(reconcileBot)) {
				t.Fatalf("message = %q", msg)
			}
			if updated["sha"] != signed || updated["force"] != true {
				t.Fatalf("ref update = %v", updated)
			}
		})
	}
}
