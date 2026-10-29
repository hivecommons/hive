package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDelegatedCompetingNumbers(t *testing.T) {
	pr := &gh.PullRequest{Number: gh.Ptr(8), Body: gh.Ptr("Fixes #7")}
	items := []StageTrackerItem{
		{Repo: "org/repo", Kind: "issue", Number: 7, LinkedPRs: []StageTrackerLinkedPR{{Number: 8}, {Number: 9}, {Number: 9}}},
		{Repo: "org/repo", Kind: "pr", Number: 10, LinkedIssues: []StageTrackerLinkedIssue{{Number: 7}}},
		{Repo: "org/repo", Kind: "pr", Number: 11, LinkedIssues: []StageTrackerLinkedIssue{{Number: 17}}},
		{Repo: "else/repo", Kind: "issue", Number: 7, LinkedPRs: []StageTrackerLinkedPR{{Number: 8}, {Number: 12}}},
	}
	before, _ := json.Marshal(items)
	if got := delegatedCompetingNumbers(items, "org/repo", pr); !reflect.DeepEqual(got, []int{9, 10}) {
		t.Fatalf("competing = %v", got)
	}
	after, _ := json.Marshal(items)
	if string(before) != string(after) {
		t.Fatal("mutated shared link evidence")
	}
}

func TestDelegatedContributorActivity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-time.Hour).Format(time.RFC3339)
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	for _, tc := range []struct {
		name, endpoint, payload string
		online, fail            bool
	}{
		{name: "quiet", online: false},
		{name: "future commit", endpoint: "commits", payload: `[{"author":{"login":"alice"},"commit":{"author":{"date":"` + now.Add(24*time.Hour).Format(time.RFC3339) + `"}}}]`},
		{name: "commit", endpoint: "commits", payload: `[{"author":{"login":"alice"},"commit":{"author":{"date":"` + fresh + `"}}}]`, online: true},
		{name: "unlinked email commit", endpoint: "commits", payload: `[{"commit":{"author":{"date":"` + fresh + `"},"committer":{"date":"` + old + `"}}}]`, online: true},
		{name: "unlinked email old commit", endpoint: "commits", payload: `[{"commit":{"author":{"date":"` + old + `"}}}]`},
		{name: "other author commit", endpoint: "commits", payload: `[{"author":{"login":"bob"},"committer":{"login":"bob"},"commit":{"author":{"date":"` + fresh + `"},"committer":{"date":"` + fresh + `"}}}]`},
		{name: "comment", endpoint: "issues/8/comments", payload: `[{"user":{"login":"alice"},"updated_at":"` + fresh + `"}]`, online: true},
		{name: "review", endpoint: "reviews", payload: `[{"user":{"login":"alice"},"submitted_at":"` + fresh + `"}]`, online: true},
		{name: "inline", endpoint: "pulls/8/comments", payload: `[{"user":{"login":"alice"},"updated_at":"` + fresh + `"}]`, online: true},
		{name: "bot nudge", endpoint: "issues/8/comments", payload: `[{"user":{"login":"hive[bot]"},"updated_at":"` + fresh + `"}]`},
		{name: "other reviewer", endpoint: "reviews", payload: `[{"user":{"login":"bob"},"submitted_at":"` + fresh + `"}]`},
		{name: "old author comment", endpoint: "issues/8/comments", payload: `[{"user":{"login":"alice"},"updated_at":"` + old + `"}]`},
		{name: "failed read", endpoint: "commits", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.endpoint != "" && strings.HasSuffix(r.URL.Path, "/"+tc.endpoint) {
					if tc.fail {
						http.Error(w, "unavailable", 503)
						return
					}
					_, _ = w.Write([]byte(tc.payload))
					return
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer server.Close()
			s := newFullServer(t)
			s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
			pr := &gh.PullRequest{Number: gh.Ptr(8), User: &gh.User{Login: gh.Ptr("alice")}, CreatedAt: &gh.Timestamp{Time: now.Add(-time.Hour)}}
			online, err := s.delegatedContributorOnline(context.Background(), "org", "repo", pr, now.Add(-15*time.Minute))
			if (err != nil) != tc.fail || online != tc.online {
				t.Fatalf("online=%v err=%v", online, err)
			}
		})
	}
}

func TestGatherDelegatedRealPRContext(t *testing.T) {
	for _, tc := range []struct {
		name, state, mergeState       string
		mergeable, draft, redCI, noCI bool
		want                          int
	}{
		{"adequate contributor", "open", "clean", true, false, false, false, 1},
		{"approval blocked contributor", "open", "blocked", true, false, false, false, 1},
		{"red checks blocked contributor", "open", "blocked", true, false, true, false, 1},
		{"checks never ran blocked contributor", "open", "blocked", true, false, false, true, 1},
		{"blocked but conflicting", "open", "blocked", false, false, false, false, 1},
		{"behind", "open", "behind", true, false, false, false, 1},
		{"unknown", "open", "unknown", false, false, false, false, 1},
		{"draft", "open", "clean", true, true, false, false, 1},
		{"closed since snapshot", "closed", "clean", true, false, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/issues/8"):
					_, _ = w.Write([]byte(`{"number":8,"state":"open","pull_request":{},"user":{"login":"stale-author"}}`))
				case strings.HasSuffix(r.URL.Path, "/pulls/8"):
					_, _ = w.Write([]byte(`{"number":8,"state":"open","body":"Fixes #7","user":{"login":"hive[bot]"},"base":{"ref":"v5"},"mergeable":true,"mergeable_state":"clean"}`))
				case strings.HasSuffix(r.URL.Path, "/pulls/9"):
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "state": tc.state, "user": map[string]string{"login": "alice"}, "base": map[string]string{"ref": "v5"}, "head": map[string]string{"sha": "abc123"}, "mergeable": tc.mergeable, "mergeable_state": tc.mergeState, "draft": tc.draft})
				case strings.HasSuffix(r.URL.Path, "/commits/abc123/status"):
					switch {
					case tc.redCI:
						_, _ = w.Write([]byte(`{"state":"failure","statuses":[{"context":"ci/test","state":"failure"}]}`))
					case tc.noCI:
						_, _ = w.Write([]byte(`{"state":"pending","statuses":[]}`))
					default:
						_, _ = w.Write([]byte(`{"state":"success","statuses":[{"context":"ci/test","state":"success"}]}`))
					}
				case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs"):
					_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			defer server.Close()
			s := newFullServer(t)
			s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
			s.deps.GHClient.SetAppBotLogin("hive[bot]")
			// Both PRs are open items of the snapshot: a linked PR the
			// snapshot does not list is not drawn or counted (closed-first
			// supersession left closed originals as bare sub-rows).
			s.status = &StatusPayload{Repos: []FrontendRepo{{Full: "org/repo",
				ActionableIssues: []any{github.Issue{Number: 7, LinkedPRs: []github.IssueLinkedPR{{Number: 8, State: "open"}, {Number: 9, State: "open"}}}},
				OpenPrs:          []any{FrontendPR{PullRequest: github.PullRequest{Number: 8, Author: "hive[bot]"}}, FrontendPR{PullRequest: github.PullRequest{Number: 9, Author: "alice"}}}}}}
			in, err := s.gatherDelegatedInput(context.Background(), "org/repo", 8, "pr", delegatedDecisionRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if !in.HiveAuthored || len(in.Competing) != tc.want {
				t.Fatalf("input = %+v", in)
			}
			if tc.want > 0 {
				adequate := (tc.mergeState == "clean" || (tc.mergeState == "blocked" && !tc.redCI && !tc.noCI)) && tc.mergeable && !tc.draft
				if in.Competing[0].Mergeable != adequate {
					t.Fatalf("competing = %+v", in.Competing)
				}
				d := delegatedecision.Run(in)
				if (d.Rule == "prefer-contributor") != adequate {
					t.Fatalf("decision = %+v", d)
				}
			}
		})
	}
}

func TestGatherDelegatedPresenceIgnoresParkCaption(t *testing.T) {
	for _, window := range []string{"15m", "2h"} {
		t.Run(window, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/issues/8"):
					_, _ = w.Write([]byte(`{"number":8,"state":"open","pull_request":{},"user":{"login":"alice"}}`))
				case strings.HasSuffix(r.URL.Path, "/pulls/8"):
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "state": "open", "user": map[string]string{"login": "alice"}, "created_at": time.Now().Add(-time.Hour), "mergeable_state": "dirty"})
				case strings.Contains(r.URL.Path, "/permission"):
					_, _ = w.Write([]byte(`{"permission":"read"}`))
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			defer server.Close()
			s := newFullServer(t)
			s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
			s.deps.Config.Governor.Labels.Stage.PresenceWindow = window
			s.status = &StatusPayload{}
			in, err := s.gatherDelegatedInput(context.Background(), "org/repo", 8, "pr", delegatedDecisionRequest{Reason: "offline"})
			if err != nil {
				t.Fatal(err)
			}
			if in.ContributorOnline != (window == "2h") {
				t.Fatalf("online=%v for %s", in.ContributorOnline, window)
			}
			d := delegatedecision.Run(in)
			if (d.Rule == "contributor-offline") != (window == "15m") {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestDelegatedContributorActivityPagination(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"author": map[string]string{"login": "alice"}, "commit": map[string]any{"author": map[string]time.Time{"date": now}}}})
	}))
	defer server.Close()
	s := newFullServer(t)
	s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
	online, err := s.delegatedContributorOnline(context.Background(), "org", "repo", &gh.PullRequest{Number: gh.Ptr(8), User: &gh.User{Login: gh.Ptr("alice")}}, now.Add(-time.Minute))
	if err != nil || !online {
		t.Fatalf("online=%v err=%v", online, err)
	}
}

func TestGatherDelegatedPRContextRequiresSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/8"):
			_, _ = w.Write([]byte(`{"number":8,"state":"open","pull_request":{},"user":{"login":"hive[bot]"}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/8"):
			_, _ = w.Write([]byte(`{"number":8,"state":"open","body":"Fixes #7","user":{"login":"hive[bot]"},"base":{"ref":"v5"}}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	s := newFullServer(t)
	s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
	s.deps.GHClient.SetAppBotLogin("hive[bot]")
	s.status = nil
	if _, err := s.gatherDelegatedInput(context.Background(), "org/repo", 8, "pr", delegatedDecisionRequest{}); !errors.Is(err, errDelegatedNoSnapshot) {
		t.Fatalf("err = %v, want %v", err, errDelegatedNoSnapshot)
	}
}

// Claim records ("covered by PR #N") live in the stage store, not the
// snapshot; the competing-PR lookup must read them like the live tracker.
func TestGatherDelegatedPRContextUsesStageStoreClaims(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/8"):
			_, _ = w.Write([]byte(`{"number":8,"state":"open","pull_request":{},"user":{"login":"hive[bot]"}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/8"):
			_, _ = w.Write([]byte(`{"number":8,"state":"open","body":"Fixes #7","user":{"login":"hive[bot]"},"base":{"ref":"v5"},"mergeable":true,"mergeable_state":"clean"}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/9"):
			_, _ = w.Write([]byte(`{"number":9,"state":"open","user":{"login":"alice"},"base":{"ref":"v5"},"head":{"sha":"abc123"},"mergeable":true,"mergeable_state":"clean"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	s := newFullServer(t)
	s.deps.GHClient = github.NewClient("token", "org", nil, slog.Default(), server.URL)
	s.deps.GHClient.SetAppBotLogin("hive[bot]")
	s.stageStore = stagelabels.NewStore("", nil)
	s.stageStore.Observe(stagelabels.Key("org/repo", 7), stagelabels.CoveredByPR, "covered by PR #9")
	s.status = &StatusPayload{Repos: []FrontendRepo{{Full: "org/repo", ActionableIssues: []any{github.Issue{Number: 7, LinkedPRs: []github.IssueLinkedPR{{Number: 8, State: "open"}}}}}}}
	in, err := s.gatherDelegatedInput(context.Background(), "org/repo", 8, "pr", delegatedDecisionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Competing) != 1 || in.Competing[0].Number != 9 || !in.Competing[0].Mergeable {
		t.Fatalf("competing = %+v", in.Competing)
	}
}
