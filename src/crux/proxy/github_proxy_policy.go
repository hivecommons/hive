package proxy

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/crux/agent"
)

// injectLinearCredential replaces the Authorization header of an agent's
// api.linear.app request with the hive's CURRENT Linear credential, for
// agents at the ISSUES_ONLY floor and above (the tier the session-env push in
// agent.Manager.linearEnvPairs uses). It reports whether a credential was
// applied.
//
// WHY THE PROXY, NOT THE ENVIRONMENT. The connected Linear app's OAuth access
// token rotates (Linear issues it with a ~24h expiry and hive refreshes it
// in the background), and hive already re-pushes the fresh value into each
// agent's tmux session environment on the hourly refresh tick. But a tmux
// session environment only reaches processes forked AFTER the push: the
// agent's CLI, forked at its last launch, keeps the token it was born with,
// and every tool shell it spawns inherits that copy. Observed live
// (2026-09-06/07): a quality agent whose session environment held the fresh
// token — verified valid through this very proxy as that agent's UID — got
// `401 AUTHENTICATION_ERROR` on every Linear call from inside its pane for
// two days, because its process still carried the pre-rotation token. Its
// earlier kicks had silently fallen back to the operator's personal
// LINEAR_API_KEY, so every issue it filed showed the operator as creator.
//
// The proxy terminates every agent request to api.linear.app anyway (this is
// where enforceLinear gates mutations by tier), so attaching the live
// credential HERE makes the copy in the agent's environment irrelevant —
// stale, absent, or a placeholder all work — exactly as the inference
// translator attaches the real gateway key server-side. The agent still
// receives a credential in its environment (for SDKs that refuse to send a
// request without one), but Linear only ever sees the proxy's.
//
// Below the floor the request is left untouched: an ADVISORY agent's Linear
// reads have always been ungated and unauthenticated-by-hive, and injecting
// a write-capable credential there would widen what the tier can do.
func (p *GitHubProxy) injectLinearCredential(req *http.Request, agentName string, mode agent.AgentMode) bool {
	if p.linearCredential == nil || !mode.CanCreateIssues() {
		return false
	}
	cred := p.linearCredential()
	switch {
	case cred.AccessToken != "":
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	case cred.APIKey != "":
		req.Header.Set("Authorization", cred.APIKey)
	default:
		return false
	}
	p.logger.Debug("linear: attached the hive's current credential to an agent request", "agent", agentName, "mode", mode.String())
	return true
}

// repoScopePath is the request path handed to the per-repo write gates (pause,
// agent scope, auto-merge, per-repo ACMM mode, repo filter). It starts from
// the client's own escaping rather than the decoded req.URL.Path so a GitLab
// "namespace%2Fproject" stays one segment and ExtractRepo resolves the exact
// project — including nested groups — instead of guessing where the project
// path ends from the sub-resource that follows it. CanonicalGatePath then
// decodes every OTHER escape so an escaped structural segment (/r%65pos/,
// repo%2Egit, pro%6Aects, merg%65) cannot hide the repo from the gates.
func repoScopePath(req *http.Request) string {
	return CanonicalGatePath(req.URL.EscapedPath())
}

// repoPauseRefusal is RepoPauseRefusal against this proxy's current predicate.
func (p *GitHubProxy) repoPauseRefusal(method, path string) (string, bool) {
	p.pausedMu.RLock()
	paused := p.repoPaused
	p.pausedMu.RUnlock()
	return RepoPauseRefusal(paused, method, path)
}

func (p *GitHubProxy) autoMergeRefusal(method, path string) (string, bool) {
	p.autoMergeMu.RLock()
	enabled := p.repoAutoMergeOn
	p.autoMergeMu.RUnlock()
	return RepoAutoMergeRefusal(enabled, method, path)
}

func (p *GitHubProxy) graphQLAutoMergeRefusal(body []byte) (string, bool) {
	p.autoMergeMu.RLock()
	enabled := p.repoAutoMergeOn
	p.autoMergeMu.RUnlock()
	return GraphQLAutoMergeRefusal(enabled, body)
}

// agentRepoScopeRefusal is AgentRepoScopeRefusal against the current predicate.
func (p *GitHubProxy) agentRepoScopeRefusal(agentName, method, path string) (string, bool) {
	p.scopeMu.RLock()
	serves := p.agentServesRepo
	p.scopeMu.RUnlock()
	return AgentRepoScopeRefusal(serves, agentName, method, path)
}

func (p *GitHubProxy) writeSurfaceEnforcedFunc() func(agent string) bool {
	p.writeSurfaceMu.RLock()
	defer p.writeSurfaceMu.RUnlock()
	return p.writeSurfaceEnforced
}

// laneWriteSurfaceEnforced reports whether agentName's lane is enforced.
func (p *GitHubProxy) laneWriteSurfaceEnforced(agentName string) bool {
	if agentName == "" || agentName == internalCallerName {
		return false
	}
	enforced := p.writeSurfaceEnforcedFunc()
	return enforced != nil && enforced(agentName)
}

// auditWriteRefused records one direct write refused under
// write_surface.enforce.
func (p *GitHubProxy) auditWriteRefused(agentName, kind, method, path string) {
	p.writeSurfaceMu.RLock()
	audit := p.writeRefusedAudit
	p.writeSurfaceMu.RUnlock()
	if audit == nil {
		return
	}
	audit(agentName, kind, method, path, WriteSurfaceRepo(path))
}

// gitInjectBasicUser is the username git expects alongside a GitHub App
// installation token in HTTP Basic auth — the same identity
// git-credential-hive.sh answers with, so upstream sees exactly the shape the
// pre-injection flow produced.
const gitInjectBasicUser = "x-access-token"

// loginPathPrefix marks the GitHub OAuth/device-flow endpoints
// (/login/device/code, /login/oauth/access_token). These authenticate the
// FLOW via their form body, not a bearer credential; injecting an App token
// there would be meaningless at best and flow-corrupting at worst, so they
// get strip-only treatment.
const loginPathPrefix = "/login/"

// rewriteGitHubAuth enforces #1861 on one MITM'd request: the proxy — not the
// agent — decides what credential GitHub sees.
//
// Threat model, per header-touching block:
//
//   - STRIP: an agent-supplied Authorization header is deleted UNCONDITIONALLY
//     (under the flag), never forwarded. Even though injection-mode agents are
//     only ever handed the inert placeholder, an agent that somehow obtained a
//     real credential (leaked from a log, smuggled via prompt, minted through
//     a side channel) must not be able to spend it through this proxy.
//
//   - INJECT: only for an agent identified by the UID-authoritative path
//     (#3888) that has a hub-held scoped token. The two lookups this chains
//     are exactly the pre-existing lanes: identity from the UID map, token
//     from the #3967 per-agent scoped mint.
//
//   - UNKNOWN AGENT = NO CREDENTIAL: when identity resolution failed
//     (agentName == ""), the request proceeds with NO Authorization and fails
//     loud at GitHub (401/403). Deliberate: any fallback — shared cache,
//     hive token, "last known agent" — would let an unattributable process
//     ride a real credential, recreating the pre-#3888 identity hole this
//     design depends on having closed.
//
//   - COPILOT AUTH-EXCHANGE PASSTHROUGH: /copilot_internal/ on a GitHub
//     host keeps the agent's own Authorization (the user's Copilot OAuth
//     token); see copilotInternalPathPrefix for why and the residual.
//
//   - INTERNAL CALLER PASSTHROUGH: the hive's own control plane
//     (internalCallerName) legitimately holds and sends its own App
//     credentials (token mint, heartbeat, hive-open-pr fulfillment); its
//     requests pass untouched. This cannot be spoofed by an agent: the name
//     is assigned only via UIDMap.IsInternalUID, never from anything the
//     caller sends.
//
// Logging: agent name and injected yes/no only — NEVER token bytes, not even
// a prefix (#1861 requirement; a token prefix is enough to fingerprint and
// correlate credentials across logs).
func (p *GitHubProxy) rewriteGitHubAuth(req *http.Request, agentName string) {
	// NOTE: callers must gate on injectsGitHubAuthForHost first; this function
	// does not know which host the request is bound for.
	if !p.injectGHAuth {
		// Flag off (the default): behavior byte-identical to before #1861 —
		// no header is touched on any path.
		return
	}
	if agentName == internalCallerName {
		return
	}
	if isCopilotAuthExchangePath(req.URL.Path) {
		// Before the strip: the Copilot session-token exchange needs the
		// user's Copilot OAuth token, which no hub-held App token can stand in
		// for (copilotInternalPathPrefix).
		p.logger.Debug("proxy auth injection: Copilot auth-exchange endpoint - agent credential passed through", "agent", agentName, "injected", false)
		return
	}

	req.Header.Del("Authorization")

	if agentName == "" {
		p.logger.Debug("proxy auth injection: caller unidentified — forwarding without credentials (fail-loud, no fallback)", "injected", false)
		return
	}
	if strings.HasPrefix(req.URL.Path, loginPathPrefix) {
		p.logger.Debug("proxy auth injection: OAuth flow endpoint — strip only", "agent", agentName, "injected", false)
		return
	}
	if p.agentTokenSource == nil {
		p.logger.Warn("proxy auth injection enabled but no token source wired — forwarding without credentials", "agent", agentName, "injected", false)
		return
	}
	token, ok := p.agentTokenSource(agentName)
	if !ok {
		// An identified agent with no hub-held token means the #3967 mint lane
		// has not (yet) delivered for this agent — e.g. the hive restarted and
		// the in-memory registry is empty until the next launch/refresh mint.
		// Fail loud (upstream 401) rather than borrow any other credential.
		p.logger.Warn("proxy auth injection: no hub-held token for identified agent — forwarding without credentials (check the per-agent mint lane)", "agent", agentName, "injected", false)
		return
	}

	if isGitPath(req.URL.Path) {
		// Git smart HTTP authenticates with Basic x-access-token:<token> —
		// the same shape git-credential-hive.sh produced pre-injection.
		basic := base64.StdEncoding.EncodeToString([]byte(gitInjectBasicUser + ":" + token))
		req.Header.Set("Authorization", "Basic "+basic)
		// Force this exchange onto its own upstream connection. The git branch
		// in proxyHTTP switches to a raw relay after forwarding this request's
		// headers, so a keep-alive reuse (git's follow-up POST after GET
		// /info/refs on the same conn) would stream its headers — placeholder
		// credential included — straight through WITHOUT passing this rewrite,
		// and fail with a confusing mid-operation 401. Connection: close makes
		// git reconnect per request; each new connection re-enters proxyHTTP
		// and gets its own rewrite.
		req.Close = true
	} else {
		// REST/GraphQL (api.github.com, GHE /api/v3): the standard GitHub
		// token scheme, identical to what gh sends.
		req.Header.Set("Authorization", "token "+token)
	}
	p.logger.Debug("proxy auth injection: scoped token attached", "agent", agentName, "injected", true)
}

// injectsGitHubAuthForHost reports whether a request relayed for host may have
// its Authorization rewritten by rewriteGitHubAuth (#9586). It is a positive
// allowlist - the empty host (proxyHTTP's "GitHub implied" convention) and the
// registered GitHub-family hosts - rather than "anything but Linear", so a
// non-GitHub host added to the MITM set later can never receive a GitHub token
// by default.
func injectsGitHubAuthForHost(host string) bool {
	return host == "" || IsGitHubHost(host)
}

func (p *GitHubProxy) inspectCanaryEgress(agentName string, req *http.Request) (reason string, deny bool, detected bool) {
	if p == nil || !p.canariesEnabled || p.canaryRegistry == nil || req == nil || req.Body == nil {
		return "", false, false
	}
	if isGitReceivePack(req.URL.Path) && p.canaryFailClosed {
		return "ioscan canary scan unavailable for git push", true, true
	}
	if !writeMethods[req.Method] || isGitPath(req.URL.Path) || !githubWriteBodyMayLeak(req.Method, req.URL.Path) {
		return "", false, false
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxGitHubWriteBodyScan+1))
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if err != nil {
		return "", false, false
	}
	if len(body) > maxGitHubWriteBodyScan {
		return "ioscan canary scan body too large", true, true
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if leak, ok := p.canaryRegistry.Scan(agentName, string(body), "github:"+req.Method+" "+req.URL.Path); ok {
		if p.canaryLeakFunc != nil {
			p.canaryLeakFunc(leak)
		}
		p.logger.Warn("ioscan canary leak detected", "agent", leak.Agent, "source", leak.Source)
		return "ioscan canary leak", p.canaryFailClosed, true
	}
	return "", false, false
}

// enforceLinear applies ACMM to one request on a MITM'd api.linear.app
// connection and returns whether it is blocked, with an agent-facing reason.
//
// The request body is read (bounded), inspected, and restored so an allowed
// request forwards byte-identically.
//
// NOTHING from the body is logged or surfaced in the 403. Linear documents
// carry issue and comment content in the query and, in variables, potentially
// tokens; the operation NAMES are schema identifiers rather than user content,
// so those are what makes a denial debuggable without leaking the payload.
func (p *GitHubProxy) enforceLinear(req *http.Request, agentName string, mode agent.AgentMode, caps agent.AgentCapabilities) (bool, string) {
	// Linear exposes exactly one endpoint. Anything else on this host is not a
	// shape this gate understands, so it is refused rather than forwarded.
	if req.Method != http.MethodPost || !IsLinearGraphQLPath(req.URL.Path) {
		return true, "linear: only POST " + linearGraphQLPath + " is permitted"
	}

	// Read one byte past the limit so hitting the cap is detectable: a body
	// that exactly fills the buffer is indistinguishable from a truncated one
	// otherwise, and truncation must deny.
	body, readErr := io.ReadAll(io.LimitReader(req.Body, linearBodyLimit+1))
	if req.Body != nil {
		_ = req.Body.Close()
	}
	truncated := len(body) > linearBodyLimit
	if truncated {
		body = body[:linearBodyLimit]
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	if readErr != nil {
		// Fail closed: a body we could not fully read is a body we could not
		// classify.
		p.logger.Warn("linear: request body read failed — denying", "agent", agentName)
		return true, "linear: request body unreadable"
	}

	decision := LinearGraphQLAllowed(mode, caps, body, truncated)
	if decision.Allowed {
		return false, ""
	}

	// Operations may be empty when the document could not be classified at
	// all, which is itself the useful signal.
	p.logger.Warn("linear: blocked GraphQL request",
		"agent", agentName,
		"mode", mode.String(),
		"operations", strings.Join(decision.Operations, ","),
		"reason", decision.Reason,
		"mutation", decision.IsMutation)

	reason := "linear: " + decision.Reason
	if len(decision.Operations) > 0 {
		reason += " (" + strings.Join(decision.Operations, ",") + ")"
	}
	return true, reason
}

func githubWriteBodyMayLeak(method, path string) bool {
	if method != http.MethodPost && method != http.MethodPatch && method != http.MethodPut {
		return false
	}
	if !strings.HasPrefix(path, "/repos/") {
		return IsGraphQLPath(path)
	}
	return strings.Contains(path, "/issues") ||
		strings.Contains(path, "/pulls") ||
		strings.Contains(path, "/comments") ||
		strings.Contains(path, "/reviews") ||
		strings.Contains(path, "/git/")
}

func isGitReceivePack(path string) bool {
	return strings.HasSuffix(path, "/git-receive-pack")
}

func isGitPath(path string) bool {
	return strings.HasSuffix(path, "/git-receive-pack") ||
		strings.HasSuffix(path, "/git-upload-pack") ||
		strings.HasSuffix(path, "/info/refs")
}
