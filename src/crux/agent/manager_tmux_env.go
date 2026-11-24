package agent

import (
	"slices"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/upstreamproxy"
)

// inheritedCredentialEnvVars are credentials the HIVE PROCESS legitimately
// holds in its own environment — the work-source key it expands from
// ${LINEAR_API_KEY} in hive.yaml, the Linear agent OAuth app's client id,
// client secret, and webhook signing secret, the full-privilege App
// installation token the entrypoint exports — and that a tmux server started
// by this process therefore inherits into its GLOBAL environment. tmux copies
// the global environment into every pane it forks, so without an explicit
// removal each of these lands in every agent's shell, and in every tool shell
// the agent's CLI spawns.
//
// Observed live (2026-09-05, per-UID hive with the Linear agent integration
// connected): the quality agent's tmux server had LINEAR_API_KEY,
// LINEAR_CLIENT_SECRET and LINEAR_WEBHOOK_SECRET in its global environment
// beside the sanctioned LINEAR_ACCESS_TOKEN in the session environment. The
// agent used LINEAR_API_KEY — the operator's PERSONAL key — for its
// issueCreate, so every issue it filed was created by the operator rather
// than by the Hive app user, and the OAuth grant the operator had connected
// for exactly that purpose went unused.
//
// The creation-time strip that was meant to prevent this used
// `set-environment -u`, which only deletes the SESSION entry: the global
// value shows straight through to the next pane (verified against tmux
// 3.5a: -u → inherited value visible; -r → absent; -r then set → the set
// value). These are removed with -r, which records a removal that hides the
// global value from every process forked afterwards; the sanctioned
// per-agent pair set afterwards overrides the removal for the one variable
// the agent is meant to have.
//
// HIVE_CLAUDE_OAUTH_TOKEN is the operator's long-lived Claude token. It is
// delivered to claude-backend agents only, as CLAUDE_CODE_OAUTH_TOKEN via
// agentEnvPairs; under its own name it must reach no pane at all, or every
// copilot/codex/bob shell (and every tool shell they spawn) could read a
// year-long credential it has no use for.
var inheritedCredentialEnvVars = []string{
	"HIVE_GITHUB_TOKEN",
	"GITLAB_TOKEN",
	"GITEA_TOKEN",
	"FORGEJO_TOKEN",
	claude.OperatorTokenEnvVar,
	linearAPIKeyEnvVar,
	linearAccessTokenEnvVar,
	"LINEAR_CLIENT_ID",
	"LINEAR_CLIENT_SECRET",
	"LINEAR_WEBHOOK_SECRET",
	// The dashboard's shared token: X-Hive-Internal / bearer with this value
	// is a server-verified OWNER on the Go API, including GET
	// /api/config/download (the raw hive.yaml). Hosted hives receive it as
	// DASHBOARD_AUTH_TOKEN from the provisioning template; Compose and Podman
	// stacks as HIVE_DASHBOARD_TOKEN.
	"DASHBOARD_AUTH_TOKEN",
	"HIVE_DASHBOARD_TOKEN",
	// Per-hive signing keys the hub injects into the hive container, and the
	// legacy master they were derived from. The terminal key alone forges a
	// /terminal grant to a ttyd shell as the hive user.
	"HIVE_TERMINAL_KEY",
	"HIVE_INVITE_KEY",
	"HIVE_HEARTBEAT_KEY",
	"HIVE_HUB_SECRET",
	"HIVE_TTYD_CREDENTIAL",
	// The per-boot key behind the user-session ttyd attach tokens
	// (terminal_session_gate.go): whoever holds it can attach any session.
	"HIVE_TTYD_ATTACH_KEY",
	// The corporate forward-proxy rules. The URL's userinfo is the proxy's
	// Basic credential; agents reach the upstream only through the MITM
	// proxy and are never handed the corporate proxy URL. The list vars
	// carry no secret but are hive-process settings, not agent ones — agents
	// get their own NO_PROXY from agentEnvPairs. The names come from
	// crux/upstreamproxy so the strip list cannot drift from what the
	// loader reads; HIVE_NO_PROXY is the name the hub's provisioning
	// template injects (saas_provision.go), stripped alongside until the
	// hub and loader agree on one.
	upstreamproxy.URLEnv,
	upstreamproxy.HostsEnv,
	upstreamproxy.NoProxyEnv,
	"HIVE_NO_PROXY",
}

func credentialEnvVars() []string {
	return append(slices.Clone(inheritedCredentialEnvVars), config.ExtraCredentialEnvVars()...)
}
