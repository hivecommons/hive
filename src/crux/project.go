package crux

import (
	"strings"

	"github.com/hivecommons/hive/crux/taskmcp"
)

// ProjectContext holds the host-derived project facts (org, repos, policy
// dir, pause state) a runtime is built with and injects into agent boot
// prompts. It is on the Crux surface so a host such as cmd/crux can build one
// without naming crux/agent.
type ProjectContext struct {
	Org             string
	Repos           []string
	PrimaryRepoName string
	ACMMLevel       int
	PRsAllowed      bool
	PolicyDir       string
	// GHHost is the bare hostname of the source forge when it is NOT public
	// github.com (e.g. "github.ibm.com" for a GHE spoke), derived from the
	// configured github.api_url. Exported to agents as GH_HOST so the gh CLI
	// targets the right host — without it every agent gh call went to
	// api.github.com where the project's repos do not exist (root-caused live
	// 2026-08-20: the security agent's issue/PR creation failed silently on
	// every GHE-hosted hive). Empty ⇒ public github.com, nothing exported.
	GHHost string
	// AppAuthoredPRs mirrors config github.app_authored_prs: when true, push-
	// capable agents get the App installation token as GITHUB_TOKEN so the GitHub
	// MCP server authors PRs/commits as the App bot. Default false → no token is
	// injected and behavior is unchanged (opt-in per hive).
	AppAuthoredPRs bool
	// SharedUIDReadToken permits read-only App caches without dedicated UIDs.
	// Cross-agent read isolation is unavailable in this explicit opt-in mode.
	SharedUIDReadToken bool
	// AppBotLogin and AppBotUserID identify the installed GitHub App's bot
	// account. When AppAuthoredPRs is on, push-capable agents use this bot's
	// noreply address ("<user id>+<login>@users.noreply.github.com") for git
	// author/committer email so GitHub links their fixer commits to the App
	// while the commit name and sign-off still name the lane. AppBotUserID
	// resolves the bot's GitHub USER id (not the App id, which GitHub never
	// links) and is called at each launch so a failed lookup (0) only costs
	// that launch the linkage; nil means unknown.
	AppBotLogin  string
	AppBotUserID func() int64
	// BobSessionPrefix mirrors governor.bob.session_prefix. Empty is
	// default-off; bob launch commands remain byte-identical unless it or a
	// per-agent bob.session_label is configured.
	BobSessionPrefix string
	// RepoPaused reports whether a repo carries an operator pause (#6203). It is
	// a live predicate rather than a snapshot list because ProjectContext is
	// built once at boot and a pause is taken mid-run — a repo frozen for a
	// release must drop out of the next agent's scope without a hive restart.
	// Nil means nothing is paused, which is every hive that does not use the
	// feature and every test that constructs a ProjectContext literal.
	RepoPaused func(repo string) bool
	// AgentRepos returns the repositories one agent serves (#6204), and
	// AgentPrimaryRepo the repo its prompts should treat as "the" repo. Both
	// are live functions rather than a snapshot map because ProjectContext is
	// built once at boot while a scope is edited mid-run — a specialist
	// re-scoped in the dashboard must reach the next agent launch without a
	// hive restart.
	//
	// Nil means no scoping: every agent gets Repos and PrimaryRepo, which is
	// what every hive did before this field and what every test that builds a
	// ProjectContext literal still gets.
	AgentRepos       func(agent string) []string
	AgentPrimaryRepo func(agent string) string
	// TaskMCPURL is the hub-served, read-only task context endpoint for
	// hub-launched agents. Empty keeps launches byte-for-byte unchanged. It
	// carries NO credential: the dashboard bearer must never reach an agent
	// (#8348), so the per-launch token comes from TaskMCPLaunchToken instead.
	TaskMCPURL string
	// TaskMCPLaunchToken mints the credential one launch presents to the task
	// MCP endpoint, bound to that launch's scope (task id, repo, agent,
	// generation) and revoked when the launch ends. Nil means the URL is
	// handed over without a token; the hub then refuses the agent's calls
	// rather than falling back to a wider credential.
	TaskMCPLaunchToken func(scope taskmcp.LaunchScope) string
	// ExperimentEnv is the hive-wide experimentation environment (RFC
	// #11585 part 5, pkg/nousengine.HiveEnv): the Nous dispatcher and data
	// root as the hive resolved them (env > yaml > default). It is exported
	// ONLY to agents whose role is config.RoleExperimenter - alongside that
	// agent's own engine name - so the strategist and every other role
	// launch byte-for-byte as before. Nil exports only the engine name.
	ExperimentEnv map[string]string
}

func (p ProjectContext) PrimaryRepo() string {
	if strings.TrimSpace(p.PrimaryRepoName) != "" {
		return strings.TrimPrefix(p.PrimaryRepoName, p.Org+"/")
	}
	if len(p.Repos) > 0 {
		return strings.TrimPrefix(p.Repos[0], p.Org+"/")
	}
	return ""
}

// ActiveRepos is Repos minus the operator-paused entries: the repos an agent
// should be told to work.
//
// PrimaryRepo deliberately does NOT filter. The primary repo is the hive's
// identity — what $HIVE_REPO names and what an agent's workdir is a checkout
// of — and re-pointing it at a different repository because the operator
// froze the first one would be a far larger surprise than the pause itself.
// A paused primary simply stops appearing in the work scope.
func (p ProjectContext) ActiveRepos() []string {
	return p.unpaused(p.Repos)
}

// ReposFor returns the repositories the named agent serves, falling back to the
// hive-wide list when the agent is unscoped or no scope function is wired.
func (p ProjectContext) ReposFor(agent string) []string {
	if p.AgentRepos == nil {
		return p.Repos
	}
	if repos := p.AgentRepos(agent); repos != nil {
		return repos
	}
	return p.Repos
}

// ActiveReposFor is ReposFor minus the operator-paused entries: the repos the
// named agent should actually be handed work on.
//
// The two narrowings compose rather than override. A scope says which repos an
// agent is FOR (#6204); a pause says which repos are open for work at all
// (#6203). An agent scoped to a repo the operator has since paused is handed
// nothing for it, and unpausing restores it without touching the scope.
func (p ProjectContext) ActiveReposFor(agent string) []string {
	return p.unpaused(p.ReposFor(agent))
}

// unpaused drops the operator-paused entries from repos, preserving order.
func (p ProjectContext) unpaused(repos []string) []string {
	if p.RepoPaused == nil {
		return repos
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if p.RepoPaused(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PrimaryRepoFor returns the repo the named agent's prompts should treat as
// "the" repo — what $HIVE_REPO names.
//
// Unlike PrimaryRepo() this is per-agent, and it has to be: the shipped
// templates say `gh issue create --repo "$HIVE_REPO"`, so handing a specialist
// scoped away from the hive primary the hive primary would aim every example it
// has at a repository the proxy then refuses. An unscoped agent is unaffected.
// Like PrimaryRepo it does not filter paused repos — see ActiveRepos.
func (p ProjectContext) PrimaryRepoFor(agent string) string {
	if p.AgentPrimaryRepo != nil {
		if repo := strings.TrimPrefix(strings.TrimSpace(p.AgentPrimaryRepo(agent)), p.Org+"/"); repo != "" {
			return repo
		}
	}
	return p.PrimaryRepo()
}
