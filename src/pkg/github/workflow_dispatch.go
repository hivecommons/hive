package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Workflow-model agent dispatch, forge side (cncf/techdocs#395).
//
// A repository bound to a workflow model derives each item's model state
// (stage_model_owned.go). When the compiled model hands that state to an
// agent profile (workflowmodel.Profile.Dispatch), the item belongs to that
// profile: the scheduler withholds it from every generic lane and the
// dispatch pass (cmd/hive/workflow_dispatch.go) hands it to the hive agent
// configured for the profile. This file answers the two forge questions
// that pass needs: "which profile owns this item now?" (pure, from the
// enumerated item) and, for a profile that waits on a reply, "has a person
// with write access posted it?" (comments, read only for items in such a
// state).

// WorkflowDispatch is one item whose bound model hands its current state to
// an agent profile.
type WorkflowDispatch struct {
	// Repo is the item's repository, always "owner/name": the enumeration
	// carries a bare name for the client's org, and the owner's "Send to"
	// path carries the qualified one, so every constructor qualifies it
	// (Client.qualifiedRepo). Ref and the dispatch log's keys are built from it, so
	// one item has one key whichever path made the record.
	Repo   string
	Number int
	Kind   workflowmodel.ItemKind
	Title  string
	URL    string
	Labels []string
	// UpdatedAt is the item's last activity as enumerated; a reply lookup
	// is cached on it.
	UpdatedAt time.Time
	// Model is the bound model as name@version.
	Model string
	// State is the model state id the item is in.
	State string
	// Target is the model's dispatch target for State.
	Target workflowmodel.DispatchTarget
	// Profile is the compiled bound model.
	Profile *workflowmodel.Profile
}

// Ref is the item's "owner/repo#N" reference.
func (d WorkflowDispatch) Ref() string { return fmt.Sprintf("%s#%d", d.Repo, d.Number) }

// WorkflowModelFor returns the compiled model bound to repo ("owner/repo" or
// a bare name in the client's org), or nil when the repository is unbound,
// workflow models are off, or the binding is the built-in hive-default.
func (c *Client) WorkflowModelFor(repo string) *workflowmodel.Profile {
	if c == nil {
		return nil
	}
	owner, name := c.splitRepo(repo)
	p := c.stageWorkflowFor(owner, name)
	if p == nil || p.Name == hiveDefaultWorkflowName {
		return nil
	}
	return p
}

// WorkflowDispatchForIssue reports whether the bound model hands the
// issue's current state to an agent profile. Pure over the enumerated
// issue: no forge call.
func (c *Client) WorkflowDispatchForIssue(issue Issue) (WorkflowDispatch, bool) {
	p := c.WorkflowModelFor(issue.Repo)
	if !p.Dispatches() {
		return WorkflowDispatch{}, false
	}
	owner, name := c.splitRepo(issue.Repo)
	f := issueFactsFrom(issueStageInputFrom(issue), func(string, int) stagelabels.Stage { return stagelabels.None })
	f.Ref.Owner, f.Ref.Repo = owner, name
	return workflowDispatchFrom(p, stagelabels.IssueItem, f, WorkflowDispatch{
		Repo: c.qualifiedRepo(issue.Repo), Number: issue.Number, Kind: workflowmodel.KindIssue,
		Title: issue.Title, URL: issue.URL, Labels: append([]string(nil), issue.Labels...), UpdatedAt: issue.UpdatedAt,
	})
}

// WorkflowDispatchForPR is WorkflowDispatchForIssue for an open pull
// request. The facts are the ones a list call carries (labels, draft); a
// model rule that needs checks or reviews to tell its dispatch state apart
// sees them unknown.
func (c *Client) WorkflowDispatchForPR(pr PullRequest) (WorkflowDispatch, bool) {
	p := c.WorkflowModelFor(pr.Repo)
	if !p.Dispatches() {
		return WorkflowDispatch{}, false
	}
	owner, name := c.splitRepo(pr.Repo)
	f := stageengine.Facts{
		Ref:    stageengine.Ref{Owner: owner, Repo: name, Number: pr.Number, Kind: stagelabels.PRItem},
		Labels: append([]string(nil), pr.Labels...),
		Title:  pr.Title,
		Open:   true,
		Draft:  pr.Draft,
		Park:   stageengine.ParkFacts{Holds: stagelabels.Holds(pr.Labels)},
		PR:     &stageengine.PRFacts{},
	}
	return workflowDispatchFrom(p, stagelabels.PRItem, f, WorkflowDispatch{
		Repo: c.qualifiedRepo(pr.Repo), Number: pr.Number, Kind: workflowmodel.KindPR,
		Title: pr.Title, URL: pr.URL, Labels: append([]string(nil), pr.Labels...), UpdatedAt: pr.UpdatedAt,
	})
}

func workflowDispatchFrom(p *workflowmodel.Profile, kind stagelabels.Item, f stageengine.Facts, d WorkflowDispatch) (WorkflowDispatch, bool) {
	dec := stageengine.DeriveFromModel(p, kind).Derive(f)
	if dec.Abstain || dec.ModelState == "" {
		return WorkflowDispatch{}, false
	}
	t, ok := p.DispatchFor(dec.ModelState)
	if !ok || !t.AppliesTo(d.Kind) {
		return WorkflowDispatch{}, false
	}
	d.Model, d.State, d.Target, d.Profile = p.Name+"@"+p.Version, dec.ModelState, t, p
	return d, true
}

// ListWorkflowDraftPRs lists the open DRAFT pull requests of repo. The
// enumeration leaves ordinary drafts out of the actionable set (someone's
// work in progress), so a model state that lives on drafts (TechDocs
// Drafting) is only visible through this call. One list call per bound
// repository whose model dispatches a pull-request state.
func (c *Client) ListWorkflowDraftPRs(ctx context.Context, repo string) ([]PullRequest, error) {
	if c == nil || c.client == nil {
		return nil, nil
	}
	owner, name := c.splitRepo(repo)
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []PullRequest
	for {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("listing pull requests for %s/%s: %w", owner, name, err)
		}
		for _, pr := range prs {
			if pr == nil || !pr.GetDraft() {
				continue
			}
			out = append(out, PullRequest{
				Repo:      repo,
				Number:    pr.GetNumber(),
				Title:     pr.GetTitle(),
				Author:    safeGetLogin(pr.GetUser()),
				Labels:    issueLabelNames(pr.Labels),
				Draft:     true,
				UpdatedAt: pr.GetUpdatedAt().Time,
				URL:       pr.GetHTMLURL(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// DispatchReply is the human reply that starts a reply-triggered dispatch.
type DispatchReply struct {
	CommentID int64
	Author    string
	At        time.Time
	URL       string
}

// dispatchReplyCandidateLimit bounds how many matching comments, newest
// first, have their author's permission checked.
const dispatchReplyCandidateLimit = 3

// LatestDispatchReply returns the newest comment on repo#number whose first
// line opens with reply (`/verify`, case-insensitive, trailing punctuation
// ignored), written by a human with write access and not edited since. A
// `/hive <verb>` command is not a match: the model-reply path answers that
// one and moves the item itself.
func (c *Client) LatestDispatchReply(ctx context.Context, repo string, number int, reply string) (DispatchReply, bool, error) {
	reply = strings.TrimSpace(reply)
	if c == nil || c.client == nil || reply == "" || number <= 0 {
		return DispatchReply{}, false, nil
	}
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return DispatchReply{}, false, err
	}
	checked := 0
	for i := len(comments) - 1; i >= 0 && checked < dispatchReplyCandidateLimit; i-- {
		cm := comments[i]
		if cm == nil || !IsDispatchReply(cm.GetBody(), reply) {
			continue
		}
		created := cm.GetCreatedAt().Time
		if updated := cm.GetUpdatedAt().Time; updated.After(created) {
			continue
		}
		if !c.isHumanAuthor(cm.GetUser()) {
			continue
		}
		checked++
		login := safeGetLogin(cm.GetUser())
		allowed, err := c.commenterMayUnpark(ctx, owner, name, login, false)
		if err != nil {
			return DispatchReply{}, false, err
		}
		if !allowed {
			continue
		}
		return DispatchReply{CommentID: cm.GetID(), Author: login, At: created, URL: cm.GetHTMLURL()}, true, nil
	}
	return DispatchReply{}, false, nil
}

// IsDispatchReply reports whether body's first non-empty line opens with
// reply as its own word.
func IsDispatchReply(body, reply string) bool {
	line := firstNonEmptyLine(body)
	if line == "" || strings.HasPrefix(line, ">") {
		return false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimRight(fields[0], ".:,;!"), strings.TrimSpace(reply))
}

// WorkflowDispatchForRef reads repo#number fresh from the forge and reports
// whether its bound model hands the item's current state to a profile. It
// is the owner's "Send to <profile>" path: one GET, so the decision is made
// on the item as it is now rather than on the last enumeration. Issue
// coverage is not known from a single read; the issue's state is derived as
// if no pull request covered it.
func (c *Client) WorkflowDispatchForRef(ctx context.Context, repo string, number int) (WorkflowDispatch, bool, error) {
	if c == nil || c.client == nil || number <= 0 {
		return WorkflowDispatch{}, false, nil
	}
	if !c.WorkflowModelFor(repo).Dispatches() {
		return WorkflowDispatch{}, false, nil
	}
	owner, name := c.splitRepo(repo)
	it, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return WorkflowDispatch{}, false, fmt.Errorf("reading %s/%s#%d: %w", owner, name, number, err)
	}
	if it.GetState() != "open" {
		return WorkflowDispatch{}, false, nil
	}
	labels := issueLabelNames(it.Labels)
	if it.IsPullRequest() {
		d, ok := c.WorkflowDispatchForPR(PullRequest{Repo: repo, Number: number, Title: it.GetTitle(), Labels: labels,
			Draft: it.GetDraft(), UpdatedAt: it.GetUpdatedAt().Time, URL: it.GetHTMLURL()})
		return d, ok, nil
	}
	d, ok := c.WorkflowDispatchForIssue(Issue{Repo: repo, Number: number, Title: it.GetTitle(), Labels: labels,
		UpdatedAt: it.GetUpdatedAt().Time, URL: it.GetHTMLURL()})
	return d, ok, nil
}
