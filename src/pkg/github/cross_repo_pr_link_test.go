package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A PR in another managed repo that says `Refs owner/repo#N` claims the
// issue under the issue's configured spelling, so the issue is staged as
// covered and mirrors the PR's stage (hivecommons/hive#18676:
// quorum-companion#3 `Refs hivecommons/hive#18660` at needs-decision while
// #18660 still read Actionable with no PR).
func TestCrossRepoRefsPRCoversIssueAndMirrorsPRStage(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:1", "o", []string{"r", "companion"}, testLogger())
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), testLogger())
	got := c.RecordOpenedPRClaims(ledger, PROpenedDetail{
		Repo: "companion", Number: 3, URL: "https://github.com/o/companion/pull/3",
		Title: "feat: Windows capture", Body: "Refs o/r#18660", Author: "hive[bot]", Head: "scanner/windows-capture",
	}, HiveIdentity{AppLogin: "hive[bot]"}, time.Now())
	if len(got) != 1 || got[0].Repo != "r" || got[0].Issue != 18660 || got[0].PRRepo != "companion" || !got[0].Reference {
		t.Fatalf("claims = %+v, want one reference claim keyed by the configured spelling r#18660", got)
	}
	if _, ok := ledger.Lookup("r", 18660); !ok {
		t.Fatal("ledger lookup by the enumerated spelling must find the cross-repo claim")
	}

	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "r", Number: 18660, Title: "Windows capture", Labels: []string{stagelabels.Actionable.Label()}}})
	FilterClaimedIssues(result, ledger, nil, testLogger())
	ins := IssueStageInputs(result)
	if len(ins) != 1 || ins[0].Claim == nil || ins[0].Claim.PRRepo != "companion" || ins[0].Claim.Decision != ClaimDecisionCoveredByOpenPR {
		t.Fatalf("stage input = %+v", ins)
	}
	stage, _, reason := IssueStageFor(ins[0], func(repo string, n int) stagelabels.Stage {
		if repo == "companion" && n == 3 {
			return stagelabels.NeedsDecision
		}
		return stagelabels.None
	})
	if stage != stagelabels.CoveredByPR || !strings.Contains(reason, stagelabels.NeedsDecision.String()) {
		t.Fatalf("stage = %v (%s), want covered-by-pr mirroring the PR's needs-decision", stage, reason)
	}
}

// Same-repo and unconfigured repos keep their spelling.
func TestCanonicalizeClaimRepos(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:1", "o", []string{"r", "x/full"}, testLogger())
	claims := []IssueClaim{{Repo: "O/R"}, {Repo: "r"}, {Repo: "x/full"}, {Repo: "other/r"}, {Repo: "full"}}
	c.canonicalizeClaimRepos(claims)
	want := []string{"r", "r", "x/full", "other/r", "full"}
	for i, w := range want {
		if claims[i].Repo != w {
			t.Fatalf("claim %d repo = %q, want %q", i, claims[i].Repo, w)
		}
	}
}

// The issue gets the stage-pr-link comment for a PR in another managed
// repo, naming it in full owner/repo#N form; the PR body is never touched.
func TestEnsureIssueStagePRLinkCrossRepoPR(t *testing.T) {
	var lookups []string
	state := "OPEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request %s %s (cross-repo PR bodies must never be edited)", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		lookups = append(lookups, req.Variables["owner"].(string)+"/"+req.Variables["name"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"repository": map[string]any{"pullRequest": map[string]any{
				"state": state, "baseRefName": "main", "url": "https://github.com/o/companion/pull/3",
			}},
		}})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r", "companion"}, testLogger())
	c.SetAppBotLogin("hive[bot]")
	claim := &IssueClaimContext{PRNumber: 3, PRRepo: "companion", PRURL: "https://github.com/o/companion/pull/3", PRAuthor: "hive[bot]", Reference: true}

	got, err := c.EnsureIssueStagePRLink(context.Background(), "r", 18660, claim)
	if err != nil {
		t.Fatalf("EnsureIssueStagePRLink: %v", err)
	}
	if len(lookups) != 1 || lookups[0] != "o/companion" {
		t.Fatalf("PR must be looked up in its own repo: %v", lookups)
	}
	if got.EditedBody || got.Skipped != "cross-repo" {
		t.Fatalf("result = %+v", got)
	}
	if !strings.HasPrefix(got.StageComment, "<!-- hive:issue-stage-pr-link pr=3 repo=o/companion -->") ||
		!strings.Contains(got.StageComment, "PR o/companion#3 covers this issue: https://github.com/o/companion/pull/3") {
		t.Fatalf("stage comment = %q", got.StageComment)
	}

	state = "MERGED"
	got, err = c.EnsureIssueStagePRLink(context.Background(), "r", 18660, claim)
	if err != nil || got.StageComment != "" || got.Skipped != "pr-not-open" {
		t.Fatalf("closed cross-repo PR: %+v %v", got, err)
	}
}
