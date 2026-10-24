package proxy

import (
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/crux/types"
)

// This file holds the three host services the GitHub proxy consumes without
// importing their Hive implementations (hivecommons/hive#13536, Stage 2
// prerequisite 3). Each is a narrow interface the composition root (cmd/hive)
// satisfies with the real package: pkg/ioscan's canary registry, pkg/tokens'
// inference sink and pkg/github's REST accounting. A proxy with none of them
// installed still enforces every policy; it only stops observing.

// CanaryScanner recognises planted canary tokens (and their encodings) in a
// GitHub write body. agentName is the identified caller, text the raw body and
// source a label for the surface. *ioscan.CanaryRegistry implements it; a nil
// *CanaryRegistry is a registry with no canaries and scans nothing.
type CanaryScanner interface {
	Scan(agentName, text, source string) (types.CanaryLeak, bool)
}

// UsageSink receives per-agent inference token usage the proxy observes on
// upstream responses: Record for the OpenAI-compatible gateway path,
// RecordCopilot and RecordCopilotWithCache for the Copilot sniff path.
// *tokens.InferenceSink implements it; the proxy tolerates a nil sink.
type UsageSink interface {
	Record(agent, model string, inputTokens, outputTokens int64)
	RecordCopilot(agent, model string, inputTokens, outputTokens int64)
	RecordCopilotWithCache(agent, model string, inputTokens, outputTokens, cacheRead, cacheCreation int64)
}

// GitHubRESTAccounting is the host's view of GitHub REST traffic the proxy
// relays: one record per response, split by whether the daemon's injected
// credential (whose rate-limit headers describe the shared core bucket) or
// the agent's own was used, plus the current shared core-bucket snapshot the
// agent GitHub budget reads for its reserve floor. pkg/github's package-level
// RecordRESTRequest / RecordInjectedRESTRequest / CoreRateLimitSnapshot are
// the implementation cmd/hive installs.
type GitHubRESTAccounting interface {
	RecordRESTRequest(caller, method, path string, status int, h http.Header)
	RecordInjectedRESTRequest(caller, method, path string, status int, h http.Header)
	CoreRateLimitSnapshot() (remaining int, reset time.Time, ok bool)
}

// SetGitHubRESTAccounting installs the host's REST accounting. Until it is
// called the proxy records nothing and the agent GitHub budget sees no shared
// core-bucket snapshot (it then applies the per-agent cap alone).
func (p *GitHubProxy) SetGitHubRESTAccounting(a GitHubRESTAccounting) {
	p.restAccounting = a
	if a == nil {
		p.githubReserveSnapshot = nil
		return
	}
	p.githubReserveSnapshot = a.CoreRateLimitSnapshot
}

func (p *GitHubProxy) recordGitHubREST(agentName string, req *http.Request, resp *http.Response) {
	if p.restAccounting == nil {
		return
	}
	if p.injectGHAuth && agentName != internalCallerName {
		p.restAccounting.RecordInjectedRESTRequest("agent:"+agentName, req.Method, req.URL.Path, resp.StatusCode, resp.Header)
		return
	}
	p.restAccounting.RecordRESTRequest("agent:"+agentName, req.Method, req.URL.Path, resp.StatusCode, resp.Header)
}

// The three record helpers make a missing sink a no-op, the contract the
// previous concrete *tokens.InferenceSink field gave for free on a nil
// pointer.

func (p *GitHubProxy) recordUsage(agent, model string, in, out int64) {
	if p.tokenSink != nil {
		p.tokenSink.Record(agent, model, in, out)
	}
}

func (p *GitHubProxy) recordCopilotUsage(agent, model string, in, out int64) {
	if p.tokenSink != nil {
		p.tokenSink.RecordCopilot(agent, model, in, out)
	}
}

func (p *GitHubProxy) recordCopilotUsageWithCache(agent, model string, in, out, cacheRead, cacheCreation int64) {
	if p.tokenSink != nil {
		p.tokenSink.RecordCopilotWithCache(agent, model, in, out, cacheRead, cacheCreation)
	}
}
