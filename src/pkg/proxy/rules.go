package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/agent"
)

// ProxyRule maps a GitHub API (method, path-pattern) to the minimum
// AgentMode required. Rules are evaluated first-match-wins.
type ProxyRule struct {
	PathPattern *regexp.Regexp
	Method      string
	MinMode     agent.AgentMode

	// Capability, when non-nil, is an ADDITIONAL grant path for this rule
	// (#4492): the operation is permitted when the agent holds the capability
	// OR when its mode reaches MinMode. It never narrows — a rule with a
	// capability behaves exactly as it did before for an agent that holds none,
	// which is what keeps the change invisible on existing hives.
	//
	// This is deliberately an OR and not a replacement for MinMode. The two
	// conversational routes sit at different tiers today (issue comments at
	// ISSUES_ONLY, PR reviews at ISSUES_AND_PRS), so swapping the tier check
	// out for a single capability check would have to change one of them.
	Capability func(agent.AgentCapabilities) bool
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
// api.github.com needs MITM for GraphQL/REST mutation inspection.
//
// api.linear.app needs it for the same reason and a sharper one (#4492 F): it
// is a single POST /graphql for reads AND writes, so tunneling it opaquely —
// which is what happened until now — meant every agent write to Linear went out
// ungated. Note this is the FIRST non-GitHub host under mode enforcement, which
// is why the IsGitHubHost checks at the CONNECT seams could no longer double as
// the "should I inspect this?" test; see NeedsInspection.
func NeedsMITM(host string) bool {
	return host == "api.github.com" || IsLinearHost(host)
}

// NeedsInspection reports whether the proxy must terminate TLS for this host to
// enforce ACMM on the requests inside it.
//
// This exists because IsGitHubHost and NeedsMITM used to be entangled: every
// call site asked IsGitHubHost first and returned early, so NeedsMITM was only
// ever consulted for hosts already known to be GitHub. Adding a non-GitHub host
// to NeedsMITM alone would therefore have been silently inert — the request
// would still have been tunneled by the IsGitHubHost guard above it. Call sites
// ask this instead, so a host added to NeedsMITM is actually inspected.
func NeedsInspection(host string) bool {
	return (IsGitHubHost(host) || IsLinearHost(host)) && NeedsMITM(host)
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
	{PathPattern: regexp.MustCompile(`^/login/device/code$`), Method: "POST", MinMode: agent.ModeAdvisory},
	{PathPattern: regexp.MustCompile(`^/login/oauth/access_token$`), Method: "POST", MinMode: agent.ModeAdvisory},

	// ── Merge — HARD DENY for every mode (see denyRules) ──
	// NOTE: direct merge (PUT /pulls/{n}/merge) is NOT here — it is a HARD DENY
	// for every mode, handled by denyRules below, so agents route through the
	// bound `hive-merge` relay (SHA-pinned + merge-eligible binding) instead of
	// `gh api -X PUT .../merge`. This mirrors the GraphQL mergePullRequest deny
	// (GraphQLAllowed) so neither REST nor GraphQL is an agent-reachable bypass.

	// Updating a PR branch from its base is part of landing a PR: an
	// auto-merging agent hits it whenever the base has moved on. Without a rule
	// it matches nothing and AllowedByMode falls through to its deny-by-default,
	// so it was refused even at ISSUES_PRS_MERGE. It is gated at the same level
	// as the merge itself — it writes to the PR's head branch, nothing more.
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/update-branch$`), Method: "PUT", MinMode: agent.ModeIssuesPRsMerge},

	// ── PR operations — ISSUES_AND_PRS and above ──
	// NOTE: direct PR creation (POST /pulls) is NOT here — it is a HARD DENY for
	// every mode, handled by denyRules below, so agents route through hive-open-pr
	// (App-bot authorship) instead of `gh pr create` / the GitHub MCP create_pull_request.
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+$`), Method: "PATCH", MinMode: agent.ModeIssuesAndPRs},
	// Leaving review commentary is conversation, not code. It sat behind
	// ModeIssuesAndPRs only because it lives under /pulls/, which meant an agent
	// had to be able to push branches and open PRs before it could say "this
	// looks wrong" (#4492). The tier is unchanged for everyone; `converse` is an
	// additional way in.
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/reviews`), Method: "POST", MinMode: agent.ModeIssuesAndPRs, Capability: agent.AgentCapabilities.CanConverse},

	// ── Git fetch — all modes (read-only, despite using POST) ──
	{PathPattern: regexp.MustCompile(`\.git/git-upload-pack$`), Method: "POST", MinMode: agent.ModeAdvisory},

	// ── Git push operations — ISSUES_AND_PRS and above ──
	{PathPattern: regexp.MustCompile(`\.git/git-receive-pack$`), Method: "POST", MinMode: agent.ModeIssuesAndPRs},
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/refs$`), Method: "POST", MinMode: agent.ModeIssuesAndPRs},
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/commits$`), Method: "POST", MinMode: agent.ModeIssuesAndPRs},
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/refs/`), Method: "DELETE", MinMode: agent.ModeIssuesAndPRs},

	// ── Issue operations — ISSUES_ONLY and above ──
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues$`), Method: "POST", MinMode: agent.ModeIssuesOnly},
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+$`), Method: "PATCH", MinMode: agent.ModeIssuesOnly},
	// Commenting is conversation; creating, editing and relabelling are artifact
	// production. Bundling them at one tier meant an ADVISORY agent that noticed
	// something on a thread could not reply — only emit a bead the reporter never
	// sees (#4492). The tier is unchanged; `converse` is an additional way in.
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+/comments`), Method: "POST", MinMode: agent.ModeIssuesOnly, Capability: agent.AgentCapabilities.CanConverse},
	{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+/labels`), Method: "POST", MinMode: agent.ModeIssuesOnly},

	// ── Read operations — ADVISORY and above ──
	// Catch-all: any GET/HEAD/OPTIONS on any path.
	{PathPattern: regexp.MustCompile(`.*`), Method: "GET", MinMode: agent.ModeAdvisory},
	{PathPattern: regexp.MustCompile(`.*`), Method: "HEAD", MinMode: agent.ModeAdvisory},
	{PathPattern: regexp.MustCompile(`.*`), Method: "OPTIONS", MinMode: agent.ModeAdvisory},
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

// denyRules are checked BEFORE the mode rules. Two cases are hard-denied for
// EVERY agent mode:
//
//  1. Direct PR creation: a POST /repos/*/pulls — whether from `gh pr create`
//     or the GitHub MCP create_pull_request/create_pull_request_with_copilot
//     tool — authors the PR as the Copilot login user, not the App bot.
//     Blocking it here forces agents to use `hive-open-pr`, which the hive
//     fulfills with the App token so the PR is authored by the App bot. This
//     closes the MCP path the gh-wrapper redirect cannot see.
//
//  2. Direct PR merge: a PUT /repos/*/pulls/{n}/merge (H1, CWE-863). Permitting
//     this at ModeIssuesPRsMerge on a UID-derived mode check ALONE let an
//     injected merge-mode agent `gh api -X PUT .../merge` any reachable PR,
//     bypassing the bound merge relay's fail-closed SHA pin + merge-eligible
//     binding (see bindMergeAuthz). Hard-denying it forces ALL agent-initiated
//     merges through `hive-merge`, which the hive fulfills as the App over REST
//     with those bindings enforced. This mirrors the GraphQL mergePullRequest
//     deny (GraphQLAllowed treats it as a mutation), so neither REST nor GraphQL
//     is an agent-reachable merge bypass.
//
// In BOTH cases the hive's OWN call (CreatePR / MergePR) does NOT traverse this
// agent proxy — it originates from the hive process (owner UID), which the
// forced-egress iptables redirect exempts — so the relay is unaffected.
var denyRules = []denyRule{
	{
		PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`),
		Method:      "POST",
		Msg:         "direct PR creation is disabled for agents — use `hive-open-pr --repo <owner/repo> --head <branch> --title <t> --body <b>` so the hive opens the PR as the App bot (never the login user). Do NOT use `gh pr create` or the GitHub MCP create_pull_request/create_pull_request_with_copilot.",
	},
	{
		PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/merge$`),
		Method:      "PUT",
		Msg:         "direct PR merge is disabled for agents — use `hive-merge --repo <owner/repo> --number <n> --expect-sha <head-sha>` so the hive merges as the App bot with the SHA-pin + merge-eligible binding enforced. Do NOT use `gh api -X PUT .../merge`, `gh pr merge`, or the GitHub MCP merge_pull_request.",
	},
}

func AllowedByMode(mode agent.AgentMode, method, path string) bool {
	return AllowedByModeCaps(mode, agent.AgentCapabilities{}, method, path)
}

// AllowedByModeCaps is AllowedByMode with the agent's orthogonal capabilities
// taken into account (#4492).
//
// A matched rule is permitted when the agent holds the rule's capability OR
// when its mode reaches MinMode. The capability is checked first only because
// it is the cheaper branch; the two are an OR, so evaluation order carries no
// meaning. Rules with no capability are pure tier checks, exactly as before.
//
// Three properties this preserves, all of which the tests assert:
//
//   - Hard denies still win. denyRules are consulted before any rule matches,
//     so no capability can reach a hive-mediated operation (PR create, merge).
//   - Deny-by-default still holds. An unmatched (method, path) is refused; a
//     capability can only widen a rule that already exists.
//   - A zero AgentCapabilities is byte-identical to the old behaviour, which is
//     why AllowedByMode above still exists and why every pre-existing test
//     calls it unchanged.
func AllowedByModeCaps(mode agent.AgentMode, caps agent.AgentCapabilities, method, path string) bool {
	// Hard denies win over any mode rule or capability.
	if _, denied := DeniedMessage(method, path); denied {
		return false
	}
	for _, r := range rules {
		if r.Method == method && r.PathPattern.MatchString(path) {
			if r.Capability != nil && r.Capability(caps) {
				return true
			}
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

// graphQLMergeMutationRe matches mutations that merge a pull request. Merging is
// the highest-privilege write and must require the same mode as the REST
// `PUT /pulls/{n}/merge` rule (ModeIssuesPRsMerge) — otherwise an ISSUES_ONLY
// agent could bypass merge-gating entirely via GraphQL (CWE-863).
var graphQLMergeMutationRe = regexp.MustCompile(`(?i)\b(mergePullRequest|mergeBranch|enablePullRequestAutoMerge)\b`)

// graphQLPRWriteMutationRe matches mutations that create/modify PRs or push code
// (refs/commits/branches). These mirror the REST rules that require
// ModeIssuesAndPRs — an ISSUES_ONLY agent must not open PRs or write code via
// GraphQL any more than it can over REST.
var graphQLPRWriteMutationRe = regexp.MustCompile(`(?i)\b(createPullRequest|createCommitOnBranch|createRef|updateRef|deleteRef|createBranchProtectionRule|updateBranchProtectionRule|markPullRequestReadyForReview|convertPullRequestToDraft|addPullRequestReview|submitPullRequestReview)\b`)

// converseMutations are the GraphQL mutations that only ever produce
// conversation: a comment on an issue, PR or discussion, or a PR review. They
// are the GraphQL faces of the two REST routes the `converse` capability
// widens (#4492) — and they matter, because `gh issue comment` and
// `gh pr review` go through GraphQL, not REST. A converse capability that
// covered only the REST table would look broken to anyone using the gh CLI.
//
// Deliberately excluded: anything that edits or hides an existing comment
// (updateIssueComment, minimizeComment, deleteIssueComment). Those mutate an
// artifact — someone else's, usually — and belong on the mode ladder with the
// other edits.
var converseMutations = map[string]bool{
	"addComment":                      true,
	"addDiscussionComment":            true,
	"addPullRequestReview":            true,
	"addPullRequestReviewComment":     true,
	"addPullRequestReviewThread":      true,
	"addPullRequestReviewThreadReply": true,
	"submitPullRequestReview":         true,
}

// graphQLIdentRe matches a GraphQL name token.
var graphQLIdentRe = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*`)

// topLevelMutationFields returns the field names selected directly inside a
// GraphQL document's single mutation operation, or nil when the document is
// anything else.
//
// It exists so `converse` can be granted on the WHOLE request rather than on a
// substring match. The pre-existing classifier regexes ask "does this query
// mention mergePullRequest anywhere", which is the right question for
// ESCALATING a requirement: a batched mutation that merges is a merge whatever
// else it also does. It is the wrong question for RELAXING one — a document
// containing both addComment and issueUpdate would match "mentions addComment"
// and, if that were enough, converse would have quietly granted the issue edit
// too.
//
// So this is strict, and returns nil (no grant, fall through to the unchanged
// tier check) on anything it cannot read confidently: more than one operation,
// an unbalanced document, an alias it cannot resolve, or no fields at all.
func topLevelMutationFields(query string) []string {
	// More than one `mutation` keyword means a multi-operation document; the
	// operationName decides which runs and this parser does not track that.
	// Refuse rather than guess.
	if strings.Count(query, "mutation") != 1 {
		return nil
	}
	i := strings.Index(query, "mutation")
	if i < 0 {
		return nil
	}
	i += len("mutation")

	// Skip the optional operation name and a balanced variable-definition
	// block before the selection set opens.
	depth := 0
	for ; i < len(query); i++ {
		switch query[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '{':
			if depth == 0 {
				goto body
			}
		}
	}
	return nil

body:
	i++ // step past the opening brace of the selection set

	var fields []string
	braces := 1
	for i < len(query) {
		c := query[i]
		switch {
		case c == '#': // comment to end of line
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '"': // string literal (block strings start with the same quote)
			if strings.HasPrefix(query[i:], `"""`) {
				end := strings.Index(query[i+3:], `"""`)
				if end < 0 {
					return nil
				}
				i += 3 + end + 3
			} else {
				i++
				for i < len(query) && query[i] != '"' {
					if query[i] == '\\' {
						i++
					}
					i++
				}
				i++
			}
		case c == '{':
			braces++
			i++
		case c == '}':
			braces--
			if braces == 0 {
				return fields
			}
			i++
		case c == '@':
			// A directive (@include, @skip) is not a field selection — step
			// past the '@' and its name so the name is not collected as one.
			i++
			i += len(graphQLIdentRe.FindString(query[i:]))
		case c == '(':
			// Argument list — skip it balanced. Strings inside may contain
			// parens, so honour quoting here too.
			d := 1
			i++
			for i < len(query) && d > 0 {
				switch query[i] {
				case '"':
					i++
					for i < len(query) && query[i] != '"' {
						if query[i] == '\\' {
							i++
						}
						i++
					}
				case '(':
					d++
				case ')':
					d--
				}
				i++
			}
		case braces == 1 && (isGraphQLNameStart(c)):
			name := graphQLIdentRe.FindString(query[i:])
			i += len(name)
			// An alias (`alias: field`) names the RESPONSE key, not the
			// mutation — the real field follows the colon.
			j := i
			for j < len(query) && (query[j] == ' ' || query[j] == '\t' || query[j] == '\n' || query[j] == '\r') {
				j++
			}
			if j < len(query) && query[j] == ':' {
				j++
				for j < len(query) && (query[j] == ' ' || query[j] == '\t' || query[j] == '\n' || query[j] == '\r') {
					j++
				}
				real := graphQLIdentRe.FindString(query[j:])
				if real == "" {
					return nil
				}
				name = real
				i = j + len(real)
			}
			fields = append(fields, name)
		default:
			i++
		}
	}
	// Ran off the end without closing the selection set.
	return nil
}

func isGraphQLNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isConversationOnlyMutation reports whether EVERY top-level field of the
// document's mutation is conversational. Empty or unreadable documents are not.
func isConversationOnlyMutation(query string) bool {
	fields := topLevelMutationFields(query)
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		if !converseMutations[f] {
			return false
		}
	}
	return true
}

// GraphQLAllowed inspects a GraphQL request body and returns whether the
// operation is allowed for the given mode. Queries (reads) are allowed at
// ADVISORY and above. Mutations are classified by the capability they exercise
// and require the SAME minimum mode as the equivalent REST route, so GraphQL
// cannot be used to bypass the REST rule table:
//   - merge mutations                → ModeIssuesPRsMerge
//   - PR-create / code-write mutations → ModeIssuesAndPRs
//   - all other mutations (issues, comments, labels, …) → ModeIssuesOnly
//
// Returns (allowed, isMutation). Body must be the raw JSON request body.
func GraphQLAllowed(mode agent.AgentMode, body []byte) (bool, bool) {
	return GraphQLAllowedCaps(mode, agent.AgentCapabilities{}, body)
}

// GraphQLAllowedCaps is GraphQLAllowed with the agent's orthogonal capabilities
// taken into account (#4492).
//
// `converse` grants a mutation only when EVERY top-level field of the document's
// mutation is conversational — see isConversationOnlyMutation. The existing
// substring classifiers below are unchanged and still run first for merge and
// PR-write mutations, so a batched document that comments AND merges is still a
// merge. Widening on a substring match would have been a hole; widening on the
// whole document is not.
//
// With a zero AgentCapabilities this is the pre-existing function exactly.
func GraphQLAllowedCaps(mode agent.AgentMode, caps agent.AgentCapabilities, body []byte) (bool, bool) {
	if mode < agent.ModeAdvisory {
		return false, false
	}

	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return false, false
	}

	query := strings.TrimSpace(req.Query)
	if !graphQLMutationRe.MatchString(query) {
		return true, false // read-only query
	}

	// Classify the mutation and require the matching capability tier.
	switch {
	case graphQLMergeMutationRe.MatchString(query):
		// Merge is never conversation, whatever else the document does.
		return mode >= agent.ModeIssuesPRsMerge, true
	case graphQLPRWriteMutationRe.MatchString(query):
		// This bucket holds both real code writes (createPullRequest,
		// createCommitOnBranch, createRef) and the review mutations, which are
		// only here because they live under the PR object. converse releases
		// the latter without touching the former.
		if caps.CanConverse() && isConversationOnlyMutation(query) {
			return true, true
		}
		return mode >= agent.ModeIssuesAndPRs, true
	default:
		if caps.CanConverse() && isConversationOnlyMutation(query) {
			return true, true
		}
		return mode >= agent.ModeIssuesOnly, true
	}
}
