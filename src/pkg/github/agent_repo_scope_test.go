package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// onlyRepo scopes one agent to one repo; every other agent is unscoped.
func onlyRepo(agentName, repo string) func(string, string) bool {
	return func(a, r string) bool {
		if a != agentName {
			return true
		}
		return strings.EqualFold(r, repo) || strings.EqualFold(baseName(r), baseName(repo))
	}
}

func baseName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func scopeTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := NewClientForTest(srvURL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.prAuthz = func(agent string, uid int) error { return nil }
	c.mergeAuthz = func(agent string, uid int, repo string, number int, expectSHA string) error { return nil }
	c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
	return c
}

func TestAgentServesRepo_FailsOpenWithoutPredicate(t *testing.T) {
	var nilClient *Client
	if !nilClient.AgentServesRepo("schema", "r") {
		t.Error("nil client must fail open")
	}
	c := NewClientForTest("http://127.0.0.1:1", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !c.AgentServesRepo("schema", "r") {
		t.Error("a client with no scope predicate must fail open")
	}
	c.SetAgentRepoScopeFunc(onlyRepo("schema", "other"))
	if c.AgentServesRepo("schema", "r") {
		t.Error("scoped agent served a repo outside its scope")
	}
	if !c.AgentServesRepo("scanner", "r") {
		t.Error("unscoped agent was refused")
	}
	if !c.AgentServesRepo("", "r") {
		t.Error("an unnamed agent must fail open")
	}
}

// The proxy hard-denies direct POST /pulls for every agent mode, so this relay
// is the ONLY way an agent opens a PR — and the hive's fulfilment never
// traverses the proxy. Without this gate a specialist could open PRs on repos
// it was never defined for.
func TestPRRequestWatcher_RefusesOutOfScopeRepo(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetAgentRepoScopeFunc(onlyRepo("schema", "somewhere-else"))

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "schema/fix-1", Title: "[schema] fix: thing", Body: "Fixes #1", Agent: "schema"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("%d PRs opened on an out-of-scope repo, want 0", created)
	}
	// Quarantined, not retried forever: a scope cannot be resolved by trying again.
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Errorf("request was not quarantined: %v", err)
	}
	resBytes, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res PRResponse
	if err := json.Unmarshal(resBytes, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Error("result reports OK for a refused request")
	}
	if !strings.Contains(res.Error, "not scoped") || !strings.Contains(res.Error, "schema") {
		t.Errorf("result does not explain the scope: %q", res.Error)
	}
}

func TestPRRequestWatcher_InScopeRepoStillOpens(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetAgentRepoScopeFunc(onlyRepo("schema", "r"))

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "schema/fix-2", Title: "[schema] fix: thing", Body: "Fixes #1", Agent: "schema"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	if created != 1 {
		t.Errorf("an in-scope PR was blocked: %d created, want 1", created)
	}
}

func TestMergeRequestWatcher_RefusesOutOfScopeRepo(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetAgentRepoScopeFunc(onlyRepo("schema", "somewhere-else"))

	dir := t.TempDir()
	old := mergeRequestDirForTest
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = old }()

	// UpdateBranch true: an out-of-scope repo must receive no write at all, not
	// even the head-branch push that "update branch" performs.
	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 7, Agent: "schema", ExpectSHA: "deadbeef", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatalf("%d merges on an out-of-scope repo, want 0", merges)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("merge request was not quarantined: %v", err)
	}
	if resp := readMergeResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "not scoped") {
		t.Errorf("result does not explain the scope: %+v", resp)
	}
}

// Issue noise on repos that never wanted the agent is the specific cost #6204
// is about, so the issue relay carries the same gate.
func TestIssueRequestWatcher_RefusesOutOfScopeRepo(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "", &created, nil, nil)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetAgentRepoScopeFunc(onlyRepo("schema", "somewhere-else"))

	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "[schema] found a thing", Body: "detail", Agent: "schema"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("%d issues filed on an out-of-scope repo, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("issue request was not quarantined: %v", err)
	}
}

// ---------- FilterActionableForRepos ----------

func TestFilterActionableForRepos(t *testing.T) {
	res := &ActionableResult{
		Issues: IssueResultFromItems([]Issue{
			{Repo: "console", Number: 1, AgeMinutes: 999},
			{Repo: "dashboard", Number: 2, AgeMinutes: 999},
		}),
		PRs: PRResult{
			Count:       2,
			Items:       []PullRequest{{Repo: "console", Number: 10}, {Repo: "dashboard", Number: 11}},
			StaleDrafts: []PullRequest{{Repo: "dashboard", Number: 12}},
		},
		Hold: HoldResult{
			Issues: 1, PRs: 1, Total: 2,
			Items: []HoldItem{{Repo: "console", Number: 20, Type: "issue"}, {Repo: "dashboard", Number: 21, Type: "pr"}},
		},
		Clusters: []IssueCluster{
			{Key: "flaky", Count: 2, Issues: []Issue{{Repo: "console", Number: 1}, {Repo: "dashboard", Number: 2}}},
			{Key: "rust-only", Count: 1, Issues: []Issue{{Repo: "dashboard", Number: 2}}},
		},
		TotalByRepo: map[string]RepoCounts{"console": {Issues: 1, PRs: 1}, "dashboard": {Issues: 1, PRs: 1}},
	}
	keep := func(repo string) bool { return repo == "console" }
	got := FilterActionableForRepos(res, keep)

	if len(got.Issues.Items) != 1 || got.Issues.Items[0].Repo != "console" {
		t.Fatalf("issues = %+v, want only console", got.Issues.Items)
	}
	// Counts must be recomputed: telling a scoped agent it has two issues and
	// then listing one is exactly the confusion this feature is removing.
	if got.Issues.Count != 1 {
		t.Errorf("Issues.Count = %d, want 1", got.Issues.Count)
	}
	if got.Issues.SLAViolations != 1 {
		t.Errorf("SLAViolations = %d, want 1 recomputed from what survived", got.Issues.SLAViolations)
	}
	if len(got.PRs.Items) != 1 || got.PRs.Count != 1 {
		t.Errorf("PRs = %+v (count %d), want one console PR", got.PRs.Items, got.PRs.Count)
	}
	if len(got.PRs.StaleDrafts) != 0 {
		t.Errorf("StaleDrafts = %+v, want none (the only one was on dashboard)", got.PRs.StaleDrafts)
	}
	if got.Hold.Total != 1 || got.Hold.Issues != 1 || got.Hold.PRs != 0 {
		t.Errorf("hold = %+v, want one held issue and no held PRs", got.Hold)
	}
	if len(got.Clusters) != 1 || got.Clusters[0].Key != "flaky" || got.Clusters[0].Count != 1 {
		t.Errorf("clusters = %+v, want only flaky with one issue", got.Clusters)
	}
	if len(got.TotalByRepo) != 1 {
		t.Errorf("TotalByRepo = %v, want only console", got.TotalByRepo)
	}
	// The input is not mutated: it is shared across every agent in the fleet.
	if len(res.Issues.Items) != 2 || res.Issues.Count != 2 {
		t.Error("FilterActionableForRepos mutated the shared input")
	}
}

func TestFilterActionableForRepos_NilsAreNoOps(t *testing.T) {
	if got := FilterActionableForRepos(nil, func(string) bool { return true }); got != nil {
		t.Error("nil result should pass through")
	}
	res := &ActionableResult{Issues: IssueResultFromItems([]Issue{{Repo: "console"}})}
	if got := FilterActionableForRepos(res, nil); got != res {
		t.Error("a nil predicate should return the input unchanged, not a copy")
	}
}
