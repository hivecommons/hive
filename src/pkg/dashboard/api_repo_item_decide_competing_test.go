package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

func delegatedCompetitionServer(t *testing.T, routes map[string]string) *Server {
	t.Helper()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("gather must be read-only: %s %s", r.Method, r.URL)
			http.Error(w, "write forbidden", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		key := r.URL.Path
		if !strings.HasPrefix(key, "/repos/o/r/") {
			t.Errorf("unexpected out-of-scope read: %s", key)
		}
		if r.URL.Query().Get("page") == "2" {
			key += "?page=2"
		}
		body, ok := routes[key]
		if !ok || body == "" {
			http.NotFound(w, r)
			return
		}
		if _, paginated := routes[key+"?page=2"]; paginated {
			w.Header().Set("Link", "<http://"+r.Host+key+"?page=2>; rel=\"next\"")
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(mock.Close)
	s := newFullServer(t)
	s.deps.GHClient = ghpkg.NewClient("token", "o", []string{"r"}, slog.Default(), mock.URL)
	s.deps.GHClient.SetAppBotLogin("hive[bot]")
	return s
}

func competitionRoutes() map[string]string {
	return map[string]string{
		"/repos/o/r/issues/7":          `{"number":7,"state":"open","user":{"login":"reporter"}}`,
		"/repos/o/r/issues/7/comments": `[]`,
		"/repos/o/r/pulls":             `[{"number":8,"state":"open","body":"Fixes #7"},{"number":12,"state":"open","body":"Fixes other/repo#7"}]`,
		"/repos/o/r/pulls?page=2":      `[{"number":9,"state":"open","body":"Fixes #7"}]`,
		"/repos/o/r/pulls/9":           `{"number":9,"state":"open","user":{"login":"alice"},"mergeable":true,"mergeable_state":"clean"}`,
		"/repos/o/r/issues/8/comments": `[]`,
		"/repos/o/r/pulls/8/commits":   `[]`,
		"/repos/o/r/pulls/8/comments":  `[]`,
		"/repos/o/r/pulls/8/reviews":   `[]`,
	}
}

func competitionPR(t *testing.T, author string, now time.Time) *gh.PullRequest {
	t.Helper()
	pr := &gh.PullRequest{Number: gh.Ptr(8), Body: gh.Ptr("Fixes #7"), User: &gh.User{Login: gh.Ptr(author)}, CreatedAt: &gh.Timestamp{Time: now.Add(-24 * time.Hour)}}
	return pr
}

func TestDelegatedGatherCompetingPrefersRealContributor(t *testing.T) {
	routes := competitionRoutes()
	s := delegatedCompetitionServer(t, routes)
	in := delegatedecision.Input{Repo: "o/r", Number: 8, Kind: delegatedecision.KindPR, HiveAuthored: true, CIGreen: true}
	if err := s.gatherDelegatedCompetition(context.Background(), "o/r", competitionPR(t, "hive[bot]", time.Now()), &in, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(in.Competing) != 1 || in.Competing[0].Number != 9 || !in.Competing[0].Mergeable || !in.Competing[0].Current || in.Competing[0].HiveAuthored {
		t.Fatalf("competing = %+v", in.Competing)
	}
	d := delegatedecision.Run(in)
	if d.Rule != "prefer-contributor" || d.Verdict != delegatedecision.VerdictCloseSuperseded {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDelegatedCompetingInadequateHeadsNeverCloseHivePR(t *testing.T) {
	for _, detail := range []string{
		`{"number":9,"state":"closed","mergeable":true,"mergeable_state":"clean"}`,
		`{"number":9,"state":"open","draft":true,"mergeable":true,"mergeable_state":"clean"}`,
		`{"number":9,"state":"open","mergeable":true,"mergeable_state":"behind"}`,
		`{"number":9,"state":"open","mergeable":true,"mergeable_state":"unstable"}`,
		`{"number":9,"state":"open","mergeable_state":"unknown"}`,
	} {
		t.Run(detail, func(t *testing.T) {
			routes := competitionRoutes()
			routes["/repos/o/r/pulls/9"] = detail
			s := delegatedCompetitionServer(t, routes)
			in := delegatedecision.Input{Number: 8, Kind: delegatedecision.KindPR, HiveAuthored: true, CIGreen: true}
			if err := s.gatherDelegatedCompetition(context.Background(), "o/r", competitionPR(t, "hive[bot]", time.Now()), &in, time.Now()); err != nil {
				t.Fatal(err)
			}
			if d := delegatedecision.Run(in); d.Verdict == delegatedecision.VerdictCloseSuperseded {
				t.Fatalf("unsafe duplicate close: %+v", d)
			}
		})
	}
}

func TestDelegatedContributorPresenceUsesAuthorActivity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	stamp := now.Format(time.RFC3339)
	comment := `[{"user":{"login":"alice"},"created_at":"` + stamp + `"}]`
	for _, tc := range []struct {
		name, path, body string
		online           bool
	}{
		{"quiet", "", "", false},
		{"assignee with unrelated update", "/repos/o/r/issues/7", `{"number":7,"state":"open","user":{"login":"reporter"},"assignees":[{"login":"alice"}],"updated_at":"` + stamp + `"}`, false},
		{"PR comment", "/repos/o/r/issues/8/comments", comment, true},
		{"issue comment", "/repos/o/r/issues/7/comments", comment, true},
		{"later page comment", "/repos/o/r/issues/8/comments?page=2", comment, true},
		{"old comment", "/repos/o/r/issues/8/comments", strings.ReplaceAll(comment, stamp, now.Add(-5*time.Hour).Format(time.RFC3339)), false},
		{"inline review comment", "/repos/o/r/pulls/8/comments", comment, true},
		{"review", "/repos/o/r/pulls/8/reviews", `[{"user":{"login":"alice"},"submitted_at":"` + stamp + `"}]`, true},
		{"commit", "/repos/o/r/pulls/8/commits", `[{"author":{"login":"alice"},"commit":{"author":{"date":"` + stamp + `"}}}]`, true},
		{"other person's comment", "/repos/o/r/issues/7/comments", strings.ReplaceAll(comment, "alice", "maintainer"), false},
		{"bot commit", "/repos/o/r/pulls/8/commits", `[{"committer":{"login":"hive[bot]"},"commit":{"committer":{"date":"` + stamp + `"}}}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := competitionRoutes()
			if tc.path != "" {
				routes[tc.path] = tc.body
			}
			s := delegatedCompetitionServer(t, routes)
			in := delegatedecision.Input{Number: 8, Kind: delegatedecision.KindPR, Conflicting: true}
			pr := competitionPR(t, "alice", now)
			pr.UpdatedAt = &gh.Timestamp{Time: now} // unrelated updates must not count
			if err := s.gatherDelegatedCompetition(context.Background(), "o/r", pr, &in, now); err != nil {
				t.Fatal(err)
			}
			if in.ContributorOnline != tc.online {
				t.Fatalf("online = %v, want %v", in.ContributorOnline, tc.online)
			}
			d := delegatedecision.Run(in)
			if (d.Rule == "contributor-offline") == tc.online {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestDelegatedPresenceResolvesTrustedLiveDibs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, author, identity string
		expiry                 time.Time
		online                 bool
	}{
		{"live", "hive[bot]", "alice", now.Add(time.Hour), true},
		{"expired", "hive[bot]", "alice", now.Add(-time.Hour), false},
		{"forged", "mallory", "alice", now.Add(time.Hour), false},
		{"different contributor", "hive[bot]", "bob", now.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := competitionRoutes()
			body, err := json.Marshal([]map[string]any{{"user": map[string]string{"login": tc.author, "type": "Bot"}, "body": issueclaim.Marker(tc.identity, now.Add(-2*time.Hour), tc.expiry)}})
			if err != nil {
				t.Fatal(err)
			}
			routes["/repos/o/r/issues/7/comments"] = string(body)
			s := delegatedCompetitionServer(t, routes)
			in := delegatedecision.Input{}
			if err := s.gatherDelegatedCompetition(context.Background(), "o/r", competitionPR(t, "alice", now), &in, now); err != nil {
				t.Fatal(err)
			}
			if in.ContributorOnline != tc.online {
				t.Fatalf("online = %v, want %v", in.ContributorOnline, tc.online)
			}
		})
	}
}

func TestDelegatedGatherFailsClosedOnMissingEvidence(t *testing.T) {
	for _, path := range []string{"/repos/o/r/pulls?page=2", "/repos/o/r/pulls/9", "/repos/o/r/pulls/8/reviews", "/repos/o/r/issues/7/comments"} {
		t.Run(path, func(t *testing.T) {
			routes := competitionRoutes()
			// Keep the route key so the preceding page still advertises its
			// next link, but return 404 when the missing evidence is fetched.
			routes[path] = ""
			s := delegatedCompetitionServer(t, routes)
			in := delegatedecision.Input{}
			if err := s.gatherDelegatedCompetition(context.Background(), "o/r", competitionPR(t, "alice", time.Now()), &in, time.Now()); err == nil {
				t.Fatal("missing evidence must fail before executing a decision")
			}
		})
	}
}

func TestDelegatedCompetitionRequiresSameRepoClosingClaims(t *testing.T) {
	for _, body := range []string{"Fixes other/repo#7", "Part of #7", "Refs #7"} {
		t.Run(body, func(t *testing.T) {
			s := delegatedCompetitionServer(t, competitionRoutes())
			pr := competitionPR(t, "alice", time.Now())
			pr.Body = gh.Ptr(body)
			in := delegatedecision.Input{}
			if err := s.gatherDelegatedCompetition(context.Background(), "o/r", pr, &in, time.Now()); err != nil {
				t.Fatal(err)
			}
			if len(in.Competing) != 0 {
				t.Fatalf("non-closing or foreign reference produced competitors: %+v", in.Competing)
			}
		})
	}
	for _, body := range []string{"Part of #7", "Refs #7", "Fixes other/repo#7"} {
		if delegatedSharesIssue(body, "o/r", []ghpkg.ClaimedRef{{Repo: "o/r", Issue: 7}}) {
			t.Fatalf("non-closing or foreign competitor matched: %s", body)
		}
	}
}

func TestDelegatedCompetitionSkipsMissingIssueAndKeepsRefsAligned(t *testing.T) {
	routes := competitionRoutes()
	routes["/repos/o/r/issues/7"] = ""
	routes["/repos/o/r/issues/10"] = `{"number":10,"state":"open"}`
	routes["/repos/o/r/issues/10/comments"] = `[]`
	s := delegatedCompetitionServer(t, routes)
	pr := competitionPR(t, "alice", time.Now())
	pr.Body = gh.Ptr("Fixes #7, fixes #10")
	in := delegatedecision.Input{}
	if err := s.gatherDelegatedCompetition(context.Background(), "o/r", pr, &in, time.Now()); err != nil {
		t.Fatal(err)
	}
	if in.ContributorOnline || len(in.Competing) != 0 {
		t.Fatalf("missing issue should provide no evidence: %+v", in)
	}
}

func TestDelegatedGatherFreshActivityOverridesStalledCaption(t *testing.T) {
	routes := competitionRoutes()
	routes["/repos/o/r/issues/8"] = `{"number":8,"state":"open","user":{"login":"alice"},"pull_request":{}}`
	routes["/repos/o/r/pulls/8"] = `{"number":8,"state":"open","user":{"login":"alice"},"created_at":"` + time.Now().UTC().Format(time.RFC3339) + `","mergeable_state":"dirty","body":"Fixes #7"}`
	s := delegatedCompetitionServer(t, routes)
	in, err := s.gatherDelegatedInput(context.Background(), "o/r", 8, "pr", delegatedDecisionRequest{Reason: "adoption-stalled: contributor offline"})
	if err != nil {
		t.Fatal(err)
	}
	if !in.ContributorOnline || in.AdoptionStalled {
		t.Fatalf("stale caption overrode presence: %+v", in)
	}
}
