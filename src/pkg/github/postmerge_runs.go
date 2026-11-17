package github

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/githubactions"
)

// GitHub Actions post-merge run observer (hivecommons/hive#18645).
//
// ObservePostMergeRuns reads the default branch's first-parent history
// and the runs of every GitHub Actions workflow the model binds into a
// post-merge column, then resolves each merged PR's pipeline.status
// through githubactions.ObservePostMerge (range attribution, #18356). The
// read is bounded; a merge commit older than the history window is
// reported unknown with its reason, never guessed.

const (
	// postMergeHistoryPages bounds the default-branch commit listing
	// (100 commits a page).
	postMergeHistoryPages = 3
	// postMergeRunPages bounds each workflow's run listing (100 runs a
	// page, newest first).
	postMergeRunPages = 2
)

// PostMergeRequest names the repository, its model and the merged PRs to
// report on.
type PostMergeRequest struct {
	Owner, Repo string
	// DefaultBranch is the repository's default branch.
	DefaultBranch string
	Model         *workflowmodel.Model
	// EnabledAt is the default-branch revision at which the model was
	// enabled.
	EnabledAt string
	PRs       []workflowmodel.MergedPR
}

// ObservePostMergeRuns returns each PR's post-merge facts, keyed by PR
// number. A failed read is an error: the caller keeps pipeline.status
// unknown rather than deriving from a partial listing.
func ObservePostMergeRuns(ctx context.Context, client *gh.Client, req PostMergeRequest) (map[int]*stageengine.PostMergeFacts, error) {
	if client == nil {
		return nil, ErrNoGitHubClient
	}
	if strings.TrimSpace(req.Owner) == "" || strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.DefaultBranch) == "" {
		return nil, errors.New("ObservePostMergeRuns: owner, repo and default branch are required")
	}
	cols := githubactions.PostMergeColumns(req.Model)
	in := githubactions.PostMergeInput{Model: req.Model, DefaultBranch: req.DefaultBranch, EnabledAt: req.EnabledAt, PRs: req.PRs}
	if len(cols) > 0 {
		history, err := firstParentHistory(ctx, client, req.Owner, req.Repo, req.DefaultBranch)
		if err != nil {
			return nil, err
		}
		in.History = history
		seen := map[string]bool{}
		for _, c := range cols {
			for _, w := range c.Workflows {
				if seen[w] {
					continue
				}
				seen[w] = true
				runs, err := listWorkflowRuns(ctx, client, req.Owner, req.Repo, w)
				if err != nil {
					return nil, err
				}
				in.Runs = append(in.Runs, runs...)
			}
		}
	}
	out := make(map[int]*stageengine.PostMergeFacts, len(req.PRs))
	for n, st := range githubactions.ObservePostMerge(in) {
		out[n] = &stageengine.PostMergeFacts{Status: st.Status, Column: st.Column, Reason: st.Reason}
	}
	return out, nil
}

// ObservePostMerge is ObservePostMergeRuns over the client's own forge
// connection, for repo ("owner/repo" or a bare name in the client's org).
func (c *Client) ObservePostMerge(ctx context.Context, repo string, m *workflowmodel.Model, defaultBranch string, prs []workflowmodel.MergedPR) (map[int]*stageengine.PostMergeFacts, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	return ObservePostMergeRuns(WithRESTCaller(ctx, "hive:workflow_postmerge"), c.client, PostMergeRequest{
		Owner: owner, Repo: name, DefaultBranch: defaultBranch, Model: m, PRs: prs,
	})
}

// firstParentHistory lists the default branch and follows first parents
// from its tip, returning the chain oldest first. Commits reached only
// through a second parent (a merged branch's own commits) are left out,
// so prev..R spans merges onto the default branch only.
func firstParentHistory(ctx context.Context, client *gh.Client, owner, repo, branch string) ([]string, error) {
	parent := map[string]string{}
	tip := ""
	opts := &gh.CommitsListOptions{SHA: branch, ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < postMergeHistoryPages; page++ {
		commits, resp, err := client.Repositories.ListCommits(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing %s/%s@%s history: %w", owner, repo, branch, err)
		}
		for _, c := range commits {
			sha := c.GetSHA()
			if sha == "" {
				continue
			}
			if tip == "" {
				tip = sha
			}
			first := ""
			if len(c.Parents) > 0 {
				first = c.Parents[0].GetSHA()
			}
			parent[sha] = first
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	var chain []string
	seen := map[string]bool{}
	for sha := tip; sha != "" && !seen[sha]; {
		p, ok := parent[sha]
		if !ok {
			break
		}
		seen[sha] = true
		chain = append(chain, sha)
		sha = p
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// listWorkflowRuns lists one workflow file's recent runs.
func listWorkflowRuns(ctx context.Context, client *gh.Client, owner, repo, workflow string) ([]githubactions.WorkflowRun, error) {
	file := path.Base(workflow)
	opts := &gh.ListWorkflowRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []githubactions.WorkflowRun
	for page := 0; page < postMergeRunPages; page++ {
		runs, resp, err := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, file, opts)
		if err != nil {
			return nil, fmt.Errorf("listing %s/%s runs of %s: %w", owner, repo, workflow, err)
		}
		if runs != nil {
			for _, r := range runs.WorkflowRuns {
				p := r.GetPath()
				if p == "" {
					p = workflow
				}
				out = append(out, githubactions.WorkflowRun{
					ID:         r.GetID(),
					Path:       p,
					Event:      r.GetEvent(),
					HeadBranch: r.GetHeadBranch(),
					HeadSHA:    r.GetHeadSHA(),
					Status:     r.GetStatus(),
					Conclusion: r.GetConclusion(),
					CreatedAt:  r.GetCreatedAt().Time,
				})
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}
