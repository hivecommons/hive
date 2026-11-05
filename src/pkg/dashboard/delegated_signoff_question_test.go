package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func TestGatherDelegatedSupersederQuestionClass(t *testing.T) {
	const scopeQuestion = "@clubanderson should confirm the merge-request relay may lift the hold…"
	for _, tc := range []struct {
		name, question, path, previousPath, patch string
		secondPage, filesFail, signOff            bool
	}{
		{name: "scope question", question: scopeQuestion, path: "src/pkg/github/review_verdict_relay.go"},
		{name: "workflow sign-off", question: scopeQuestion, path: ".github/workflows/ci.yml", signOff: true},
		{name: "policy sign-off on second page", question: scopeQuestion, path: "OWNERS", secondPage: true, signOff: true},
		{name: "renamed policy file", question: scopeQuestion, path: "docs/old-owners.md", previousPath: "OWNERS", signOff: true},
		{name: "config policy patch", question: scopeQuestion, path: "deploy/governor.yaml", patch: "+    auto_merge: true", signOff: true},
		{name: "explicit sign-off guard", question: "@hanthor should confirm the sentinel finding is intended.", path: "src/pkg/github/review_verdict_relay.go", signOff: true},
		{name: "unreadable files", question: scopeQuestion, filesFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewEvidenceTestEnv(t)
			if err := review.WriteArtifact("", review.Artifact{Items: []review.Aggregate{{
				Repo: "org/repo", Number: 8, HeadSHA: "abc123", Verdict: review.VerdictRequiresHuman,
				RequiresHuman: true, DecisionRequested: true, DecisionQuestion: tc.question,
			}}}); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/issues/8"):
					_, _ = w.Write([]byte(`{"number":8,"state":"open","pull_request":{},"user":{"login":"hive[bot]"}}`))
				case strings.HasSuffix(r.URL.Path, "/pulls/8"):
					_, _ = w.Write([]byte(`{"number":8,"state":"open","body":"Supersedes #7","user":{"login":"hive[bot]"},"head":{"sha":"abc123"},"base":{"ref":"v5"},"mergeable":true,"mergeable_state":"clean"}`))
				case strings.HasSuffix(r.URL.Path, "/pulls/8/files"):
					if tc.filesFail {
						http.Error(w, "unavailable", http.StatusForbidden)
						return
					}
					if tc.secondPage && r.URL.Query().Get("page") != "2" {
						w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
						_, _ = w.Write([]byte(`[{"filename":"README.md"}]`))
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]string{{"filename": tc.path, "previous_filename": tc.previousPath, "patch": tc.patch}})
				case strings.HasSuffix(r.URL.Path, "/commits/abc123/status"):
					_, _ = w.Write([]byte(`{"state":"success","statuses":[{"context":"ci/test","state":"success"}]}`))
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
			s.status = &StatusPayload{}
			in, err := s.gatherDelegatedInput(context.Background(), "org/repo", 8, "pr", delegatedDecisionRequest{})
			if tc.filesFail {
				if err == nil || !strings.Contains(err.Error(), "reading files to classify reviewer question") {
					t.Fatalf("err = %v, want classification read failure", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.Question != tc.question || in.SignOffQuestion != tc.signOff || !in.HiveSuperseding {
				t.Fatalf("input = %+v, want question preserved with sign-off=%v on a superseder", in, tc.signOff)
			}
			d := delegatedecision.Run(in)
			if tc.signOff {
				if d.Verdict != delegatedecision.VerdictRecommendOnly || !d.KeepsPark || len(d.Actions) != 1 || d.Actions[0].Kind != delegatedecision.ActionRecommendOnly || !strings.Contains(d.Text, tc.question) {
					t.Fatalf("decision = %+v, want only a recommendation surfacing the sign-off question", d)
				}
			} else if d.Verdict != delegatedecision.VerdictApprove || d.KeepsPark {
				t.Fatalf("decision = %+v, want scope question released", d)
			}
			if d.Rule != delegatedecision.RuleHiveSuperseder {
				t.Fatalf("rule = %q, want superseder rule", d.Rule)
			}
		})
	}
}
