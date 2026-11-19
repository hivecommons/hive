package github

import (
	"context"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/prgate"
)

// prGateStatusesPerPage is one page of a head's combined status, enough for
// every context a PR head carries in practice.
const prGateStatusesPerPage = 100

// ListPRGateOpenPRs lists at most limit open PRs of repo, most recently
// updated first, for the PR gate's backstop sweep (one page, no cache).
func (c *Client) ListPRGateOpenPRs(ctx context.Context, repo string, limit int) ([]prgate.OpenPR, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	opts := &gh.PullRequestListOptions{State: "open", Sort: "updated", Direction: "desc", ListOptions: gh.ListOptions{PerPage: limit}}
	prs, _, err := c.client.PullRequests.List(WithRESTCaller(ctx, "hive:pr_gate_backstop_list"), owner, repoName, opts)
	if err != nil {
		return nil, err
	}
	out := make([]prgate.OpenPR, 0, len(prs))
	for _, pr := range prs {
		if pr == nil {
			continue
		}
		out = append(out, prgate.OpenPR{Number: pr.GetNumber(), HeadSHA: pr.GetHead().GetSHA(), Title: pr.GetTitle(), Draft: pr.GetDraft()})
	}
	return out, nil
}

// ReadPRGateHeadStatuses returns the latest status of each allowlisted gate
// context on sha (the combined status holds the latest per context).
func (c *Client) ReadPRGateHeadStatuses(ctx context.Context, repo, sha string) (map[string]prgate.Status, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	combined, _, err := c.client.Repositories.GetCombinedStatus(WithRESTCaller(ctx, "hive:pr_gate_backstop_statuses"), owner, repoName, sha, &gh.ListOptions{PerPage: prGateStatusesPerPage})
	if err != nil {
		return nil, err
	}
	out := map[string]prgate.Status{}
	if combined == nil {
		return out, nil
	}
	for _, st := range combined.Statuses {
		if st == nil || prgate.PostableContext(st.GetContext()) != nil {
			continue
		}
		if _, seen := out[st.GetContext()]; seen {
			continue
		}
		out[st.GetContext()] = prgate.Status{Context: st.GetContext(), State: st.GetState(), Description: st.GetDescription()}
	}
	return out, nil
}

// prGateBackstopAPI adapts the Client to prgate.BackstopAPI.
type prGateBackstopAPI struct{ c *Client }

func (a prGateBackstopAPI) ListOpenPRs(ctx context.Context, repo string, limit int) ([]prgate.OpenPR, error) {
	return a.c.ListPRGateOpenPRs(ctx, repo, limit)
}

func (a prGateBackstopAPI) HeadStatuses(ctx context.Context, repo, sha string) (map[string]prgate.Status, error) {
	return a.c.ReadPRGateHeadStatuses(ctx, repo, sha)
}

// Sweep runs the gate's backstop over repos with c's reads: it posts the gate
// on every open, non-draft head that does not carry it yet.
func (p *PRGate) Sweep(ctx context.Context, c *Client, repos []string) prgate.SweepResult {
	if p == nil || c == nil {
		return prgate.SweepResult{}
	}
	return p.gate.Sweep(ctx, prGateBackstopAPI{c: c}, repos)
}
