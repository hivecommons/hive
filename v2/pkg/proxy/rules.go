package proxy

import (
	"encoding/json"
	"regexp"
	"strings"

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
var githubHosts = map[string]bool{
	"api.github.com": true,
	"github.com":     true,
}

// RegisterGitHubHost adds a custom hostname (e.g. GHE instance) to the
// allowlist so that the proxy applies mode enforcement to it.
func RegisterGitHubHost(host string) {
	if host == "" {
		return
	}
	githubHosts[host] = true
}

// IsGitHubHost returns true if the host should be subject to mode enforcement.
func IsGitHubHost(host string) bool {
	return githubHosts[host]
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
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`), "POST", agent.ModeIssuesAndPRs},
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
func AllowedByMode(mode agent.AgentMode, method, path string) bool {
	for _, r := range rules {
		if r.Method == method && r.PathPattern.MatchString(path) {
			return mode >= r.MinMode
		}
	}
	return false
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
