package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

// PRs a sandboxed kick opens or pushes to (hivecommons/hive#18574,
// hivecommons/hive#18575).
//
// A sandboxed kick has no GitHub token and no pod-local relays, so the
// sandbox executor opens its PR through this client directly, not through the
// pr-request watcher. CreateAgentPR gives that PR the same credit the watcher
// gives a PR an agent requests from its pane: the attribution trailer on the
// body, the agent_pr_created audit entry, and the PR-opened hooks. Every
// governor resolver that maps a PR to its agent (the FIX-BEFORE-NEW block,
// the red-CI webhook kick, the review-thread monitor, held-PR routing) reads
// that audit entry, so the PR routes back to the agent that produced it.

// SandboxPRVia is the "via" detail pair on an agent_pr_created entry the
// sandbox path recorded, so the audit log tells the two creation paths apart.
const SandboxPRVia = "sandbox"

// createPRFunc is CreatePR's signature, injectable for tests.
type createPRFunc func(ctx context.Context, repo, head, base, title, body string) (CreatePRResult, error)

// CreateAgentPR opens a PR for a sandboxed kick by agent and credits it to
// agent (crux.AgentPRCreator).
func (c *Client) CreateAgentPR(ctx context.Context, agent, repo, head, base, title, body string) (CreatePRResult, error) {
	return c.createAgentPR(ctx, agent, repo, head, base, title, body, c.CreatePR)
}

func (c *Client) createAgentPR(ctx context.Context, agent, repo, head, base, title, body string, create createPRFunc) (CreatePRResult, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return create(ctx, repo, head, base, title, body)
	}
	meta := c.attributionMeta(agent)
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}
	res, err := create(ctx, repo, head, base, title, body)
	if err != nil {
		return res, err
	}
	c.RecordAgentPROpened(meta, repo, head, title, body, res)
	return res, nil
}

// RecordAgentPROpened records the credit for a PR the sandbox path opened
// for meta.Agent: the PR-opened hooks for a new PR, then the agent_pr_created
// audit entry (reuses included, as the watcher records them). A result with
// no PR number records nothing.
func (c *Client) RecordAgentPROpened(meta InvocationMeta, repo, head, title, body string, res CreatePRResult) {
	if c == nil || res.Number <= 0 || strings.TrimSpace(meta.Agent) == "" {
		return
	}
	if !res.AlreadyExisted {
		if hook := c.prOpenedHook.Load(); hook != nil && *hook != nil {
			go (*hook)(meta.Agent, repo, res.Number, res.URL)
		}
		if hook := c.prOpenedDetailHook.Load(); hook != nil && *hook != nil {
			go (*hook)(PROpenedDetail{Agent: meta.Agent, Repo: repo, Number: res.Number, URL: res.URL, Title: title, Body: body, Author: res.Author, Head: head})
		}
	}
	c.recordWriteAudit(AuditActionAgentPRCreated, meta,
		WriteTarget{Repo: repo, Number: res.Number},
		"author", res.Author,
		"url", res.URL,
		"reused", strconv.FormatBool(res.AlreadyExisted),
		"duplicate_tree", strconv.FormatBool(res.DuplicateTree),
		"duplicate_reason", res.DuplicateReason,
		"head", head,
		"via", SandboxPRVia)
}

// AuthorAgentForPRFromAudit is AuthorAgentForPR over the audit log at path.
func AuthorAgentForPRFromAudit(path, repo string, number int) string {
	return auditAuthorAgentForPR(repo, number, path, time.Now().UTC().Add(-reviewVerdictAuditLookback))
}

// PRPushTarget reads what the sandbox executor needs to decide whether a
// sandboxed kick may push to an existing PR (crux.PRPushTargetReader). The
// agent credited with the PR is the agent_pr_created audit entry's. Only
// when the audit log has none (an entry older than its lookback) is the
// body's attribution trailer read, and only on a PR the hive's App opened:
// on anyone else's PR the trailer is text the author could have written.
func (c *Client) PRPushTarget(ctx context.Context, repo string, number int) (crux.PRPushTarget, error) {
	if c == nil || c.client == nil {
		return crux.PRPushTarget{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:sandbox_target_pr"), owner, name, number)
	if err != nil {
		return crux.PRPushTarget{}, fmt.Errorf("reading %s/%s#%d: %w", owner, name, number, err)
	}
	out := crux.PRPushTarget{
		Number:   pr.GetNumber(),
		URL:      pr.GetHTMLURL(),
		State:    pr.GetState(),
		BaseRef:  pr.GetBase().GetRef(),
		HeadRef:  pr.GetHead().GetRef(),
		HeadSHA:  pr.GetHead().GetSHA(),
		HeadRepo: pr.GetHead().GetRepo().GetFullName(),
		Author:   pr.GetUser().GetLogin(),
	}
	if out.Number == 0 {
		out.Number = number
	}
	baseRepo := pr.GetBase().GetRepo().GetFullName()
	if baseRepo == "" {
		baseRepo = owner + "/" + name
	}
	out.Fork = out.HeadRepo == "" || !strings.EqualFold(out.HeadRepo, baseRepo)
	out.AuthorAgent = AuthorAgentForPRFromAudit(reviewVerdictAuditPath, owner+"/"+name, number)
	if out.AuthorAgent == "" && c.appBotLogin != "" && strings.EqualFold(out.Author, c.appBotLogin) {
		if meta, ok := ParseAttributionTrailer(pr.GetBody()); ok {
			out.AuthorAgent = strings.TrimSpace(meta.Agent)
		}
	}
	return out, nil
}

// Compile-time proof that the client offers the sandbox seams.
var (
	_ crux.AgentPRCreator     = (*Client)(nil)
	_ crux.PRPushTargetReader = (*Client)(nil)
)
