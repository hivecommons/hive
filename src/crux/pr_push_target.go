package crux

import "context"

// Sandboxed kicks on existing PRs (hivecommons/hive#18574) and the credit
// for PRs a sandboxed kick opens (hivecommons/hive#18575).
//
// Both are optional capabilities a host's PR client offers alongside
// PRCreator. The sandbox executor type-asserts the client it was given; a
// client without them keeps the older behaviour (CreatePR only, no pushes to
// an existing PR).

// AuditActionAgentPRCreated is the audit action that credits a PR to the
// hive agent that produced it. It is the same action the host's pr-request
// relay records for a PR it opens from an agent's pane (pkg/github's
// AuditActionAgentPRCreated), and every governor resolver that maps a PR back
// to its agent reads it. The runtime emits it through the sandbox audit
// callback only when its PR client could not record it itself.
const AuditActionAgentPRCreated = "agent_pr_created"

// AgentPRCreator opens a pull request for a sandboxed kick and credits it to
// agent through the host's PR-authorship record, the path the host's relay
// uses for a PR an agent opens from its pane: the attribution trailer on the
// body and the agent_pr_created audit entry.
type AgentPRCreator interface {
	CreateAgentPR(ctx context.Context, agent, repo, head, base, title, body string) (PRResult, error)
}

// PRPushTarget is what the host knows about an existing pull request a
// sandboxed kick would push to.
type PRPushTarget struct {
	Number int
	URL    string
	// State is GitHub's state: "open" or "closed".
	State   string
	BaseRef string
	HeadRef string
	HeadSHA string
	// HeadRepo is the full name of the repository the head branch lives in.
	// Empty when GitHub no longer knows it (a deleted fork).
	HeadRepo string
	// Fork is true when the head branch lives outside the base repository,
	// or when the host cannot tell where it lives.
	Fork bool
	// Author is the login GitHub records as the PR's author.
	Author string
	// AuthorAgent is the hive agent the host's PR-authorship record credits
	// with opening the PR, or "" when no hive agent did.
	AuthorAgent string
}

// PRPushTargetReader reads a PRPushTarget. The executor reads it before the
// sandbox starts and again before the push.
type PRPushTargetReader interface {
	PRPushTarget(ctx context.Context, repo string, number int) (PRPushTarget, error)
}
