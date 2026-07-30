package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/kubestellar/hive/v2/pkg/agent"
)

// ProxyRule maps a GitHub API (method, path-pattern) to the minimum
// AgentMode required. Rules are evaluated first-match-wins.
type ProxyRule struct {
	PathPattern *regexp.Regexp
	Method      string
	MinMode     agent.AgentMode
}

// githubHosts are the hostnames the proxy inspects.
var githubHostsMu sync.RWMutex
var githubHosts = map[string]bool{
	"api.github.com": true,
	"github.com":     true,
}

// RegisterGitHubHost adds a custom hostname (e.g. GHE instance) to the
// allowlist so that the proxy applies mode enforcement to it. Callers can
// register hosts while proxy goroutines are already serving, so the map is
// mutex-guarded.
func RegisterGitHubHost(host string) {
	if host == "" {
		return
	}
	githubHostsMu.Lock()
	githubHosts[host] = true
	githubHostsMu.Unlock()
}

// unregisterGitHubHost removes a hostname from the allowlist (test cleanup).
func unregisterGitHubHost(host string) {
	githubHostsMu.Lock()
	delete(githubHosts, host)
	githubHostsMu.Unlock()
}

// IsGitHubHost returns true if the host should be subject to mode enforcement.
func IsGitHubHost(host string) bool {
	githubHostsMu.RLock()
	defer githubHostsMu.RUnlock()
	return githubHosts[host]
}

// NeedsMITM returns true if the host requires TLS interception for request-level
// enforcement. Hosts like github.com are registered for awareness but only need
// opaque tunneling — their traffic (OAuth device flow, git smart HTTP) either
// doesn't require ACMM enforcement or is already gated by CLI --deny-tool flags.
// API hosts, including registered GitHub Enterprise hosts, need MITM for
// GraphQL/REST mutation inspection. github.com itself remains the sole opaque
// tunnel because it carries OAuth and Git smart-HTTP traffic.
func NeedsMITM(host string) bool {
	return host != "github.com" && IsGitHubHost(host)
}

// copilotAPIHostSuffix matches the GitHub Copilot completion API hosts. The
// public host is api.githubcopilot.com and enterprise plans use
// api.enterprise.githubcopilot.com (and other per-account subdomains discovered
// via copilot_internal/user), so a suffix match covers every variant without
// hardcoding each subdomain.
const copilotAPIHostSuffix = ".githubcopilot.com"

// copilotAPIHostExact is the public Copilot completion host, matched exactly so
// the bare apex (no leading dot) is also recognized.
const copilotAPIHostExact = "githubcopilot.com"

// IsCopilotAPIHost reports whether host is a GitHub Copilot completion API host
// whose /chat/completions responses carry an OpenAI-shaped usage block. The
// proxy MITMs these hosts (when a token sink is active) purely to read that
// usage block for live cost attribution — request and response bodies are
// otherwise forwarded verbatim; no content is altered beyond an optional
// stream_options.include_usage hint on streaming completion requests.
func IsCopilotAPIHost(host string) bool {
	if host == "" {
		return false
	}
	return host == copilotAPIHostExact || strings.HasSuffix(host, copilotAPIHostSuffix)
}

// rules defines every GitHub API operation and the minimum mode needed.
// Order matters: more-specific patterns must come before less-specific ones
// for the same method, because evaluation is first-match-wins.
var rules = []ProxyRule{
	// ── OAuth / device-flow login — all modes ──
	// Copilot CLI /login needs these to authenticate via GitHub device flow.
	{regexp.MustCompile(`^/login/device/code$`), "POST", agent.ModeAdvisory},
	{regexp.MustCompile(`^/login/oauth/access_token$`), "POST", agent.ModeAdvisory},

	// ── Merge — ISSUES_PRS_MERGE only ──
	// Must come before the generic pulls PATCH/PUT rules.
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/merge$`), "PUT", agent.ModeIssuesPRsMerge},

	// ── PR operations — ISSUES_AND_PRS and above ──
	// NOTE: direct PR creation (POST /pulls) is NOT here — it is a HARD DENY for
	// every mode, handled by denyRules below, so agents route through hive-open-pr
	// (App-bot authorship) instead of `gh pr create` / the GitHub MCP create_pull_request.
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+$`), "PATCH", agent.ModeIssuesAndPRs},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/reviews`), "POST", agent.ModeIssuesAndPRs},

	// ── Git fetch — all modes (read-only, despite using POST) ──
	{regexp.MustCompile(`\.git/git-upload-pack$`), "POST", agent.ModeAdvisory},

	// ── Git push operations — ISSUES_AND_PRS and above ──
	{regexp.MustCompile(`\.git/git-receive-pack$`), "POST", agent.ModeIssuesAndPRs},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/refs$`), "POST", agent.ModeIssuesAndPRs},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/commits$`), "POST", agent.ModeIssuesAndPRs},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/refs/`), "DELETE", agent.ModeIssuesAndPRs},

	// ── Issue operations — ISSUES_ONLY and above ──
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues$`), "POST", agent.ModeIssuesOnly},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+$`), "PATCH", agent.ModeIssuesOnly},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+/comments`), "POST", agent.ModeIssuesOnly},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+/labels`), "POST", agent.ModeIssuesOnly},

	// ── Read operations — ADVISORY and above ──
	// Catch-all: any GET/HEAD/OPTIONS on any path.
	{regexp.MustCompile(`.*`), "GET", agent.ModeAdvisory},
	{regexp.MustCompile(`.*`), "HEAD", agent.ModeAdvisory},
	{regexp.MustCompile(`.*`), "OPTIONS", agent.ModeAdvisory},
}

// AllowedByMode returns true if the given HTTP method+path is permitted
// for an agent running in the specified mode. Unknown operations are
// denied by default.
// denyRule is a hard block that applies to EVERY agent mode, regardless of the
// mode-escalation rules. It exists for operations agents must route through a
// hive-mediated path instead of calling GitHub directly.
type denyRule struct {
	PathPattern *regexp.Regexp
	Method      string
	Msg         string // agent-facing directive surfaced in the 403 body
}

// denyRules are checked BEFORE the mode rules. The canonical (and currently only)
// case is direct PR creation: a POST /repos/*/pulls — whether from `gh pr create`
// or the GitHub MCP create_pull_request/create_pull_request_with_copilot tool —
// authors the PR as the Copilot login user, not the App bot. Blocking it here
// forces agents to use `hive-open-pr`, which the hive fulfills with the App token
// so the PR is authored by the App bot. This closes the MCP path the gh-wrapper
// redirect cannot see. The hive's OWN CreatePR call does not traverse this agent
// proxy, so it is unaffected.
var denyRules = []denyRule{
	{
		PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`),
		Method:      "POST",
		Msg:         "direct PR creation is disabled for agents — use `hive-open-pr --repo <owner/repo> --head <branch> --title <t> --body <b>` so the hive opens the PR as the App bot (never the login user). Do NOT use `gh pr create` or the GitHub MCP create_pull_request/create_pull_request_with_copilot.",
	},
}

func AllowedByMode(mode agent.AgentMode, method, path string) bool {
	// Hard denies win over any mode rule.
	if _, denied := DeniedMessage(method, path); denied {
		return false
	}
	for _, r := range rules {
		if r.Method == method && r.PathPattern.MatchString(path) {
			return mode >= r.MinMode
		}
	}
	return false
}

// DeniedMessage returns the agent-facing directive for a request blocked by a
// hard-deny rule, and true if such a rule matched. It lets the proxy surface WHY
// a request was refused (e.g. "use hive-open-pr") instead of the generic ACMM
// message. Returns ("", false) when no deny rule matches — the caller then falls
// back to the normal mode-based block reason.
func DeniedMessage(method, path string) (string, bool) {
	for _, r := range denyRules {
		if r.Method == method && r.PathPattern.MatchString(path) {
			return r.Msg, true
		}
	}
	return "", false
}

var repoPathPrefix = regexp.MustCompile(`^/repos/([^/]+/[^/]+)`)

var gitPathPrefix = regexp.MustCompile(`^/([^/]+/[^/]+)\.git/`)

var writeMethods = map[string]bool{
	"POST":   true,
	"PUT":    true,
	"PATCH":  true,
	"DELETE": true,
}

func ExtractRepo(path string) string {
	if m := repoPathPrefix.FindStringSubmatch(path); len(m) > 1 {
		return m[1]
	}
	if m := gitPathPrefix.FindStringSubmatch(path); len(m) > 1 {
		return m[1]
	}
	return ""
}

func RepoFilterAllowed(allowedRepos map[string]bool, method, path string) bool {
	if !writeMethods[method] {
		return true
	}
	if len(allowedRepos) == 0 {
		return true
	}
	repo := ExtractRepo(path)
	if repo == "" {
		return true
	}
	return allowedRepos[repo]
}

// IsGraphQLPath returns true if the path is the GitHub GraphQL endpoint.
func IsGraphQLPath(path string) bool {
	return path == "/graphql"
}

const graphQLBodyLimit = 64 * 1024

type graphQLRequest struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
}

var graphQLMutationRe = regexp.MustCompile(`(?m)^\s*mutation\b`)

// GraphQLAllowed inspects a GraphQL request body and returns whether the
// operation is allowed for the given mode. Queries (reads) are allowed at
// ADVISORY and above. Mutations (writes) require ISSUES_ONLY and above.
// Returns (allowed, isMutation). Body must be the raw JSON request body.
func GraphQLAllowed(mode agent.AgentMode, body []byte) (bool, bool) {
	if mode < agent.ModeAdvisory {
		return false, false
	}

	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return false, false
	}

	query := strings.TrimSpace(req.Query)
	isMutation := graphQLMutationRe.MatchString(query)

	if isMutation {
		return mode >= agent.ModeIssuesOnly, true
	}
	return true, false
}
