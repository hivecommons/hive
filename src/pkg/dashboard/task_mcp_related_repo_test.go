package dashboard

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// relatedRepoServer builds the status snapshot the way the hive does: the
// enumerator stamps every item with its repository as project.repos spells
// it, and buildRepos turns that into the rows the task MCP reads. The task is
// scoped to owner/repo, as every assignment is.
func relatedRepoServer(t *testing.T, repoEntry string) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Project.Org = "owner"
	cfg.Project.Repos = []string{repoEntry}
	cfg.Project.PrimaryRepo = repoEntry
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: cfg}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.connections["alice"] = &ContributorConnection{
		currentTask: &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "do work", Key: "owner/repo#42"},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResultFromItems([]github.Issue{
			{Repo: repoEntry, Number: 42, Title: "do work", State: "open"},
			{Repo: repoEntry, Number: 50, Title: "follow-up", Body: "Split out of #42", State: "open"},
			{Repo: repoEntry, Number: 51, Title: "unrelated", Body: "nothing to do with it", State: "open"},
		}),
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{{Repo: repoEntry, Number: 43, Title: "Work on #42", Author: "bob", State: "open"}},
		},
	}
	s.status = &StatusPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Repos:     buildRepos(cfg, actionable, governor.State{}),
	}
	return s
}

func relatedNumbers(t *testing.T, s *Server) []int {
	t.Helper()
	rec := callMCP(t, s, taskmcp.ToolRelatedWork, `{"task_id":"task-1","repo":"owner/repo"}`)
	var related struct {
		Data taskmcp.RelatedWorkData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &related); err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, item := range related.Data.Items {
		if item.Repo != "owner/repo" {
			t.Errorf("related item #%d is reported for repository %q, want owner/repo", item.Number, item.Repo)
		}
		nums = append(nums, item.Number)
	}
	sort.Ints(nums)
	return nums
}

// Most hives list their repositories by bare name (project.repos: [repo] with
// project.org), and the enumerator stamps each issue and pull request with
// that spelling. The task is scoped to owner/repo, so related_work compared
// "repo" with "owner/repo", matched nothing, and always answered with an empty
// list on such a hive.
func TestTaskMCPRelatedWorkOnAHiveThatListsReposByBareName(t *testing.T) {
	for _, entry := range []string{"repo", "owner/repo"} {
		got := relatedNumbers(t, relatedRepoServer(t, entry))
		if len(got) != 2 || got[0] != 43 || got[1] != 50 {
			t.Errorf("project.repos entry %q: related_work returned %v, want the two items that cite the task's issue, [43 50]", entry, got)
		}
	}
}

// An item that names a different repository than its row stays out of scope.
func TestTaskMCPRelatedWorkStillExcludesOtherRepositories(t *testing.T) {
	s := relatedRepoServer(t, "repo")
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		map[string]any{"repo": "other/repo", "number": 60, "title": "Also about #42", "state": "open"},
		map[string]any{"repo": "elsewhere", "number": 61, "title": "Also about #42", "state": "open"},
	)
	for _, n := range relatedNumbers(t, s) {
		if n == 60 || n == 61 {
			t.Errorf("related_work served #%d, which belongs to another repository", n)
		}
	}
}
