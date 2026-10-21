package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReporterTrustUpstreamAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, source, association, login, merged   string
		pr, resolverError, localAccepted, wantHeld bool
	}{
		{name: "merged configured upstream", source: "up/source", login: "stranger", pr: true, merged: `"merged":true,"merged_at":"2026-01-01T00:00:00Z"`},
		{name: "upstream issue", source: "up/source", login: "stranger", wantHeld: true},
		{name: "unmerged upstream PR", source: "up/source", login: "stranger", pr: true, merged: `"merged":false`, wantHeld: true},
		{name: "closed without merge", source: "up/source", login: "stranger", pr: true, merged: `"state":"closed","merged":false`, wantHeld: true},
		{name: "merge lookup failure", source: "up/source", login: "stranger", pr: true, wantHeld: true},
		{name: "upstream resolution failure", source: "up/source", login: "stranger", pr: true, resolverError: true, wantHeld: true},
		{name: "watch disabled or repo absent", login: "stranger", pr: true, wantHeld: true},
		{name: "unrelated merged PR", source: "other/repo", login: "stranger", pr: true, merged: `"merged":true,"merged_at":"2026-01-01T00:00:00Z"`, wantHeld: true},
		{name: "foreign owner is not local trust", association: "OWNER", login: "stranger", wantHeld: true},
		{name: "foreign member is not local trust", association: "MEMBER", login: "stranger", wantHeld: true},
		{name: "foreign collaborator is not local trust", association: "COLLABORATOR", login: "stranger", wantHeld: true},
		{name: "explicit login still trusted", association: "MEMBER", login: "trusted"},
		{name: "foreign acceptance label is not local acceptance", login: "stranger", localAccepted: true, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/o/r/issues/1":
					fmt.Fprint(w, `{"number":1,"user":{"login":"hive[bot]","type":"Bot"}}`)
				case "/repos/up/source/issues/613":
					extra := ""
					if tc.pr {
						extra += `,"pull_request":{"url":"https://api.github.com/repos/up/source/pulls/613"}`
					}
					if tc.localAccepted {
						extra += `,"labels":[{"name":"triage/accepted"}]`
					}
					fmt.Fprintf(w, `{"number":613,"user":{"login":%q,"type":"User"},"author_association":%q%s}`, tc.login, tc.association, extra)
				case "/repos/up/source/pulls/613":
					if tc.merged == "" {
						http.Error(w, "lookup failed", 500)
						return
					}
					fmt.Fprintf(w, `{"number":613,%s}`, tc.merged)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := testClient(t, srv.URL)
			c.SetReporterTrusted(func(login, association string) bool {
				return login == "trusted" || association == "OWNER" || association == "MEMBER" || association == "COLLABORATOR"
			})
			c.SetReporterTrustUpstream(func(_ context.Context, repo string) (string, error) {
				if repo != "o/r" {
					t.Errorf("resolver repo = %s", repo)
				}
				if tc.resolverError {
					return "", fmt.Errorf("cannot resolve upstream")
				}
				return tc.source, nil
			})
			got := c.EvaluateReporterTrust(context.Background(), "o/r", "Port", "Closes #1\nRefs up/source#613", nil)
			if got.Held != tc.wantHeld {
				t.Fatalf("Held=%v, want %v: %+v", got.Held, tc.wantHeld, got)
			}
			if got.Held && !strings.Contains(got.Reason, tc.login) {
				t.Fatalf("missing reporter: %+v", got)
			}
			// Omitting the link must not block an otherwise accepted port.
			without := c.EvaluateReporterTrust(context.Background(), "o/r", "Port", "Closes #1", nil)
			if without.Held {
				t.Fatalf("hive-filed port issue held: %+v", without)
			}
		})
	}
}
